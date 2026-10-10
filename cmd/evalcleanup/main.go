// Command evalcleanup runs real transcripts through baseline vs candidate cleanup
// prompts, scores each output with deterministic gates + an LLM judge, prints a
// comparison table, and (with -apply) writes a candidate profile to the live
// prompts dir only when it beats the baseline without regressing on the hard
// gates (faithfulness, completeness, no-translation, no vocab-bleed).
//
// Usage:
//
//	GEMINI_API_KEY=… go run ./cmd/evalcleanup \
//	  -baseline ~/.soniox-dictate/prompts -candidate ./prompts-candidate \
//	  -profiles default,message,notes,handoff,summary -n 6 [-apply]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/angelospk/soniox-core/internal/cleanup"
)

func main() {
	home, _ := os.UserHomeDir()
	baseDir := flag.String("baseline", filepath.Join(home, ".soniox-dictate", "prompts"), "baseline prompts dir (live)")
	candDir := flag.String("candidate", "./prompts-candidate", "candidate prompts dir")
	sessions := flag.String("sessions", filepath.Join(home, ".soniox-dictate", "dictations", "sessions.jsonl"), "sessions.jsonl")
	vocabPath := flag.String("vocab", filepath.Join(home, ".soniox-dictate", "vocabulary.txt"), "vocabulary file")
	profilesCSV := flag.String("profiles", "default,message,notes,handoff,summary", "profiles to evaluate")
	n := flag.Int("n", 6, "number of transcripts to sample (longest)")
	minN := flag.Int("min", 4, "minimum samples required to auto-apply")
	apply := flag.Bool("apply", false, "write winning candidate profiles to the baseline (live) dir")
	paceSec := flag.Int("pace", 4, "min seconds between Gemini calls (rate-limit pacing)")
	flag.Parse()
	paceGap = time.Duration(*paceSec) * time.Second

	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "set GEMINI_API_KEY")
		os.Exit(1)
	}

	transcripts := loadTranscripts(*sessions, *n)
	if len(transcripts) == 0 {
		fmt.Fprintln(os.Stderr, "no transcripts found")
		os.Exit(1)
	}
	vocab := loadVocab(*vocabPath)
	baseProfiles, err := cleanup.LoadProfiles(*baseDir)
	must(err)
	candProfiles, err := cleanup.LoadProfiles(*candDir)
	must(err)

	fmt.Printf("Evaluating %d transcripts × %d profiles (judge: gemini-2.5-flash)\n\n", len(transcripts), len(strings.Split(*profilesCSV, ",")))

	for _, prof := range strings.Split(*profilesCSV, ",") {
		prof = strings.TrimSpace(prof)
		bp, ok1 := baseProfiles[prof]
		cp, ok2 := candProfiles[prof]
		if !ok1 || !ok2 {
			fmt.Printf("== %s: missing in baseline(%v)/candidate(%v), skipping\n", prof, ok1, ok2)
			continue
		}
		var base, cand agg
		fmt.Printf("== profile: %s ==\n", prof)
		for i, tr := range transcripts {
			b := scoreOne(key, tr, bp, vocab)
			c := scoreOne(key, tr, cp, vocab)
			base.add(b)
			cand.add(c)
			fmt.Printf("  [%d] in=%-5d  base: %s   cand: %s\n", i+1, len([]rune(tr)), b.short(), c.short())
		}
		fmt.Printf("  ---- means over %d ----\n", base.count)
		fmt.Printf("  baseline : %s\n", base.summary())
		fmt.Printf("  candidate: %s\n", cand.summary())
		win, why := decide(base, cand, *minN)
		fmt.Printf("  verdict  : %s (%s)\n", map[bool]string{true: "CANDIDATE WINS", false: "keep baseline"}[win], why)
		if win && *apply {
			src, err := joinUnderBase(*candDir, prof+".md")
			if err != nil {
				fmt.Printf("  APPLY SKIPPED: %s\n", err)
				continue
			}
			dst, err := joinUnderBase(*baseDir, prof+".md")
			if err != nil {
				fmt.Printf("  APPLY SKIPPED: %s\n", err)
				continue
			}
			data, e := os.ReadFile(src) // #nosec G304 -- operator-supplied dirs
			if e == nil {
				must(os.WriteFile(dst, data, 0o644)) // #nosec G306, G703 -- prompt file, not a secret; dst verified under baseDir by joinUnderBase
				fmt.Printf("  APPLIED  : %s -> %s\n", src, dst)
			}
		}
		fmt.Println()
	}
}

