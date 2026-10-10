// Package audio shells out to ffmpeg/ffprobe to inspect and decode audio into
// the raw PCM (s16le, mono, 24kHz) that Soniox expects.
package audio

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/angelospk/soniox-core/internal/soniox"
)

// Duration returns the audio length in seconds via ffprobe.
func Duration(ctx context.Context, path string) (float64, error) {
	// #nosec G204 -- fixed binary, args passed separately (no shell); path is a trusted local file
	out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	s := strings.TrimSpace(string(out))
	d, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration parse %q: %w", s, err)
	}
	return d, nil
}

var (
	reStart = regexp.MustCompile(`silence_start:\s*([0-9.]+)`)
	reEnd   = regexp.MustCompile(`silence_end:\s*([0-9.]+)`)
)

// parseSilences extracts silence midpoints (seconds) from ffmpeg silencedetect
// stderr output. Pure, so it can be unit-tested without ffmpeg.
func parseSilences(log string) []float64 {
	starts := floats(reStart.FindAllStringSubmatch(log, -1))
	ends := floats(reEnd.FindAllStringSubmatch(log, -1))
	mids := make([]float64, 0, len(starts))
	for i, s := range starts {
		e := s
		if i < len(ends) {
			e = ends[i]
		}
		mids = append(mids, (s+e)/2)
	}
	sort.Float64s(mids)
	return mids
}

func floats(m [][]string) []float64 {
	out := make([]float64, 0, len(m))
	for _, g := range m {
		if v, err := strconv.ParseFloat(g[1], 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

// DetectSilences returns silence midpoints (seconds) using ffmpeg silencedetect.
func DetectSilences(ctx context.Context, path string, noiseDB int, minDur float64) ([]float64, error) {
	// #nosec G204 -- fixed binary, args passed separately (no shell); path is a trusted local file
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-nostats",
		"-i", path,
		"-af", fmt.Sprintf("silencedetect=noise=%ddB:d=%g", noiseDB, minDur),
		"-f", "null", "-")
	// silencedetect writes to stderr; ffmpeg exits 0
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("silencedetect: %w", err)
	}
	return parseSilences(stderr.String()), nil
}

// DecodePCM decodes [start, start+dur) of path to raw PCM s16le mono @ 24kHz.
func DecodePCM(ctx context.Context, path string, start, dur float64) ([]byte, error) {
	// #nosec G204 -- fixed binary, args passed separately (no shell); path is a trusted local file
	out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-ss", fmt.Sprintf("%g", start), "-t", fmt.Sprintf("%g", dur), "-i", path,
		"-f", "s16le", "-acodec", "pcm_s16le",
		"-ac", strconv.Itoa(soniox.NumChannels), "-ar", strconv.Itoa(soniox.SampleRate),
		"pipe:1").Output()
	if err != nil {
		return nil, fmt.Errorf("decode %s [%g,%g]: %w", path, start, dur, err)
	}
	return out, nil
}
