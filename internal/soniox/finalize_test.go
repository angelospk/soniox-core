package soniox

import "testing"

// Manual finalization (and endpoint detection) make Soniox emit control tokens
// "<fin>" / "<end>" as FINAL tokens. They mark a boundary; they are not speech.
func TestAssemblerDropsControlTokens(t *testing.T) {
	var a Assembler
	nf := a.AddResult([]TokenLite{
		{Text: "καρδιολόγο", IsFinal: true},
		{Text: " στην Αθήνα", IsFinal: true},
		{Text: "<fin>", IsFinal: true},
	})
	if nf != "καρδιολόγο στην Αθήνα" {
		t.Fatalf("newFinal = %q", nf)
	}
	a.AddResult([]TokenLite{{Text: "<end>", IsFinal: true}, {Text: " πίσω", IsFinal: false}})
	if got := a.Full(); got != "καρδιολόγο στην Αθήνα πίσω" {
		t.Fatalf("Full = %q", got)
	}
}

// A finalize request must reach the SDK client while audio keeps flowing, and a
// closed/nil control channel must not break the pump.
func TestPumpForwardsFinalize(t *testing.T) {
	audio := make(chan []byte, 2)
	fin := make(chan struct{}, 1)
	var sent, finals int
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		pump(audio, fin, func(b []byte) error { sent++; return nil }, func() error { finals++; return nil }, func() {}, stop)
	}()
	audio <- []byte{1}
	fin <- struct{}{}
	audio <- []byte{2}
	close(audio)
	<-done
	if sent != 2 || finals != 1 {
		t.Fatalf("sent=%d finals=%d, want 2/1", sent, finals)
	}
}
