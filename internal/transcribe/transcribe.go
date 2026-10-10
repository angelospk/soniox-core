// Package transcribe is the high-level engine: it decodes a file, splits it at
// silences, transcribes the segments concurrently over Soniox, merges them in
// order, and optionally runs an LLM cleanup pass.
package transcribe

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/angelospk/soniox-core/internal/audio"
	"github.com/angelospk/soniox-core/internal/cleanup"
	"github.com/angelospk/soniox-core/internal/segment"
	"github.com/angelospk/soniox-core/internal/soniox"
)

const (
	maxJobs   = 18 // measured concurrency ceiling for one temp key
	targetSeg = 60 // seconds per segment in auto mode
)

// FileOptions configure a file transcription.
type FileOptions struct {
	Key       string   // Soniox key (static; reused across parallel segments)
	Langs     []string // e.g. ["el","en"]
	Jobs      int      // 0 = auto
	NoiseDB   int      // silence threshold (e.g. -30)
	Cleanup   bool
	GeminiKey string
	Vocab     string
}

// Result holds both the raw merged transcript and the cleaned version.
type Result struct {
	Raw      string
	Cleaned  string
	Jobs     int
	Segments int
	Duration float64
}

// File transcribes a local audio file. Pure-CPU work (decode) and network work
// (Soniox) for each segment run concurrently, capped at Jobs.
func File(ctx context.Context, path string, opt FileOptions) (Result, error) {
	if opt.NoiseDB == 0 {
		opt.NoiseDB = -30
	}
	dur, err := audio.Duration(ctx, path)
	if err != nil {
		return Result{}, err
	}
	jobs := opt.Jobs
	if jobs <= 0 {
		jobs = segment.AutoJobs(dur, targetSeg, maxJobs)
	}
	silences, err := audio.DetectSilences(ctx, path, opt.NoiseDB, 0.4)
	if err != nil {
		return Result{}, err
	}
	spans := segment.Plan(dur, jobs, silences)

	results := make([]string, len(spans))
	sem := make(chan struct{}, jobs)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	record := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
	}

	for i, sp := range spans {
		wg.Add(1)
		go func(i int, sp segment.Span) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				record(ctx.Err())
				return
			}
			pcm, err := audio.DecodePCM(ctx, path, sp.Start, sp.Dur())
			if err != nil {
				record(err)
				return
			}
			ch := make(chan []byte)
			go feedPaced(ctx, ch, pcm)
			txt, err := soniox.Stream(ctx,
				soniox.Options{APIKey: opt.Key, Langs: opt.Langs, EndpointDetection: false},
				ch, soniox.Callbacks{})
			if err != nil {
				record(err)
				return
			}
			results[i] = strings.TrimSpace(txt)
		}(i, sp)
	}
	wg.Wait()
	if firstErr != nil {
		return Result{}, firstErr
	}

	parts := make([]string, 0, len(results))
	for _, r := range results {
		if r != "" {
			parts = append(parts, r)
		}
	}
	raw := strings.Join(parts, " ")

	res := Result{Raw: raw, Cleaned: raw, Jobs: jobs, Segments: len(spans), Duration: dur}
	if opt.Cleanup && opt.GeminiKey != "" && raw != "" {
		if c, err := cleanup.Clean(ctx, opt.GeminiKey, raw,
			cleanup.Options{Vocab: opt.Vocab}); err == nil {
			res.Cleaned = c
		}
	}
	return res, nil
}

// feedPaced streams pcm into ch at ~real time (Soniox processes at ~1x; blasting
// a long segment overruns its buffer), closing ch when done.
func feedPaced(ctx context.Context, ch chan<- []byte, pcm []byte) {
	defer close(ch)
	cb := soniox.ChunkBytes(100)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for i := 0; i < len(pcm); i += cb {
		end := i + cb
		if end > len(pcm) {
			end = len(pcm)
		}
		select {
		case ch <- pcm[i:end]:
		case <-ctx.Done():
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
