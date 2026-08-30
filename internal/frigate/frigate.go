// Package frigate decodes what Frigate publishes over MQTT
// (docs/DESIGN.md "Perception"): tracked-object events, review items,
// and the per-camera / per-zone object counts.  Only the fields the
// policy engine reads are decoded; the recording keeps the rest, byte
// for byte, for the day a later rule wants more.
//
// Tolerant by design: Frigate's JSON shifts between versions, so
// unknown fields are ignored, and the few fields that have changed
// shape (sub_label) accept every shape seen.
package frigate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kind is what a topic carries.
type Kind int

const (
	// KindOther is anything not decoded here (status heartbeats,
	// motion, snapshots, stats).
	KindOther Kind = iota
	// KindEvents is frigate/events: one tracked object's lifetime as
	// new / update / end.
	KindEvents
	// KindReviews is frigate/reviews: Frigate's own alert/detection
	// grouping of events.
	KindReviews
	// KindCount is frigate/<camera>/<label> or
	// frigate/<camera>/<zone>/<label>: how many of label are in frame
	// (or in zone) right now, as a bare integer, retained.
	KindCount
	// KindAvailable is frigate/available: "online" / "offline".
	KindAvailable
)

// Topic is a classified topic name.  Camera, Zone and Label are set
// for KindCount only.
type Topic struct {
	Kind   Kind
	Camera string
	Zone   string
	Label  string
}

// Sub-topics under frigate/<camera>/ that look like counts by shape
// but are not.
var notCounts = map[string]bool{
	"motion": true, "audio": true, "status": true, "snapshot": true,
	"active": true, "ptz_autotracker": true, "recordings": true,
	"detect": true, "record": true, "enabled": true, "birdseye": true,
	"improve_contrast": true, "motion_threshold": true,
	"motion_contour_area": true, "review_alerts": true,
	"review_detections": true, "classification": true, "review_status": true,
}

// ParseTopic classifies a topic by its shape alone; a KindCount
// payload still has to parse as an integer (ParseCount).
func ParseTopic(topic string) Topic {
	parts := strings.Split(topic, "/")
	if len(parts) < 2 || parts[0] != "frigate" {
		return Topic{}
	}
	switch {
	case len(parts) == 2 && parts[1] == "events":
		return Topic{Kind: KindEvents}
	case len(parts) == 2 && parts[1] == "reviews":
		return Topic{Kind: KindReviews}
	case len(parts) == 2 && parts[1] == "available":
		return Topic{Kind: KindAvailable}
	case len(parts) == 3 && !notCounts[parts[2]]:
		return Topic{Kind: KindCount, Camera: parts[1], Label: parts[2]}
	case len(parts) == 4 && !notCounts[parts[2]] && !notCounts[parts[3]]:
		return Topic{Kind: KindCount, Camera: parts[1], Zone: parts[2], Label: parts[3]}
	}
	return Topic{}
}

// Seconds is Frigate's timestamp: Unix seconds with a fraction, or
// null when not yet set.
type Seconds float64

// Time converts; the zero Seconds (null) is the zero Time.
func (s Seconds) Time() time.Time {
	if s == 0 {
		return time.Time{}
	}
	sec, frac := math.Modf(float64(s))
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// SubLabel is a classifier's refinement of the label.  Frigate has
// published it as a string, as null, and as a [name, score] pair;
// all decode to the name.
type SubLabel string

// UnmarshalJSON accepts null, "name", or ["name", score].
func (s *SubLabel) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = ""
		return nil
	}
	var name string
	if err := json.Unmarshal(b, &name); err == nil {
		*s = SubLabel(name)
		return nil
	}
	var pair []json.RawMessage
	if err := json.Unmarshal(b, &pair); err != nil || len(pair) == 0 {
		return fmt.Errorf("sub_label: unrecognized shape %s", b)
	}
	if err := json.Unmarshal(pair[0], &name); err != nil {
		return fmt.Errorf("sub_label: %w", err)
	}
	*s = SubLabel(name)
	return nil
}

// Object is one tracked object as Frigate describes it in an event.
type Object struct {
	ID            string   `json:"id"`
	Camera        string   `json:"camera"`
	Label         string   `json:"label"`
	SubLabel      SubLabel `json:"sub_label"`
	Score         float64  `json:"score"`
	TopScore      float64  `json:"top_score"`
	FalsePositive bool     `json:"false_positive"`
	StartTime     Seconds  `json:"start_time"`
	EndTime       Seconds  `json:"end_time"`
	Active        bool     `json:"active"`
	Stationary    bool     `json:"stationary"`
	CurrentZones  []string `json:"current_zones"`
	EnteredZones  []string `json:"entered_zones"`
	HasClip       bool     `json:"has_clip"`
	HasSnapshot   bool     `json:"has_snapshot"`
	MaxSeverity   string   `json:"max_severity"`
}

// Change is an event message's type.
type Change string

const (
	New    Change = "new"
	Update Change = "update"
	End    Change = "end"
)

// Event is one frigate/events message: the object before and after
// whatever changed.  After is always present.
type Event struct {
	Type   Change  `json:"type"`
	Before *Object `json:"before"`
	After  *Object `json:"after"`
}

// ErrShape is returned when a payload parsed as JSON but is not the
// message it should be.
var ErrShape = errors.New("frigate: unexpected message shape")

// ParseEvent decodes a frigate/events payload.
func ParseEvent(payload []byte) (*Event, error) {
	var e Event
	if err := json.Unmarshal(payload, &e); err != nil {
		return nil, fmt.Errorf("frigate/events: %w", err)
	}
	switch e.Type {
	case New, Update, End:
	default:
		return nil, fmt.Errorf("%w: frigate/events type %q", ErrShape, e.Type)
	}
	if e.After == nil || e.After.ID == "" || e.After.Camera == "" {
		return nil, fmt.Errorf("%w: frigate/events without after.id/camera", ErrShape)
	}
	return &e, nil
}

// ReviewData is what a review item groups.
type ReviewData struct {
	// Event ids (Object.ID) in this review item.
	Detections []string `json:"detections"`
	Objects    []string `json:"objects"`
	SubLabels  []string `json:"sub_labels"`
	Zones      []string `json:"zones"`
}

// ReviewItem is Frigate's grouping of one or more events into one
// thing to review, with its own severity.
type ReviewItem struct {
	ID        string     `json:"id"`
	Camera    string     `json:"camera"`
	StartTime Seconds    `json:"start_time"`
	EndTime   Seconds    `json:"end_time"`
	Severity  string     `json:"severity"` // "alert" or "detection"
	ThumbPath string     `json:"thumb_path"`
	Data      ReviewData `json:"data"`
}

// Review is one frigate/reviews message.
type Review struct {
	Type   Change      `json:"type"`
	Before *ReviewItem `json:"before"`
	After  *ReviewItem `json:"after"`
}

// ParseReview decodes a frigate/reviews payload.
func ParseReview(payload []byte) (*Review, error) {
	var r Review
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("frigate/reviews: %w", err)
	}
	switch r.Type {
	case New, Update, End:
	default:
		return nil, fmt.Errorf("%w: frigate/reviews type %q", ErrShape, r.Type)
	}
	if r.After == nil || r.After.ID == "" {
		return nil, fmt.Errorf("%w: frigate/reviews without after.id", ErrShape)
	}
	return &r, nil
}

// ParseCount decodes a count topic's payload: a bare non-negative
// integer.
func ParseCount(payload []byte) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(string(payload)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: count %q", ErrShape, payload)
	}
	return n, nil
}
