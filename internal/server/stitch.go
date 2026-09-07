// The stitched follow view: the scored cut (internal/edl) burned
// into ONE mp4 per event, because eight synchronized video decoders
// is more than a browser owes anybody.  The lo variant (640x360)
// renders eagerly when an event ends; hi (1280x720) when the worker
// is otherwise idle.  CPU only, ended events only -- household
// decisions, like holding the last camera through gaps.
package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeffbstewart/curtilage/internal/edl"
	"github.com/jeffbstewart/curtilage/internal/mediacache"
	"github.com/jeffbstewart/curtilage/internal/policy"
)

// ffmpegPath is the container's static binary (Dockerfile), or PATH's
// ffmpeg on a dev machine; "" disables stitching.
var ffmpegPath = func() string {
	if _, err := os.Stat("/ffmpeg"); err == nil {
		return "/ffmpeg"
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}()

// stitchKey is the cache identity of one event's stitched render.
func stitchKey(e policy.Event, variant string) mediacache.Key {
	start, end, _ := clipCut(e, time.Time{}, false) // ended: bounds fixed
	return mediacache.Key{Media: "stitch", Ref: e.ID, Start: start.Unix(), End: end.Unix(), Variant: variant}
}

// StitchCached reports whether the event's stitched render exists.
func (s *Server) StitchCached(e policy.Event, variant string) bool {
	return s.Cache != nil && !e.EndedAt.IsZero() && s.Cache.Contains(stitchKey(e, variant))
}

// StitchPath is the stitched render's file, for the house page to
// serve.
func (s *Server) StitchPath(e policy.Event, variant string) (string, bool) {
	if s.Cache == nil || e.EndedAt.IsZero() {
		return "", false
	}
	return s.Cache.Get(stitchKey(e, variant))
}

// PutStitch adopts a finished render into the cache.
func (s *Server) PutStitch(e policy.Event, variant, src string) error {
	return s.Cache.Put(stitchKey(e, variant), src)
}

// RequestStitch queues one render; a full queue or a not-yet-started
// worker drops the request (the next view asks again).
func (s *Server) RequestStitch(e policy.Event, variant string) {
	q := s.stitchLo
	if variant == "hi" {
		q = s.stitchHi
	}
	if q == nil {
		return
	}
	select {
	case q <- e:
	default:
	}
}

// stitchWorker renders one job at a time, lo before hi -- "hi when
// idle" is nothing more than this priority.  A finished lo queues its
// own hi.
func (s *Server) stitchWorker(ctx context.Context) {
	for {
		var e policy.Event
		variant := "lo"
		select {
		case <-ctx.Done():
			return
		case e = <-s.stitchLo:
		default:
			select {
			case <-ctx.Done():
				return
			case e = <-s.stitchLo:
			case e = <-s.stitchHi:
				variant = "hi"
			}
		}
		if err := s.stitch(ctx, e, variant); err != nil {
			log.Printf("stitch: %s %s: %v", e.ID, variant, err)
			continue
		}
		if variant == "lo" {
			s.RequestStitch(e, "hi")
		}
	}
}

// CanStitch reports whether renders can happen at all (a cache to
// keep them in; the worker also needs ffmpeg, which every shipped
// image has).
func (s *Server) CanStitch() bool { return s.Cache != nil }

// stitch renders one event's cut into the cache.  No usable cut (no
// box evidence, unscorable cameras) is a logged no-op, not an error
// -- the silence cost an investigation once.
func (s *Server) stitch(ctx context.Context, e policy.Event, variant string) error {
	if ffmpegPath == "" || s.Cache == nil || e.EndedAt.IsZero() {
		return nil
	}
	key := stitchKey(e, variant)
	if s.Cache.Contains(key) {
		return nil
	}
	start, end, _ := clipCut(e, time.Time{}, false)
	cut := edl.Build(e, start, end, s.DetectDims(eventCameras(e)))
	if len(cut) == 0 {
		log.Printf("stitch: %s: no usable cut (%d box samples over %d cameras)", e.ID, len(e.Boxes), len(eventCameras(e)))
		return nil
	}
	// Every camera's full final cut, from the cache (the warmer has
	// usually filled these already; a miss fetches).
	paths := map[string]string{}
	for _, seg := range cut {
		if _, ok := paths[seg.C]; ok {
			continue
		}
		p, err := s.Cache.Fill(ctx, clipKey(seg.C, start, end), s.clipFetch(seg.C, start, end))
		if err != nil {
			return fmt.Errorf("cut %s: %w", seg.C, err)
		}
		paths[seg.C] = p
	}
	w, h, crf := 640, 360, "28"
	if variant == "hi" {
		w, h, crf = 1280, 720, "23"
	}
	// One shot at a time: the inputs are full-resolution recordings
	// (some 4K), and a single decoder is already a few hundred MB --
	// opening one PER SEGMENT at once OOM-killed the 512Mi pod
	// (2026-09-07 18:25).  Each shot renders alone to an mpegts
	// intermediate at target size; the final pass stream-copies them
	// together (concat demuxer, near-zero memory).
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	began := time.Now()
	work, err := os.MkdirTemp(filepath.Dir(paths[cut[0].C]), "stitch-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=15", w, h, w, h)
	var list strings.Builder
	for i, seg := range cut {
		shot := filepath.Join(work, fmt.Sprintf("shot%03d.ts", i))
		if err := runFFmpeg(cctx,
			"-threads", "2",
			"-ss", fmt.Sprintf("%.3f", seg.S), "-t", fmt.Sprintf("%.3f", seg.E-seg.S), "-i", paths[seg.C],
			"-vf", vf,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", crf, "-threads", "2",
			"-an", "-f", "mpegts", shot); err != nil {
			return fmt.Errorf("shot %d (%s): %w", i, seg.C, err)
		}
		fmt.Fprintf(&list, "file '%s'\n", filepath.ToSlash(shot))
	}
	listPath := filepath.Join(work, "shots.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0o644); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(paths[cut[0].C]), "stitch-*.part")
	if err != nil {
		return err
	}
	out.Close()
	defer os.Remove(out.Name()) // no-op once Put has renamed it away
	if err := runFFmpeg(cctx,
		"-f", "concat", "-safe", "0", "-i", listPath,
		"-c", "copy", "-movflags", "+faststart", "-f", "mp4", out.Name()); err != nil {
		return fmt.Errorf("concat: %w", err)
	}
	if err := s.PutStitch(e, variant, out.Name()); err != nil {
		return err
	}
	log.Printf("stitch: %s %s: %d shots, %s", e.ID, variant, len(cut), time.Since(began).Round(time.Second))
	return nil
}

// runFFmpeg runs one invocation with the quiet common flags.
func runFFmpeg(ctx context.Context, args ...string) error {
	full := append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)
	cmd := exec.CommandContext(ctx, ffmpegPath, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
