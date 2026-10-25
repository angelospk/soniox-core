package server

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestPumpAudioExitsWhenStreamEnds: after Stream gives up (maxReconnects) nobody
// reads the audio channel any more. The reader must still exit once the handler
// signals stop, instead of blocking forever on `audio <- data` (goroutine leak).
func TestPumpAudioExitsWhenStreamEnds(t *testing.T) {
	audio := make(chan []byte) // unbuffered and never read: the send would block
	stop := make(chan struct{})
	next := func() (int, []byte, error) { return websocket.BinaryMessage, []byte{1, 2}, nil }
	exited := make(chan struct{})
	go func() {
		pumpAudio(next, audio, stop, func([]byte) {}, func(error) {})
		close(exited)
	}()
	time.Sleep(20 * time.Millisecond) // let it block on the send
	close(stop)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("reader still blocked on audio <- data after stop")
	}
	if _, ok := <-audio; ok {
		t.Fatal("audio channel not closed on exit")
	}
}

// TestPumpAudioForwardsUntilDone: normal path is unchanged: binary frames reach the
// channel in order, the text "done" frame ends the reader, and the channel closes.
func TestPumpAudioForwardsUntilDone(t *testing.T) {
	frames := []struct {
		mt   int
		data string
	}{{websocket.BinaryMessage, "a"}, {websocket.TextMessage, "ping"}, {websocket.BinaryMessage, "b"}, {websocket.TextMessage, "done"}}
	i := 0
	next := func() (int, []byte, error) { f := frames[i]; i++; return f.mt, []byte(f.data), nil }
	audio := make(chan []byte, 8)
	var counted int
	pumpAudio(next, audio, make(chan struct{}), func(d []byte) { counted += len(d) }, func(error) { t.Fatal("not gone") })
	var got []string
	for d := range audio {
		got = append(got, string(d))
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" || counted != 2 {
		t.Fatalf("got %v counted %d", got, counted)
	}
}
