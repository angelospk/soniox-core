package server

import (
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/angelospk/soniox-core/internal/soniox"
)

// liveStats counts what one /live session received and produced, so a dictation
// that came back empty can be traced to its layer: no audio, a silent mic (low
// peak), a dropped link (reconnects/err), or audio that Soniox never transcribed.
type liveStats struct {
	mu                           sync.Mutex
	start                        time.Time
	audioBytes                   int
	peak                         int // max |sample| of the s16le PCM, 0..32768
	partials, finals, reconnects int
	client                       string // why the client socket closed early, if it did
}

// clientGone records a client read error other than a normal close.
func (s *liveStats) clientGone(err error) {
	if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		return
	}
	s.mu.Lock()
	s.client = err.Error()
	s.mu.Unlock()
}

func (s *liveStats) addAudio(b []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioBytes += len(b)
	for i := 0; i+1 < len(b); i += 2 {
		v := int(int16(uint16(b[i]) | uint16(b[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > s.peak {
			s.peak = v
		}
	}
}

func (s *liveStats) inc(n *int) {
	s.mu.Lock()
	*n++
	s.mu.Unlock()
}

// line is the one log line written when the session ends. EMPTY marks a session
// that produced no text at all.
func (s *liveStats) line(chars int, err error) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	audioS := float64(s.audioBytes) / float64(soniox.SampleRate*2)
	l := fmt.Sprintf("live end dur=%.0f audioS=%.1f peak=%d partials=%d finals=%d reconnects=%d chars=%d",
		time.Since(s.start).Seconds(), audioS, s.peak, s.partials, s.finals, s.reconnects, chars)
	if err != nil {
		l += fmt.Sprintf(" err=%q", err.Error())
	}
	if s.client != "" {
		l += fmt.Sprintf(" client=%q", s.client)
	}
	if chars == 0 {
		l += " EMPTY"
	}
	return l
}
