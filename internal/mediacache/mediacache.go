// Package mediacache is a disk-backed LRU of immutable media cuts.
// A cut -- one camera's clip between fixed bounds, or an ended
// event's snapshot -- never changes, so a cached copy is good until
// evicted, and every consumer (the event page's panes, the warmer, a
// future mobile transcode) shares one Frigate fetch per cut.
//
// Fill is single-flight per key: concurrent requests for the same cut
// wait for one fetch.  Entries are whole files; a fill writes
// <name>.part and renames on success, so a complete file is always a
// complete cut.  New empties its directory: the cache does not
// outlive the process's idea of what is in it.
package mediacache

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Key names one immutable cut.  Media is "clip" or "snapshot"; for a
// clip, Ref is the camera and Start/End the cut bounds in unix
// seconds; for a snapshot, Ref is the source event id.  Variant is
// "" for the original bytes; a mobile transcode is the same cut under
// variant "mobile".
type Key struct {
	Media      string
	Ref        string
	Start, End int64
	Variant    string
}

func (k Key) filename() string {
	v := k.Variant
	if v == "" {
		v = "orig"
	}
	return fmt.Sprintf("%s-%s-%d-%d-%s", k.Media, k.Ref, k.Start, k.End, v)
}

type entry struct {
	path string
	size int64
	seq  uint64 // last use, for LRU
}

type flight struct {
	done chan struct{}
	path string
	err  error
}

// Cache is the store.  Safe for concurrent use.
type Cache struct {
	dir    string
	budget int64

	mu       sync.Mutex
	entries  map[Key]*entry
	inflight map[Key]*flight
	size     int64
	seq      uint64
}

// New empties dir (creating it if needed) and returns a cache that
// will hold at most budget bytes of completed files.
func New(dir string, budget int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		os.Remove(filepath.Join(dir, n.Name()))
	}
	return &Cache{dir: dir, budget: budget, entries: map[Key]*entry{}, inflight: map[Key]*flight{}}, nil
}

// Get returns the path of a completed cut and marks it used.
func (c *Cache) Get(key Key) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.seq++
	e.seq = c.seq
	return e.path, true
}

// Contains reports whether the cut is cached, without touching its
// LRU standing: an inventory question (the house page's bolt), not a
// use.
func (c *Cache) Contains(key Key) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}

// Fill returns the cut's path, fetching and storing it if absent.
// Concurrent calls for one key share a single fetch; the losers wait.
// The fetch's reader is drained to disk and closed.
func (c *Cache) Fill(ctx context.Context, key Key, fetch func(context.Context) (io.ReadCloser, error)) (string, error) {
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		c.seq++
		e.seq = c.seq
		c.mu.Unlock()
		return e.path, nil
	}
	if f, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.path, f.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f := &flight{done: make(chan struct{})}
	c.inflight[key] = f
	c.mu.Unlock()

	f.path, f.err = c.fill(ctx, key, fetch)
	c.mu.Lock()
	delete(c.inflight, key)
	c.mu.Unlock()
	close(f.done)
	return f.path, f.err
}

func (c *Cache) fill(ctx context.Context, key Key, fetch func(context.Context) (io.ReadCloser, error)) (string, error) {
	body, err := fetch(ctx)
	if err != nil {
		return "", err
	}
	defer body.Close()
	path := filepath.Join(c.dir, key.filename())
	part := path + ".part"
	w, err := os.Create(part)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(w, body)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(part, path)
	}
	if err != nil {
		os.Remove(part)
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	c.entries[key] = &entry{path: path, size: n, seq: c.seq}
	c.size += n
	c.shed()
	return path, nil
}

// shed evicts least-recently-used entries until the budget holds.
// Callers hold mu.  Removing an open file is safe on every platform
// we run: readers keep their handle, the space returns on close.
func (c *Cache) shed() {
	for c.size > c.budget && len(c.entries) > 1 {
		var oldest Key
		var lru *entry
		for k, e := range c.entries {
			if lru == nil || e.seq < lru.seq {
				oldest, lru = k, e
			}
		}
		c.drop(oldest, lru)
	}
}

// Evict removes one cut (a superseded checkpoint) if present.
func (c *Cache) Evict(key Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		c.drop(key, e)
	}
}

func (c *Cache) drop(key Key, e *entry) {
	if err := os.Remove(e.path); err != nil {
		log.Printf("mediacache: evict %s: %v", key.filename(), err)
	}
	c.size -= e.size
	delete(c.entries, key)
}

// Stats is the cache's current shape, for /metrics.
type Stats struct {
	Entries int
	Bytes   int64
}

// Stats reports the cache's current shape.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{Entries: len(c.entries), Bytes: c.size}
}
