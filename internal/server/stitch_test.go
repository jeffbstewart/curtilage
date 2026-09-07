package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
)

func TestStitchPlumbing(t *testing.T) {
	s := warmServer(t)
	e := policy.Event{ID: "ev1", Camera: "cam-a", StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now().Add(-30 * time.Second)}
	// Queues not started (no Warm): a request is a quiet no-op.
	s.RequestStitch(e, "lo")
	if s.StitchCached(e, "lo") {
		t.Fatal("cached before any render")
	}
	if _, ok := s.StitchPath(e, "lo"); ok {
		t.Fatal("path before any render")
	}
	f := filepath.Join(t.TempDir(), "render.mp4")
	if err := os.WriteFile(f, []byte("stitched-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutStitch(e, "lo", f); err != nil {
		t.Fatal(err)
	}
	if !s.StitchCached(e, "lo") || s.StitchCached(e, "hi") {
		t.Fatal("variant confusion")
	}
	p, ok := s.StitchPath(e, "lo")
	if !ok {
		t.Fatal("no path after put")
	}
	if b, _ := os.ReadFile(p); string(b) != "stitched-bytes" {
		t.Fatalf("render bytes: %q", b)
	}
	// A running event never stitches or serves.
	live := policy.Event{ID: "ev2", Camera: "cam-a", StartedAt: time.Now()}
	if s.StitchCached(live, "lo") {
		t.Fatal("live event cached")
	}
}

// The real render, when ffmpeg is on this machine (the container has
// /ffmpeg; CI's builder does not, and skips).
func TestStitchRendersWithFFmpeg(t *testing.T) {
	if ffmpegPath == "" {
		t.Skip("no ffmpeg on this machine")
	}
	s := warmServer(t)
	s.SetDetectDims(map[string][2]int{"cam-a": {640, 360}, "cam-b": {640, 360}})
	t0 := time.Now().Add(-2 * time.Minute).Truncate(time.Second)
	e := policy.Event{ID: "chase", Kind: policy.KindActivity, Camera: "cam-a",
		Cameras: []string{"cam-a", "cam-b"}, Label: "person",
		StartedAt: t0, EndedAt: t0.Add(10 * time.Second), Clip: policy.ClipFinal}
	for d := 0 * time.Second; d < 5*time.Second; d += 2 * time.Second {
		e.Boxes = append(e.Boxes, policy.BoxSample{Camera: "cam-a", At: t0.Add(d), Box: [4]float32{0, 0, 300, 300}})
	}
	for d := 5 * time.Second; d < 10*time.Second; d += 2 * time.Second {
		e.Boxes = append(e.Boxes, policy.BoxSample{Camera: "cam-b", At: t0.Add(d), Box: [4]float32{0, 0, 400, 300}})
	}
	// The two cameras' cuts: 20s of synthetic video each (the cut
	// window is [start-5s, end+5s]).
	start, end, _ := clipCut(e, time.Time{}, false)
	gen := t.TempDir()
	for i, cam := range []string{"cam-a", "cam-b"} {
		f := filepath.Join(gen, cam+".mp4")
		src := []string{"testsrc=size=320x240:rate=10:duration=20", "smptebars=size=320x240:rate=10:duration=20"}[i]
		out, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", src, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", f).CombinedOutput()
		if err != nil {
			t.Fatalf("generate %s: %v: %s", cam, err, out)
		}
		if err := s.Cache.Put(clipKey(cam, start, end), f); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.stitch(context.Background(), e, "lo"); err != nil {
		t.Fatal(err)
	}
	p, ok := s.StitchPath(e, "lo")
	if !ok {
		t.Fatal("no render cached")
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() < 1000 {
		t.Fatalf("render too small: %v %v", fi, err)
	}
	// Idempotent: a second render is a no-op, not a re-encode.
	before, _ := os.Stat(p)
	if err := s.stitch(context.Background(), e, "lo"); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(p); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("re-rendered a cached stitch")
	}
}
