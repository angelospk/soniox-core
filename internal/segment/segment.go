// Package segment plans where to cut an audio file for parallel transcription.
//
// A single Soniox realtime stream runs at ~1x real time, so to go faster we split
// the file into N pieces and transcribe them concurrently. Cuts are snapped to
// silences so we never slice through a word.
package segment

import (
	"math"
	"sort"
)

// Span is a [Start, End) time range in seconds.
type Span struct {
	Start float64
	End   float64
}

// Dur returns the span length in seconds.
func (s Span) Dur() float64 { return s.End - s.Start }

// AutoJobs picks a sane parallelism for a clip of the given duration: aim for
// ~targetSeg-second segments, clamped to [1, maxJobs].
func AutoJobs(duration, targetSeg float64, maxJobs int) int {
	if duration <= 0 || targetSeg <= 0 {
		return 1
	}
	n := int(math.Round(duration / targetSeg))
	if n < 1 {
		n = 1
	}
	if n > maxJobs {
		n = maxJobs
	}
	return n
}

// Plan returns nSegments spans covering [0, duration), with each internal cut
// snapped to the nearest silence midpoint when one is within half a segment.
// silences is a list of silence-midpoint timestamps (seconds); it may be empty.
func Plan(duration float64, nSegments int, silences []float64) []Span {
	if duration <= 0 {
		return nil
	}
	if nSegments <= 1 || len(silences) == 0 {
		return []Span{{0, duration}}
	}

	sorted := append([]float64(nil), silences...)
	sort.Float64s(sorted)
	segLen := duration / float64(nSegments)

	cuts := []float64{0}
	for k := 1; k < nSegments; k++ {
		ideal := segLen * float64(k)
		nearest := nearestTo(sorted, ideal)
		if math.Abs(nearest-ideal) <= segLen/2 {
			cuts = append(cuts, round2(nearest))
		} else {
			cuts = append(cuts, round2(ideal))
		}
	}
	cuts = append(cuts, duration)

	cuts = dedupSorted(cuts)
	spans := make([]Span, 0, len(cuts)-1)
	for i := 0; i+1 < len(cuts); i++ {
		if cuts[i+1]-cuts[i] > 0.2 { // drop slivers
			spans = append(spans, Span{cuts[i], cuts[i+1]})
		}
	}
	if len(spans) == 0 {
		return []Span{{0, duration}}
	}
	return spans
}

func nearestTo(sorted []float64, target float64) float64 {
	best := sorted[0]
	bestD := math.Abs(best - target)
	for _, s := range sorted[1:] {
		if d := math.Abs(s - target); d < bestD {
			best, bestD = s, d
		}
	}
	return best
}

func dedupSorted(xs []float64) []float64 {
	sort.Float64s(xs)
	out := xs[:0:0]
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }
