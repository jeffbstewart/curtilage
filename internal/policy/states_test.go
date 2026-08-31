package policy

import (
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
)

func TestStatesRules(t *testing.T) {
	s := NewStates([]StateModel{{
		Model: "bmw_garage_door", Hold: 20 * time.Second, News: true,
		AlarmClass: "bmw_garage_door_open", AlarmAfter: time.Hour,
	}})
	t0 := time.Date(2026, 8, 31, 13, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	class := func(sec float64, c string) []Change {
		return s.Observe(at(sec), "frigate/garage/classification/bmw_garage_door", []byte(c))
	}
	tick := func(sec float64) []Change {
		return s.Observe(at(sec), "frigate/garage/status/detect", []byte("ON"))
	}

	// Cold start adopts the first value silently.
	if c := class(0, "gmw_garage_door_closed"); c != nil {
		t.Fatalf("cold start -> %+v", c)
	}
	// A change is raw immediately, believed only after the hold.
	if c := class(10, "bmw_garage_door_open"); c != nil {
		t.Fatalf("instant belief: %+v", c)
	}
	st := s.Snapshot()[0]
	if st.Held != "gmw_garage_door_closed" || st.Raw != "bmw_garage_door_open" {
		t.Fatalf("snapshot mid-hold: %+v", st)
	}
	c := tick(31)
	if len(c) != 1 || c[0].Event.Kind != KindState || Describe(c[0].Event) != "Bmw garage door: open" {
		t.Fatalf("believed open: %+v", c)
	}
	// A flap shorter than the hold never surfaces.
	class(60, "gmw_garage_door_closed")
	class(65, "bmw_garage_door_open")
	if c := tick(120); c != nil {
		t.Fatalf("flap believed: %+v", c)
	}
	// Left open an hour: one alarm, duration included; no repeat.
	c = tick(3611) // held open since 10s
	if len(c) != 1 || Describe(c[0].Event) != "Bmw garage door: open for 1h0m0s" {
		t.Fatalf("alarm: %+v (%s)", c, Describe(c[0].Event))
	}
	if c := tick(4000); c != nil {
		t.Fatalf("alarm repeated: %+v", c)
	}
	// Close and reopen: the alarm re-arms.
	class(5000, "gmw_garage_door_closed")
	// The typo'd class name cannot be prefix-stripped: shown whole.
	if c := tick(5021); len(c) != 1 || Describe(c[0].Event) != "Bmw garage door: gmw garage door closed" {
		t.Fatalf("believed close: %+v", c)
	}
	class(6000, "bmw_garage_door_open")
	tick(6021)
	c = tick(6000 + 3601)
	if len(c) != 1 || Describe(c[0].Event) != "Bmw garage door: open for 1h0m0s" {
		t.Fatalf("re-armed alarm: %+v", c)
	}
}

// The real day: lunch's genuine transitions are believed (and the
// door's are news); the landscapers' lies -- a 35 s fake car and a
// 4-minute fake bins absence -- never are.
func TestStatesOnTheRealDay(t *testing.T) {
	s := NewStates([]StateModel{
		{Model: "bmw_garage_door", Hold: 20 * time.Second, News: true},
		{Model: "bmw_ix_presence", Hold: 10 * time.Minute},
		{Model: "sienna_2020_presence", Hold: 10 * time.Minute},
		{Model: "bins_at_curb", Hold: 15 * time.Minute},
	})
	var got []Change
	for _, name := range []string{fixture.ClassifiersLunch, fixture.ClassifiersLandscapers} {
		for _, r := range fixture.Records(t, name) {
			got = append(got, s.Observe(r.GetReceivedAt().AsTime(), r.GetTopic(), r.GetPayload())...)
		}
	}
	for _, c := range got {
		t.Logf("%s %s", c.Event.StartedAt.Format("15:04:05"), Describe(c.Event))
	}
	// Exactly the door's four believed transitions are events: the
	// lunch departure sandwich and the return one.
	if len(got) != 4 {
		t.Fatalf("%d events, want the door's 4", len(got))
	}
	// The model's closed class is misspelled "gmw_...", so Humanize
	// cannot strip the model prefix and honestly shows the whole
	// token.  The fixture is frozen history; when the class is
	// renamed, new recordings will read "closed".
	want := []string{"open", "gmw garage door closed", "open", "gmw garage door closed"}
	for i, c := range got {
		if c.Event.Label != "bmw_garage_door" || Humanize(c.Event.Label, c.Event.SubLabel) != want[i] {
			t.Errorf("event %d = %s (%s)", i, Describe(c.Event), c.Event.SubLabel)
		}
	}
	// The believed end-state is the truth of the world at 15:25.
	held := map[string]string{}
	for _, m := range s.Snapshot() {
		held[m.Model] = m.Held
	}
	for model, wantHeld := range map[string]string{
		"bmw_garage_door":      "gmw_garage_door_closed",
		"bmw_ix_presence":      "bmw_ix_present",
		"sienna_2020_presence": "sienna_2020_not_present",
		"bins_at_curb":         "bins_at_curb",
	} {
		if held[model] != wantHeld {
			t.Errorf("%s held %q, want %q", model, held[model], wantHeld)
		}
	}
}

func TestHumanize(t *testing.T) {
	cases := []struct{ model, class, want string }{
		{"bmw_garage_door", "bmw_garage_door_open", "open"},
		{"bmw_garage_door", "gmw_garage_door_closed", "gmw garage door closed"}, // the class-name typo, shown as-is
		{"bins_at_curb", "bins_not_at_curb", "bins not at curb"},
		{"bmw_ix_presence", "bmw_ix_present", "bmw ix present"},
	}
	for _, c := range cases {
		if got := Humanize(c.model, c.class); got != c.want {
			t.Errorf("Humanize(%s, %s) = %q, want %q", c.model, c.class, got, c.want)
		}
	}
}
