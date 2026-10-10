package soniox

import "testing"

func TestAssemblerProgressiveFinal(t *testing.T) {
	var a Assembler
	// message 1: partial only
	if nf := a.AddResult([]TokenLite{{Text: "Λοι"}, {Text: "πόν"}}); nf != "" {
		t.Errorf("no finals yet, got newFinal %q", nf)
	}
	if a.Partial() != "Λοιπόν" {
		t.Errorf("partial = %q", a.Partial())
	}
	if a.Final() != "" {
		t.Errorf("final should be empty, got %q", a.Final())
	}
	// message 2: first token finalizes, new partial tail
	nf := a.AddResult([]TokenLite{{Text: "Λοιπόν", IsFinal: true}, {Text: ", πά"}})
	if nf != "Λοιπόν" {
		t.Errorf("newFinal = %q, want Λοιπόν", nf)
	}
	if a.Final() != "Λοιπόν" {
		t.Errorf("final = %q", a.Final())
	}
	if a.Partial() != ", πά" {
		t.Errorf("partial = %q", a.Partial())
	}
	if a.Full() != "Λοιπόν, πά" {
		t.Errorf("full = %q", a.Full())
	}
}

func TestAssemblerCodeSwitch(t *testing.T) {
	var a Assembler
	a.AddResult([]TokenLite{{Text: "κάνε ", IsFinal: true}, {Text: "commit", IsFinal: true}, {Text: " και push", IsFinal: true}})
	if a.Full() != "κάνε commit και push" {
		t.Errorf("full = %q", a.Full())
	}
}

func TestChunkBytes(t *testing.T) {
	if got := ChunkBytes(100); got != 4800 { // 24000*2*0.1
		t.Errorf("ChunkBytes(100) = %d, want 4800", got)
	}
}

func TestLowConfidenceWords(t *testing.T) {
	toks := []TokenLite{
		{Text: " hel", IsFinal: true, Confidence: 0.92},
		{Text: "lo", IsFinal: true, Confidence: 0.51},
		{Text: ",", IsFinal: true, Confidence: 0.10}, // punctuation must not lower "hello"
		{Text: " wor", IsFinal: true, Confidence: 0.80},
		{Text: "ld", IsFinal: true, Confidence: 0.79},
		{Text: " new york", IsFinal: true, Confidence: 0.40},
		{Text: " skip", IsFinal: false, Confidence: 0.01}, // non-final ignored
	}
	got := LowConfidenceWords(toks, 0.6)
	want := []string{"hello", "new", "york"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLowConfidenceRanges(t *testing.T) {
	toks := []TokenLite{
		{Text: " hel", IsFinal: true, Confidence: 0.92},
		{Text: "lo", IsFinal: true, Confidence: 0.51},
		{Text: ",", IsFinal: true, Confidence: 0.10}, // punctuation excluded from the range
		{Text: " wor", IsFinal: true, Confidence: 0.80},
		{Text: "ld", IsFinal: true, Confidence: 0.79},
		{Text: " new york", IsFinal: true, Confidence: 0.40},
		{Text: " skip", IsFinal: false, Confidence: 0.01}, // non-final ignored
	}
	// Assembled final text: " hello, world new york".
	// "hello" at 1..5, "new" at 14..16, "york" at 18..21.
	got := LowConfidenceRanges(toks, 0.6)
	want := []LowConfRange{{Start: 1, Len: 5}, {Start: 14, Len: 3}, {Start: 18, Len: 4}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("range %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLowConfidenceRangesGreekUTF16(t *testing.T) {
	// Greek (BMP, 1 UTF-16 unit each) + an astral emoji (2 UTF-16 units) before the
	// low-confidence word, proving UTF-16 offsets (not byte/rune) are emitted.
	toks := []TokenLite{
		{Text: "😀 ", IsFinal: true, Confidence: 0.99},   // emoji=2 u16 + space=1 → next at 3
		{Text: "καλά", IsFinal: true, Confidence: 0.30}, // low-confidence
	}
	got := LowConfidenceRanges(toks, 0.6)
	want := []LowConfRange{{Start: 3, Len: 4}} // "καλά" = 4 UTF-16 units at offset 3
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}
