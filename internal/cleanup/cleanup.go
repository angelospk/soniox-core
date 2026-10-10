// Package cleanup runs a transcript through Gemini for light correction without
// changing the speaker's words, voice, meaning, or language. It fails safe: any
// error returns an error so the caller can keep the raw text.
package cleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// DefaultModel: 2.5-flash-lite has stable sub-second latency for this task.
// 3.1-flash-lite (preview) measured a spiky 1–11s for the same output.
const DefaultModel = "gemini-2.5-flash-lite"

// DefaultModels is the fallback chain tried in order. The two fast Gemini-lite
// models sit in separate free-tier quota buckets, so a 429 on one often clears on
// the next; gemma-4-31b-it is the last resort — slower (it's a thinking model,
// ~15-40s) but on a SEPARATE Gemma quota that stays available when Gemini is
// rate-limited, and it cleans Greek/English transcripts very well.
var DefaultModels = []string{
	"gemini-2.5-flash-lite", "gemini-3.1-flash-lite", "gemini-2.0-flash-lite", "gemma-4-31b-it",
}

// isGemma reports whether a model is a Gemma model. Gemma is served on the Gemini
// API but rejects thinkingConfig (HTTP 400) and emits separate "thought" parts.
func isGemma(model string) bool { return strings.Contains(strings.ToLower(model), "gemma") }

// genConfig builds the per-model generationConfig. Only non-Gemma (Gemini) models
// accept thinkingConfig — sending it to Gemma returns 400 "Thinking budget is not
// supported for this model."
func genConfig(model string) map[string]any {
	cfg := map[string]any{"temperature": 0.1, "topP": 0.5}
	if !isGemma(model) {
		// cleanup is mechanical — skip the model's "thinking" pass to cut latency
		cfg["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
	}
	return cfg
}

const instructions = `You clean up a raw speech-to-text transcript into a lightly condensed version that still reads exactly like this person talking.

KEEP — never lose:
- Every distinct point, fact, name, number, decision, question and request the speaker made.
- The speaker's own words, vocabulary, tone and meaning. Do NOT make it formal, polished or AI-sounding.
- The original language(s). The text may mix Greek and English — keep every word in the language it was spoken. NEVER translate.

REMOVE — only this:
- Filler and hesitations (εεε, χμμ, ε…), stutters, and accidental immediate repetitions.
- False starts and self-corrections — keep only the final intent the speaker settled on.

FIX:
- Obvious mis-transcriptions, spelling, capitalization, punctuation and sentence breaks.

VOCABULARY: use the list ONLY to correct a word clearly mis-heard as a near-match of a listed term. Never insert a listed term that was not said. Never mention or explain the vocabulary.

Output ONLY the cleaned transcript. No preamble, explanations, quotes, markdown fences or em dashes.`

// BuildPrompt assembles the full prompt with the default instructions.
func BuildPrompt(text, vocab string) string {
	return buildPromptBody(instructions, text, vocab, "", "")
}

// langDirective returns the forced-output-language block, or "" for auto/empty/
// unknown (preserve the spoken language). It is placed before the profile body
// and explicitly overrides any "never translate" rule the body may contain.
func langDirective(lang string) string {
	switch strings.ToLower(strings.TrimSpace(lang)) {
	case "el":
		return "OUTPUT LANGUAGE: Write the entire output in Greek. Render in Greek " +
			"anything transcribed in another language, EXCEPT keep verbatim the " +
			"technical or proper terms normally written in English (product names, " +
			"code, shell commands, library/API/file names, paths). This language " +
			"rule overrides any \"never translate\" or \"preserve language\" " +
			"instruction below."
	case "en":
		return "OUTPUT LANGUAGE: Write the entire output in English. Render in English " +
			"anything transcribed in another language, EXCEPT keep verbatim proper " +
			"names and code. This language rule overrides any \"never translate\" or " +
			"\"preserve language\" instruction below."
	default:
		return ""
	}
}

// buildPromptBody assembles the full prompt by concatenation (no format string),
// so braces in the transcript or vocab are never treated as placeholders. body is
// the profile-specific instruction block; adHoc is an optional one-off instruction
// appended AFTER the body (subordinate to its rules); lang optionally forces output
// language.
func buildPromptBody(body, text, vocab, lang, adHoc string) string {
	if strings.TrimSpace(vocab) == "" {
		vocab = "(none)"
	}
	if strings.TrimSpace(body) == "" {
		body = instructions
	}
	if d := langDirective(lang); d != "" {
		body = d + "\n\n" + body
	}
	if strings.TrimSpace(adHoc) != "" {
		body += "\n\n# Έκτακτη οδηγία ΜΟΝΟ για αυτό το κείμενο (ακολούθησέ την, αλλά κράτα τους κανόνες πάνω):\n" + strings.TrimSpace(adHoc)
	}
	return body +
		"\n\nVOCABULARY (correct spellings this speaker uses):\n" + vocab +
		"\n\nRAW TRANSCRIPT:\n" + text
}

// Sanitize strips a stray ```...``` fence the model may have added.
func Sanitize(out string) string {
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "```") {
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
		out = strings.TrimSuffix(strings.TrimSpace(out), "```")
	}
	return strings.TrimSpace(out)
}

