# Phase 1: Silence Trimming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop paying 1x real-time streaming cost for silence during file transcription, and collapse `N+2` ffmpeg processes into 1.

**Architecture:** Decode the whole file to a PCM temp file once, cap over-long silent runs in pure Go, plan segments over the trimmed stream, and feed each segment to Soniox from an `io.SectionReader` instead of a per-segment ffmpeg decode. Job count is computed from the *original* duration so the parallelism win survives trimming.

**Tech Stack:** Go 1.26, stdlib only (`encoding/binary`, `math`, `io`, `os`). ffmpeg is shelled out to exactly once. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-15-silence-trim-and-voice-ingest-design.md`

**Base commit:** `d958681`

---

## Background the engineer needs

`internal/transcribe/transcribe.go` transcribes a local file by splitting it into
N spans and streaming each to Soniox concurrently. **A Soniox realtime stream runs
at ~1x**, so `feedPaced` deliberately sends one 100 ms PCM chunk per 100 ms tick —
blasting a long segment overruns Soniox's buffer. Wall clock is therefore the
*byte count* of the largest segment, in real time.

Consequence that is easy to get wrong: a segment full of silence does **not**
finish early. It sends the same number of bytes. Trimming helps only by reducing
total bytes.

PCM everywhere in this codebase is **s16le, mono, 24 kHz** (`soniox.SampleRate =
24000`, `soniox.NumChannels = 1`). That is `48000` bytes per second. All sample
arithmetic must stay on integer sample indices; convert to seconds only at the
boundaries of the package.

**Three thresholds that must stay separate** (conflating them is the main
correctness trap):

| knob | default | meaning |
|---|---|---|
| `BoundaryMin` | 0.4 s | a silent run at least this long may host a segment cut |
| `TrimMin` | 0.6 s | a silent run longer than this gets shortened |
| `Keep` | 0.3 s | what survives of a shortened run |

If you use `TrimMin` for cut candidates, a file whose pauses are all 0.4–0.6 s
gets zero candidates, `segment.Plan` returns one span, and parallelism collapses
to 1.

---

## File Structure

| file | responsibility |
|---|---|
| `internal/audio/decode.go` (create) | one-shot whole-file decode to a PCM temp file; duration from byte size |
| `internal/audio/trim.go` (create) | silence scan, trim planning (pure), and the trimming writer |
| `internal/audio/trim_test.go` (create) | unit tests over synthetic PCM — no ffmpeg |
| `internal/audio/decode_test.go` (create) | duration arithmetic |
| `internal/audio/audio.go` (modify) | unchanged API; `DecodePCM`/`DetectSilences` stay for the non-trimmed path |
| `internal/transcribe/transcribe.go` (modify) | new pipeline; `feedPaced` reads from `io.Reader` |
| `internal/transcribe/transcribe_test.go` (create) | job-count and pacing tests |
| `internal/server/server.go` (modify) | `?trim=` tri-state parsing into `FileOptions` |
| `internal/server/transcribe_test.go` (modify) | HTTP-level trim parsing tests |
| `internal/config/config.go` (modify) | env defaults for the trim knobs |

---

## Task 1: Decode once, feed from file sections

Removes the `ffprobe` pass and all N per-segment `ffmpeg` decodes. **No audio
semantics change** — this task must produce byte-identical transcription input.

**Files:**
- Create: `internal/audio/decode.go`
- Create: `internal/audio/decode_test.go`
- Modify: `internal/transcribe/transcribe.go`

- [ ] **Step 1: Write the failing test for duration arithmetic**

Create `internal/audio/decode_test.go`:

```go
package audio

import (
	"math"
	"testing"
)

func TestDurationFromPCMBytes(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want float64
	}{
		{"empty", 0, 0},
		{"one second", 48000, 1},
		{"half second", 24000, 0.5},
		{"ten minutes", 48000 * 600, 600},
		{"odd trailing byte is ignored", 48001, 1},
	}
	for _, c := range cases {
		if got := DurationFromPCMBytes(c.size); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: DurationFromPCMBytes(%d) = %v, want %v", c.name, c.size, got, c.want)
		}
	}
}

