package audio

import (
	"math"
	"testing"
)

func TestParseSilences(t *testing.T) {
	log := `
[silencedetect @ 0x1] silence_start: 2.5
[silencedetect @ 0x1] silence_end: 3.5 | silence_duration: 1.0
[silencedetect @ 0x1] silence_start: 10.0
[silencedetect @ 0x1] silence_end: 11.0 | silence_duration: 1.0
`
	mids := parseSilences(log)
	want := []float64{3.0, 10.5}
	if len(mids) != len(want) {
		t.Fatalf("got %d mids, want %d (%v)", len(mids), len(want), mids)
	}
	for i := range want {
		if math.Abs(mids[i]-want[i]) > 1e-9 {
			t.Errorf("mid[%d] = %v, want %v", i, mids[i], want[i])
		}
	}
}

func TestParseSilencesUnpairedStart(t *testing.T) {
	// trailing silence_start with no matching end -> midpoint = start
	log := `silence_start: 5.0
silence_end: 6.0
silence_start: 20.0`
	mids := parseSilences(log)
	if len(mids) != 2 || math.Abs(mids[1]-20.0) > 1e-9 {
		t.Errorf("unpaired start mishandled: %v", mids)
	}
}

func TestParseSilencesEmpty(t *testing.T) {
	if got := parseSilences("no silences here"); len(got) != 0 {
		t.Errorf("expected none, got %v", got)
	}
}
