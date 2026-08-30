package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
)

var t0 = time.Date(2026, 8, 30, 11, 0, 0, 0, time.UTC)

func ev(i int, camera string) policy.Event {
	return policy.Event{ID: fmt.Sprintf("e%03d", i), Camera: camera, Label: "car", Kind: policy.KindDetection,
		StartedAt: t0.Add(time.Duration(i) * time.Minute), SourceID: fmt.Sprintf("src%d", i)}
}

func fill(s *Store, n int) {
	for i := 0; i < n; i++ {
		cam := "a"
		if i%3 == 0 {
			cam = "b"
		}
		s.Apply(t0.Add(time.Duration(i)*time.Minute), policy.Change{Op: policy.OpStarted, Event: ev(i, cam)})
	}
}

func TestListPagesNewestFirstToTheEnd(t *testing.T) {
	s := New(7 * 24 * time.Hour)
	fill(s, 25)
	var got []string
	token := ""
	pages := 0
	for {
		page, next, err := s.List(nil, 10, token)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			if next != "" {
				t.Error("empty page must carry no token")
			}
			break
		}
		pages++
		for _, e := range page {
			got = append(got, e.ID)
		}
		token = next
	}
	if pages != 3 || len(got) != 25 || got[0] != "e024" || got[24] != "e000" {
		t.Fatalf("pages=%d got=%v", pages, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("not newest first at %d: %v", i, got)
		}
	}
}

func TestListFiltersCamerasAndRejectsBadToken(t *testing.T) {
	s := New(time.Hour)
	fill(s, 9)
	page, _, err := s.List([]string{"b"}, 100, "")
	if err != nil || len(page) != 3 {
		t.Fatalf("camera filter: %d events, %v", len(page), err)
	}
	for _, e := range page {
		if e.Camera != "b" {
			t.Errorf("wrong camera %+v", e)
		}
	}
	if _, _, err := s.List(nil, 10, "not-a-token"); !errors.Is(err, ErrBadToken) {
		t.Errorf("bad token -> %v", err)
	}
}

func TestApplyUpsertsAndReorders(t *testing.T) {
	s := New(time.Hour)
	fill(s, 3)
	e := ev(1, "a")
	e.EndedAt = e.StartedAt.Add(time.Minute)
	s.Apply(t0.Add(time.Hour), policy.Change{Op: policy.OpEnded, Event: e})
	got, ok := s.Get("e001")
	if !ok || got.Running() {
		t.Fatalf("upsert lost: %+v %v", got, ok)
	}
	if st := s.Stats(); st.Events != 3 || st.Live != 2 || st.Applied != 4 {
		t.Errorf("stats %+v", st)
	}
	if cams := s.Cameras(); len(cams) != 2 || cams[0] != "a" || cams[1] != "b" {
		t.Errorf("cameras %v", cams)
	}
	s.SawCamera("zed")
	if cams := s.Cameras(); cams[2] != "zed" {
		t.Errorf("cameras %v", cams)
	}
}

func TestHistory(t *testing.T) {
	s := New(time.Hour)
	e := ev(1, "a")
	s.Apply(t0, policy.Change{Op: policy.OpStarted, Event: e})
	e.Zones = []string{"yard"}
	s.Apply(t0.Add(time.Second), policy.Change{Op: policy.OpUpdated, Event: e})
	e.EndedAt = t0.Add(time.Minute)
	s.Apply(t0.Add(time.Minute), policy.Change{Op: policy.OpEnded, Event: e})
	h := s.History("e001")
	if len(h) != 3 || h[0].Op != policy.OpStarted || h[1].Op != policy.OpUpdated || h[2].Op != policy.OpEnded ||
		len(h[0].Event.Zones) != 0 || len(h[1].Event.Zones) != 1 || h[2].Event.Running() {
		t.Fatalf("history = %+v", h)
	}
	if s.History("nope") != nil {
		t.Error("history of an unknown event")
	}
	// Bounded: the first revision survives, the middle is dropped.
	for i := 0; i < 200; i++ {
		e.Zones = append(e.Zones, "z")
		s.Apply(t0.Add(time.Hour), policy.Change{Op: policy.OpUpdated, Event: e})
	}
	h = s.History("e001")
	if len(h) != maxHistory || h[0].Op != policy.OpStarted || len(h[len(h)-1].Event.Zones) != 201 {
		t.Errorf("bounded history: %d revisions, first %v, last zones %d", len(h), h[0].Op, len(h[len(h)-1].Event.Zones))
	}
}

func TestPrune(t *testing.T) {
	s := New(time.Hour)
	fill(s, 120) // two hours of one-a-minute
	n := s.Prune(t0.Add(120 * time.Minute))
	if n != 60 {
		t.Fatalf("pruned %d, want 60", n)
	}
	if page, _, _ := s.List(nil, 1000, ""); len(page) != 60 || page[len(page)-1].ID != "e060" {
		t.Errorf("after prune: %d events, oldest %s", len(page), page[len(page)-1].ID)
	}
	if _, ok := s.Get("e000"); ok {
		t.Error("pruned event still there")
	}
}

func TestWatchReplaysThenFollows(t *testing.T) {
	s := New(time.Hour)
	fill(s, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Watch(ctx, t0.Add(3*time.Minute), []string{"a"})
	// Replay: e003 (a), e004 (a); e000/e003 are b -> e003 is b, so just e004.
	first := <-ch
	if first.Event.ID != "e004" || first.Op != policy.OpStarted {
		t.Fatalf("replay = %+v", first)
	}
	s.Apply(t0.Add(time.Hour), policy.Change{Op: policy.OpStarted, Event: ev(10, "b")}) // filtered out
	s.Apply(t0.Add(time.Hour), policy.Change{Op: policy.OpStarted, Event: ev(11, "a")})
	select {
	case c := <-ch:
		if c.Event.ID != "e011" {
			t.Fatalf("live = %+v", c)
		}
	case <-time.After(time.Second):
		t.Fatal("no live change")
	}
	cancel()
	if _, open := <-ch; open {
		// drain until closed
		for range ch {
		}
	}
	if st := s.Stats(); st.Watchers != 0 {
		t.Errorf("watcher not dropped: %+v", st)
	}
}

func TestWatchCutsOffALaggard(t *testing.T) {
	s := New(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Watch(ctx, time.Time{}, nil)
	for i := 0; i < watchBuffer+10; i++ {
		s.Apply(t0, policy.Change{Op: policy.OpStarted, Event: ev(i, "a")})
	}
	n := 0
	for range ch { // must close, not block
		n++
	}
	if n != watchBuffer {
		t.Errorf("delivered %d before cutoff, want %d", n, watchBuffer)
	}
	cancel()
	time.Sleep(10 * time.Millisecond) // the drop goroutine must not double-close
}
