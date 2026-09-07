package house

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/mediacache"
	"github.com/jeffbstewart/curtilage/internal/policy"
)

var edlDims = map[string][2]int{"porch-down": {640, 360}, "driveway-down": {640, 360}}

// samplesEvery adds a box report for cam every 2s over [from, to).
func samplesEvery(e *policy.Event, cam string, t0 time.Time, from, to time.Duration, box [4]float32) {
	for d := from; d < to; d += 2 * time.Second {
		e.Boxes = append(e.Boxes, policy.BoxSample{Camera: cam, At: t0.Add(d), Box: box})
	}
}

// The event page embeds the scored cut for an ended activity with box
// evidence, using preset detect dims (no Frigate call).
func TestEventPageCarriesEDL(t *testing.T) {
	h := handler(t)
	h.API.SetDetectDims(edlDims)
	now := h.Now()
	e := policy.Event{ID: "chase", Kind: policy.KindActivity, Camera: "porch-down",
		Cameras: []string{"porch-down", "driveway-down"}, Label: "person",
		Objects: map[string]int{"person": 1}, Path: []string{"porch"}, Zones: []string{"porch"},
		StartedAt: now.Add(-10 * time.Minute), EndedAt: now.Add(-9 * time.Minute),
		SourceID: "src-chase", SourceIDs: []string{"src-chase"}}
	samplesEvery(&e, "porch-down", e.StartedAt, 0, 20*time.Second, [4]float32{0, 0, 200, 200})
	samplesEvery(&e, "driveway-down", e.StartedAt, 20*time.Second, 40*time.Second, [4]float32{0, 0, 300, 300})
	h.Store.Apply(now, policy.Change{Op: policy.OpEnded, Event: e})
	code, body := get(t, h, "192.168.1.50:1", "", "event/chase")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if !strings.Contains(body, `"c":"porch-down"`) || !strings.Contains(body, `"c":"driveway-down"`) {
		t.Errorf("page lacks the scored cut: %.300s", body[strings.Index(body, "const edl"):])
	}
	// The walk activity has no box evidence: its page keeps an empty
	// cut and the span heuristic drives.
	if _, body := get(t, h, "192.168.1.50:1", "", "event/walk"); !strings.Contains(body, "const edl = [];") {
		t.Error("boxless event should carry an empty cut")
	}
}

// The stitched render: 404 (and a queued request) before it exists,
// served with ranges after; the event page promotes it to the primary
// tab and holds the grid's decoders until asked.
func TestStitchRouteAndPage(t *testing.T) {
	h := handler(t)
	mc, err := mediacache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.API.Cache = mc
	now := h.Now()
	e := policy.Event{ID: "chase", Kind: policy.KindActivity, Camera: "porch-down",
		Cameras: []string{"porch-down", "driveway-down"}, Label: "person",
		Objects: map[string]int{"person": 1}, Path: []string{"porch"}, Zones: []string{"porch"},
		StartedAt: now.Add(-10 * time.Minute), EndedAt: now.Add(-9 * time.Minute),
		SourceID: "src-chase", SourceIDs: []string{"src-chase"}}
	h.Store.Apply(now, policy.Change{Op: policy.OpEnded, Event: e})

	if code, _ := get(t, h, "192.168.1.50:1", "", "stitch/chase"); code != 404 {
		t.Fatalf("unstitched -> %d", code)
	}
	if _, body := get(t, h, "192.168.1.50:1", "", "event/chase"); strings.Contains(body, `id="tabstitch"`) {
		t.Error("page offers a stitched tab before the render exists")
	}

	f := filepath.Join(t.TempDir(), "render.mp4")
	if err := os.WriteFile(f, []byte("stitched-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.API.PutStitch(e, "lo", f); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, h, "192.168.1.50:1", "", "stitch/chase")
	if code != 200 || body != "stitched-bytes" {
		t.Fatalf("stitched -> %d %q", code, body)
	}
	_, page := get(t, h, "192.168.1.50:1", "", "event/chase")
	for _, want := range []string{`id="tabstitch"`, `/house/stitch/chase?v=lo`, `preload="none"`} {
		if !strings.Contains(page, want) {
			t.Errorf("event page lacks %q", want)
		}
	}
	if strings.Contains(page, `id="stitchhd"`) {
		t.Error("720p offered before the hi render exists")
	}
	// A running event's stitch path is never served.
	live := policy.Event{ID: "liv", Camera: "porch-down", Label: "person", Kind: policy.KindActivity,
		Objects: map[string]int{"person": 1}, Path: []string{"porch"}, Zones: []string{"porch"},
		StartedAt: now.Add(-time.Minute), SourceID: "src-liv", SourceIDs: []string{"src-liv"}}
	h.Store.Apply(now, policy.Change{Op: policy.OpStarted, Event: live})
	if code, _ := get(t, h, "192.168.1.50:1", "", "stitch/liv"); code != 404 {
		t.Fatalf("live stitch -> %d", code)
	}
}
