package mediacache

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func body(s string) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(s)), nil
	}
}

func TestFillGetEvict(t *testing.T) {
	c, err := New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	k := Key{Media: "clip", Ref: "garage", Start: 100, End: 160}
	if _, ok := c.Get(k); ok {
		t.Fatal("hit before fill")
	}
	p, err := c.Fill(context.Background(), k, body("mp4-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "mp4-bytes" {
		t.Fatalf("cached file: %q, %v", b, err)
	}
	if p2, ok := c.Get(k); !ok || p2 != p {
		t.Fatalf("Get: %q, %v", p2, ok)
	}
	// A second fill is a hit, not a fetch.
	p3, err := c.Fill(context.Background(), k, func(context.Context) (io.ReadCloser, error) {
		t.Fatal("refetched a cached cut")
		return nil, nil
	})
	if err != nil || p3 != p {
		t.Fatalf("refill: %q, %v", p3, err)
	}
	c.Evict(k)
	if _, ok := c.Get(k); ok {
		t.Fatal("hit after evict")
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file survived evict: %v", err)
	}
}

func TestFillSingleFlight(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	k := Key{Media: "clip", Ref: "garage", Start: 1, End: 2}
	var fetches atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (io.ReadCloser, error) {
		fetches.Add(1)
		<-release
		return io.NopCloser(strings.NewReader("x")), nil
	}
	var wg sync.WaitGroup
	paths := make([]string, 4)
	for i := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths[i], _ = c.Fill(context.Background(), k, fetch)
		}()
	}
	// One goroutine reaches the fetch; release it and all four share it.
	for fetches.Load() == 0 {
	}
	close(release)
	wg.Wait()
	if n := fetches.Load(); n != 1 {
		t.Fatalf("%d fetches for one key", n)
	}
	for _, p := range paths[1:] {
		if p != paths[0] {
			t.Fatalf("paths differ: %q vs %q", p, paths[0])
		}
	}
}

// Fills of distinct keys are bounded at maxFills: the dirty-page
// budget that OOM-killed the 128Mi pod is capped, not per-key.
func TestFillConcurrencyBounded(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	var running, peak atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) (io.ReadCloser, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return io.NopCloser(strings.NewReader("x")), nil
	}
	var wg sync.WaitGroup
	for i := range maxFills + 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Fill(context.Background(), Key{Media: "clip", Ref: "cam", Start: int64(i), End: int64(i) + 1}, fetch)
		}()
	}
	for running.Load() < maxFills {
	}
	close(release)
	wg.Wait()
	if p := peak.Load(); p > maxFills {
		t.Fatalf("%d concurrent fills, cap is %d", p, maxFills)
	}
}

func TestBudgetEvictsLRU(t *testing.T) {
	c, _ := New(t.TempDir(), 25) // room for two 10-byte cuts
	k1 := Key{Media: "clip", Ref: "a", Start: 1, End: 2}
	k2 := Key{Media: "clip", Ref: "b", Start: 1, End: 2}
	k3 := Key{Media: "clip", Ref: "c", Start: 1, End: 2}
	ten := body("0123456789")
	c.Fill(context.Background(), k1, ten)
	c.Fill(context.Background(), k2, ten)
	c.Get(k1) // k1 is now fresher than k2
	c.Fill(context.Background(), k3, ten)
	if _, ok := c.Get(k2); ok {
		t.Fatal("LRU entry survived the budget")
	}
	if _, ok := c.Get(k1); !ok {
		t.Fatal("freshened entry was evicted")
	}
	if st := c.Stats(); st.Entries != 2 || st.Bytes != 20 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestFetchErrorIsNotCached(t *testing.T) {
	c, _ := New(t.TempDir(), 1<<20)
	k := Key{Media: "snapshot", Ref: "ev1"}
	boom := errors.New("boom")
	if _, err := c.Fill(context.Background(), k, func(context.Context) (io.ReadCloser, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("fill error: %v", err)
	}
	if _, err := c.Fill(context.Background(), k, body("jpeg")); err != nil {
		t.Fatalf("fill after error: %v", err)
	}
}

func TestNewEmptiesDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/stale", []byte("x"), 0o644)
	os.WriteFile(dir+"/stale.part", []byte("x"), 0o644)
	if _, err := New(dir, 1<<20); err != nil {
		t.Fatal(err)
	}
	names, _ := os.ReadDir(dir)
	if len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
	}
}