// tooShort guards against truncation / over-trimming: a real cleanup stays close
// in length to the input.
func tooShort(out, in string) bool {
	return len([]rune(out)) < int(0.6*float64(len([]rune(in))))
}

// accept reports whether the cleaned output should replace the raw input. Empty
// output is always rejected; a large shrink is rejected unless this is a
// summarizing profile (allowShrink).
func accept(out, in string, allowShrink bool) bool {
	if strings.TrimSpace(out) == "" {
		return false
	}
	if !allowShrink && tooShort(out, in) {
		return false
	}
	if looksTruncated(out) {
		return false
	}
	return true
}

// truncatedTail are GR/EN function words an output should almost never end on;
// ending there usually means the generation was cut off mid-sentence.
var truncatedTail = map[string]bool{
	"και": true, "ή": true, "να": true, "θα": true, "που": true, "ο": true,
	"η": true, "το": true, "οι": true, "τα": true, "της": true, "του": true,
	"των": true, "στο": true, "στη": true, "στην": true, "στον": true, "με": true,
	"σε": true, "για": true, "από": true, "ως": true, "αλλά": true,
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true,
	"of": true, "in": true, "on": true, "for": true, "with": true, "but": true,
}

// looksTruncated flags output that ends mid-thought (trailing function word) or
// has an unbalanced ``` fence — both signal a cut-off or malformed generation.
func looksTruncated(out string) bool {
	out = strings.TrimSpace(out)
	if out == "" {
		return false
	}
	if strings.Count(out, "```")%2 != 0 {
		return true
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return false
	}
	last := strings.ToLower(strings.TrimFunc(fields[len(fields)-1], func(r rune) bool {
		return !unicode.IsLetter(r)
	}))
	return truncatedTail[last]
}

// Outcome categorises how a cleanup attempt resolved. The values are
// non-overlapping so logs and the client can act on them unambiguously.
type Outcome string

const (
	Cleaned          Outcome = "cleaned"           // AI output accepted
	FallbackShort    Outcome = "fallback_short"    // AI output rejected by guard → raw
	FallbackError    Outcome = "fallback_error"    // AI failed/timed out → raw
	FallbackDisabled Outcome = "fallback_disabled" // cleanup off / no key → raw
	RawTooShort      Outcome = "raw_too_short"     // input skipped before AI
	Skipped          Outcome = "skipped"           // /live?autoclean=0 — LLM cleanup not run
)

// Result is the full outcome of a cleanup call. Text is always safe to use
// (cleaned output, or raw fallback); Outcome/Reason explain what happened.
type Result struct {
	Text      string
	Outcome   Outcome
	Reason    string
	Model     string // the model that produced Text (the one that succeeded), if any
	InLen     int
	OutLen    int
	Ratio     float64
	Attempts  int // how many models were tried
	ElapsedMs int64
}

