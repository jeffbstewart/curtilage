package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
	"github.com/jeffbstewart/curtilage/internal/mediacache"
	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// warmServer is a Server with a cache and a Frigate stub that answers
// every cam-a clip cut.
func warmServer(t *testing.T) *Server {
	t.Helper()
	fr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cam-a/start/") && strings.HasSuffix(r.URL.Path, "/clip.mp4") {
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("mp4" + r.URL.Path))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fr.Close)
	fc, err := frigate.NewClient(fr.URL)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := mediacache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Version: "test", Store: store.New(time.Hour), Frigate: fc, Cache: mc}
}

// waitFor polls until the key is cached or the deadline passes.
func waitFor(t *testing.T, mc *mediacache.Cache, key mediacache.Key, what string) {
	t.Helper()
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(10 * time.Millisecond) {
		if _, ok := mc.Get(key); ok {
			return
		}
	}
	t.Fatalf("%s never cached", what)
}

func TestWarmLifecycle(t *testing.T) {
	oldL, oldG := warmLadder, warmGrace
	warmLadder = []time.Duration{40 * time.Millisecond}
	warmGrace = 5 * time.Millisecond
	t.Cleanup(func() { warmLadder, warmGrace = oldL, oldG })

	s := warmServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Warm(ctx, s.Store)
	time.Sleep(100 * time.Millisecond) // let the warmer subscribe

	// A running event: its first checkpoint is cut and cached.
	started := time.Now().Add(-7 * time.Second)
	e := policy.Event{ID: "live1", Camera: "cam-a", Label: "car", Kind: policy.KindArrival, StartedAt: started}
	s.Store.Apply(time.Now(), policy.Change{Op: policy.OpStarted, Event: e})
	cp := clipKey("cam-a", started.Add(-clipMargin), started.Add(40*time.Millisecond))
	waitFor(t, s.Cache, cp, "checkpoint")

	// The event ends (settling time already past): the final cut is
	// cached and the checkpoint evicted.
	e.EndedAt = started.Add(time.Second)
	s.Store.Apply(time.Now(), policy.Change{Op: policy.OpEnded, Event: e})
	final := clipKey("cam-a", started.Add(-clipMargin), e.EndedAt.Add(clipMargin))
	waitFor(t, s.Cache, final, "final cut")
	for start := time.Now(); time.Since(start) < 3*time.Second; time.Sleep(10 * time.Millisecond) {
		if _, ok := s.Cache.Get(cp); !ok {
			break
		}
	}
	if _, ok := s.Cache.Get(cp); ok {
		t.Fatal("checkpoint survived the seal")
	}

	// An instantaneous event (an occupancy arrival) is warmed at once.
	at := time.Now().Add(-8 * time.Second)
	inst := policy.Event{ID: "pkg1", Camera: "cam-a", Label: "package", Kind: policy.KindArrival,
		StartedAt: at, EndedAt: at, Clip: policy.ClipFinal}
	s.Store.Apply(time.Now(), policy.Change{Op: policy.OpStarted, Event: inst})
	waitFor(t, s.Cache, clipKey("cam-a", at.Add(-clipMargin), at.Add(clipMargin)), "instant event's cut")
}
