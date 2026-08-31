package policy

import (
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
)

// Watch is one occupancy question: how many <Labels> are in <Zone>?
type Watch struct {
	Zone   string
	Labels []string
	// ArriveAfter is how long an object must hold the zone before it
	// counts: the one-second maneuvering flaps never arrive.
	ArriveAfter time.Duration
	// DepartAfter is how long the count must stay down before a
	// departure is believed: occlusion and night dropouts are shorter.
	DepartAfter time.Duration
}

// Occupancy is the ledger of what is parked where (docs/DESIGN.md
// "Policy"; built for cars, deliberately label-generic so the day
// Frigate can see a garbage can, the cans are one Watch).
//
// Frigate publishes no per-zone counts here, so the ledger is built
// from tracked objects.  A parked car is one multi-hour track and its
// end IS the departure signal; but the same object is often tracked
// by TWO cameras at once (both driveway cameras see the side lot), so
// the count for a zone is the MAXIMUM over cameras of that camera's
// dwell-qualified tracks -- never the sum.  Arrivals fire when the
// qualified count rises; departures when the raw count has stayed
// below the ledger for DepartAfter.
//
// Safe for concurrent reads (Presence); Observe itself, like every
// Engine, is called from one goroutine.
type Occupancy struct {
	mu      sync.Mutex
	watches map[string]*Watch // by zone
	ledgers map[string]*ledger
	tracks  map[string]*occTrack
}

type occTrack struct {
	source, camera, label string
	// plate is LPR's read, subLabel a known plate's friendly name;
	// both sticky: a later read never erases an earlier one.
	plate, subLabel string
	snapshot        bool
	// entered is when the track first held each watched zone; a zone
	// left is deleted, so re-entry restarts the dwell clock.
	entered map[string]time.Time
}

// ledger is one (zone, label) count with its transition state.
type ledger struct {
	watch      *Watch
	label      string
	present    int       // what we believe
	since      time.Time // when present last rose
	lastSeen   time.Time // last moment raw sightings covered present
	belowSince time.Time // raw count has been under present since; zero if not
}

// ZoneState is one ledger, for the house page and (later) MQTT out.
type ZoneState struct {
	Zone, Label     string
	Count           int
	Since, LastSeen time.Time
}

// NewOccupancy returns a ledger engine for the watches.
func NewOccupancy(watches []Watch) *Occupancy {
	o := &Occupancy{watches: map[string]*Watch{}, ledgers: map[string]*ledger{}, tracks: map[string]*occTrack{}}
	for i := range watches {
		w := watches[i]
		if len(w.Labels) == 0 {
			w.Labels = []string{"car"}
		}
		if w.ArriveAfter <= 0 {
			w.ArriveAfter = time.Minute
		}
		if w.DepartAfter <= 0 {
			w.DepartAfter = 5 * time.Minute
		}
		o.watches[w.Zone] = &w
		for _, l := range w.Labels {
			o.ledgers[w.Zone+"|"+l] = &ledger{watch: &w, label: l}
		}
	}
	return o
}

