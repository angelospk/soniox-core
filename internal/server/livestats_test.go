package server

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLiveStatsAudioPeak(t *testing.T) {
	var s liveStats
	s.addAudio([]byte{0x00, 0x00, 0x10, 0x00}) // samples 0, 16
	s.addAudio([]byte{0x00, 0x80, 0x05})       // -32768, then a dangling odd byte
	if s.audioBytes != 7 || s.peak != 32768 {
		t.Fatalf("bytes=%d peak=%d", s.audioBytes, s.peak)
	}
}

// The log line answers "mic silent, link dropped, or Soniox gave nothing?" for a
// dictation that came back empty.
func TestLiveStatsLine(t *testing.T) {
	s := liveStats{start: time.Now().Add(-90 * time.Second), audioBytes: 4320000, peak: 9000,
		partials: 0, finals: 0, reconnects: 2, client: "read: reset"}
	got := s.line(0, errors.New("disconnected: x"))
	for _, want := range []string{"live end", "dur=90", "audioS=90.0", "peak=9000", "partials=0", "finals=0",
		"reconnects=2", "chars=0", "EMPTY", `err="disconnected: x"`, `client="read: reset"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	ok3 := liveStats{start: time.Now(), finals: 3}
	if ok := ok3.line(42, nil); strings.Contains(ok, "EMPTY") || !strings.Contains(ok, "chars=42") {
		t.Fatalf("got %q", ok)
	}
}

// ptext is the longest partial seen: >0 on an EMPTY line means Soniox heard
// words that never became final text; 0 means it recognized nothing.
func TestLiveStatsPartialText(t *testing.T) {
	s := &liveStats{start: time.Now()}
	s.partial("")
	s.partial("καλη")
	s.partial("κα")
	if got := s.line(0, nil); !strings.Contains(got, "partials=3") || !strings.Contains(got, "ptext=4") {
		t.Fatalf("line = %q", got)
	}
}
