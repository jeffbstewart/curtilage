package policy

import (
	"slices"
	"sort"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
)

// IncidentConfig tunes the Incidents engine.
type IncidentConfig struct {
	// Labels that make an activity: person, dog.  Everything else is
	// handed to the passthrough engine as a detection.
	Labels []string
	// Gap is how long since an incident's last activity before the
	// next qualifying object starts a new incident, and how long an
	// incident waits after its last activity before it ends.  Data:
	// 20 s between objects inside a dog walk, 22 min between walks.
	Gap time.Duration
	// Overlap is how long two objects of one label must be live at
	// once on one camera to count as two things rather than one
	// split track.
	Overlap time.Duration
}

// DefaultIncidentConfig is what the house starts with.
func DefaultIncidentConfig() IncidentConfig {
	return IncidentConfig{Labels: []string{"person", "dog"}, Gap: 60 * time.Second, Overlap: 3 * time.Second}
}

// Incidents is the engine that folds people and dogs moving about
// into one event per thing that happened (docs/DESIGN.md "Policy").
//
// Each camera mints its own ids, and one camera re-mints on a lost
// track, so no object is ever followed across cameras.  Activity is
// clustered in time instead: a qualifying object that starts within
// Gap of the open incident's last activity joins it; otherwise it
// opens a new one.  An incident becomes an event once any member has
// touched a NAMED zone (the street is not news), ends Gap after its
// last activity, and every change to what is known about it -- more
// objects, another zone, a snapshot, the end -- is an update to the
// same event, so the app's one notification gets better as it goes.
//
// Counts per label are the most objects of that label live at once on
// any single camera (overlapping by at least Overlap), so one person
// on four cameras in turn is one person, and two side by side are two.
type Incidents struct {
	cfg    IncidentConfig
	labels map[string]bool
	rest   *Passthrough
	open   *incident
	// closed remembers objects of ended incidents for a while so a
	// straggling update for one cannot open a new incident.
	closed map[string]time.Time
}

type member struct {
	source, camera, label string
	start, end            time.Time // end zero while live
	zones                 []string
	snapshot, clip        bool
}

type incident struct {
	first   time.Time // earliest member start
	last    time.Time // latest activity (any member message)
	members []*member
	byID    map[string]*member
	path    []string // named zones, order first entered
	sent    bool     // Started has been emitted
	event   Event    // as last emitted
}

// NewIncidents returns an engine with cfg (zero fields take defaults).
func NewIncidents(cfg IncidentConfig) *Incidents {
	d := DefaultIncidentConfig()
	if len(cfg.Labels) == 0 {
		cfg.Labels = d.Labels
	}
	if cfg.Gap <= 0 {
		cfg.Gap = d.Gap
	}
	if cfg.Overlap <= 0 {
		cfg.Overlap = d.Overlap
	}
	e := &Incidents{cfg: cfg, labels: map[string]bool{}, rest: NewPassthrough(), closed: map[string]time.Time{}}
	for _, l := range cfg.Labels {
		e.labels[l] = true
	}
	return e
}

// Undecodable is what could not be parsed, for /metrics.
func (e *Incidents) Undecodable() uint64 { return e.rest.Undecodable }

// Observe implements Engine.  Every message is a clock tick: an open
// incident past its gap ends on whatever arrives next, event or not.
func (e *Incidents) Observe(at time.Time, topic string, payload []byte) []Change {
	var changes []Change
	if e.open != nil && at.Sub(e.open.last) >= e.cfg.Gap {
		changes = append(changes, e.close(at)...)
	}
	if frigate.ParseTopic(topic).Kind != frigate.KindEvents {
		return changes
	}
	msg, err := frigate.ParseEvent(payload)
	if err != nil || msg.After.FalsePositive || !e.labels[msg.After.Label] {
		return append(changes, e.rest.Observe(at, topic, payload)...)
	}
	obj := msg.After
	if _, gone := e.closed[obj.ID]; gone {
		return changes // a straggler from an incident that already ended
	}
	if e.open == nil {
		if msg.Type == frigate.End {
			return changes // an end for something never seen starting: not news
		}
		e.open = &incident{byID: map[string]*member{}}
	}
	inc := e.open
	inc.last = at
	m, known := inc.byID[obj.ID]
	if !known {
		m = &member{source: obj.ID, camera: obj.Camera, label: obj.Label, start: obj.StartTime.Time()}
		if m.start.IsZero() {
			m.start = at
		}
		inc.members = append(inc.members, m)
		inc.byID[obj.ID] = m
		if inc.first.IsZero() || m.start.Before(inc.first) {
			inc.first = m.start
		}
	}
	m.snapshot = m.snapshot || obj.HasSnapshot
	m.clip = m.clip || obj.HasClip
	for _, z := range append(append([]string(nil), obj.EnteredZones...), obj.CurrentZones...) {
		if z != "" && !slices.Contains(m.zones, z) {
			m.zones = append(m.zones, z)
		}
		if z != "" && !slices.Contains(inc.path, z) {
			inc.path = append(inc.path, z)
		}
	}
	if msg.Type == frigate.End {
		m.end = obj.EndTime.Time()
		if m.end.IsZero() {
			m.end = at
		}
	}
	return append(changes, e.emit(at)...)
}

// emit compares what is now known to what was last said.
func (e *Incidents) emit(at time.Time) []Change {
	inc := e.open
	if len(inc.path) == 0 {
		return nil // not news until something happens in a named zone
	}
	next := e.build(inc, at, false)
	if !inc.sent {
		inc.sent, inc.event = true, next
		return []Change{{OpStarted, next}}
	}
	if sameActivity(inc.event, next) {
		return nil
	}
	inc.event = next
	return []Change{{OpUpdated, next}}
}