func (r Result) withMetrics(in string, start time.Time) Result {
	r.InLen = len([]rune(in))
	r.OutLen = len([]rune(r.Text))
	if r.InLen > 0 {
		r.Ratio = float64(r.OutLen) / float64(r.InLen)
	}
	r.ElapsedMs = time.Since(start).Milliseconds()
	return r
}

// modelFunc is the injectable model seam: real Gemini/Gemma in production, a fake
// in tests. It receives the model id and the fully-assembled prompt and returns
// the raw model text (thought parts already stripped).
type modelFunc func(ctx context.Context, model, prompt string) (string, error)

// Options configure a Clean call.
type Options struct {
	Model         string   // single model; ignored if Models is set
	Models        []string // fallback chain, tried in order; empty → DefaultModels
	Vocab         string
	Body          string        // profile instructions; empty = default
	AdHoc         string        // one-off instruction appended after Body; "" = none
	Lang          string        // forced output language: "el"/"en"; "" or "auto" = preserve
	AllowShrink   bool          // summarizing profile — skip the tooShort length guard
	Timeout       time.Duration // per-attempt (per-model) base timeout
	TotalDeadline time.Duration // hard cap across the whole chain
	caller        modelFunc     // test seam; nil = real Gemini/Gemma
}

// modelChain returns the ordered list of models to try: explicit Models, else a
// single explicit Model, else the built-in DefaultModels chain.
func (o Options) modelChain() []string {
	if len(o.Models) > 0 {
		return o.Models
	}
	if o.Model != "" {
		return []string{o.Model}
	}
	return DefaultModels
}

func (o Options) withDefaults() Options {
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.TotalDeadline == 0 {
		// Long enough to let the chain reach the slow Gemma fallback on a bad day.
		o.TotalDeadline = 60 * time.Second
	}
	return o
}

// attemptTimeout scales the per-attempt budget with input length (longer text
// legitimately takes longer), caps it, and never exceeds the remaining deadline.
func attemptTimeout(base time.Duration, inLen int, remaining time.Duration) time.Duration {
	scaled := base + time.Duration(inLen/1000)*3*time.Second
	if scaled > 25*time.Second {
		scaled = 25 * time.Second
	}
	if scaled > remaining {
		scaled = remaining
	}
	return scaled
}

// sanitizeReason maps a model error to a short, non-sensitive category so logs
// never leak request/response bodies.
func sanitizeReason(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline"), strings.Contains(s, "timeout"), strings.Contains(s, "Timeout"):
		return "timeout"
	case strings.Contains(s, "HTTP 429"):
		return "rate_limited"
	case strings.Contains(s, "HTTP 5"):
		return "server_error"
	case strings.Contains(s, "HTTP 4"):
		return "client_error"
	case strings.Contains(s, "no candidates"):
		return "no_candidates"
	default:
		return "error"
	}
}

// backoff sleeps a short, growing delay between attempts, bounded by the deadline.
func backoff(attempt int, deadline time.Time) {
	d := time.Duration(150*(attempt+1)) * time.Millisecond
	if r := time.Until(deadline); d > r {
		d = r
	}
	if d > 0 {
		time.Sleep(d)
	}
}

type geminiReq struct {
	Contents         []content      `json:"contents"`
	GenerationConfig map[string]any `json:"generationConfig"`
}
type content struct {
	Parts []part `json:"parts"`
}
type part struct {
	Text string `json:"text"`
	// Thought marks a Gemma/thinking-model reasoning part. It must be EXCLUDED from
	// the cleaned output (otherwise the model's internal reasoning leaks into the
	// pasted text). omitempty so request parts never send it.
	Thought bool `json:"thought,omitempty"`
}
type geminiResp struct {
	Candidates []struct {
		Content content `json:"content"`
	} `json:"candidates"`
}