// ---- scoring ----

type score struct {
	completeness, faithfulness, formatting float64
	numFrac, latinFrac                     float64
	translation, bleed                     int // violation counts (0/1 per sample)
	outcome                                string
}

func (s score) overall() float64 { return (s.completeness + s.faithfulness + s.formatting) / 3 }
func (s score) short() string {
	return fmt.Sprintf("C%.1f F%.1f Fmt%.1f num%.0f%% lat%.0f%% tr%d bl%d %s",
		s.completeness, s.faithfulness, s.formatting, s.numFrac*100, s.latinFrac*100, s.translation, s.bleed, s.outcome)
}

func scoreOne(key, raw string, p cleanup.Profile, vocab []string) score {
	out, outcome := genCleanup(key, raw, p, vocab)
	s := score{outcome: outcome}
	s.numFrac = preservedFrac(numbers(raw), out)
	s.latinFrac = preservedFrac(latinTokens(raw), strings.ToLower(out))
	if translated(raw, out) {
		s.translation = 1
	}
	s.bleed = vocabBleed(vocab, raw, out)
	if outcome == "cleaned" {
		j := judge(key, raw, out, p)
		s.completeness, s.faithfulness, s.formatting = j.Completeness, j.Faithfulness, j.Formatting
	}
	// A failed generation (rate-limited/error) scores 0 so it never wins; the
	// printed outcome makes the reason visible.
	return s
}

// genCleanup builds the cleanup prompt for a profile and runs it through a paced,
// 429-aware Gemini call. Returns the (sanitized, accepted) output or the raw text
// with a fallback outcome. Decoupled from cleanup.CleanResult so the eval controls
// pacing across hundreds of calls.
func genCleanup(key, raw string, p cleanup.Profile, vocab []string) (string, string) {
	allowShrink := p.Summarize && strings.TrimSpace(p.Body) != ""
	v := strings.Join(vocab, "\n")
	if strings.TrimSpace(v) == "" {
		v = "(none)"
	}
	prompt := p.Body + "\n\nVOCABULARY (correct spellings this speaker uses):\n" + v +
		"\n\nRAW TRANSCRIPT:\n" + raw
	txt, ok := geminiRetry(key, cleanup.DefaultModel, prompt)
	if !ok {
		return raw, "fallback_error"
	}
	out := cleanup.Sanitize(txt)
	if strings.TrimSpace(out) == "" {
		return raw, "fallback_short"
	}
	if !allowShrink && len([]rune(out)) < int(0.6*float64(len([]rune(raw)))) {
		return raw, "fallback_short"
	}
	return out, "cleaned"
}

type agg struct {
	count                                  int
	completeness, faithfulness, formatting float64
	numFrac, latinFrac                     float64
	translation, bleed                     int
}

func (a *agg) add(s score) {
	a.count++
	a.completeness += s.completeness
	a.faithfulness += s.faithfulness
	a.formatting += s.formatting
	a.numFrac += s.numFrac
	a.latinFrac += s.latinFrac
	a.translation += s.translation
	a.bleed += s.bleed
}
func (a agg) mean(v float64) float64 {
	if a.count == 0 {
		return 0
	}
	return v / float64(a.count)
}
func (a agg) meanOverall() float64 {
	return (a.mean(a.completeness) + a.mean(a.faithfulness) + a.mean(a.formatting)) / 3
}
func (a agg) summary() string {
	return fmt.Sprintf("overall %.2f | C %.2f F %.2f Fmt %.2f | num %.0f%% lat %.0f%% | translations %d bleed %d",
		a.meanOverall(), a.mean(a.completeness), a.mean(a.faithfulness), a.mean(a.formatting),
		a.mean(a.numFrac)*100, a.mean(a.latinFrac)*100, a.translation, a.bleed)
}