// close ends the open incident; a never-sent one just disappears.
func (e *Incidents) close(at time.Time) []Change {
	inc := e.open
	e.open = nil
	for id := range inc.byID {
		e.closed[id] = at
	}
	for id, t := range e.closed { // bounded: forget after an hour
		if at.Sub(t) > time.Hour {
			delete(e.closed, id)
		}
	}
	if !inc.sent {
		return nil
	}
	return []Change{{OpEnded, e.build(inc, at, true)}}
}

// build is the event as the incident currently stands.  Members that
// never touched a named zone still count (a real person crossing
// between zones) but never ANCHOR the event's public face: the start
// time, the first camera, and the snapshot come from zoned members,
// so a misclassified parked car folded into a walk cannot become its
// picture or its opening scene.
func (e *Incidents) build(inc *incident, at time.Time, ended bool) Event {
	first := inc.members[0]
	ev := Event{
		ID:      MintID("activity/" + first.source),
		Kind:    KindActivity,
		Path:    slices.Clone(inc.path),
		Zones:   slices.Clone(inc.path),
		Objects: map[string]int{},
	}
	var last time.Time
	var unzonedCams []string
	var anySnapshot string
	anyClip := false
	for _, m := range inc.members {
		zoned := len(m.zones) > 0
		if zoned {
			if ev.StartedAt.IsZero() || m.start.Before(ev.StartedAt) {
				ev.StartedAt = m.start
			}
			if !slices.Contains(ev.Cameras, m.camera) {
				ev.Cameras = append(ev.Cameras, m.camera)
			}
			if m.snapshot && ev.SourceID == "" {
				ev.SourceID, ev.HasSnapshot = m.source, true
			}
		} else if !slices.Contains(unzonedCams, m.camera) {
			unzonedCams = append(unzonedCams, m.camera)
		}
		if m.snapshot && anySnapshot == "" {
			anySnapshot = m.source
		}
		ev.SourceIDs = append(ev.SourceIDs, m.source)
		ev.Spans = append(ev.Spans, Span{Camera: m.camera, Label: m.label, Start: m.start, End: m.end})
		anyClip = anyClip || m.clip
		end := m.end
		if end.IsZero() {
			end = at
		}
		if end.After(last) {
			last = end
		}
		ev.Objects[m.label] = max(ev.Objects[m.label], e.concurrent(inc, m, at))
	}
	if ev.StartedAt.IsZero() {
		ev.StartedAt = inc.first // no zoned member (never emitted): moot
	}
	for _, c := range unzonedCams {
		if !slices.Contains(ev.Cameras, c) {
			ev.Cameras = append(ev.Cameras, c)
		}
	}
	if ev.SourceID == "" && anySnapshot != "" {
		ev.SourceID, ev.HasSnapshot = anySnapshot, true
	}
	if ev.SourceID == "" {
		ev.SourceID = first.source
	}
	ev.Camera = ev.Cameras[0]
	ev.Label = primaryLabel(ev.Objects)
	if ended {
		ev.EndedAt = last
	}
	switch {
	case !anyClip:
		ev.Clip = ClipNone
	case ended:
		ev.Clip = ClipFinal
	default:
		ev.Clip = ClipGrowing
	}
	return ev
}

// concurrent is how many objects of m's label on m's camera are live
// at the instant m has been live for Overlap, m included, counting
// only objects that started no later than m.  Taken over every m as
// the anchor this is the largest set that was genuinely on screen
// together; a chain of overlaps (A with B, B with C, never all three)
// counts two, and a split track that replaces a dying one counts one.
func (e *Incidents) concurrent(inc *incident, m *member, at time.Time) int {
	t := m.start.Add(e.cfg.Overlap)
	if liveEnd(m, at).Before(t) {
		return 1 // gone before it proved itself: never more than itself
	}
	n := 0
	for _, o := range inc.members {
		if o.camera != m.camera || o.label != m.label || o.start.After(m.start) {
			continue
		}
		if !liveEnd(o, at).Before(t) {
			n++
		}
	}
	return max(n, 1)
}

func liveEnd(m *member, at time.Time) time.Time {
	if m.end.IsZero() {
		return at
	}
	return m.end
}

// primaryLabel is "person" when present, else the most numerous, ties
// alphabetical.
func primaryLabel(objects map[string]int) string {
	if objects["person"] > 0 {
		return "person"
	}
	labels := make([]string, 0, len(objects))
	for l := range objects {
		labels = append(labels, l)
	}
	sort.Slice(labels, func(i, j int) bool {
		if objects[labels[i]] != objects[labels[j]] {
			return objects[labels[i]] > objects[labels[j]]
		}
		return labels[i] < labels[j]
	})
	if len(labels) == 0 {
		return ""
	}
	return labels[0]
}

// sameActivity reports whether nothing worth an update changed.
func sameActivity(a, b Event) bool {
	if len(a.Objects) != len(b.Objects) {
		return false
	}
	for l, n := range a.Objects {
		if b.Objects[l] != n {
			return false
		}
	}
	return slices.Equal(a.Path, b.Path) && slices.Equal(a.Cameras, b.Cameras) &&
		a.HasSnapshot == b.HasSnapshot && a.Clip == b.Clip && a.EndedAt.Equal(b.EndedAt)
}
