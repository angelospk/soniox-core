// Package weekly turns recurring cleanup review findings into a weekly report
// and the rule/vocabulary edits that fix them for good.
package weekly

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/angelospk/soniox-core/internal/cleanup"
)

// Sep joins the texts of one chunk.
const Sep = "\n\n"

// exampleContext is how many bytes of context Collect keeps on each side of
// a finding when building an Item example.
const exampleContext = 40

type sessionLine struct {
	StartedAt string `json:"started_at"`
	Text      string `json:"text"`
	Raw       string `json:"raw"`
}

// LoadTexts reads sessions JSONL from path and returns the texts of the
// sessions that started at or after since. It prefers text and falls back to
// raw when text is blank. Malformed lines and bad timestamps are skipped.
func LoadTexts(path string, since time.Time) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var texts []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var sl sessionLine
		if err := json.Unmarshal(line, &sl); err != nil {
			continue
		}
		started, err := time.Parse(time.RFC3339, sl.StartedAt)
		if err != nil || started.Before(since) {
			continue
		}
		text := sl.Text
		if strings.TrimSpace(text) == "" {
			text = sl.Raw
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		texts = append(texts, text)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return texts, nil
}

// Chunks greedily joins texts with Sep while the chunk stays within max bytes.
// A single text longer than max is truncated at a rune boundary and becomes
// its own chunk.
func Chunks(texts []string, max int) []string {
	if len(texts) == 0 || max <= 0 {
		return nil
	}
	var out []string
	cur := ""
	for _, t := range texts {
		if len(t) > max {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			out = append(out, truncateRunes(t, max))
			continue
		}
		if cur == "" {
			cur = t
			continue
		}
		if len(cur)+len(Sep)+len(t) <= max {
			cur += Sep + t
			continue
		}
		out = append(out, cur)
		cur = t
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ChunkFindings pairs a chunk's text with the findings the review returned
// for it.
type ChunkFindings struct {
	Text     string
	Findings []cleanup.Finding
}

// Collect runs review once per chunk in order and keeps every successful
// result, including results with no findings. Failed chunks are skipped.
func Collect(ctx context.Context, chunks []string, review func(context.Context, string) ([]cleanup.Finding, error)) []ChunkFindings {
	var out []ChunkFindings
	for _, c := range chunks {
		findings, err := review(ctx, c)
		if err != nil {
			continue
		}
		out = append(out, ChunkFindings{Text: c, Findings: findings})
	}
	return out
}

// Item is one aggregated finding: how often it recurred, where it first
// occurred, and the rule it proposes.
type Item struct {
	ID      int    `json:"id"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	To      string `json:"to"`
	Count   int    `json:"count"`
	Example string `json:"example"`
	Why     string `json:"why"`
}

type aggregateKey struct {
	kind string
	from string
	to   string
}

// Aggregate groups findings by kind, case-insensitive from, and to. It ranks
// groups by count descending, then from ascending, and assigns IDs from 1.
func Aggregate(in []ChunkFindings) []Item {
	var items []Item
	index := make(map[aggregateKey]int)
	for _, cf := range in {
		for _, f := range cf.Findings {
			key := aggregateKey{kind: f.Kind, from: strings.ToLower(f.From), to: f.To}
			if i, ok := index[key]; ok {
				items[i].Count++
				continue
			}
			index[key] = len(items)
			items = append(items, Item{
				Kind:    f.Kind,
				From:    f.From,
				To:      f.To,
				Count:   1,
				Example: example(cf.Text, f),
				Why:     f.Why,
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Count != items[j].Count {
			return items[i].Count > items[j].Count
		}
		return items[i].From < items[j].From
	})
	for i := range items {
		items[i].ID = i + 1
	}
	return items
}

func example(text string, f cleanup.Finding) string {
	start, end := f.Start, f.End
	if !(0 <= start && start < end && end <= len(text) && text[start:end] == f.From) {
		idx := strings.Index(text, f.From)
		if idx < 0 {
			return f.From
		}
		start, end = idx, idx+len(f.From)
	}

	lo := start - exampleContext
	if lo < 0 {
		lo = 0
	}
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	hi := end + exampleContext
	if hi > len(text) {
		hi = len(text)
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}

	var b strings.Builder
	if lo > 0 {
		b.WriteString("…")
	}
	b.WriteString(text[lo:hi])
	if hi < len(text) {
		b.WriteString("…")
	}
	return b.String()
}

// Apply appends rule lines for ids and vocabulary terms for vocabIDs to the
// corrections and vocabulary files. It validates every id before writing, so
// an unknown id leaves both files untouched. It returns the number of lines
// added.
func Apply(items []Item, ids, vocabIDs []int, correctionsPath, vocabPath string) (int, error) {
	byID := make(map[int]Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}
	for _, id := range ids {
		it, ok := byID[id]
		if !ok {
			return 0, fmt.Errorf("weekly: unknown item id %d", id)
		}
		if strings.TrimSpace(it.From) == "" || strings.ContainsAny(it.From+it.To, "\t\r\n") {
			return 0, fmt.Errorf("weekly: item %d is not a safe rule (empty from, or tab/newline)", id)
		}
	}
	for _, id := range vocabIDs {
		it, ok := byID[id]
		if !ok {
			return 0, fmt.Errorf("weekly: unknown item id %d", id)
		}
		if strings.ContainsAny(it.To, "\r\n") {
			return 0, fmt.Errorf("weekly: item %d term has a newline", id)
		}
	}

	corrExisting, err := readOrEmpty(correctionsPath)
	if err != nil {
		return 0, err
	}
	vocabExisting, err := readOrEmpty(vocabPath)
	if err != nil {
		return 0, err
	}

	corrSeen := make(map[string]bool)
	for _, line := range strings.Split(corrExisting, "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, "\r"), " ")
		from := line
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			from = line[:i]
		}
		corrSeen[from] = true
	}
	vocabSeen := make(map[string]bool)
	for _, line := range strings.Split(vocabExisting, "\n") {
		vocabSeen[strings.TrimSpace(line)] = true
	}

	var addCorr []string
	for _, id := range ids {
		it := byID[id]
		if corrSeen[it.From] {
			continue
		}
		corrSeen[it.From] = true
		addCorr = append(addCorr, it.From+"\t"+it.To)
	}
	var addVocab []string
	for _, id := range vocabIDs {
		it := byID[id]
		term := strings.TrimSpace(it.To)
		if term == "" || vocabSeen[term] {
			continue
		}
		vocabSeen[term] = true
		addVocab = append(addVocab, it.To)
	}

	if len(addCorr) > 0 {
		if err := appendLines(correctionsPath, corrExisting, addCorr); err != nil {
			return 0, err
		}
	}
	if len(addVocab) > 0 {
		if err := appendLines(vocabPath, vocabExisting, addVocab); err != nil {
			return 0, err
		}
	}
	return len(addCorr) + len(addVocab), nil
}

func readOrEmpty(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

func appendLines(path, existing string, lines []string) error {
	var b strings.Builder
	b.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteString("\n")
	}
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// SaveReport writes items as indented JSON to path, creating parent
// directories as needed.
func SaveReport(path string, items []Item) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

// LoadReport reads items from a JSON report written by SaveReport.
func LoadReport(path string) ([]Item, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []Item
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Markdown renders items as a markdown table, one row per item.
func Markdown(items []Item) string {
	var b strings.Builder
	b.WriteString("| # | kind | from | to | count | example |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, it := range items {
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %d | %s |\n",
			it.ID,
			mdCell(it.Kind),
			mdCell(it.From),
			mdCell(it.To),
			it.Count,
			mdCell(it.Example),
		)
	}
	return b.String()
}

func mdCell(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

// DropKnown removes items whose From already has a rule in the corrections file
// (the part before the first tab, leading spaces and \r ignored) and renumbers the
// rest 1..N. A missing file keeps every item.
func DropKnown(items []Item, correctionsPath string) []Item {
	existing, _ := readOrEmpty(correctionsPath)
	known := map[string]bool{}
	for _, line := range strings.Split(existing, "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, "\r"), " ")
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			known[line[:i]] = true
		}
	}
	var out []Item
	for _, it := range items {
		if known[it.From] {
			continue
		}
		it.ID = len(out) + 1
		out = append(out, it)
	}
	return out
}
