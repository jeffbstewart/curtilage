// Package store holds the events the API serves: in memory, rebuilt
// from the day's recordings at startup, pruned to the retention
// window (docs/DESIGN.md).  There is no database; the MCAP files are
// the source of truth and this is a view of them.
package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jeffbstewart/curtilage/internal/policy"
)

// ErrBadToken is returned by List for a continuation token it did not
// mint.
var ErrBadToken = errors.New("store: invalid continuation token")

// Store is safe for concurrent use.
type Store struct {
	mu        sync.RWMutex
	retention time.Duration
	byID      map[string]*entry
	ordered   []*entry // newest first: StartedAt desc, ID desc
	cameras   map[string]bool
	subs      map[*subscriber]struct{}
	applied   uint64
	pruned    uint64
}

type entry struct {
	ev        policy.Event
	updatedAt time.Time
	// history is every state the event has been in, oldest first:
	// how the engine's understanding evolved.  Bounded.
	history []Revision
}

// Revision is one state an event was in, and when it was said.
type Revision struct {
	At    time.Time
	Op    policy.Op
	Event policy.Event
}

// maxHistory bounds an event's revisions; an activity gets a few
// dozen updates at most, a parked car could heartbeat forever.
const maxHistory = 64

// New returns an empty store keeping events for retention.
func New(retention time.Duration) *Store {
	return &Store{
		retention: retention,
		byID:      map[string]*entry{},
		cameras:   map[string]bool{},
		subs:      map[*subscriber]struct{}{},
	}
}

// Retention is how far back events are kept.
func (s *Store) Retention() time.Duration { return s.retention }

// Apply records a change observed at at and fans it out to watchers.
func (s *Store) Apply(at time.Time, c policy.Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applied++
	if c.Event.Camera != "" {
		s.cameras[c.Event.Camera] = true
	}
	e, ok := s.byID[c.Event.ID]
	var history []Revision
	if ok {
		s.remove(e)
		history = e.history
	}
	history = append(history, Revision{At: at, Op: c.Op, Event: c.Event})
	if len(history) > maxHistory {
		history = append(history[:1], history[len(history)-maxHistory+1:]...) // keep the first, drop the middle
	}
	e = &entry{ev: c.Event, updatedAt: at, history: history}
	s.byID[c.Event.ID] = e
	s.insert(e)
	for sub := range s.subs {
		sub.offer(c)
	}
}

// History is every state the event has been in, oldest first.
func (s *Store) History(id string) []Revision {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	if !ok {
		return nil
	}
	return append([]Revision(nil), e.history...)
}

// SawCamera notes a camera name seen in a topic, so ListCameras knows
// it before it has produced an event.
func (s *Store) SawCamera(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	s.cameras[name] = true
	s.mu.Unlock()
}

// Cameras is every camera seen, sorted.
func (s *Store) Cameras() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.cameras))
	for c := range s.cameras {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Get is one event by id.
func (s *Store) Get(id string) (policy.Event, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	if !ok {
		return policy.Event{}, false
	}
	return e.ev, true
}

// List is one page, newest first, optionally only these cameras.
// token is "" for the first page; the returned token is "" only when
// this page is empty (the edge of retention).
func (s *Store) List(cameras []string, pageSize int, token string) ([]policy.Event, string, error) {
	var after *cursor
	if token != "" {
		c, err := parseCursor(token)
		if err != nil {
			return nil, "", err
		}
		after = &c
	}
	want := cameraSet(cameras)
	s.mu.RLock()
	defer s.mu.RUnlock()
	start := 0
	if after != nil {
		// First entry strictly older than the cursor.
		start = sort.Search(len(s.ordered), func(i int) bool { return s.ordered[i].older(*after) })
	}
	var page []policy.Event
	for i := start; i < len(s.ordered) && len(page) < pageSize; i++ {
		if want != nil && !want[s.ordered[i].ev.Camera] {
			continue
		}
		page = append(page, s.ordered[i].ev)
	}
	if len(page) == 0 {
		return nil, "", nil
	}
	last := page[len(page)-1]
	return page, cursor{last.StartedAt, last.ID}.String(), nil
}

// Watch delivers every change from now on, preceded by one change per
// event touched since since (as OpStarted, or OpEnded if it has
// ended: whole state, applied as an upsert).  The channel closes when
// ctx ends or when the watcher fell too far behind, in which case it
// should watch again with a since.
func (s *Store) Watch(ctx context.Context, since time.Time, cameras []string) <-chan policy.Change {
	sub := &subscriber{ch: make(chan policy.Change, watchBuffer), want: cameraSet(cameras)}
	s.mu.Lock()
	// Snapshot and subscribe under one lock: nothing slips between.
	if !since.IsZero() {
		for i := len(s.ordered) - 1; i >= 0; i-- { // oldest first
			e := s.ordered[i]
			if e.updatedAt.Before(since) {
				continue
			}
			op := policy.OpStarted
			if !e.ev.Running() {
				op = policy.OpEnded
			}
			sub.offer(policy.Change{Op: op, Event: e.ev})
		}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		s.drop(sub)
		s.mu.Unlock()
	}()
	return sub.ch
}

// Prune forgets events that started before now-retention; returns
// how many.
func (s *Store) Prune(now time.Time) int {
	cutoff := now.Add(-s.retention)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for i := len(s.ordered) - 1; i >= 0 && s.ordered[i].ev.StartedAt.Before(cutoff); i-- {
		delete(s.byID, s.ordered[i].ev.ID)
		s.ordered = s.ordered[:i]
		n++
	}
	s.pruned += uint64(n)
	return n
}

// Stats for /metrics.
type Stats struct {
	Events, Live, Watchers int
	Applied, Pruned        uint64
}

// Stats returns a snapshot.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{Events: len(s.byID), Watchers: len(s.subs), Applied: s.applied, Pruned: s.pruned}
	for _, e := range s.byID {
		if e.ev.Running() {
			st.Live++
		}
	}
	return st
}

