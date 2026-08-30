package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/record"
)

// Feed runs one recorded message through the engine into the store:
// the single path for live traffic, startup rebuild, and replay.
func Feed(s *Store, eng policy.Engine, r *record.Record) {
	at := r.GetReceivedAt().AsTime()
	if top := frigate.ParseTopic(r.GetTopic()); top.Kind == frigate.KindCount {
		s.SawCamera(top.Camera)
	}
	for _, c := range eng.Observe(at, r.GetTopic(), r.GetPayload()) {
		s.Apply(at, c)
	}
}

// Rebuilt is what Rebuild did.
type Rebuilt struct {
	Files, Records, Deleted int
	// Truncated files (the one being written, or a torn one) were
	// read as far as they go.
	Truncated int
}

// Rebuild replays every recording in dir into the store through eng,
// oldest first, first deleting any recording that started before
// now-retention.  A file that fails to open or parse is skipped and
// logged into the error list, not fatal: one bad day must not stop
// the server.
func Rebuild(ctx context.Context, s *Store, eng policy.Engine, dir string, now time.Time) (Rebuilt, []error) {
	var res Rebuilt
	var errs []error
	names, err := recordings(dir)
	if err != nil {
		return res, []error{err}
	}
	cutoff := now.Add(-s.retention)
	for _, n := range names {
		if n.start.Before(cutoff) {
			if err := os.Remove(n.path); err != nil {
				errs = append(errs, fmt.Errorf("prune %s: %w", n.path, err))
			} else {
				res.Deleted++
			}
			continue
		}
		out := make(chan *record.Record, 256)
		var readErr error
		go func() {
			readErr = record.Read(ctx, n.path, out)
			close(out)
		}()
		for r := range out {
			Feed(s, eng, r)
			res.Records++
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, record.ErrTruncated), errors.Is(readErr, record.ErrCorrupt):
			res.Truncated++
		default:
			errs = append(errs, fmt.Errorf("%s: %w", n.path, readErr))
			continue
		}
		res.Files++
	}
	return res, errs
}

// PruneFiles deletes recordings that started before now-retention.
func PruneFiles(dir string, retention time.Duration, now time.Time) (int, []error) {
	names, err := recordings(dir)
	if err != nil {
		return 0, []error{err}
	}
	cutoff := now.Add(-retention)
	deleted := 0
	var errs []error
	for _, n := range names {
		if !n.start.Before(cutoff) {
			continue
		}
		if err := os.Remove(n.path); err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", n.path, err))
			continue
		}
		deleted++
	}
	return deleted, errs
}

type recording struct {
	path  string
	start time.Time
}

// recordings lists dir's recordings oldest first; a missing dir is
// simply empty.
func recordings(dir string) ([]recording, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []recording
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		start, ok := record.ParseFileName(e.Name())
		if !ok {
			continue
		}
		out = append(out, recording{path: filepath.Join(dir, e.Name()), start: start})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path }) // name order == time order
	return out, nil
}
