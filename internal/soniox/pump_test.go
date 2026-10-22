package soniox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A failed send must end the session (so Stream reconnects) instead of silently
// dropping every later chunk while the session idles on keepalives.
func TestPumpReportsSendError(t *testing.T) {
	audio := make(chan []byte, 4)
	audio <- []byte{1}
	audio <- []byte{2}
	boom := errors.New("write timeout")
	var sent int
	var got error
	var lost []byte
	stopped := false
	pump(context.Background(), audio, nil,
		func(b []byte) error {
			sent++
			if sent == 2 {
				return boom
			}
			return nil
		},
		func() { stopped = true },
		func(err error, chunk []byte) { got, lost = err, chunk })
	if !errors.Is(got, boom) || stopped || sent != 2 || string(lost) != "\x02" {
		t.Fatalf("got=%v stopped=%v sent=%d", got, stopped, sent)
	}
}

func TestPumpStopsOnInputClose(t *testing.T) {
	audio := make(chan []byte, 2)
	audio <- []byte{1}
	audio <- []byte{} // empty chunks are skipped
	close(audio)
	var sent int
	stopped := false
	var first []byte
	pump(context.Background(), audio, []byte{9}, func(b []byte) error {
		if sent == 0 {
			first = b
		}
		sent++
		return nil
	}, func() { stopped = true }, func(error, []byte) { t.Fatal("no error expected") })
	if !stopped || sent != 2 || first[0] != 9 {
		t.Fatalf("stopped=%v sent=%d", stopped, sent)
	}
}

func TestPumpReturnsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pump(ctx, make(chan []byte), nil, func([]byte) error { return nil }, func() {}, func(error, []byte) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pump did not return on cancel")
	}
}
