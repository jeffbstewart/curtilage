// Package policy is the seam between perception and presentation
// (docs/DESIGN.md "Policy"): an Engine consumes the broker stream and
// emits Changes to Events, and Events are what the API serves.
//
// The Engine interface is the whole contract.  Passthrough, the first
// implementation, treats every tracked object as a detection: enough
// to build the event list and the app against real traffic while the
// state machine (arrivals, departures, packages, quiet rules) is
// tuned on the same recordings, behind the same interface.
package policy

import (
	"crypto/sha256"
	"encoding/base32"
	"slices"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
)

// Kind is the engine's verdict on an event.
type Kind int

const (
	KindUnknown Kind = iota
	KindArrival
	KindDeparture
	KindPackage
	KindDetection
)

// ClipState is whether there is a clip and whether it is done.
type ClipState int

const (
	ClipUnknown ClipState = iota
	ClipNone
	ClipGrowing
	ClipFinal
)

// Event is one thing that happened, presentation-shaped: the API's
// Event is a straight mapping of this.
type Event struct {
	// Opaque and stable: MintID of the source id, so a restart that
	// rebuilds from recordings mints the same ids.
	ID       string
	Camera   string
	Label    string
	SubLabel string
	// Every zone the object has been in, in the order entered.
	Zones       []string
	Kind        Kind
	StartedAt   time.Time
	EndedAt     time.Time // zero while running
	HasSnapshot bool
	Clip        ClipState
	// The perception layer's id (a Frigate event id).  Debug only.
	SourceID string
}

// Running reports whether the event has not ended.
func (e Event) Running() bool { return e.EndedAt.IsZero() }

// Audience is who an event is (or would be) delivered to.  Delivery
// does not exist yet; this is the verdict the house page shows so the
// rules can be tuned against real days before anything is pushed.
// Quiet rules and per-person routing will refine it (docs/DESIGN.md
// "Policy").
func Audience(k Kind) string {
	switch k {
	case KindArrival, KindDeparture, KindPackage:
		return "household"
	case KindDetection:
		return "nobody (list only)"
	}
	return "nobody"
}

// Sent reports whether an event of this kind goes to anyone.
func Sent(k Kind) bool {
	switch k {
	case KindArrival, KindDeparture, KindPackage:
		return true
	}
	return false
}

// String names a kind for display.
func (k Kind) String() string {
	switch k {
	case KindArrival:
		return "arrival"
	case KindDeparture:
		return "departure"
	case KindPackage:
		return "package"
	case KindDetection:
		return "detection"
	}
	return "unknown"
}

// String names a clip state for display.
func (c ClipState) String() string {
	switch c {
	case ClipNone:
		return "none"
	case ClipGrowing:
		return "growing"
	case ClipFinal:
		return "final"
	}
	return "unknown"
}

// Op is how an event changed.
type Op int

const (
	OpUnknown Op = iota
	OpStarted
	OpUpdated
	OpEnded
)

// Change is one transition, carrying the event's whole state after it.
type Change struct {
	Op    Op
	Event Event
}

// Engine turns broker messages into event changes.  Observe is called
// with every message, in order, whether live or replayed; it is not
// safe for concurrent use.
type Engine interface {
	Observe(at time.Time, topic string, payload []byte) []Change
}

// MintID derives an event id from a source id: deterministic (replay
// and rebuild agree), opaque, and not the source id itself.
func MintID(sourceID string) string {
	sum := sha256.Sum256([]byte("curtilage/event/" + sourceID))
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])[:20]
}

// Passthrough is the engine without opinions: every tracked object
// Frigate reports (that it did not itself call a false positive) is a
// detection from its first message to its end.
type Passthrough struct {
	live map[string]Event // by source id
	// Undecodable counts what Observe could not parse, for /metrics.
	Undecodable uint64
}

// NewPassthrough returns an empty engine.
func NewPassthrough() *Passthrough { return &Passthrough{live: map[string]Event{}} }

// Live is a snapshot of the events still running, in no order.
func (p *Passthrough) Live() []Event {
	out := make([]Event, 0, len(p.live))
	for _, e := range p.live {
		out = append(out, e)
	}
	return out
}

// Observe implements Engine.
func (p *Passthrough) Observe(at time.Time, topic string, payload []byte) []Change {
	if frigate.ParseTopic(topic).Kind != frigate.KindEvents {
		return nil
	}
	msg, err := frigate.ParseEvent(payload)
	if err != nil {
		p.Undecodable++
		return nil
	}
	obj := msg.After
	if obj.FalsePositive {
		return nil
	}
	next := fromObject(obj, at)
	prev, known := p.live[obj.ID]
	var changes []Change
	switch msg.Type {
	case frigate.New:
		if known { // duplicate new (reconnect replay): nothing changed
			return nil
		}
		p.live[obj.ID] = next
		return []Change{{OpStarted, next}}
	case frigate.Update:
		if !known { // joined mid-lifetime: to the reader it just started
			p.live[obj.ID] = next
			return []Change{{OpStarted, next}}
		}
		if same(prev, next) {
			return nil
		}
		p.live[obj.ID] = next
		return []Change{{OpUpdated, next}}
	case frigate.End:
		if !known { // never saw it start: say so, then end it
			started := next
			started.EndedAt = time.Time{}
			started.Clip = clipState(obj, true)
			changes = append(changes, Change{OpStarted, started})
		}
		delete(p.live, obj.ID)
		if next.EndedAt.IsZero() {
			next.EndedAt = at
		}
		next.Clip = clipState(obj, false)
		return append(changes, Change{OpEnded, next})
	}
	return nil
}

// fromObject is the presentation view of a Frigate object.
func fromObject(o *frigate.Object, at time.Time) Event {
	started := o.StartTime.Time()
	if started.IsZero() {
		started = at
	}
	zones := o.EnteredZones
	if len(zones) == 0 {
		zones = o.CurrentZones
	}
	e := Event{
		ID:          MintID(o.ID),
		Camera:      o.Camera,
		Label:       o.Label,
		SubLabel:    string(o.SubLabel),
		Zones:       slices.Clone(zones),
		Kind:        KindDetection,
		StartedAt:   started,
		EndedAt:     o.EndTime.Time(),
		HasSnapshot: o.HasSnapshot,
		SourceID:    o.ID,
	}
	e.Clip = clipState(o, e.Running())
	return e
}

func clipState(o *frigate.Object, running bool) ClipState {
	switch {
	case !o.HasClip:
		return ClipNone
	case running:
		return ClipGrowing
	}
	return ClipFinal
}

// same reports whether nothing a viewer would notice changed.
func same(a, b Event) bool {
	return a.SubLabel == b.SubLabel && a.HasSnapshot == b.HasSnapshot &&
		a.Clip == b.Clip && a.Kind == b.Kind && slices.Equal(a.Zones, b.Zones)
}
