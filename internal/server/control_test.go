package server

import "testing"

func TestControlFrame(t *testing.T) {
	fin := make(chan struct{}, 1)
	if handleControlFrame("finalize", fin) {
		t.Fatal("finalize must not end the session")
	}
	if len(fin) != 1 {
		t.Fatal("finalize must be queued")
	}
	// A burst of finalize frames while one is pending coalesces instead of blocking
	// the read loop (which would stall audio).
	if handleControlFrame("finalize", fin) || len(fin) != 1 {
		t.Fatal("second finalize must coalesce without blocking")
	}
	if !handleControlFrame("done", fin) {
		t.Fatal("done must end the session")
	}
	if handleControlFrame("whatever", fin) {
		t.Fatal("unknown frames are ignored")
	}
}