// extractAnswer joins the answer parts of the first candidate, skipping any
// "thought" parts (Gemma and other thinking models emit reasoning as separate
// thought parts that must not appear in the output).
func extractAnswer(gr geminiResp) string {
	if len(gr.Candidates) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, p := range gr.Candidates[0].Content.Parts {
		if p.Thought {
			continue
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// realModel is the production modelFunc: it hits the live Gemini/Gemma endpoint
// with per-model generationConfig (no thinkingConfig for Gemma) and strips thought
// parts from the response.
func realModel(apiKey string) modelFunc {
	return func(ctx context.Context, model, prompt string) (string, error) {
		body, _ := json.Marshal(geminiReq{
			Contents:         []content{{Parts: []part{{Text: prompt}}}},
			GenerationConfig: genConfig(model),
		})
		url := fmt.Sprintf(
			"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s",
			model, apiKey)
		return callOnce(ctx, url, body)
	}
}

// CleanResult runs cleanup through the model fallback chain and returns a
// fully-categorised Result. Text is always safe to use: cleaned output on success,
// raw fallback on rejection/failure. Each model is tried once, in order; on a
// transport error (429/5xx/timeout/400) it falls through to the next model. A
// complete-but-rejected response (too short / truncated) is NOT retried — that is a
// quality signal, not an availability one. The whole chain is bounded by TotalDeadline.
func CleanResult(ctx context.Context, apiKey, text string, opt Options) Result {
	start := time.Now()
	if strings.TrimSpace(text) == "" {
		return Result{Text: text, Outcome: RawTooShort}.withMetrics(text, start)
	}
	if apiKey == "" {
		return Result{Text: text, Outcome: FallbackDisabled, Reason: "no_key"}.withMetrics(text, start)
	}
	opt = opt.withDefaults()
	caller := opt.caller
	if caller == nil {
		caller = realModel(apiKey)
	}
	prompt := buildPromptBody(opt.Body, text, opt.Vocab, opt.Lang, opt.AdHoc)
	inLen := len([]rune(text))
	chain := opt.modelChain()

	deadline := time.Now().Add(opt.TotalDeadline)
	reason := "no_attempt"
	tried := 0
	for i, model := range chain {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			reason = "deadline"
			break
		}
		tried++
		actx, cancel := context.WithTimeout(ctx, attemptTimeout(opt.Timeout, inLen, remaining))
		out, err := caller(actx, model, prompt)
		cancel()
		if err != nil {
			reason = sanitizeReason(err)
			backoff(i, deadline) // brief, jitter-free pause before the next model
			continue
		}
		out = Sanitize(out)
		if accept(out, text, opt.AllowShrink) {
			return Result{Text: out, Outcome: Cleaned, Model: model, Attempts: tried}.withMetrics(text, start)
		}
		// A complete-but-rejected response is a quality signal — keep raw, don't
		// burn the rest of the chain (and the slow Gemma fallback) on it.
		return Result{Text: text, Outcome: FallbackShort, Reason: "rejected", Model: model, Attempts: tried}.withMetrics(text, start)
	}
	return Result{Text: text, Outcome: FallbackError, Reason: reason, Attempts: tried}.withMetrics(text, start)
}

// Clean is the back-compat wrapper around CleanResult. It returns the usable text
// and a non-nil error only when all model attempts failed, so existing callers
// keep their raw-fallback branch.
func Clean(ctx context.Context, apiKey, text string, opt Options) (string, error) {
	r := CleanResult(ctx, apiKey, text, opt)
	if r.Outcome == FallbackError {
		return r.Text, fmt.Errorf("cleanup failed: %s", r.Reason)
	}
	return r.Text, nil
}

func callOnce(ctx context.Context, url string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return "", fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, b.String())
	}
	var gr geminiResp
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", err
	}
	if len(gr.Candidates) == 0 {
		return "", fmt.Errorf("gemini: no candidates")
	}
	return extractAnswer(gr), nil
}