// insert keeps ordered sorted; callers hold mu.
func (s *Store) insert(e *entry) {
	c := cursor{e.ev.StartedAt, e.ev.ID}
	i := sort.Search(len(s.ordered), func(i int) bool { return s.ordered[i].older(c) })
	s.ordered = append(s.ordered, nil)
	copy(s.ordered[i+1:], s.ordered[i:])
	s.ordered[i] = e
}

// remove takes e out of ordered; callers hold mu.
func (s *Store) remove(e *entry) {
	c := cursor{e.ev.StartedAt, e.ev.ID}
	i := sort.Search(len(s.ordered), func(i int) bool { return !s.ordered[i].newer(c) })
	if i < len(s.ordered) && s.ordered[i] == e {
		s.ordered = append(s.ordered[:i], s.ordered[i+1:]...)
	}
}

func (s *Store) drop(sub *subscriber) {
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		if !sub.dead { // offer already closed it when it fell behind
			sub.dead = true
			close(sub.ch)
		}
	}
}

// older / newer compare an entry to a cursor in list order (newest
// first): StartedAt desc, then ID desc.
func (e *entry) older(c cursor) bool {
	if !e.ev.StartedAt.Equal(c.startedAt) {
		return e.ev.StartedAt.Before(c.startedAt)
	}
	return e.ev.ID < c.id
}

func (e *entry) newer(c cursor) bool {
	if !e.ev.StartedAt.Equal(c.startedAt) {
		return e.ev.StartedAt.After(c.startedAt)
	}
	return e.ev.ID > c.id
}

// cursor is a position in list order, encoded as the continuation
// token.
type cursor struct {
	startedAt time.Time
	id        string
}

func (c cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.startedAt.UnixNano(), 10) + "|" + c.id))
}

func parseCursor(token string) (cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, ErrBadToken
	}
	ns, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return cursor{}, ErrBadToken
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: %v", ErrBadToken, err)
	}
	return cursor{time.Unix(0, n).UTC(), id}, nil
}

func cameraSet(cameras []string) map[string]bool {
	if len(cameras) == 0 {
		return nil
	}
	m := make(map[string]bool, len(cameras))
	for _, c := range cameras {
		m[c] = true
	}
	return m
}

// watchBuffer is how far a watcher may lag before it is cut off; a
// burst of a few hundred changes is a busy minute, not a stuck client.
const watchBuffer = 256

type subscriber struct {
	ch   chan policy.Change
	want map[string]bool
	dead bool
}

// offer delivers without blocking; a full buffer ends the
// subscription (the channel is closed by drop, under the store lock,
// which every caller of offer holds).
func (sub *subscriber) offer(c policy.Change) {
	if sub.dead || (sub.want != nil && !sub.want[c.Event.Camera]) {
		return
	}
	select {
	case sub.ch <- c:
	default:
		sub.dead = true
		close(sub.ch)
	}
}
