package frigate

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
)

func TestParseTopic(t *testing.T) {
	cases := map[string]Topic{
		"frigate/events":                                          {Kind: KindEvents},
		"frigate/reviews":                                         {Kind: KindReviews},
		"frigate/available":                                       {Kind: KindAvailable},
		"frigate/porch-east/car":                                  {Kind: KindCount, Camera: "porch-east", Label: "car"},
		"frigate/porch-east/all":                                  {Kind: KindCount, Camera: "porch-east", Label: "all"},
		"frigate/driveway-corner/pad/car":                         {Kind: KindCount, Camera: "driveway-corner", Zone: "pad", Label: "car"},
		"frigate/porch-east/all/active":                           {},
		"frigate/porch-east/car/snapshot":                         {},
		"frigate/garage/classification/bmw_garage_door":           {Kind: KindClassification, Camera: "garage", Model: "bmw_garage_door"},
		"frigate/driveway-winchester/classification/bins_at_curb": {Kind: KindClassification, Camera: "driveway-winchester", Model: "bins_at_curb"},
		"frigate/garage/classification/Sienna 2025":               {Kind: KindClassification, Camera: "garage", Model: "Sienna 2025"},
		"frigate/garage/classification/too/deep":                  {},
		"frigate/porch-east/motion":                               {},
		"frigate/porch-east/status/detect":                        {},
		"frigate/stats":                                           {},
		"frigate/porch-east/classification":                       {},
		"zigbee2mqtt/porch_light":                                 {},
		"frigate":                                                 {},
	}
	for topic, want := range cases {
		if got := ParseTopic(topic); got != want {
			t.Errorf("ParseTopic(%q) = %+v, want %+v", topic, got, want)
		}
	}
}

func TestSubLabelShapes(t *testing.T) {
	cases := map[string]SubLabel{
		`null`:                   "",
		`"blue minivan"`:         "blue minivan",
		`["blue minivan", 0.87]`: "blue minivan",
	}
	for in, want := range cases {
		var got SubLabel
		if err := json.Unmarshal([]byte(in), &got); err != nil || got != want {
			t.Errorf("SubLabel %s = %q, %v; want %q", in, got, err, want)
		}
	}
	var bad SubLabel
	if err := json.Unmarshal([]byte(`42`), &bad); err == nil {
		t.Error("SubLabel accepted a number")
	}
}

func TestSecondsTime(t *testing.T) {
	if !Seconds(0).Time().IsZero() {
		t.Error("null time must be the zero Time")
	}
	got := Seconds(1788049526.709964).Time()
	want := time.Date(2026, 8, 30, 0, 25, 26, 709964000, time.UTC)
	if d := got.Sub(want); d > time.Microsecond || d < -time.Microsecond {
		t.Errorf("Seconds.Time() = %v, want %v", got, want)
	}
}

func TestParseCount(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "1": 1, "12\n": 12} {
		if got, err := ParseCount([]byte(in)); err != nil || got != want {
			t.Errorf("ParseCount(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-1", "one", `{"a":1}`} {
		if _, err := ParseCount([]byte(in)); err == nil {
			t.Errorf("ParseCount(%q) accepted", in)
		}
	}
}

func TestParseEventShape(t *testing.T) {
	for _, in := range []string{
		`not json`,
		`{"type":"weird","after":{"id":"x","camera":"c"}}`,
		`{"type":"new"}`,
		`{"type":"new","after":{"id":"","camera":"c"}}`,
	} {
		if _, err := ParseEvent([]byte(in)); err == nil {
			t.Errorf("ParseEvent(%s) accepted", in)
		}
	}
}

// Every message in the real recording must decode, and the decoded
// fields must look like what the design relies on.
func TestFixtureDecodes(t *testing.T) {
	var events, reviews, counts, other int
	types := map[Change]int{}
	ids := map[string]bool{}
	var withZones, withSnapshot int
	for _, r := range fixture.Records(t, fixture.Driveway) {
		top := ParseTopic(r.GetTopic())
		switch top.Kind {
		case KindEvents:
			e, err := ParseEvent(r.GetPayload())
			if err != nil {
				t.Fatalf("%s: %v", r.GetTopic(), err)
			}
			events++
			types[e.Type]++
			ids[e.After.ID] = true
			if e.After.StartTime.Time().IsZero() {
				t.Errorf("event %s has no start_time", e.After.ID)
			}
			if e.Type == End && e.After.EndTime.Time().IsZero() {
				t.Errorf("end of %s has no end_time", e.After.ID)
			}
			if len(e.After.EnteredZones) > 0 {
				withZones++
			}
			if e.After.HasSnapshot {
				withSnapshot++
			}
		case KindReviews:
			rv, err := ParseReview(r.GetPayload())
			if err != nil {
				t.Fatalf("%s: %v", r.GetTopic(), err)
			}
			reviews++
			if rv.After.Severity != "alert" && rv.After.Severity != "detection" {
				t.Errorf("review %s severity %q", rv.After.ID, rv.After.Severity)
			}
			if len(rv.After.Data.Detections) == 0 {
				t.Errorf("review %s groups no events", rv.After.ID)
			}
		case KindCount:
			if _, err := ParseCount(r.GetPayload()); err != nil {
				t.Fatalf("%s: %v", r.GetTopic(), err)
			}
			counts++
		default:
			other++
		}
	}
	t.Logf("events=%d (%v) distinct objects=%d with zones=%d with snapshot=%d; reviews=%d counts=%d other=%d",
		events, types, len(ids), withZones, withSnapshot, reviews, counts, other)
	if events == 0 || reviews == 0 || counts == 0 {
		t.Fatal("fixture is missing a kind of message")
	}
	if types[New] == 0 || types[Update] == 0 || types[End] == 0 {
		t.Errorf("fixture lacks one of new/update/end: %v", types)
	}
	if other != 2 { // the two frigate/available messages
		t.Errorf("unclassified records = %d, want 2 (frigate/available)", other)
	}
}
