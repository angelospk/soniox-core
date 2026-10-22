package soniox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdk "github.com/moxierobots/soniox-stt-go"
)

// fakeClient stands in for the SDK client. failOn[n] makes the n-th send
// (counted across all sessions, from 1) fail.
type fakeClient struct {
	opts sdk.ClientOptions
	rec  *fakeRec
}

type fakeRec struct {
	mu       sync.Mutex
	sessions int
	sends    int
	failOn   map[int]bool
	got      [][]byte // chunks accepted, in order
	failAll  bool
}

func (f *fakeClient) Start(context.Context, sdk.SessionOptions) error { return nil }
func (f *fakeClient) SendAudio(b []byte) error {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	f.rec.sends++
	if f.rec.failAll || f.rec.failOn[f.rec.sends] {
		return errors.New("write deadline exceeded")
	}
	f.rec.got = append(f.rec.got, append([]byte(nil), b...))
	return nil
}
func (f *fakeClient) Stop() error  { go f.opts.OnFinished(); return nil }
func (f *fakeClient) Cancel()      {}
func (f *fakeClient) Close() error { return nil }

func useFake(t *testing.T, rec *fakeRec) {
	t.Helper()
	oldNew, oldBackoff := newClient, backoff
	newClient = func(o sdk.ClientOptions) sttClient {
		rec.mu.Lock()
		rec.sessions++
		rec.mu.Unlock()
		return &fakeClient{opts: o, rec: rec}
	}
	backoff = func(int) time.Duration { return 0 }
	t.Cleanup(func() { newClient, backoff = oldNew, oldBackoff })
}

// A send error mid-dictation reconnects, resends the failed chunk, and drains
// the rest of the audio in the new session.
func TestStreamReconnectsOnSendError(t *testing.T) {
	rec := &fakeRec{failOn: map[int]bool{2: true}}
	useFake(t, rec)
	audio := make(chan []byte, 3)
	audio <- []byte("a")
	audio <- []byte("b")
	audio <- []byte("c")
	close(audio)
	var reconnects int
	_, err := Stream(context.Background(), Options{}, audio,
		Callbacks{OnReconnect: func(int) { reconnects++ }})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rec.sessions != 2 || reconnects != 1 {
		t.Fatalf("sessions=%d reconnects=%d", rec.sessions, reconnects)
	}
	want := []string{"a", "b", "c"}
	if len(rec.got) != len(want) {
		t.Fatalf("got %q, want %q", rec.got, want)
	}
	for i := range want {
		if string(rec.got[i]) != want[i] {
			t.Fatalf("got %q, want %q", rec.got, want)
		}
	}
}

// When every send fails, Stream gives up after maxReconnects and reports it.
func TestStreamSendErrorExhaustsRetries(t *testing.T) {
	rec := &fakeRec{failAll: true}
	useFake(t, rec)
	audio := make(chan []byte, 1)
	audio <- []byte("a")
	done := make(chan error, 1)
	go func() {
		_, err := Stream(context.Background(), Options{}, audio, Callbacks{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not give up")
	}
	if rec.sessions != maxReconnects+1 {
		t.Fatalf("sessions=%d", rec.sessions)
	}
}
