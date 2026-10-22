package soniox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/moxierobots/soniox-stt-go"
)

const maxReconnects = 4

// sttClient is the part of the SDK client a session uses; a seam for tests.
type sttClient interface {
	Start(ctx context.Context, opts sdk.SessionOptions) error
	SendAudio(data []byte) error
	Stop() error
	Cancel()
	Close() error
}

var (
	newClient = func(o sdk.ClientOptions) sttClient { return sdk.NewClient(o) }
	backoff   = func(attempt int) time.Duration { return time.Duration(min(2*(attempt+1), 8)) * time.Second }
)

// Options configure a streaming session.
type Options struct {
	APIKey            string                 // static key
	APIKeyFunc        func() (string, error) // dynamic key; wins over APIKey
	Langs             []string
	EndpointDetection bool // false for continuous dictation / whole files
	Diarization       bool
	LowConfThreshold  float64 // words below this min-confidence are flagged (0 → default)
}

// defaultLowConf is used when Options.LowConfThreshold is unset. Conservative so the
// orange "unsure" marks stay trustworthy rather than noisy.
const defaultLowConf = 0.5

// Callbacks receive incremental text during a stream.
type Callbacks struct {
	OnFinal     func(text string, lowConf []LowConfRange) // lowConf: char ranges (UTF-16) in text finalized with low STT confidence
	OnPartial   func(tail string)
	OnReconnect func(attempt int) // fired before each reconnect attempt
}

// Stream runs a transcription session and auto-reconnects on a mid-stream drop
// (network blip, temp-key expiry) until the caller closes audio or retries are
// exhausted. It returns the full transcript assembled across reconnects.
func Stream(ctx context.Context, opt Options, audio <-chan []byte, cb Callbacks) (string, error) {
	var (
		mu          sync.Mutex
		asm         Assembler
		inputClosed atomic.Bool
		lastErr     error
		carry       []byte // chunk whose send failed; resent by the next session
	)
	for attempt := 0; ; attempt++ {
		clean, err := runSession(ctx, opt, &mu, &asm, audio, cb, &inputClosed, &carry)
		if clean {
			lastErr = nil
			break
		}
		if inputClosed.Load() || ctx.Err() != nil || attempt >= maxReconnects {
			lastErr = err
			if ctx.Err() != nil {
				lastErr = ctx.Err()
			}
			break
		}
		lastErr = err
		if cb.OnReconnect != nil {
			cb.OnReconnect(attempt + 1)
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			lastErr = ctx.Err()
			return finalText(&mu, &asm), lastErr
		}
	}
	return finalText(&mu, &asm), lastErr
}

// runSession runs a single SDK session. clean is true only on a normal finish
// (OnFinished). On a drop it returns clean=false with the error; the audio
// channel is left open so the caller can reconnect.
func runSession(ctx context.Context, opt Options, mu *sync.Mutex, asm *Assembler,
	audio <-chan []byte, cb Callbacks, inputClosed *atomic.Bool, carry *[]byte) (bool, error) {

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		once       sync.Once
		finished   = make(chan struct{})
		finishedOK bool
		sessErr    error
	)
	done := func() { once.Do(func() { close(finished) }) }

	client := newClient(sdk.ClientOptions{
		APIKey:     opt.APIKey,
		APIKeyFunc: opt.APIKeyFunc,
		KeepAlive:  true,
		OnResult: func(r *sdk.Response) {
			toks := make([]TokenLite, 0, len(r.Tokens))
			for _, tk := range r.Tokens {
				toks = append(toks, TokenLite{Text: tk.Text, IsFinal: tk.IsFinal, Confidence: tk.Confidence})
			}
			mu.Lock()
			nf := asm.AddResult(toks)
			partial := asm.Partial()
			mu.Unlock()
			if nf != "" && cb.OnFinal != nil {
				thr := opt.LowConfThreshold
				if thr <= 0 {
					thr = defaultLowConf
				}
				cb.OnFinal(nf, LowConfidenceRanges(toks, thr))
			}
			if cb.OnPartial != nil {
				cb.OnPartial(partial)
			}
		},
		OnFinished: func() {
			mu.Lock()
			finishedOK = true
			mu.Unlock()
			done()
		},
		OnError: func(e *sdk.Error) {
			mu.Lock()
			if sessErr == nil {
				sessErr = e
			}
			mu.Unlock()
			done()
		},
		OnDisconnected: func(reason string) {
			mu.Lock()
			if !finishedOK && sessErr == nil {
				sessErr = fmt.Errorf("disconnected: %s", reason)
			}
			mu.Unlock()
			done()
		},
	})

	if err := client.Start(ctx, sdk.SessionOptions{
		Model:                    Model,
		AudioFormat:              AudioFormat,
		SampleRate:               SampleRate,
		NumChannels:              NumChannels,
		LanguageHints:            opt.Langs,
		EnableEndpointDetection:  opt.EndpointDetection,
		EnableSpeakerDiarization: opt.Diarization,
	}); err != nil {
		return false, err
	}

	// sender: pump audio until the channel closes (input done) or the session
	// ends (sessionCtx cancelled) — without closing the shared audio channel.
	// A failed send ends the session with an error so Stream reconnects and the
	// next session resends that chunk; before, the sender exited silently and
	// the rest of the dictation was dropped.
	first := *carry
	*carry = nil
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		pump(sessionCtx, audio, first, client.SendAudio,
			func() { inputClosed.Store(true); client.Stop() },
			func(err error, chunk []byte) {
				mu.Lock()
				*carry = chunk
				if sessErr == nil {
					sessErr = fmt.Errorf("send audio: %w", err)
				}
				mu.Unlock()
				done()
			})
	}()

	select {
	case <-finished:
	case <-ctx.Done():
		client.Cancel()
		mu.Lock()
		if sessErr == nil {
			sessErr = ctx.Err()
		}
		mu.Unlock()
	}
	cancel() // stop the sender
	client.Close()
	<-pumped // a late send must not race the next session for carry or audio

	mu.Lock()
	ok, e := finishedOK, sessErr
	mu.Unlock()
	return ok, e
}

// pump sends first (if any), then forwards audio chunks to send until audio
// closes (then stop) or ctx ends. On a send error it calls fail with the
// unsent chunk and returns.
func pump(ctx context.Context, audio <-chan []byte, first []byte,
	send func([]byte) error, stop func(), fail func(error, []byte)) {
	if len(first) > 0 {
		if err := send(first); err != nil {
			fail(err, first)
			return
		}
	}
	for {
		select {
		case chunk, ok := <-audio:
			if !ok {
				stop()
				return
			}
			if len(chunk) > 0 {
				if err := send(chunk); err != nil {
					fail(err, chunk)
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func finalText(mu *sync.Mutex, asm *Assembler) string {
	mu.Lock()
	defer mu.Unlock()
	return asm.Full()
}
