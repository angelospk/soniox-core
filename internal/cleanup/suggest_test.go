package cleanup

import (
	"context"
	"reflect"
	"testing"
)

func items(words ...string) []SuggestItem {
	out := make([]SuggestItem, 0, len(words))
	for _, w := range words {
		out = append(out, SuggestItem{Key: w, Word: w})
	}
	return out
}

func TestSuggest_PlainJSON(t *testing.T) {
	caller := fakeCaller(`{"rite":["right","write"],"there":["their"]}`, nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "I will rite there", items("rite", "there"), "en", caller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := map[string][]string{"rite": {"right", "write"}, "there": {"their"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSuggest_FencedJSON(t *testing.T) {
	caller := fakeCaller("```json\n{\"rite\":[\"right\"]}\n```", nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite", items("rite"), "", caller)
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"rite": {"right"}}) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestSuggest_GarbageIsError(t *testing.T) {
	caller := fakeCaller("sorry, I cannot help with that", nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite", items("rite"), "", caller)
	if err == nil {
		t.Fatalf("expected parse error, got %+v", got)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map on error, got %+v", got)
	}
}

func TestSuggest_DropsHallucinatedKeysAndSanitizes(t *testing.T) {
	// "rite" valid; "ghost" never requested → dropped. Alt equal to the word → dropped.
	// >3 alts → capped. Duplicate (case-insensitive) → deduped. Newline-bearing → dropped.
	caller := fakeCaller(`{"rite":["right","Right","write","wright","ok\nno"],"ghost":["boo"]}`, nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite", items("rite"), "", caller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := map[string][]string{"rite": {"right", "write", "wright"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSuggest_KeyDistinctFromWord(t *testing.T) {
	// Client uses an opaque key; model echoes the display word. Suggest maps it back.
	caller := fakeCaller(`{"rite":["right"]}`, nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite", []SuggestItem{{Key: "k0", Word: "rite"}}, "", caller)
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"k0": {"right"}}) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestSuggest_DuplicateWordDistinctKeysBothResolve(t *testing.T) {
	// Two positions of the same word carry distinct keys; the model echoes the word once,
	// and both keys must receive the suggestions (word-keyed v1 semantics).
	caller := fakeCaller(`{"rite":["right","write"]}`, nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite ... rite",
		[]SuggestItem{{Key: "k0", Word: "rite"}, {Key: "k1", Word: "rite"}}, "", caller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := map[string][]string{"k0": {"right", "write"}, "k1": {"right", "write"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSuggest_RejectsControlChars(t *testing.T) {
	// Second alt embeds an escaped vertical-tab (\u000b) — a control char that is not
	// \n/\r/\t — so it must be dropped, leaving only the clean alt.
	caller := fakeCaller("{\"rite\":[\"right\",\"wri\\u000bte\"]}", nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite", items("rite"), "", caller)
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"rite": {"right"}}) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestSuggest_ToleratesTrailingFragment(t *testing.T) {
	// Observed live: gemini-2.5-flash-lite appended a duplicated partial object after a
	// valid one. Decoding the first value must succeed and ignore the trailing junk.
	caller := fakeCaller("{\"rite\": [\"write\", \"right\"], \"their\": [\"there\"]}\n\": [\"there\"]}", nil, new(int))
	got, _, err := Suggest(context.Background(), "key", "rite their", items("rite", "their"), "en", caller)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := map[string][]string{"rite": {"write", "right"}, "their": {"there"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSuggest_NoInputNoCall(t *testing.T) {
	calls := 0
	caller := fakeCaller(`{}`, nil, &calls)
	for _, tc := range []struct {
		key, text string
		its       []SuggestItem
	}{
		{"", "x", items("x")},      // no api key
		{"key", "   ", items("x")}, // blank text
		{"key", "x", nil},          // no items
	} {
		got, _, err := Suggest(context.Background(), tc.key, tc.text, tc.its, "", caller)
		if err != nil || len(got) != 0 {
			t.Fatalf("expected empty/nil, got %+v err %v", got, err)
		}
	}
	if calls != 0 {
		t.Fatalf("model should not be called for empty input, calls=%d", calls)
	}
}
