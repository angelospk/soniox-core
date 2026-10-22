package weekly

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/angelospk/soniox-core/internal/cleanup"
)

func TestLoadTexts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sessions.jsonl")
	lines := strings.Join([]string{
		`{"started_at":"2026-10-01T10:00:00Z","text":"παλιό"}`,
		`{"started_at":"2026-10-20T10:00:00Z","text":"  "}`,
		`not json`,
		`{"started_at":"2026-10-21T10:00:00Z","text":"φρέσκο κείμενο"}`,
		`{"started_at":"2026-10-21T11:00:00Z","text":"","raw":"μόνο raw"}`,
	}, "\n")
	if err := os.WriteFile(p, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	got, err := LoadTexts(p, since)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"φρέσκο κείμενο", "μόνο raw"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
	if _, err := LoadTexts(filepath.Join(t.TempDir(), "missing"), since); err == nil {
		t.Fatal("want error for missing file")
	}
}

func TestChunks(t *testing.T) {
	got := Chunks([]string{"aaa", "bbb", "ccc"}, 10)
	if len(got) != 2 || got[0] != "aaa"+Sep+"bbb" || got[1] != "ccc" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("α", 20) // 40 bytes
	got = Chunks([]string{long}, 11)
	if strings.Join(got, "") != long {
		t.Fatalf("oversized text lost runes: %q", got)
	}
	for _, c := range got {
		if len(c) > 11 || !utf8.ValidString(c) {
			t.Fatalf("piece not split at a rune boundary: %q", got)
		}
	}
	if got := Chunks(nil, 10); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestCollectSkipsFailedChunks(t *testing.T) {
	calls := 0
	review := func(_ context.Context, text string) ([]cleanup.Finding, error) {
		calls++
		if text == "bad" {
			return nil, errors.New("boom")
		}
		return []cleanup.Finding{{From: "x", To: "y", Kind: cleanup.KindMisheard}}, nil
	}
	got := Collect(context.Background(), []string{"ok", "bad", "ok2"}, review)
	if calls != 3 || len(got) != 2 || got[0].Text != "ok" || got[1].Text != "ok2" {
		t.Fatalf("calls=%d got=%+v", calls, got)
	}
}

func TestAggregateRanksRecurringFirst(t *testing.T) {
	c1 := "πάμε στον Πορταρά αύριο, χμ, ναι"
	c2 := "ο πορταρά είναι ωραίος"
	in := []ChunkFindings{
		{Text: c1, Findings: []cleanup.Finding{
			{Start: 10, End: 24, From: "Πορταρά", To: "Πρωταρά", Kind: cleanup.KindMisheard, Why: "τόπος"},
			{From: "χμ", To: "", Kind: cleanup.KindFiller},
		}},
		{Text: c2, Findings: []cleanup.Finding{
			{From: "πορταρά", To: "Πρωταρά", Kind: cleanup.KindMisheard},
		}},
	}
	items := Aggregate(in)
	if len(items) != 2 {
		t.Fatalf("got %+v", items)
	}
	top := items[0]
	if top.ID != 1 || top.Count != 2 || top.To != "Πρωταρά" || top.Kind != cleanup.KindMisheard {
		t.Fatalf("top = %+v", top)
	}
	if !strings.Contains(top.Example, "Πορταρά") {
		t.Fatalf("example lacks the word: %q", top.Example)
	}
	if items[1].ID != 2 || items[1].Count != 1 || items[1].Kind != cleanup.KindFiller {
		t.Fatalf("second = %+v", items[1])
	}
}

func TestApply(t *testing.T) {
	dir := t.TempDir()
	corr := filepath.Join(dir, "corrections.tsv")
	vocab := filepath.Join(dir, "vocabulary.txt")
	if err := os.WriteFile(corr, []byte("χμ\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{ID: 1, Kind: cleanup.KindMisheard, From: "Πορταρά", To: "Πρωταρά", Count: 2},
		{ID: 2, Kind: cleanup.KindFiller, From: "χμ", To: "", Count: 1},
		{ID: 3, Kind: cleanup.KindNonsense, From: "βλα", To: "μπλα", Count: 1},
	}
	n, err := Apply(items, []int{1, 2}, []int{1}, corr, vocab)
	if err != nil || n != 2 { // one rule + one vocab term; χμ already present
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, err := Apply(items, []int{1, 2}, []int{1}, corr, vocab); err != nil || n != 0 {
		t.Fatalf("not idempotent: n=%d err=%v", n, err)
	}
	b, _ := os.ReadFile(corr)
	if string(b) != "χμ\t\nΠορταρά\tΠρωταρά\n" {
		t.Fatalf("corrections = %q", b)
	}
	v, _ := os.ReadFile(vocab)
	if string(v) != "Πρωταρά\n" {
		t.Fatalf("vocab = %q", v)
	}
	if _, err := Apply(items, []int{9}, nil, corr, vocab); err == nil {
		t.Fatal("want error for unknown id")
	}
}

func TestReportRoundTripAndMarkdown(t *testing.T) {
	items := []Item{{ID: 1, Kind: cleanup.KindMisheard, From: "Πορταρά", To: "Πρωταρά", Count: 2, Example: "…στον Πορταρά αύριο…"}}
	p := filepath.Join(t.TempDir(), "r.json")
	if err := SaveReport(p, items); err != nil {
		t.Fatal(err)
	}
	got, err := LoadReport(p)
	if err != nil || len(got) != 1 || got[0] != items[0] {
		t.Fatalf("got %+v err=%v", got, err)
	}
	md := Markdown(items)
	for _, want := range []string{"| 1 |", "Πορταρά", "Πρωταρά", "2"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown lacks %q:\n%s", want, md)
		}
	}
}

// A model-supplied tab, newline, or empty from would corrupt the TSV rule file.
func TestApplyRejectsUnsafeRules(t *testing.T) {
	dir := t.TempDir()
	corr := filepath.Join(dir, "corrections.tsv")
	items := []Item{
		{ID: 1, Kind: cleanup.KindMisheard, From: "α\tβ", To: "γ"},
		{ID: 2, Kind: cleanup.KindMisheard, From: "δ", To: "ε\nζ"},
		{ID: 3, Kind: cleanup.KindMisheard, From: "  ", To: "η"},
	}
	for _, id := range []int{1, 2, 3} {
		if _, err := Apply(items, []int{id}, nil, corr, filepath.Join(dir, "v.txt")); err == nil {
			t.Fatalf("id %d: want error", id)
		}
	}
	if _, err := os.Stat(corr); !os.IsNotExist(err) {
		t.Fatal("file written despite the error")
	}
}

// Items whose from already has a rule are not offered again; IDs stay 1..N.
func TestDropKnown(t *testing.T) {
	corr := filepath.Join(t.TempDir(), "corrections.tsv")
	if err := os.WriteFile(corr, []byte("# comment\n  Πορταρά\tΠρωταρά\r\nχμ\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{ID: 1, From: "Πορταρά", To: "Πρωταρά", Count: 5},
		{ID: 2, From: "νέο", To: "καινούριο", Count: 3},
		{ID: 3, From: "χμ", Count: 2},
		{ID: 4, From: "άλλο", To: "αλλιώς", Count: 1},
	}
	got := DropKnown(items, corr)
	if len(got) != 2 || got[0].From != "νέο" || got[0].ID != 1 || got[1].From != "άλλο" || got[1].ID != 2 {
		t.Fatalf("got %+v", got)
	}
	if got := DropKnown(items, filepath.Join(t.TempDir(), "missing")); len(got) != 4 {
		t.Fatalf("missing file must keep all items, got %d", len(got))
	}
}

// A dictation longer than one chunk is split, never cut: no text is lost.
func TestChunksSplitsLongTextWithoutLoss(t *testing.T) {
	long := strings.Repeat("λέξη ", 10) // 50 bytes of words... in Greek, 9 bytes per "λέξη "
	got := Chunks([]string{long}, 20)
	if len(got) < 2 {
		t.Fatalf("not split: %q", got)
	}
	var words int
	for _, c := range got {
		if len(c) > 20 || !utf8.ValidString(c) {
			t.Fatalf("bad chunk %q", c)
		}
		words += len(strings.Fields(c))
	}
	if words != 10 {
		t.Fatalf("lost words: %d of 10 in %q", words, got)
	}
}

// Count reflects how often the error occurs, not how often the model reported it.
func TestAggregateCountsOccurrences(t *testing.T) {
	in := []ChunkFindings{{Text: "Πορταρά, πορταρά και ΠΟΡΤΑΡΆ", Findings: []cleanup.Finding{
		{From: "Πορταρά", To: "Πρωταρά", Kind: cleanup.KindMisheard},
	}}}
	if got := Aggregate(in); len(got) != 1 || got[0].Count != 3 { // case-insensitive
		t.Fatalf("got %+v", got)
	}
}

func TestApplyLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	corr := filepath.Join(dir, "corrections.tsv")
	items := []Item{{ID: 1, Kind: cleanup.KindMisheard, From: "α", To: "β"}}
	if _, err := Apply(items, []int{1}, []int{1}, corr, filepath.Join(dir, "v.txt")); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("leftover files: %v", names)
	}
}

// Every run gets its own report, so ids picked from one report never hit another.
func TestReportNameIsUniquePerRun(t *testing.T) {
	a := ReportName(time.Date(2026, 10, 22, 14, 5, 1, 0, time.UTC))
	b := ReportName(time.Date(2026, 10, 22, 14, 5, 2, 0, time.UTC))
	if a == b || !strings.HasSuffix(a, ".json") || !strings.HasPrefix(a, "2026-10-22") {
		t.Fatalf("a=%q b=%q", a, b)
	}
}

// Whole words only, and a key counts once per chunk even if the model reports it
// in two casings.
func TestAggregateCountsWholeWordsOncePerChunk(t *testing.T) {
	in := []ChunkFindings{{Text: "να ναι να", Findings: []cleanup.Finding{
		{From: "να", To: "νά", Kind: cleanup.KindMisheard},
		{From: "Να", To: "νά", Kind: cleanup.KindMisheard},
	}}}
	if got := Aggregate(in); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestSaveNewReportNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 22, 14, 5, 1, 0, time.UTC)
	a, err := SaveNewReport(dir, at, []Item{{ID: 1, From: "a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := SaveNewReport(dir, at, []Item{{ID: 1, From: "b"}})
	if err != nil || a == b {
		t.Fatalf("a=%q b=%q err=%v", a, b, err)
	}
	ia, _ := LoadReport(a)
	if len(ia) != 1 || ia[0].From != "a" {
		t.Fatalf("first report changed: %+v", ia)
	}
}
