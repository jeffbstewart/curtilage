package edl

import (
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
)

// The scored edit decision list: which camera the follow view shows,
// when.  The first follow mode chose by sighting spans alone and was
// retired -- a span says a camera saw SOMETHING, not how well.  Box
// evidence says how well: the subject's detection-box area, as a
// fraction of that camera's frame, is how prominently the camera
// sees them.  The cut follows the biggest view, with hysteresis so a
// hand-off is a decision and not a flicker, and holds the last
// camera through gaps (the household's gap policy).

const (
	// edlStep is the scoring interval.
	edlStep = 500 * time.Millisecond
	// edlStale is how old a box report may be and still speak for its
	// camera; Frigate publishes on change, so fresh silence usually
	// means "unchanged", not "gone".
	edlStale = 5 * time.Second
	// edlMinShot is the least time between switches: no cut, however
	// justified, before the eye has settled.
	edlMinShot = 2500 * time.Millisecond
	// edlAdvantage is how much bigger a challenger's view must be to
	// take the cut from the incumbent.
	edlAdvantage = 1.3
)

type Segment struct {
	C string  `json:"c"`
	S float64 `json:"s"`
	E float64 `json:"e"`
}

// Build scores e's box evidence into segments of clip-relative
// seconds over [clipStart, clipEnd].  dims maps camera to detect
// resolution; a camera without dims cannot be scored and never wins.
// Returns nil when there is no usable evidence -- the page then falls
// back to the span heuristic.
func Build(e policy.Event, clipStart, clipEnd time.Time, dims map[string][2]int) []Segment {
	// Per-camera samples, in time order (they arrive in order).
	perCam := map[string][]policy.BoxSample{}
	for _, b := range e.Boxes {
		d, ok := dims[b.Camera]
		if !ok || d[0] <= 0 || d[1] <= 0 {
			continue
		}
		perCam[b.Camera] = append(perCam[b.Camera], b)
	}
	if len(perCam) == 0 {
		return nil
	}
	// score is the freshest sample's box area as a fraction of the
	// frame, or -1 when the camera has said nothing within edlStale.
	idx := map[string]int{}
	score := func(cam string, t time.Time) float64 {
		samples := perCam[cam]
		i := idx[cam]
		for i < len(samples) && !samples[i].At.After(t) {
			i++
		}
		idx[cam] = i
		if i == 0 {
			return -1
		}
		s := samples[i-1]
		if t.Sub(s.At) > edlStale {
			return -1
		}
		d := dims[cam]
		w := float64(s.Box[2] - s.Box[0])
		h := float64(s.Box[3] - s.Box[1])
		if w <= 0 || h <= 0 {
			return -1
		}
		return w * h / float64(d[0]*d[1])
	}

	var segs []Segment
	current := ""
	var since time.Time // when current took the cut
	emit := func(from, to time.Time) {
		if current == "" || !to.After(from) {
			return
		}
		s, en := from.Sub(clipStart).Seconds(), to.Sub(clipStart).Seconds()
		if n := len(segs); n > 0 && segs[n-1].C == current {
			segs[n-1].E = en
			return
		}
		segs = append(segs, Segment{C: current, S: s, E: en})
	}
	segStart := clipStart
	for t := clipStart; t.Before(clipEnd); t = t.Add(edlStep) {
		best, bestScore := "", -1.0
		for cam := range perCam {
			if sc := score(cam, t); sc > bestScore {
				best, bestScore = cam, sc
			}
		}
		switch {
		case bestScore < 0:
			// Nobody has fresh evidence: hold the last camera.
		case current == "":
			current, since, segStart = best, t, t
			if len(segs) == 0 {
				segStart = clipStart // the opening shot reaches back to the top
			}
		case best != current && t.Sub(since) >= edlMinShot && bestScore >= score(current, t)*edlAdvantage:
			emit(segStart, t)
			current, since, segStart = best, t, t
		}
	}
	emit(segStart, clipEnd)
	return segs
}
