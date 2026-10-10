package cleanup

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func rf(from, to, kind, key string) rawFinding {
	return rawFinding{From: from, To: to, Kind: kind, Key: key, Why: "because"}
}

func TestValidateFindingsComputesRanges(t *testing.T) {
	text := "Είπα στον Χάρη Λαμπίδη ότι ναι."
	got := validateFindings(text, []rawFinding{rf("Λαμπίδη", "Λαμπρίδη", KindMisheard, "")}, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 finding, got %d", len(got))
	}
	if text[got[0].Start:got[0].End] != "Λαμπίδη" {
		t.Fatalf("range does not cover the quoted text: %q", text[got[0].Start:got[0].End])
	}
}

// The model quoting something that isn't in the transcript is the main hallucination
// mode; it must never reach the UI as a highlight.
func TestValidateFindingsDropsUnquotedText(t *testing.T) {
	got := validateFindings("Είπα ναι.", []rawFinding{rf("Λαμπίδη", "Λαμπρίδη", KindMisheard, "")}, nil)
	if len(got) != 0 {
		t.Fatalf("want 0, got %+v", got)
	}
}

// A repeated phrase without an anchoring key is ambiguous — dropping it beats
// highlighting the wrong occurrence.
func TestValidateFindingsDropsAmbiguousQuote(t *testing.T) {
	text := "ναι και ναι"
	if got := validateFindings(text, []rawFinding{rf("ναι", "όχι", KindMisheard, "")}, nil); len(got) != 0 {
		t.Fatalf("want 0 for ambiguous quote, got %+v", got)
	}
}

func TestValidateFindingsUsesKeyToDisambiguate(t *testing.T) {
	text := "ναι και ναι"
	unc := []ReviewUncertain{{Key: "w2", Word: "ναι", Start: 14, End: 20}} // the SECOND "ναι" (byte offsets)
	got := validateFindings(text, []rawFinding{rf("ναι", "όχι", KindMisheard, "w2")}, unc)
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].Start != 14 {
		t.Fatalf("want the occurrence nearest the key (14), got %d", got[0].Start)
	}
}

// kind and `to` must agree, or a button labelled "delete" would insert text.
func TestValidateFindingsEnforcesKindToAgreement(t *testing.T) {
	text := "Ναι τέλος πάντων όχι"
	cases := []rawFinding{
		rf("τέλος πάντων", "whatever", KindFiller, ""), // delete kind with a replacement
		rf("τέλος πάντων", "", KindMisheard, ""),       // replace kind with no replacement
		rf("τέλος πάντων", "x", "bogus", ""),           // unknown kind
	}
	for i, c := range cases {
		if got := validateFindings(text, []rawFinding{c}, nil); len(got) != 0 {
			t.Fatalf("case %d: want 0, got %+v", i, got)
		}
	}
	if got := validateFindings(text, []rawFinding{rf("τέλος πάντων", "", KindRedundant, "")}, nil); len(got) != 1 {
		t.Fatalf("valid deletion rejected: %+v", got)
	}
}

// `to` reaches corrections.tsv and vocabulary.txt, both line/tab delimited.
func TestValidateFindingsRejectsDelimiterBearingReplacement(t *testing.T) {
	text := "Ναι όχι"
	for _, bad := range []string{"a\tb", "a\nb", "a\x00b"} {
		if got := validateFindings(text, []rawFinding{rf("Ναι", bad, KindMisheard, "")}, nil); len(got) != 0 {
			t.Fatalf("accepted %q: %+v", bad, got)
		}
	}
}

func TestValidateFindingsDropsNoOpEdit(t *testing.T) {
	text := "Ναι όχι"
	if got := validateFindings(text, []rawFinding{rf("Ναι", "Ναι", KindMisheard, "")}, nil); len(got) != 0 {
		t.Fatalf("want 0 for no-op, got %+v", got)
	}
}

// Applying one of two overlapping findings would invalidate the other's range.
func TestValidateFindingsDropsOverlapsAndSortsByPosition(t *testing.T) {
	text := "alpha beta gamma"
	got := validateFindings(text, []rawFinding{
		rf("gamma", "delta", KindMisheard, ""),
		rf("beta gamma", "x", KindMisheard, ""),
		rf("alpha", "A", KindMisheard, ""),
	}, nil)
	if len(got) != 2 {
		t.Fatalf("want 2 after overlap drop, got %+v", got)
	}
	if got[0].From != "alpha" || got[1].From != "beta gamma" {
		t.Fatalf("not sorted by position: %+v", got)
	}
}

func TestValidateFindingsCapsCount(t *testing.T) {
	text := "a1 a2 a3 a4 a5 a6 a7 a8"
	var raws []rawFinding
	for _, w := range strings.Fields(text) {
		raws = append(raws, rf(w, "X"+w, KindMisheard, ""))
	}
	if got := validateFindings(text, raws, nil); len(got) != maxReviewFindings {
		t.Fatalf("want %d, got %d", maxReviewFindings, len(got))
	}
}

