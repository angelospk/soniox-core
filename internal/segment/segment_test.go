package segment

import (
	"math"
	"testing"
)

func TestAutoJobs(t *testing.T) {
	cases := []struct {
		dur  float64
		want int
	}{
		{30, 1}, {45, 1}, {90, 2}, {180, 3}, {600, 10}, {1285, 18}, {3600, 18},
	}
	for _, c := range cases {
		if got := AutoJobs(c.dur, 60, 18); got != c.want {
			t.Errorf("AutoJobs(%.0f) = %d, want %d", c.dur, got, c.want)
		}
	}
	if AutoJobs(0, 60, 18) != 1 || AutoJobs(100, 0, 18) != 1 {
		t.Error("degenerate inputs should give 1")
	}
}

func TestPlanCoversWholeDuration(t *testing.T) {
	silences := []float64{}
	for s := 5.0; s < 1285; s += 5 {
		silences = append(silences, s)
	}
	spans := Plan(1285, 18, silences)
	if len(spans) < 2 {
		t.Fatalf("expected multiple spans, got %d", len(spans))
	}
	if spans[0].Start != 0 {
		t.Errorf("first span must start at 0, got %v", spans[0].Start)
	}
	if math.Abs(spans[len(spans)-1].End-1285) > 0.01 {
		t.Errorf("last span must end at duration, got %v", spans[len(spans)-1].End)
	}
	// contiguous, no gaps/overlaps
	for i := 1; i < len(spans); i++ {
		if math.Abs(spans[i].Start-spans[i-1].End) > 0.01 {
			t.Errorf("gap/overlap between span %d and %d", i-1, i)
		}
	}
}

func TestPlanSnapsToSilence(t *testing.T) {
	// one segment boundary near t=50; a silence sits at 48 -> cut should snap to 48
	spans := Plan(100, 2, []float64{48})
	if len(spans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(spans))
	}
	if math.Abs(spans[0].End-48) > 0.01 {
		t.Errorf("cut should snap to silence 48, got %v", spans[0].End)
	}
}

func TestPlanNoSilenceFallsBackToEven(t *testing.T) {
	spans := Plan(100, 2, nil)
	if len(spans) != 1 { // no silences -> single span (can't safely cut)
		t.Errorf("no silences should yield 1 span, got %d", len(spans))
	}
}

func TestPlanSingleSegment(t *testing.T) {
	spans := Plan(100, 1, []float64{50})
	if len(spans) != 1 || spans[0] != (Span{0, 100}) {
		t.Errorf("nSegments=1 should be one full span, got %v", spans)
	}
}