// Presence is every ledger, sorted by zone then label.
func (o *Occupancy) Presence() []ZoneState {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]ZoneState, 0, len(o.ledgers))
	for _, l := range o.ledgers {
		out = append(out, ZoneState{Zone: l.watch.Zone, Label: l.label, Count: l.present, Since: l.since, LastSeen: l.lastSeen})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Zone != out[j].Zone {
			return out[i].Zone < out[j].Zone
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// Observe implements Engine.  Every message is also a clock tick for
// the timers, whatever its topic.
func (o *Occupancy) Observe(at time.Time, topic string, payload []byte) []Change {
	o.mu.Lock()
	defer o.mu.Unlock()
	if frigate.ParseTopic(topic).Kind == frigate.KindEvents {
		if msg, err := frigate.ParseEvent(payload); err == nil && !msg.After.FalsePositive {
			o.absorb(at, msg)
		}
	}
	return o.tick(at)
}

// absorb updates the track table from one event message.
func (o *Occupancy) absorb(at time.Time, msg *frigate.Event) {
	obj := msg.After
	watched := false
	for _, w := range o.watches {
		if slices.Contains(w.Labels, obj.Label) {
			watched = true
		}
	}
	if !watched {
		return
	}
	if msg.Type == frigate.End {
		delete(o.tracks, obj.ID)
		return
	}
	tr, ok := o.tracks[obj.ID]
	if !ok {
		tr = &occTrack{source: obj.ID, camera: obj.Camera, label: obj.Label, entered: map[string]time.Time{}}
		o.tracks[obj.ID] = tr
	}
	tr.snapshot = tr.snapshot || obj.HasSnapshot
	if p := string(obj.Plate); p != "" {
		tr.plate = p
	}
	if s := string(obj.SubLabel); s != "" {
		tr.subLabel = s
	}
	// Zone membership is CURRENT zones while the object moves -- a car
	// that drove through the driveway and parked beyond it has left
	// the driveway.  But Frigate empties current_zones once an object
	// goes STATIONARY, so for a parked object the membership is the
	// LAST zone it entered: where it stopped.
	in := map[string]bool{}
	claim := func(z string) {
		if _, ok := o.watches[z]; ok {
			in[z] = true
			if _, seen := tr.entered[z]; !seen {
				tr.entered[z] = at
			}
		}
	}
	for _, z := range obj.CurrentZones {
		claim(z)
	}
	if obj.Stationary && len(obj.EnteredZones) > 0 {
		claim(obj.EnteredZones[len(obj.EnteredZones)-1])
	}
	for z := range tr.entered {
		if !in[z] {
			delete(tr.entered, z)
		}
	}
}

// counts is (qualified, raw) for one ledger at at: qualified tracks
// have held the zone for ArriveAfter; raw is any current sighting.
// Both are max over cameras.
func (o *Occupancy) counts(l *ledger, at time.Time) (qualified, raw int) {
	perCamQ, perCamR := map[string]int{}, map[string]int{}
	for _, tr := range o.tracks {
		if tr.label != l.label {
			continue
		}
		ent, ok := tr.entered[l.watch.Zone]
		if !ok {
			continue
		}
		perCamR[tr.camera]++
		if at.Sub(ent) >= l.watch.ArriveAfter {
			perCamQ[tr.camera]++
		}
	}
	for _, n := range perCamQ {
		qualified = max(qualified, n)
	}
	for _, n := range perCamR {
		raw = max(raw, n)
	}
	return qualified, raw
}

// tick advances every ledger and returns the transitions.
func (o *Occupancy) tick(at time.Time) []Change {
	var changes []Change
	for _, l := range o.ledgers {
		q, raw := o.counts(l, at)
		if raw >= l.present {
			l.belowSince = time.Time{}
			if raw > 0 {
				l.lastSeen = at
			}
		} else if l.belowSince.IsZero() {
			l.belowSince = at
		}
		if q > l.present {
			// Something new has held the zone long enough: an arrival.
			l.present, l.since, l.belowSince = q, at, time.Time{}
			changes = append(changes, Change{OpStarted, o.event(l, at, KindArrival)})
			continue
		}
		if l.present > 0 && !l.belowSince.IsZero() && at.Sub(l.belowSince) >= l.watch.DepartAfter {
			// Nothing has covered the ledger for the whole grace: gone.
			l.present, l.belowSince = max(q, raw), time.Time{}
			changes = append(changes, Change{OpStarted, o.event(l, at, KindDeparture)})
		}
	}
	return changes
}

// event is one instantaneous arrival or departure.  The camera and
// snapshot come from a track in the zone when one exists (arrivals
// always have one; a departure may not).
func (o *Occupancy) event(l *ledger, at time.Time, kind Kind) Event {
	ev := Event{
		ID:        MintID(fmt.Sprintf("occupancy/%s/%s/%d", l.watch.Zone, l.label, at.UnixNano())),
		Kind:      kind,
		Label:     l.label,
		Zones:     []string{l.watch.Zone},
		Path:      []string{l.watch.Zone},
		Objects:   map[string]int{l.label: l.present},
		StartedAt: at,
		EndedAt:   at,
		Clip:      ClipFinal, // the recording-range clip around the moment
	}
	for _, tr := range o.tracks {
		if tr.label != l.label {
			continue
		}
		if _, ok := tr.entered[l.watch.Zone]; !ok {
			continue
		}
		ev.Camera = tr.camera
		ev.SourceID = tr.source
		ev.HasSnapshot = tr.snapshot
		if ev.Plate == "" {
			ev.Plate = tr.plate
		}
		if ev.SubLabel == "" {
			ev.SubLabel = tr.subLabel
		}
		if !slices.Contains(ev.Cameras, tr.camera) {
			ev.Cameras = append(ev.Cameras, tr.camera)
		}
		ev.SourceIDs = append(ev.SourceIDs, tr.source)
	}
	return ev
}

// Multi fans one broker stream into several engines and concatenates
// what they say.
type Multi []Engine

// Observe implements Engine.
func (m Multi) Observe(at time.Time, topic string, payload []byte) []Change {
	var out []Change
	for _, e := range m {
		out = append(out, e.Observe(at, topic, payload)...)
	}
	return out
}