func TestParseReviewTolerantOfFencesAndTrailingJunk(t *testing.T) {
	raw := "```json\n{\"findings\":[{\"from\":\"a\",\"to\":\"b\",\"kind\":\"misheard\"}]}\n```\n\": [garbage"
	got, err := parseReview(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].From != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseReviewEmptyFindingsIsValid(t *testing.T) {
	got, err := parseReview(`{"findings":[]}`)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// A model that fails must not end the chain — the next model sits in a different
// free-tier quota bucket, which is the whole reason the chain exists.
func TestReviewFallsThroughToNextModel(t *testing.T) {
	var tried []string
	caller := func(_ context.Context, model, _ string) (string, error) {
		tried = append(tried, model)
		if model == "m1" {
			return "", errors.New("429")
		}
		return `{"findings":[{"from":"ναι","to":"όχι","kind":"misheard"}]}`, nil
	}
	got, model, err := Review(context.Background(), "k", "Είπα ναι.", "el", nil, nil,
		[]string{"m1", "m2"}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if model != "m2" || len(tried) != 2 {
		t.Fatalf("tried %v, model %q", tried, model)
	}
	if len(got) != 1 || got[0].To != "όχι" {
		t.Fatalf("got %+v", got)
	}
}

func TestReviewEmptyInputsAreNoOps(t *testing.T) {
	call := func(context.Context, string, string) (string, error) {
		t.Fatal("model must not be called")
		return "", nil
	}
	if got, _, err := Review(context.Background(), "", "text", "el", nil, nil, nil, call); got != nil || err != nil {
		t.Fatalf("no key should short-circuit: %+v %v", got, err)
	}
	if got, _, err := Review(context.Background(), "k", "  ", "el", nil, nil, nil, call); got != nil || err != nil {
		t.Fatalf("blank text should short-circuit: %+v %v", got, err)
	}
}

func TestClampReviewInput(t *testing.T) {
	big := strings.Repeat("x", MaxReviewTextBytes+1)
	if _, _, _, err := ClampReviewInput(big, nil, nil); err == nil {
		t.Fatal("want error for oversized text")
	}
	text := "hello world"
	unc := []ReviewUncertain{
		{Key: "a", Word: "hello", Start: 0, End: 5},
		{Key: "b", Word: "", Start: 6, End: 11},       // blank word
		{Key: "c", Word: "world", Start: 6, End: 999}, // out of range
		{Key: "d", Word: "world", Start: 8, End: 8},   // empty range
	}
	_, gotUnc, gotVocab, err := ClampReviewInput(text, unc, []string{" Foo ", "", "b\x00d"})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotUnc) != 1 || gotUnc[0].Key != "a" {
		t.Fatalf("uncertain not filtered: %+v", gotUnc)
	}
	if len(gotVocab) != 1 || gotVocab[0] != "Foo" {
		t.Fatalf("vocab not filtered: %+v", gotVocab)
	}
}

// A single-word replacement must not match inside a longer word: "να" lives inside "ναι",
// and rewriting it there corrupts an unrelated word.
func TestValidateFindingsRequiresWholeWordForReplacements(t *testing.T) {
	text := "Είπα ναι"
	if got := validateFindings(text, []rawFinding{rf("να", "δεν", KindMisheard, "")}, nil); len(got) != 0 {
		t.Fatalf("matched inside a longer word: %+v", got)
	}
	// The same word standing alone is fine.
	if got := validateFindings("Είπα να πάμε", []rawFinding{rf("να", "δεν", KindMisheard, "")}, nil); len(got) != 1 {
		t.Fatalf("standalone word rejected: %+v", got)
	}
}

// A Greek accent is part of the word, not a boundary.
func TestWholeWordTreatsCombiningMarksAsWordCharacters(t *testing.T) {
	text := "Ναι τελος παντωνάκι όχι"
	if got := validateFindings(text, []rawFinding{rf("παντων", "πάντων", KindMisheard, "")}, nil); len(got) != 0 {
		t.Fatalf("matched a prefix of a longer word: %+v", got)
	}
}

// A deletion quotes a phrase, which carries its own boundaries — literal matching stays.
func TestDeletionsKeepLiteralMatching(t *testing.T) {
	text := "Ναι, τέλος πάντων, όχι."
	if got := validateFindings(text, []rawFinding{rf("τέλος πάντων", "", KindRedundant, "")}, nil); len(got) != 1 {
		t.Fatalf("phrase deletion rejected: %+v", got)
	}
}

// `from` becomes a corrections.tsv key, so it obeys the same delimiter rules as `to`.
func TestValidateFindingsRejectsDelimiterBearingFrom(t *testing.T) {
	text := "a\tb"
	if got := validateFindings(text, []rawFinding{rf("a\tb", "", KindRedundant, "")}, nil); len(got) != 0 {
		t.Fatalf("accepted a tab-bearing from: %+v", got)
	}
}
