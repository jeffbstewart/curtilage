package edl

import (
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
)

var edlDims = map[string][2]int{"porch-down": {640, 360}, "driveway-down": {640, 360}}

// samplesEvery adds a box report for cam every 2s over [from, to).
func samplesEvery(e *policy.Event, cam string, t0 time.Time, from, to time.Duration, box [4]float32) {
	for d := from; d < to; d += 2 * time.Second {
		e.Boxes = append(e.Boxes, policy.BoxSample{Camera: cam, At: t0.Add(d), Box: box})
	}
}

func TestBuildEDLHandsOffToTheBiggerView(t *testing.T) {
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	e := policy.Event{StartedAt: t0, EndedAt: t0.Add(time.Minute)}
	// porch sees them small the whole first half; the driveway sees
	// them big from 15s on (and stops reporting at 30s -- the gap is
	// held, not cut away from).
	samplesEvery(&e, "porch-down", t0, 2*time.Second, 20*time.Second, [4]float32{0, 0, 200, 200})
	samplesEvery(&e, "driveway-down", t0, 15*time.Second, 30*time.Second, [4]float32{0, 0, 300, 300})
	clipStart, clipEnd := t0.Add(-5*time.Second), t0.Add(65*time.Second)
	segs := Build(e, clipStart, clipEnd, edlDims)
	if len(segs) != 2 || segs[0].C != "porch-down" || segs[1].C != "driveway-down" {
		t.Fatalf("segments: %+v", segs)
	}
	// The opening shot reaches back to the top of the clip; the last
	// segment holds to the end through the silence.
	if segs[0].S != 0 || segs[1].E != 70 {
		t.Errorf("bounds: %+v", segs)
	}
	// The switch lands near the driveway's first big report (+5s clip
	// margin), within a couple of scoring steps.
	if sw := segs[1].S; sw < 19 || sw > 22 {
		t.Errorf("switch at %.1fs, want ~20s", sw)
	}
}

func TestBuildEDLIgnoresBrieflyBiggerAndUnknownCameras(t *testing.T) {
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	e := policy.Event{StartedAt: t0, EndedAt: t0.Add(30 * time.Second)}
	samplesEvery(&e, "porch-down", t0, 0, 30*time.Second, [4]float32{0, 0, 250, 250})
	// One driveway report, bigger but not by the advantage margin: no
	// cut for a wobble.
	e.Boxes = append(e.Boxes, policy.BoxSample{Camera: "driveway-down", At: t0.Add(10 * time.Second), Box: [4]float32{0, 0, 260, 260}})
	// A camera with no known dims can never win.
	samplesEvery(&e, "mystery-cam", t0, 0, 30*time.Second, [4]float32{0, 0, 600, 350})
	segs := Build(e, t0, t0.Add(30*time.Second), edlDims)
	if len(segs) != 1 || segs[0].C != "porch-down" {
		t.Fatalf("segments: %+v", segs)
	}
	// No scorable evidence at all: nil, and the page falls back.
	only := policy.Event{StartedAt: t0}
	samplesEvery(&only, "mystery-cam", t0, 0, 10*time.Second, [4]float32{0, 0, 100, 100})
	if segs := Build(only, t0, t0.Add(10*time.Second), edlDims); segs != nil {
		t.Fatalf("unknown-only segments: %+v", segs)
	}
}

// The event page's use of the cut is tested in internal/house.
