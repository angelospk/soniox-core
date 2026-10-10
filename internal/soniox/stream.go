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

// Options configure a streaming session.
type Options struct {
	APIKey            string                 // static key
	APIKeyFunc        func() (string, error) // dynamic key; wins over APIKey
	Langs             []string
	EndpointDetection bool // false for continuous dictation / whole files
	Diarization       bool
	LowConfThreshold  float64 // words below this min-confidence are flagged (0 → default)
	// Finalize, when set, asks Soniox to finalize pending tokens NOW (each receive =
	// one manual finalization). The client uses it when its VAD sees the user stop
	// talking, so the final arrives in ~100ms instead of waiting for Soniox's own
	// pause detection. Audio after it simply forms the next utterance.
	Finalize <-chan struct{}
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
	)
	for attempt := 0; ; attempt++ {
		clean, err := runSession(ctx, opt, &mu, &asm, audio, cb, &inputClosed)
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
		case <-time.After(time.Duration(min(2*(attempt+1), 8)) * time.Second):
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
	audio <-chan []byte, cb Callbacks, inputClosed *atomic.Bool) (bool, error) {

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		once       sync.Once
		finished   = make(chan struct{})
		finishedOK bool
		sessErr    error
	)
	done := func() { once.Do(func() { close(finished) }) }

	client := sdk.NewClient(sdk.ClientOptions{
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
		Model:                    sdk.DefaultModel,
		AudioFormat:              AudioFormat,
		SampleRate:               SampleRate,
		NumChannels:              NumChannels,
		LanguageHints:            opt.Langs,
		EnableEndpointDetection:  opt.EndpointDetection,
		EnableSpeakerDiarization: opt.Diarization,
	}); err != nil {
		return false, err
	}

	// sender: pump audio (and finalize requests) until the audio channel closes
	// (input done) or the session ends — without closing the shared audio channel.
	go pump(audio, opt.Finalize, client.SendAudio, func() error { return client.Finalize() },
		func() { inputClosed.Store(true); client.Stop() }, sessionCtx.Done())

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

	mu.Lock()
	ok, e := finishedOK, sessErr
	mu.Unlock()
	return ok, e
}

func finalText(mu *sync.Mutex, asm *Assembler) string {
	mu.Lock()
	defer mu.Unlock()
	return asm.Full()
}

// pump forwards audio chunks and finalize requests to one SDK session. It returns
// when audio closes (calling onInputClosed) or stop fires. A nil finalize channel
// blocks forever in select, i.e. it is simply never taken.
func pump(audio <-chan []byte, finalize <-chan struct{}, send func([]byte) error,
	fin func() error, onInputClosed func(), stop <-chan struct{}) {
	for {
		select {
		case chunk, ok := <-audio:
			if !ok {
				onInputClosed()
				return
			}
			if len(chunk) > 0 {
				if err := send(chunk); err != nil {
					return
				}
			}
		case <-finalize:
			_ = fin() // best effort: a failed finalize just means the final comes later
		case <-stop:
			return
		}
	}
}
