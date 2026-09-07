// The warmer: media is fetched when an event HAPPENS, not when
// someone finally looks.  The newest few sent events are kept hot --
// a running event's clip is re-cut at each warmLadder boundary, an
// ended one's final cut and snapshot are fetched once -- so by the
// time a person opens the page the cuts are on disk and the panes
// spool at once (media.go serveCached asks for the same keys).
package server

import (
	"context"
	"log"
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
	"github.com/jeffbstewart/curtilage/internal/store"
)

// warmSet is how many of the newest sent events stay hot.  Older
// events warm lazily on first view (and are then cached like any
// other cut).
const warmSet = 4

// Warm follows the event stream and keeps the newest events' media
// cached.  It blocks until ctx ends; run it in a goroutine after the
// store is rebuilt.  No-op without a cache or Frigate.
func (s *Server) Warm(ctx context.Context, st *store.Store) {
	if s.Cache == nil || s.Frigate == nil {
		return
	}
	s.stitchLo, s.stitchHi = make(chan policy.Event, 16), make(chan policy.Event, 16)
	go s.stitchWorker(ctx)
	// The newest already-known events first: after a restart the page
	// a person is most likely to open is one of these.
	recent, _, err := st.List(nil, 20, "")
	if err == nil {
		warmed := 0
		for _, e := range recent { // newest first
			if warmed == warmSet {
				break
			}
			if !e.Sent() || e.Running() {
				continue
			}
			warmed++
			s.warmFinal(ctx, e)
		}
	}
	type live struct {
		cancel func()
		ended  chan struct{}
	}
	watching := map[string]*live{}
	order := []string{} // watching's ids, oldest first
	for c := range st.Watch(ctx, time.Now(), nil) {
		e := c.Event
		if !e.Sent() {
			continue
		}
		switch c.Op {
		case policy.OpStarted:
			if _, ok := watching[e.ID]; ok {
				break
			}
			if !e.Running() { // instantaneous (an occupancy event): hot at once
				go s.warmFinal(ctx, e)
				break
			}
			wctx, cancel := context.WithCancel(ctx)
			w := &live{cancel: cancel, ended: make(chan struct{})}
			watching[e.ID] = w
			order = append(order, e.ID)
			go func(id string) {
				s.warmLive(wctx, st, id, w.ended)
				cancel() // release the context once the seal is done
			}(e.ID)
			// The working set is the newest warmSet: an older live
			// event stops warming (its cuts stay until evicted).
			for len(order) > warmSet {
				old := order[0]
				order = order[1:]
				watching[old].cancel()
				delete(watching, old)
			}
		case policy.OpEnded:
			if w, ok := watching[e.ID]; ok {
				close(w.ended)
				delete(watching, e.ID)
				order = deleteID(order, e.ID)
			}
		}
	}
}

func deleteID(ids []string, id string) []string {
	for i, v := range ids {
		if v == id {
			return append(ids[:i:i], ids[i+1:]...)
		}
	}
	return ids
}

// warmLive re-cuts one running event's clips at each ladder boundary,
// evicting the boundary before, then seals it with the final cut when
// the event ends.  ended closes on the event's OpEnded; ctx cancels
// when the event leaves the working set.
func (s *Server) warmLive(ctx context.Context, st *store.Store, id string, ended <-chan struct{}) {
	e, ok := st.Get(id)
	if !ok {
		return
	}
	prev := map[string]time.Time{} // camera -> checkpoint end already cached
	for _, b := range warmLadder {
		wake := e.StartedAt.Add(b + warmGrace)
		t := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-ended:
			t.Stop()
			if e, ok = st.Get(id); ok {
				s.warmFinal(ctx, e)
			}
			s.evictCheckpoints(e, prev)
			return
		case <-t.C:
		}
		if e, ok = st.Get(id); !ok {
			return
		}
		if !e.Running() { // ended between messages; the seal follows on OpEnded
			return
		}
		end := e.StartedAt.Add(b)
		start := e.StartedAt.Add(-clipMargin)
		for _, cam := range eventCameras(e) {
			if _, err := s.Cache.Fill(ctx, clipKey(cam, start, end), s.clipFetch(cam, start, end)); err != nil {
				log.Printf("warm: %s %s: %v", id, cam, err)
				continue
			}
			if old, ok := prev[cam]; ok {
				s.Cache.Evict(clipKey(cam, start, old))
			}
			prev[cam] = end
		}
	}
	// Past the last rung: hold there until the event ends.
	select {
	case <-ctx.Done():
	case <-ended:
		if e, ok = st.Get(id); ok {
			s.warmFinal(ctx, e)
		}
		s.evictCheckpoints(e, prev)
	}
}

// warmFinal caches an ended event's final cuts and snapshot.  It
// first waits out the cut's settling time -- the final cut ends
// clipMargin past the event, and cutting before that recording exists
// would freeze a truncated clip under the final key.
func (s *Server) warmFinal(ctx context.Context, e policy.Event) {
	if e.EndedAt.IsZero() {
		return
	}
	if d := time.Until(e.EndedAt.Add(clipMargin + warmGrace)); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
	start, end, stable := clipCut(e, time.Now(), true)
	if !stable {
		return
	}
	for _, cam := range eventCameras(e) {
		if e.Clip == policy.ClipNone {
			break
		}
		if _, err := s.Cache.Fill(ctx, clipKey(cam, start, end), s.clipFetch(cam, start, end)); err != nil {
			log.Printf("warm: %s %s: %v", e.ID, cam, err)
		}
	}
	if e.HasSnapshot && !e.EndedAt.IsZero() {
		if _, err := s.Cache.Fill(ctx, snapKey(e), s.snapshotFetch(e.SourceID)); err != nil {
			log.Printf("warm: %s snapshot: %v", e.ID, err)
		}
	}
	// The cuts are on disk: render the stitched follow view (a no-op
	// for events without box evidence).
	s.RequestStitch(e, "lo")
}

func (s *Server) evictCheckpoints(e policy.Event, prev map[string]time.Time) {
	start := e.StartedAt.Add(-clipMargin)
	for cam, end := range prev {
		s.Cache.Evict(clipKey(cam, start, end))
	}
}

// eventCameras is every camera with a view of e, the leading one
// first.
func eventCameras(e policy.Event) []string {
	cams := []string{}
	if e.Camera != "" {
		cams = append(cams, e.Camera)
	}
	for _, c := range e.Cameras {
		if c != e.Camera {
			cams = append(cams, c)
		}
	}
	return cams
}