// decide implements gated auto-apply: candidate wins only if it has enough samples,
// is better overall, and regresses on NO hard gate.
func decide(base, cand agg, minN int) (bool, string) {
	if cand.count < minN {
		return false, fmt.Sprintf("only %d samples (<%d)", cand.count, minN)
	}
	if cand.translation > base.translation {
		return false, "more translation violations"
	}
	if cand.bleed > base.bleed {
		return false, "more vocab bleed"
	}
	if cand.mean(cand.faithfulness) < base.mean(base.faithfulness)-0.05 {
		return false, "faithfulness regression"
	}
	if cand.mean(cand.completeness) < base.mean(base.completeness)-0.05 {
		return false, "completeness regression"
	}
	if cand.meanOverall() <= base.meanOverall() {
		return false, fmt.Sprintf("not better overall (%.2f vs %.2f)", cand.meanOverall(), base.meanOverall())
	}
	return true, fmt.Sprintf("better overall (%.2f vs %.2f), no hard-gate regression", cand.meanOverall(), base.meanOverall())
}

// ---- deterministic gates ----

var numRe = regexp.MustCompile(`\d+`)
var latinRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{2,}`)
var modelRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validModel reports whether model is a plain Gemini model id safe to
// interpolate into the fixed generativelanguage.googleapis.com path: no
// slashes, query strings, fragments or URL metacharacters that could reshape
// the request URL.
func validModel(model string) bool { return modelRe.MatchString(model) }

func numbers(s string) []string { return uniq(numRe.FindAllString(s, -1)) }
func latinTokens(s string) []string {
	var out []string
	for _, t := range latinRe.FindAllString(s, -1) {
		out = append(out, strings.ToLower(t))
	}
	return uniq(out)
}
func preservedFrac(items []string, hay string) float64 {
	if len(items) == 0 {
		return 1
	}
	got := 0
	for _, it := range items {
		if strings.Contains(hay, it) {
			got++
		}
	}
	return float64(got) / float64(len(items))
}

// translated flags a likely translation: a Greek-dominant input whose output lost
// almost all Greek (or vice versa).
func translated(raw, out string) bool {
	rg := greekRatio(raw)
	og := greekRatio(out)
	if rg > 0.5 && og < 0.2 {
		return true
	}
	if rg < 0.1 && og > 0.5 {
		return true
	}
	return false
}
func greekRatio(s string) float64 {
	var letters, greek int
	for _, r := range s {
		if unicode.IsLetter(r) {
			letters++
			if r >= 0x0370 && r <= 0x03FF || r >= 0x1F00 && r <= 0x1FFF {
				greek++
			}
		}
	}
	if letters == 0 {
		return 0
	}
	return float64(greek) / float64(letters)
}

func vocabBleed(vocab []string, raw, out string) int {
	rl, ol := strings.ToLower(raw), strings.ToLower(out)
	n := 0
	for _, v := range vocab {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if strings.Contains(ol, v) && !strings.Contains(rl, v) {
			n++
		}
	}
	return n
}

// ---- LLM judge ----

type judgeResult struct {
	Completeness float64 `json:"completeness"`
	Faithfulness float64 `json:"faithfulness"`
	Formatting   float64 `json:"formatting"`
	Note         string  `json:"note"`
}

func judge(key, raw, out string, p cleanup.Profile) judgeResult {
	intent := firstLine(p.Body)
	prompt := "You evaluate a cleanup of a raw Greek/English speech-to-text transcript. " +
		"Score on a 1-5 scale (integers ok):\n" +
		"- completeness: did OUTPUT preserve every distinct point, decision, number and name from RAW? 5=nothing lost.\n" +
		"- faithfulness: did OUTPUT avoid inventing anything not in RAW, and avoid translating to another language? 5=fully faithful.\n" +
		"- formatting: is OUTPUT clean and does it follow the stated profile intent, with no preamble/quotes/fences? 5=perfect.\n" +
		"PROFILE INTENT: " + intent + "\n\nRAW:\n" + raw + "\n\nOUTPUT:\n" + out + "\n\n" +
		`Return ONLY JSON: {"completeness":N,"faithfulness":N,"formatting":N,"note":"<=12 words"}`
	txt, ok := geminiRetry(key, "gemini-2.5-flash", prompt)
	if !ok {
		return judgeResult{Completeness: 3, Faithfulness: 3, Formatting: 3, Note: "judge_unavailable"}
	}
	txt = stripFence(txt)
	var jr judgeResult
	if err := json.Unmarshal([]byte(txt), &jr); err != nil {
		return judgeResult{Completeness: 3, Faithfulness: 3, Formatting: 3, Note: "parse_error"}
	}
	return jr
}

// ---- paced, 429-aware Gemini call ----

var (
	paceMu   sync.Mutex
	lastCall time.Time
	paceGap  = 4 * time.Second // min interval between calls; tune with -pace
)

// pace blocks until at least paceGap has elapsed since the previous call, so the
// eval stays under the key's requests-per-minute limit.
func pace() {
	paceMu.Lock()
	defer paceMu.Unlock()
	if wait := paceGap - time.Since(lastCall); wait > 0 {
		time.Sleep(wait)
	}
	lastCall = time.Now()
}

// geminiRetry makes a paced call and, on a 429 or transport error, backs off
// (15s, 30s, 60s) and retries. Returns (text, true) on success.
func geminiRetry(key, model, prompt string) (string, bool) {
	backoffs := []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second}
	for attempt := 0; ; attempt++ {
		pace()
		txt, status, err := callGemini(key, model, prompt)
		if err == nil && status == http.StatusOK && strings.TrimSpace(txt) != "" {
			return txt, true
		}
		if status != http.StatusTooManyRequests && err == nil && status != 0 {
			// a non-retryable HTTP error (4xx other than 429, etc.)
			return "", false
		}
		if attempt >= len(backoffs) {
			return "", false
		}
		fmt.Printf("    (rate-limited/err status=%d; backing off %s)\n", status, backoffs[attempt])
		time.Sleep(backoffs[attempt])
	}
}

func callGemini(key, model, prompt string) (string, int, error) {
	if !validModel(model) {
		return "", 0, fmt.Errorf("invalid model: %q", model)
	}
	body, _ := json.Marshal(map[string]any{
		"contents":         []any{map[string]any{"parts": []any{map[string]any{"text": prompt}}}},
		"generationConfig": map[string]any{"temperature": 0, "thinkingConfig": map[string]any{"thinkingBudget": 0}},
	})
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", model, key)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body)) // #nosec G704 -- fixed Google endpoint, model allow-listed by validModel
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- request URL is the fixed endpoint above
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, nil
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.NewDecoder(resp.Body).Decode(&gr) != nil || len(gr.Candidates) == 0 {
		return "", resp.StatusCode, nil
	}
	var sb strings.Builder
	for _, p := range gr.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	return sb.String(), http.StatusOK, nil
}

// ---- io helpers ----

func loadTranscripts(path string, n int) []string {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	var raws []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Raw string `json:"raw"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		r := strings.TrimSpace(rec.Raw)
		if len([]rune(r)) < 200 || seen[r] {
			continue
		}
		seen[r] = true
		raws = append(raws, r)
	}
	sort.Slice(raws, func(i, j int) bool { return len([]rune(raws[i])) > len([]rune(raws[j])) })
	if n < len(raws) {
		raws = raws[:n]
	}
	return raws
}

func loadVocab(path string) []string {
	b, err := os.ReadFile(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			out = append(out, ln)
		}
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// joinUnderBase joins name onto baseDir and returns an error if the result
// escapes baseDir (e.g. a profile name containing "../"), so an -apply write
// can never land outside the operator's prompts dir.
func joinUnderBase(baseDir, name string) (string, error) {
	p := filepath.Join(baseDir, name)
	// Rel rather than a string-prefix test, so a baseDir of "." still works.
	rel, err := filepath.Rel(baseDir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes base dir: %q", name)
	}
	return p, nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
