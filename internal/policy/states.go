package policy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jeffbstewart/curtilage/internal/frigate"
)

// StateModel is one watched classifier and how much to trust it.
type StateModel struct {
	Model string
	// Hold is how long a new class must persist before it is
	// believed; the sensors are twitchy and their lies have lasted
	// minutes.
	Hold time.Duration
	// News: believed changes become household events.
	News bool
	// AlarmClass/AlarmAfter: alarm when the believed state has been
	// this class this long (a door left open an hour).
	AlarmClass string
	AlarmAfter time.Duration
}

// States believes classifier verdicts slowly (docs/DESIGN.md
// "Policy": react to enduring states, not transients).  Raw values
// are tracked instantly and shown honestly; the BELIEVED state only
// changes after a raw value survives the model's hold, and only
// believed changes are ever news.  Safe for concurrent reads
// (Snapshot); Observe is single-goroutine like every Engine.
type States struct {
	mu     sync.Mutex
	models map[string]*stateModel
}

type stateModel struct {
	cfg     StateModel
	camera  string
	raw     string
	rawAt   time.Time // when raw last CHANGED
	held    string
	heldAt  time.Time // when held last changed
	alarmed bool      // this episode already alarmed
}

// ModelState is one model's truth and its still-unproven raw value.
type ModelState struct {
	Model, Camera string
	Held          string
	HeldSince     time.Time
	// Raw differs from Held while a change is still on hold.
	Raw      string
	RawSince time.Time
}

// NewStates returns a States engine for the models.
func NewStates(models []StateModel) *States {
	s := &States{models: map[string]*stateModel{}}
	for _, m := range models {
		if m.Hold <= 0 {
			m.Hold = 10 * time.Minute
		}
		s.models[m.Model] = &stateModel{cfg: m}
	}
	return s
}

// Snapshot is every model's state, sorted by model name.
func (s *States) Snapshot() []ModelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ModelState, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, ModelState{Model: m.cfg.Model, Camera: m.camera,
			Held: m.held, HeldSince: m.heldAt, Raw: m.raw, RawSince: m.rawAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Observe implements Engine.  Every message is a clock tick.
func (s *States) Observe(at time.Time, topic string, payload []byte) []Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	if top := frigate.ParseTopic(topic); top.Kind == frigate.KindClassification {
		if m, ok := s.models[top.Model]; ok {
			if class, err := frigate.ParseClass(payload); err == nil {
				m.camera = top.Camera
				if class != m.raw {
					m.raw, m.rawAt = class, at
				}
				if m.held == "" {
					// Cold start: the first value is adopted as the
					// belief without ceremony -- it is not a change.
					m.held, m.heldAt = class, at
				}
			}
		}
	}
	return s.tick(at)
}

func (s *States) tick(at time.Time) []Change {
	var changes []Change
	for _, m := range s.models {
		if m.raw != "" && m.raw != m.held && at.Sub(m.rawAt) >= m.cfg.Hold {
			m.held, m.heldAt, m.alarmed = m.raw, m.rawAt, false
			if m.cfg.News {
				changes = append(changes, Change{OpStarted, s.event(m, m.rawAt, m.rawAt)})
			}
		}
		if !m.alarmed && m.cfg.AlarmClass != "" && m.held == m.cfg.AlarmClass && at.Sub(m.heldAt) >= m.cfg.AlarmAfter {
			m.alarmed = true
			changes = append(changes, Change{OpStarted, s.event(m, m.heldAt, at)})
		}
	}
	return changes
}

// event is a believed transition (started == ended) or an alarm about
// an enduring state (started..ended is how long it has endured).
func (s *States) event(m *stateModel, started, at time.Time) Event {
	return Event{
		ID:        MintID(fmt.Sprintf("state/%s/%s/%d", m.cfg.Model, m.held, at.UnixNano())),
		Kind:      KindState,
		Camera:    m.camera,
		Cameras:   []string{m.camera},
		Label:     m.cfg.Model,
		SubLabel:  m.held,
		StartedAt: started,
		EndedAt:   at,
		Clip:      ClipFinal,
	}
}

// Humanize turns a class or model token into words: with the model
// name as a prefix stripped, "bmw_garage_door_open" under model
// "bmw_garage_door" reads "open".
func Humanize(model, class string) string {
	c := strings.TrimPrefix(class, model)
	c = strings.Trim(c, "_ ")
	if c == "" {
		c = class
	}
	return strings.ReplaceAll(c, "_", " ")
}