func TestDurationFromPCMBytesNegative(t *testing.T) {
	if got := DurationFromPCMBytes(-5); got != 0 {
		t.Errorf("negative size should clamp to 0, got %v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd ~/projects/soniox-core && go test ./internal/audio/ -run TestDurationFromPCM -v`
Expected: FAIL — `undefined: DurationFromPCMBytes`

- [ ] **Step 3: Write the minimal implementation**

Create `internal/audio/decode.go`:

```go
package audio

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/angelospk/soniox-core/internal/soniox"
)

// BytesPerSecond is the byte rate of the PCM this package produces
// (s16le, mono, 24 kHz) — 48000 B/s.
const BytesPerSecond = soniox.SampleRate * soniox.NumChannels * 2

// DurationFromPCMBytes converts a PCM byte count to seconds. A dangling odd
// byte cannot form a sample and is ignored.
func DurationFromPCMBytes(size int64) float64 {
	if size <= 0 {
		return 0
	}
	return float64(size-size%2) / float64(BytesPerSecond)
}

// DecodeAll decodes the whole file to raw PCM (s16le, mono, 24 kHz) in a temp
// file and returns it open and rewound. The caller must Close and Remove it.
//
// One process replaces the previous ffprobe pass plus one ffmpeg decode per
// segment: segments are now byte ranges of this file.
func DecodeAll(ctx context.Context, path string) (*os.File, error) {
	out, err := os.CreateTemp("", "soniox-pcm-*.raw")
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		name := out.Name()
		_ = out.Close()
		_ = os.Remove(name)
	}

	// #nosec G204 -- fixed binary, args passed separately (no shell); path is a trusted local file
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path,
		"-f", "s16le", "-acodec", "pcm_s16le",
		"-ac", strconv.Itoa(soniox.NumChannels), "-ar", strconv.Itoa(soniox.SampleRate),
		"pipe:1")
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		cleanup()
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		cleanup()
		return nil, err
	}
	return out, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd ~/projects/soniox-core && go test ./internal/audio/ -run TestDurationFromPCM -v`
Expected: PASS (both tests)

- [ ] **Step 5: Change `feedPaced` to stream from an `io.Reader`**

In `internal/transcribe/transcribe.go`, replace the existing `feedPaced` (currently
takes `pcm []byte`) with:

```go
// feedPaced streams r into ch at ~real time (Soniox processes at ~1x; blasting a
// long segment overruns its buffer), closing ch when done. Reading one chunk at
// a time keeps memory bounded no matter how long the segment is.
func feedPaced(ctx context.Context, ch chan<- []byte, r io.Reader) {
	defer close(ch)
	cb := soniox.ChunkBytes(100)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		buf := make([]byte, cb)
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			select {
			case ch <- buf[:n]:
			case <-ctx.Done():
				return
			}
		}
		if err != nil { // io.EOF or io.ErrUnexpectedEOF -> last chunk sent
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
```

Note: a **fresh** `buf` per iteration is required. The chunk is handed to another
goroutine over `ch`; reusing one buffer would race.

Add `"io"` to the import block; `"os"` will be needed in the next step.

- [ ] **Step 6: Rewire `transcribe.File` to decode once**

In `internal/transcribe/transcribe.go`, replace the body of `File` from the
`audio.Duration` call through the end of the goroutine loop. The new head:

```go
func File(ctx context.Context, path string, opt FileOptions) (Result, error) {
	if opt.NoiseDB == 0 {
		opt.NoiseDB = -30
	}
	pcm, err := audio.DecodeAll(ctx, path)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		name := pcm.Name()
		_ = pcm.Close()
		_ = os.Remove(name)
	}()
	info, err := pcm.Stat()
	if err != nil {
		return Result{}, err
	}
	dur := audio.DurationFromPCMBytes(info.Size())
	if dur <= 0 {
		return Result{Jobs: 0, Segments: 0, Duration: 0}, nil
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
```

and inside the goroutine, replace the `audio.DecodePCM` call and the `feedPaced`
call with a section reader over the shared PCM file:

```go
			off, n := byteRange(sp, info.Size())
			if n <= 0 {
				return
			}
			ch := make(chan []byte)
			go feedPaced(ctx, ch, io.NewSectionReader(pcm, off, n))
```

Delete the now-unused `pcm, err := audio.DecodePCM(...)` block and its error
handling. `io.SectionReader` is safe for concurrent use because it reads via
`ReaderAt` — never share a `Seek`+`Read` handle across goroutines.

Add this helper at the bottom of the file:

```go
// byteRange maps a time span to a sample-aligned byte range of the PCM file,
// clamped to the file. Alignment matters: an odd offset would shift every
// following sample by one byte and turn the segment into noise.
func byteRange(sp segment.Span, size int64) (off, n int64) {
	off = int64(sp.Start * float64(audio.BytesPerSecond))
	end := int64(sp.End * float64(audio.BytesPerSecond))
	off -= off % 2
	end -= end % 2
	if off < 0 {
		off = 0
	}
	if end > size-size%2 {
		end = size - size%2
	}
	if end <= off {
		return off, 0
	}
	return off, end - off
}
```

- [ ] **Step 7: Verify the whole package builds and the suite is green**

Run: `cd ~/projects/soniox-core && go build ./... && go test ./... -race`
Expected: PASS. `audio.DecodePCM` is still referenced by nothing in `transcribe`
but stays exported for the `internal/audio` API — the build must not complain.

- [ ] **Step 8: Commit**

```bash
cd ~/projects/soniox-core
git add internal/audio/decode.go internal/audio/decode_test.go internal/transcribe/transcribe.go
git commit -m "perf(transcribe): decode once and feed segments from file sections

Replaces the ffprobe duration pass and the per-segment ffmpeg decode with a
single whole-file decode to a PCM temp file. Segments are now io.SectionReader
ranges over that file, so memory stays bounded and ffmpeg runs twice (decode +
silencedetect) instead of N+2 times. No audio semantics change."
```

---

## Task 2: The silence trimmer (pure, unused)

Three pieces: a streaming scanner, a **pure** planner, and a writer that glues
them. Only the planner holds policy, so all the tricky rules are testable with no
I/O at all.

**Files:**
- Create: `internal/audio/trim.go`
- Create: `internal/audio/trim_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/audio/trim_test.go`:

```go
package audio

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// pcm builds s16le mono PCM: alternating runs described as (seconds, amplitude).
// amplitude 0 is digital silence; 8000 is ordinary speech level (~-12 dBFS).
func pcm(runs ...[2]float64) []byte {
	var b bytes.Buffer
	for _, r := range runs {
		n := int(r[0] * float64(SampleRate))
		for i := 0; i < n; i++ {
			// alternate sign so a non-zero run has real RMS rather than DC
			v := int16(r[1])
			if i%2 == 1 {
				v = -v
			}
			_ = binary.Write(&b, binary.LittleEndian, v)
		}
	}
	return b.Bytes()
}

func defaultOpts() TrimOptions {
	return TrimOptions{NoiseDB: -30, BoundaryMin: 0.4, TrimMin: 0.6, Keep: 0.3, MaxSilenceRatio: 0.8}
}

func trim(t *testing.T, in []byte, opt TrimOptions) (out []byte, res TrimResult) {
	t.Helper()
	var buf bytes.Buffer
	res, err := TrimSilence(&buf, bytes.NewReader(in), int64(len(in)), opt)
	if err != nil {
		t.Fatalf("TrimSilence: %v", err)
	}
	return buf.Bytes(), res
}

func TestTrimNoSilenceIsUnchanged(t *testing.T) {
	in := pcm([2]float64{2, 8000})
	out, res := trim(t, in, defaultOpts())
	if !bytes.Equal(in, out) {
		t.Errorf("continuous speech must pass through: in %d bytes, out %d", len(in), len(out))
	}
	if res.Trimmed {
		t.Errorf("Trimmed should be false when nothing was shortened")
	}
}

func TestTrimShortensLongPause(t *testing.T) {
	// 1s speech, 2s silence, 1s speech -> silence capped to 0.3s
	in := pcm([2]float64{1, 8000}, [2]float64{2, 0}, [2]float64{1, 8000})
	out, res := trim(t, in, defaultOpts())
	want := int(2.3 * float64(BytesPerSecond))
	if len(out) != want {
		t.Errorf("out = %d bytes, want %d", len(out), want)
	}
	if !res.Trimmed {
		t.Errorf("Trimmed should be true")
	}
	if res.OutBytes != int64(len(out)) {
		t.Errorf("OutBytes = %d, want %d", res.OutBytes, len(out))
	}
}

func TestTrimKeepsSubTrimMinPauseWhole(t *testing.T) {
	// a 0.5s pause is below TrimMin (0.6) -> survives intact
	in := pcm([2]float64{1, 8000}, [2]float64{0.5, 0}, [2]float64{1, 8000})
	out, _ := trim(t, in, defaultOpts())
	if len(out) != len(in) {
		t.Errorf("0.5s pause must not be trimmed: out %d, in %d", len(out), len(in))
	}
}

func TestBoundariesSurviveWhenNothingIsTrimmed(t *testing.T) {
	// two 0.5s pauses: nothing trimmed, but BOTH are cut candidates (>= 0.4)
	in := pcm([2]float64{1, 8000}, [2]float64{0.5, 0}, [2]float64{1, 8000},
		[2]float64{0.5, 0}, [2]float64{1, 8000})
	_, res := trim(t, in, defaultOpts())
	if len(res.Boundaries) != 2 {
		t.Fatalf("want 2 boundary candidates, got %v", res.Boundaries)
	}
	for i, want := range []float64{1.25, 2.75} {
		if math.Abs(res.Boundaries[i]-want) > 0.02 {
			t.Errorf("boundary[%d] = %v, want ~%v", i, res.Boundaries[i], want)
		}
	}
}

func TestBoundaryIgnoresSubBoundaryMinPause(t *testing.T) {
	// 0.3s pause is below BoundaryMin (0.4) -> not a candidate
	in := pcm([2]float64{1, 8000}, [2]float64{0.3, 0}, [2]float64{1, 8000})
	_, res := trim(t, in, defaultOpts())
	if len(res.Boundaries) != 0 {
		t.Errorf("want no candidates, got %v", res.Boundaries)
	}
}

func TestBoundariesAreOnTheTrimmedTimeline(t *testing.T) {
	// 1s speech, 2s pause (-> 0.3s), 1s speech, 1s pause, 1s speech.
	// After trimming the second pause starts at 2.3s, so its mid is ~3.8s.
	in := pcm([2]float64{1, 8000}, [2]float64{2, 0}, [2]float64{1, 8000},
		[2]float64{1, 0}, [2]float64{1, 8000})
	_, res := trim(t, in, defaultOpts())
	if len(res.Boundaries) != 2 {
		t.Fatalf("want 2 candidates, got %v", res.Boundaries)
	}
	for i, want := range []float64{1.15, 3.8} {
		if math.Abs(res.Boundaries[i]-want) > 0.03 {
			t.Errorf("boundary[%d] = %v, want ~%v", i, res.Boundaries[i], want)
		}
	}
}

func TestTrimFailsOpenOnLowGainAudio(t *testing.T) {
	// every sample is below -30 dBFS: this is a quiet phone recording, not
	// silence. The guard must return the input byte-identical.
	in := pcm([2]float64{5, 300}) // ~-40 dBFS
	out, res := trim(t, in, defaultOpts())
	if !bytes.Equal(in, out) {
		t.Fatalf("low-gain audio must pass through untouched: in %d, out %d", len(in), len(out))
	}
	if res.Trimmed {
		t.Errorf("Trimmed must be false when the guard fires")
	}
	if len(res.Boundaries) != 0 {
		t.Errorf("a passed-through stream offers no candidates, got %v", res.Boundaries)
	}
}

func TestTrimAllSilence(t *testing.T) {
	// pure digital silence: ratio is 1.0 > MaxSilenceRatio -> pass through
	in := pcm([2]float64{5, 0})
	out, res := trim(t, in, defaultOpts())
	if !bytes.Equal(in, out) || res.Trimmed {
		t.Errorf("all-silence input must pass through, got %d bytes trimmed=%v", len(out), res.Trimmed)
	}
}

func TestTrimEmptyInput(t *testing.T) {
	out, res := trim(t, nil, defaultOpts())
	if len(out) != 0 || res.Trimmed || res.OutBytes != 0 {
		t.Errorf("empty input: out=%d trimmed=%v outBytes=%d", len(out), res.Trimmed, res.OutBytes)
	}
}

func TestTrimClipShorterThanKeep(t *testing.T) {
	in := pcm([2]float64{0.1, 8000})
	out, _ := trim(t, in, defaultOpts())
	if !bytes.Equal(in, out) {
		t.Errorf("clip shorter than Keep must be unchanged")
	}
}

func TestTrimOddTrailingByte(t *testing.T) {
	in := append(pcm([2]float64{1, 8000}, [2]float64{2, 0}, [2]float64{1, 8000}), 0x7f)
	out, _ := trim(t, in, defaultOpts())
	if len(out)%2 != 0 {
		t.Errorf("output must have an even byte length, got %d", len(out))
	}
}

func TestTrimBoundaryValues(t *testing.T) {
	cases := []struct {
		name      string
		pause     float64
		wantTrim  bool
		wantCands int
	}{
		{"below boundary min", 0.399, false, 0},
		{"at boundary min", 0.400, false, 1},
		{"below trim min", 0.599, false, 1},
		{"at trim min", 0.600, false, 1},
		{"above trim min", 0.601, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := pcm([2]float64{1, 8000}, [2]float64{c.pause, 0}, [2]float64{1, 8000})
			_, res := trim(t, in, defaultOpts())
			if res.Trimmed != c.wantTrim {
				t.Errorf("Trimmed = %v, want %v", res.Trimmed, c.wantTrim)
			}
			if len(res.Boundaries) != c.wantCands {
				t.Errorf("candidates = %v, want %d", res.Boundaries, c.wantCands)
			}
		})
	}
}

func TestTrimOutputIsMonotonicAndCovering(t *testing.T) {
	// invariant: planTrim spans are ordered, non-overlapping, in range
	in := pcm([2]float64{1, 8000}, [2]float64{2, 0}, [2]float64{1, 8000},
		[2]float64{3, 0}, [2]float64{1, 8000})
	runs, total, err := scanSilence(bytes.NewReader(in), defaultOpts())
	if err != nil {
		t.Fatalf("scanSilence: %v", err)
	}
	keep, _, _ := planTrim(runs, total, defaultOpts())
	var prev int64
	for i, s := range keep {
		if s.start < prev {
			t.Errorf("span %d starts at %d, before previous end %d", i, s.start, prev)
		}
		if s.end <= s.start {
			t.Errorf("span %d is empty or inverted: %+v", i, s)
		}
		if s.end > total {
			t.Errorf("span %d ends at %d, past total %d", i, s.end, total)
		}
		prev = s.end
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd ~/projects/soniox-core && go test ./internal/audio/ -run 'TestTrim|TestBoundar' -v`
Expected: FAIL — `undefined: TrimOptions`, `undefined: TrimSilence`, etc.

- [ ] **Step 3: Write the implementation**

Create `internal/audio/trim.go`:

```go
package audio

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/angelospk/soniox-core/internal/soniox"
)

// frameMS is the classification window. 20 ms is short enough to place a cut
// precisely and long enough for a stable RMS estimate.
const frameMS = 20

// fadeMS is the ramp applied on both sides of a splice. Frame RMS below the
// threshold does not bound individual samples, so a hard cut can leave a DC
// step; a short fade is cheaper and more robust than zero-crossing hunting.
const fadeMS = 5

// TrimOptions configure silence capping. Durations are seconds.
//
// BoundaryMin, TrimMin and Keep are deliberately independent: using TrimMin as
// the cut-candidate threshold would leave a file whose pauses are all
// BoundaryMin..TrimMin with no candidates at all, collapsing parallelism to one
// segment.
type TrimOptions struct {
	NoiseDB         float64 // silence threshold in dBFS (e.g. -30)
	BoundaryMin     float64 // a silent run this long may host a segment cut
	TrimMin         float64 // a silent run longer than this is shortened
	Keep            float64 // what survives of a shortened run
	MaxSilenceRatio float64 // fail open above this silent share (e.g. 0.8)
}

// TrimResult reports what happened. Boundaries are silence midpoints on the
// OUTPUT timeline, in seconds, ready for segment.Plan.
type TrimResult struct {
	Trimmed    bool
	OutBytes   int64
	Boundaries []float64
}

type span struct{ start, end int64 } // sample indices, [start, end)

// run is a maximal stretch of frames sharing one silent/loud classification.
type run struct {
	silent     bool
	start, end int64 // sample indices
}

func samples(sec float64) int64 { return int64(sec * float64(soniox.SampleRate)) }

// frameDB returns the RMS of one s16le frame in dBFS, or -Inf for pure silence.
func frameDB(b []byte) float64 {
	n := len(b) / 2
	if n == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(b[i*2:]))) / 32768
		sum += s * s
	}
	rms := math.Sqrt(sum / float64(n))
	if rms <= 0 {
		return math.Inf(-1)
	}
	return 20 * math.Log10(rms)
}

// scanSilence classifies the stream frame by frame and collapses the result to
// runs. Memory is bounded: one frame buffer plus the run list.
func scanSilence(r io.Reader, opt TrimOptions) ([]run, int64, error) {
	fs := samples(frameMS / 1000.0)
	buf := make([]byte, fs*2)
	var runs []run
	var total int64
	for {
		n, err := io.ReadFull(r, buf)
		n -= n % 2 // a dangling odd byte cannot form a sample
		if n > 0 {
			got := int64(n / 2)
			silent := frameDB(buf[:n]) < opt.NoiseDB
			if len(runs) > 0 && runs[len(runs)-1].silent == silent {
				runs[len(runs)-1].end = total + got
			} else {
				runs = append(runs, run{silent: silent, start: total, end: total + got})
			}
			total += got
		}
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return runs, total, nil
			}
			return nil, 0, err
		}
	}
}

// planTrim decides what to keep. Pure: no I/O, so every policy rule above is
// unit-testable against synthetic runs.
//
// Returns the kept sample spans, the surviving silence midpoints on the OUTPUT
// timeline, and whether anything was actually shortened. A false third return
// with a single full-length span means "pass through untouched".
func planTrim(runs []run, total int64, opt TrimOptions) ([]span, []float64, bool) {
	if total <= 0 || len(runs) == 0 {
		return nil, nil, false
	}

	// Fail open. An absolute threshold misreads a low-gain recording as entirely
	// silent, and capping would then delete the speaker's words. False negatives
	// cost speed; false positives cost words.
	var silent int64
	var loud int64
	for _, r := range runs {
		if r.silent {
			silent += r.end - r.start
		} else {
			loud += r.end - r.start
		}
	}
	if loud == 0 || float64(silent)/float64(total) > opt.MaxSilenceRatio {
		return []span{{0, total}}, nil, false
	}

	trimMin, keep, boundMin := samples(opt.TrimMin), samples(opt.Keep), samples(opt.BoundaryMin)
	var kept []span
	var bounds []float64
	var out int64 // running length of the output, in samples
	trimmed := false

	add := func(s span) {
		if s.end <= s.start {
			return
		}
		if n := len(kept); n > 0 && kept[n-1].end == s.start {
			kept[n-1].end = s.end // merge contiguous
		} else {
			kept = append(kept, s)
		}
		out += s.end - s.start
	}

	for _, r := range runs {
		length := r.end - r.start
		if !r.silent {
			add(span{r.start, r.end})
			continue
		}
		startOut := out
		survived := length
		if length > trimMin {
			// Keep both shoulders, not an arbitrary contiguous block, so the
			// transitions into and out of the pause both survive.
			half := keep / 2
			add(span{r.start, r.start + half})
			add(span{r.end - (keep - half), r.end})
			survived = keep
			trimmed = true
		} else {
			add(span{r.start, r.end})
		}
		// Candidacy is judged on the ORIGINAL length: a capped 3-second pause is
		// still an excellent place to cut.
		if length >= boundMin {
			mid := float64(startOut+survived/2) / float64(soniox.SampleRate)
			bounds = append(bounds, mid)
		}
	}
	return kept, bounds, trimmed
}

// TrimSilence caps over-long silent runs in src (PCM s16le mono 24 kHz, `size`
// bytes) and writes the result to dst.
//
// When the fail-open guard fires, or when nothing needed shortening, src is
// copied through byte for byte and Trimmed is false.
func TrimSilence(dst io.Writer, src io.ReaderAt, size int64, opt TrimOptions) (TrimResult, error) {
	if size < 2 {
		return TrimResult{}, nil
	}
	runs, total, err := scanSilence(io.NewSectionReader(src, 0, size), opt)
	if err != nil {
		return TrimResult{}, err
	}
	kept, bounds, trimmed := planTrim(runs, total, opt)
	if !trimmed {
		n, err := io.Copy(dst, io.NewSectionReader(src, 0, total*2))
		return TrimResult{Trimmed: false, OutBytes: n, Boundaries: bounds}, err
	}

	fade := samples(fadeMS / 1000.0)
	var written int64
	for i, s := range kept {
		buf := make([]byte, (s.end-s.start)*2)
		if _, err := src.ReadAt(buf, s.start*2); err != nil && err != io.EOF {
			return TrimResult{}, err
		}
		// Ramp only where a real cut happened — the first span's head and the
		// last span's tail are the untouched edges of the recording.
		if i > 0 {
			applyFade(buf, fade, true)
		}
		if i < len(kept)-1 {
			applyFade(buf, fade, false)
		}
		n, err := dst.Write(buf)
		written += int64(n)
		if err != nil {
			return TrimResult{}, err
		}
	}
	return TrimResult{Trimmed: true, OutBytes: written, Boundaries: bounds}, nil
}

// applyFade ramps n samples at the head (in) or tail (out) of a PCM buffer.
func applyFade(b []byte, n int64, in bool) {
	total := int64(len(b) / 2)
	if total == 0 {
		return
	}
	if n > total {
		n = total
	}
	for i := int64(0); i < n; i++ {
		idx := i
		if !in {
			idx = total - 1 - i
		}
		g := float64(i) / float64(n)
		v := int16(binary.LittleEndian.Uint16(b[idx*2:]))
		binary.LittleEndian.PutUint16(b[idx*2:], uint16(int16(float64(v)*g)))
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd ~/projects/soniox-core && go test ./internal/audio/ -v`
Expected: PASS — every `TestTrim*`, `TestBoundar*`, plus the pre-existing
`TestParseSilences*` tests.

If `TestTrimShortensLongPause` is off by a frame's worth of bytes, the cause is
frame quantisation: run boundaries land on 20 ms multiples. The assertion allows
none, so adjust the test to a tolerance of one frame (`960` bytes) rather than
loosening the implementation.

- [ ] **Step 5: Run the race detector and vet**

Run: `cd ~/projects/soniox-core && go vet ./internal/audio/ && go test ./internal/audio/ -race`
Expected: PASS, no vet diagnostics.

- [ ] **Step 6: Commit**

```bash
cd ~/projects/soniox-core
git add internal/audio/trim.go internal/audio/trim_test.go
git commit -m "feat(audio): add silence trimmer with fail-open guard

Caps silent runs longer than TrimMin down to Keep, retaining both shoulders,
and reports surviving silence midpoints on the trimmed timeline as segment cut
candidates. BoundaryMin is independent of TrimMin so short pauses stay usable as
cut points. Fails open (byte-identical passthrough) when the silent share
exceeds MaxSilenceRatio, so a low-gain recording is never gutted. Not wired in
yet."
```

---

## Task 3: Wire trimming in, default OFF

**Files:**
- Modify: `internal/transcribe/transcribe.go`
- Create: `internal/transcribe/transcribe_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/transcribe_test.go`

- [ ] **Step 1: Write the failing test for job-count preservation**

This is the regression that silently erases the entire speedup. Create
`internal/transcribe/transcribe_test.go`:

```go
package transcribe

import "testing"

// Trimming must not reduce the job count. A 10-minute file gets 10 jobs; if the
// trimmed 6-minute stream were re-measured it would get 6 jobs of the same
// length and finish in the same wall clock — the whole optimization erased.
func TestJobsComeFromOriginalDuration(t *testing.T) {
	got := planJobs(600, 360, 0)
	if got != 10 {
		t.Errorf("planJobs(orig=600, trimmed=360) = %d, want 10 (from the original)", got)
	}
}

func TestJobsRespectExplicitOverride(t *testing.T) {
	if got := planJobs(600, 360, 4); got != 4 {
		t.Errorf("explicit Jobs must win, got %d", got)
	}
}

func TestJobsClampedByMinimumSegment(t *testing.T) {
	// 30s trimmed / 10 jobs would be 3s segments; minSeg (15s) caps it at 2.
	if got := planJobs(600, 30, 0); got != 2 {
		t.Errorf("planJobs clamped by minSeg = %d, want 2", got)
	}
}

func TestJobsNeverBelowOne(t *testing.T) {
	if got := planJobs(1, 0.1, 0); got != 1 {
		t.Errorf("planJobs = %d, want at least 1", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd ~/projects/soniox-core && go test ./internal/transcribe/ -v`
Expected: FAIL — `undefined: planJobs`

- [ ] **Step 3: Implement `planJobs` and the trim path**

In `internal/transcribe/transcribe.go`, add the constant and helper:

```go
const minSeg = 15 // seconds; below this the per-stream overhead dominates

// planJobs picks parallelism from the ORIGINAL duration, then clamps so the
// trimmed stream still yields segments worth opening a socket for.
func planJobs(origDur, trimmedDur float64, explicit int) int {
	if explicit > 0 {
		return explicit
	}
	jobs := segment.AutoJobs(origDur, targetSeg, maxJobs)
	if trimmedDur > 0 {
		if max := int(trimmedDur / minSeg); max < jobs {
			jobs = max
		}
	}
	if jobs < 1 {
		jobs = 1
	}
	return jobs
}
```

Extend `FileOptions` with the trim knobs:

```go
	Trim     bool            // cap over-long silences before streaming
	TrimOpts audio.TrimOptions
```

Then replace the planning block in `File` (added in Task 1) with:

```go
	origDur := audio.DurationFromPCMBytes(info.Size())
	if origDur <= 0 {
		return Result{}, nil
	}

	stream, streamSize := io.ReaderAt(pcm), info.Size()
	dur := origDur
	var silences []float64

	if opt.Trim {
		trimmed, err := os.CreateTemp("", "soniox-trim-*.raw")
		if err != nil {
			return Result{}, err
		}
		defer func() {
			name := trimmed.Name()
			_ = trimmed.Close()
			_ = os.Remove(name)
		}()
		res, err := audio.TrimSilence(trimmed, pcm, info.Size(), opt.TrimOpts)
		if err != nil {
			return Result{}, err
		}
		stream, streamSize = trimmed, res.OutBytes
		dur = audio.DurationFromPCMBytes(res.OutBytes)
		silences = res.Boundaries
	} else {
		// legacy path: a second ffmpeg pass over the source file
		if silences, err = audio.DetectSilences(ctx, path, opt.NoiseDB, 0.4); err != nil {
			return Result{}, err
		}
	}

	jobs := planJobs(origDur, dur, opt.Jobs)
	spans := segment.Plan(dur, jobs, silences)
```

`info.Size()` is no longer the section source when trimming, so update the
section reader in the goroutine to use `stream` and `streamSize`:

```go
			off, n := byteRange(sp, streamSize)
			if n <= 0 {
				return
			}
			ch := make(chan []byte)
			go feedPaced(ctx, ch, io.NewSectionReader(stream, off, n))
```

Report the **original** duration to callers — `Result.Duration` is the length of
the user's recording, not an internal artefact:

```go
	res := Result{Raw: raw, Cleaned: raw, Jobs: jobs, Segments: len(spans), Duration: origDur}
```

- [ ] **Step 4: Run to verify the tests pass**

Run: `cd ~/projects/soniox-core && go test ./internal/transcribe/ -v && go build ./...`
Expected: PASS (4 tests), clean build.

- [ ] **Step 5: Add env-backed defaults**

In `internal/config/config.go`, next to `GeminiKey()`, add:

```go
// TrimDefaults reads the silence-trim knobs from the environment. Every value
// falls back to the tuned default when unset or unparseable, so a typo degrades
// to stock behaviour rather than to a broken threshold.
func TrimDefaults() audio.TrimOptions {
	return audio.TrimOptions{
		NoiseDB:         envFloat("SONIOX_TRIM_NOISE_DB", -30),
		BoundaryMin:     envFloat("SONIOX_TRIM_BOUNDARY_MIN", 0.4),
		TrimMin:         envFloat("SONIOX_TRIM_MIN", 0.6),
		Keep:            envFloat("SONIOX_TRIM_KEEP", 0.3),
		MaxSilenceRatio: envFloat("SONIOX_TRIM_MAX_SILENCE_RATIO", 0.8),
	}
}

// TrimEnabledDefault is the server-wide default when the request says nothing.
func TrimEnabledDefault() bool {
	return os.Getenv("SONIOX_TRIM") == "1"
}

func envFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil || v <= 0 && def > 0 {
		return def
	}
	return v
}
```

Add `"strconv"`, `"strings"` and the `internal/audio` import if absent.

Note the `v <= 0 && def > 0` clause: it rejects a zero or negative duration while
still permitting `NoiseDB`, whose sane values are negative.

- [ ] **Step 5b: Test that bad thresholds fall back instead of breaking**

Append to `internal/config/config_test.go`:

```go
func TestEnvFloatFallsBackOnGarbage(t *testing.T) {
	cases := []struct {
		name, val string
		def, want float64
	}{
		{"unset", "", 0.6, 0.6},
		{"garbage", "abc", 0.6, 0.6},
		{"zero rejected for a duration", "0", 0.6, 0.6},
		{"negative rejected for a duration", "-1", 0.6, 0.6},
		{"negative allowed for a dB level", "-40", -30, -40},
		{"valid", "0.9", 0.6, 0.9},
		{"whitespace tolerated", "  0.9  ", 0.6, 0.9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SONIOX_TRIM_TEST_KEY", c.val)
			if got := envFloat("SONIOX_TRIM_TEST_KEY", c.def); got != c.want {
				t.Errorf("envFloat(%q, %v) = %v, want %v", c.val, c.def, got, c.want)
			}
		})
	}
}
```

Run: `cd ~/projects/soniox-core && go test ./internal/config/ -run TestEnvFloat -v`
Expected: PASS. If "negative allowed for a dB level" fails, the guard clause has
lost its `&& def > 0` and is rejecting valid negative dB thresholds.

- [ ] **Step 6: Add the failing HTTP parsing test**

Append to `internal/server/transcribe_test.go`:

```go
func TestParseTrimFlag(t *testing.T) {
	cases := []struct {
		q    string
		def  bool
		want bool
	}{
		{"", false, false},
		{"", true, true},
		{"0", true, false},
		{"0", false, false},
		{"1", false, true},
		{"nonsense", true, true},
		{"nonsense", false, false},
	}
	for _, c := range cases {
		if got := parseTrim(c.q, c.def); got != c.want {
			t.Errorf("parseTrim(%q, %v) = %v, want %v", c.q, c.def, got, c.want)
		}
	}
}
```

- [ ] **Step 7: Run to verify it fails**

Run: `cd ~/projects/soniox-core && go test ./internal/server/ -run TestParseTrim -v`
Expected: FAIL — `undefined: parseTrim`

- [ ] **Step 8: Implement the handler wiring**

In `internal/server/server.go`, add near `handleTranscribe`:

```go
// parseTrim resolves the tri-state ?trim= flag: absent or unparseable keeps the
// server default, "0" disables, "1" enables.
func parseTrim(v string, def bool) bool {
	switch v {
	case "0":
		return false
	case "1":
		return true
	default:
		return def
	}
}
```

Add `Trim bool` and `TrimOpts audio.TrimOptions` to the `Server` struct, and in
`handleTranscribe` pass them through:

```go
	res, err := s.transcribeFile(r.Context(), tmp.Name(), transcribe.FileOptions{
		Key: key, Langs: s.Langs,
		Trim:     parseTrim(r.URL.Query().Get("trim"), s.Trim),
		TrimOpts: s.TrimOpts,
	})
```

In `cmd/soniox/main.go`, populate them where the `Server` is constructed:

```go
		Trim:     config.TrimEnabledDefault(),
		TrimOpts: config.TrimDefaults(),
```

- [ ] **Step 9: Run the full suite**

Run: `cd ~/projects/soniox-core && go build ./... && go vet ./... && go test ./... -race`
Expected: PASS throughout.

- [ ] **Step 10: Commit**

```bash
cd ~/projects/soniox-core
git add internal/transcribe/ internal/config/config.go internal/server/ cmd/soniox/main.go
git commit -m "feat(transcribe): opt-in silence trimming behind ?trim=

Adds FileOptions.Trim, env-backed thresholds, and tri-state ?trim= parsing.
Default stays off pending measurement. Job count now comes from the original
duration via planJobs, so trimming cannot shrink parallelism and cancel its own
benefit; Result.Duration keeps reporting the real recording length."
```

---

## Task 4: Measure, then flip the default on

**Files:**
- Create: `docs/superpowers/notes/2026-08-15-trim-measurements.md`
- Modify: `internal/config/config.go`
- Modify: `README.md`

- [ ] **Step 1: Build and restart the server**

```bash
cd ~/projects/soniox-core && make install
launchctl kickstart -k gui/$(id -u)/com.soniox.server
```

- [ ] **Step 2: Collect four real recordings**

Place them in `/tmp/trimbench/` as `.m4a`/`.mp3`/`.wav`:

1. `pauses.*` — speech with long thinking pauses (the case this targets)
2. `dense.*` — continuous speech, few pauses (must not regress)
3. `lowgain.*` — a quiet phone recording (the guard must fire)
4. `short.*` — under 30 seconds

- [ ] **Step 3: Run the A/B**

```bash
cd /tmp/trimbench
for f in *; do
  for t in 0 1; do
    printf '%s trim=%s ' "$f" "$t"
    /usr/bin/time -p curl -s -F "file=@$f" \
      "http://127.0.0.1:8722/transcribe?trim=$t" -o "${f%.*}.trim$t.json" 2>&1 |
      awk '/^real/{printf "%ss\n", $2}'
  done
done
```

- [ ] **Step 4: Compare the transcripts, not just the clock**

```bash
cd /tmp/trimbench
for f in *.trim0.json; do
  b="${f%.trim0.json}"
  echo "=== $b ==="
  diff <(python3 -c "import json,sys;print(json.load(open('$f'))['raw'])") \
       <(python3 -c "import json,sys;print(json.load(open('${b}.trim1.json'))['raw'])") \
    && echo "identical"
done
```

Acceptance:
- `pauses` — materially faster with no substantive change in text or punctuation.
- `dense` — no slower than `trim=0` (a small loss to the scan pass is acceptable).
- `lowgain` — text **identical**; confirm the guard fired rather than the audio
  merely lacking silence.
- `short` — no crash, no empty transcript.

- [ ] **Step 4b: Confirm exactly one ffmpeg process on the trim path**

The codebase has no injectable command runner, so observe the real processes.
In one terminal:

```bash
while true; do pgrep -fl "ffmpeg|ffprobe" | grep -v pgrep; sleep 0.2; done
```

In another, transcribe the longest file with `?trim=1`. Expected: a single
`ffmpeg` line (the whole-file decode), **no** `ffprobe` and **no** second
`ffmpeg` for `silencedetect`. Repeat with `?trim=0` — that path legitimately
shows two, since it still runs `silencedetect`.

- [ ] **Step 5: Record the numbers**

Write `docs/superpowers/notes/2026-08-15-trim-measurements.md` with a table of
file, duration, `trim=0` seconds, `trim=1` seconds, and a note on transcript
differences. **If `pauses` shows no meaningful gain, stop here and do not flip the
default** — the honest conclusion is that this user's speech lacks the long
pauses the optimization targets, and the next lever (pacing, or the Soniox async
file API) should be investigated instead.

- [ ] **Step 6: Flip the default**

In `internal/config/config.go`:

```go
// TrimEnabledDefault is the server-wide default when the request says nothing.
// On by default since the 2026-08-15 measurements; SONIOX_TRIM=0 opts out.
func TrimEnabledDefault() bool {
	return os.Getenv("SONIOX_TRIM") != "0"
}
```

- [ ] **Step 7: Document it in the README**

Add to the file-transcription section:

```markdown
### Silence trimming

File transcription caps silent runs longer than 0.6 s down to 0.3 s before
streaming, because a Soniox realtime stream runs at ~1x and silence otherwise
costs full wall-clock time. Pass `?trim=0` (or set `SONIOX_TRIM=0`) to disable.
Thresholds: `SONIOX_TRIM_NOISE_DB`, `SONIOX_TRIM_BOUNDARY_MIN`, `SONIOX_TRIM_MIN`,
`SONIOX_TRIM_KEEP`, `SONIOX_TRIM_MAX_SILENCE_RATIO`.

A recording whose speech sits below the noise threshold throughout (a low-gain
phone recording) is passed through untouched rather than gutted.
```

- [ ] **Step 8: Full suite, then commit**

```bash
cd ~/projects/soniox-core
go build ./... && go vet ./... && go test ./... -race
git add internal/config/config.go README.md docs/superpowers/notes/
git commit -m "feat(transcribe): enable silence trimming by default

Measured on real recordings (see docs/superpowers/notes/). SONIOX_TRIM=0 or
?trim=0 restores the previous behaviour."
```

- [ ] **Step 9: Run CodeRabbit over the branch**

```bash
coderabbit --agent
```

Fix `critical` and `important` findings before declaring Phase 1 done. Leave
`nit` findings as notes.

---

## Definition of done

- `go test ./... -race` green.
- Exactly **one** ffmpeg process per file transcription on the default path
  (verify: `ps` during a long transcription, or count invocations with a fake
  command runner).
- Measurements recorded, including a negative result if that is what the data
  says.
- A low-gain recording returns byte-identical audio through the trimmer.
