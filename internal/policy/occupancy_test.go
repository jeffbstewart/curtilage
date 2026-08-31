package policy

import (
	"fmt"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
)

func carMsg(typ, id, camera string, zones []string, start float64) []byte {
	z := "[]"
	if len(zones) > 0 {
		z = `["` + zones[0] + `"]`
	}
	return []byte(fmt.Sprintf(`{"type":%q,"after":{"id":%q,"camera":%q,"label":"car","start_time":%f,"end_time":null,"entered_zones":%s,"current_zones":%s,"has_snapshot":true,"has_clip":true,"false_positive":false}}`,
		typ, id, camera, start, z, z))
}

func TestOccupancyRules(t *testing.T) {
	o := NewOccupancy([]Watch{{Zone: "side_parking", ArriveAfter: time.Minute, DepartAfter: 5 * time.Minute}})
	t0 := time.Date(2026, 8, 30, 22, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	obs := func(s float64, payload []byte) []Change {
		topic := "frigate/porch-east/status/detect"
		if payload == nil {
			payload = []byte("ON")
		} else {
			topic = "frigate/events"
		}
		return o.Observe(at(s), topic, payload)
	}

	// A car enters the zone: nothing until it has dwelt a minute.
	if c := obs(0, carMsg("new", "c1", "driveway-corner", []string{"side_parking"}, 0)); c != nil {
		t.Fatalf("instant arrival: %+v", c)
	}
	if c := obs(30, nil); c != nil {
		t.Fatalf("at 30s: %+v", c)
	}
	c := obs(61, nil)
	if len(c) != 1 || c[0].Event.Kind != KindArrival || Describe(c[0].Event) != "A car arrived in the side parking" {
		t.Fatalf("dwell arrival: %+v", c)
	}
	if st := o.Presence(); st[0].Count != 1 || st[0].Zone != "side_parking" {
		t.Fatalf("presence: %+v", st)
	}

	// The same car seen by the second camera: max, not sum -- silence.
	obs(70, carMsg("new", "c1b", "driveway-winchester", []string{"side_parking"}, 70))
	if c := obs(140, nil); c != nil {
		t.Fatalf("second camera counted as second car: %+v", c)
	}

	// A second car on the SAME camera: two, after its dwell.
	obs(200, carMsg("new", "c2", "driveway-corner", []string{"side_parking"}, 200))
	c = obs(261, nil)
	if len(c) != 1 || c[0].Event.Kind != KindArrival || c[0].Event.Objects["car"] != 2 || Describe(c[0].Event) != "A car arrived in the side parking (2 present)" {
		t.Fatalf("second arrival: %+v", c)
	}

	// A one-second flap never arrives.
	obs(300, carMsg("new", "flap", "driveway-corner", []string{"side_parking"}, 300))
	obs(301, carMsg("end", "flap", "driveway-corner", []string{"side_parking"}, 300))
	if c := obs(400, nil); c != nil {
		t.Fatalf("flap: %+v", c)
	}

	// One car's tracks end; the dip must last the grace before the
	// departure is believed -- and a re-sighting cancels it.
	obs(500, carMsg("end", "c2", "driveway-corner", nil, 200))
	if c := obs(700, nil); c != nil {
		t.Fatalf("departure before grace: %+v", c)
	}
	obs(710, carMsg("new", "c2again", "driveway-corner", []string{"side_parking"}, 710))
	if c := obs(900, nil); c != nil {
		t.Fatalf("re-sighting should cancel the dip: %+v", c)
	}
	// (c2again also qualifies at 770 -- but present is already 2, so
	// no arrival fires: re-acquisition is not news.)

	// Now both corner tracks end; winchester still sees one car, so
	// the ledger settles to 1, not 0.
	obs(1000, carMsg("end", "c1", "driveway-corner", nil, 0))
	obs(1000, carMsg("end", "c2again", "driveway-corner", nil, 710))
	c = obs(1301, nil)
	if len(c) != 1 || c[0].Event.Kind != KindDeparture || c[0].Event.Objects["car"] != 1 || Describe(c[0].Event) != "A car left the side parking (1 remains)" {
		t.Fatalf("departure to 1: %+v", c)
	}
	// The last sighting ends too: empty after another grace.
	obs(1400, carMsg("end", "c1b", "driveway-winchester", nil, 70))
	c = obs(1701, nil)
	if len(c) != 1 || c[0].Event.Kind != KindDeparture || Describe(c[0].Event) != "A car left the side parking (empty now)" {
		t.Fatalf("departure to empty: %+v", c)
	}
	if st := o.Presence(); st[0].Count != 0 {
		t.Fatalf("presence after: %+v", st)
	}
}

// The red car leaving the garage: its 6.5-hour right_parking_space
// track ends at 22:20; the 2-minute maneuver track must not fake an
// extra arrival, and the departure fires after the grace.  With that
// day's polygons the side lot never sees the car arrive -- the
// geometry gap this fixture documents.
func TestOccupancyRedCar(t *testing.T) {
	o := NewOccupancy([]Watch{
		{Zone: "right_parking_space"}, {Zone: "left_parking_space"}, {Zone: "side_parking"},
	})
	var got []Change
	for _, r := range fixture.Records(t, fixture.RedCar) {
		got = append(got, o.Observe(r.GetReceivedAt().AsTime(), r.GetTopic(), r.GetPayload())...)
	}
	for _, c := range got {
		t.Logf("%s %s", c.Event.StartedAt.Format("15:04:05"), Describe(c.Event))
	}
	// The window opens with cars already parked, so the ledger COLD
	// STARTS: it discovers the minivan and the garage car and emits
	// their "arrivals" as it learns of them, within the first dwell
	// minutes.  Those are the ledger booting, not news judgment; the
	// assertions that matter are what happens after it is warm.
	warm := time.Date(2026, 8, 30, 22, 10, 0, 0, time.UTC)
	var deps, lateArrs int
	for _, c := range got {
		switch c.Event.Kind {
		case KindDeparture:
			deps++
			if c.Event.Zones[0] != "right_parking_space" {
				t.Errorf("departure from %v, want right_parking_space", c.Event.Zones)
			}
			if c.Event.StartedAt.Before(time.Date(2026, 8, 30, 22, 25, 0, 0, time.UTC)) {
				t.Errorf("departure at %s, before the grace could have elapsed", c.Event.StartedAt.Format("15:04:05"))
			}
		case KindArrival:
			if c.Event.StartedAt.After(warm) {
				lateArrs++
			}
		}
	}
	if deps != 1 {
		t.Errorf("%d departures, want exactly 1 (the red car leaving the garage)", deps)
	}
	if lateArrs != 0 {
		t.Errorf("%d arrivals after warm-up, want 0: the maneuver must not fake one, and the side lot could not see the parked car that day", lateArrs)
	}
	// Presence agrees: the garage right space is empty.
	for _, st := range o.Presence() {
		if st.Zone == "right_parking_space" && st.Count != 0 {
			t.Errorf("right_parking_space count %d after the car left", st.Count)
		}
	}
}

// The morning arrival: a car takes the side lot at 11:30, seen by
// both driveway cameras at once -- one arrival, count 1.
func TestOccupancySideArrival(t *testing.T) {
	o := NewOccupancy([]Watch{{Zone: "side_parking"}})
	var got []Change
	for _, r := range fixture.Records(t, fixture.SideArrival) {
		got = append(got, o.Observe(r.GetReceivedAt().AsTime(), r.GetTopic(), r.GetPayload())...)
	}
	for _, c := range got {
		t.Logf("%s %s (count %d)", c.Event.StartedAt.Format("15:04:05"), Describe(c.Event), c.Event.Objects["car"])
	}
	if len(got) == 0 {
		t.Fatal("no transitions in the arrival window")
	}
	first := got[0].Event
	if first.Kind != KindArrival || first.Objects["car"] != 1 || first.Zones[0] != "side_parking" {
		t.Errorf("first transition = %s %+v", Describe(first), first.Objects)
	}
	if !first.HasSnapshot && first.SourceID == "" {
		t.Errorf("arrival has no evidence: %+v", first)
	}
}
