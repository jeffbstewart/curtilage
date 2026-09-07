package policy

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
)

// run feeds a fixture through an engine and returns every change, and
// the final state of every event.
func run(t *testing.T, eng Engine, name string) ([]Change, map[string]Event) {
	t.Helper()
	var all []Change
	final := map[string]Event{}
	for _, r := range fixture.Records(t, name) {
		for _, c := range eng.Observe(r.GetReceivedAt().AsTime(), r.GetTopic(), r.GetPayload()) {
			all = append(all, c)
			final[c.Event.ID] = c.Event
		}
	}
	return all, final
}

func activities(final map[string]Event) []Event {
	var out []Event
	for _, e := range final {
		if e.Kind == KindActivity {
			out = append(out, e)
		}
	}
	return out
}

// A person on the porch and a dog in the fenced back yard at the same
// moment are two events, not one walk that teleports: the walkable
// graph keeps disconnected cameras in separate, CONCURRENT incidents.
func TestDisconnectedCamerasSplitIncidents(t *testing.T) {
	msg := func(id, cam, label, zone string) []byte {
		return []byte(fmt.Sprintf(`{"type":"new","after":{"id":%q,"camera":%q,"label":%q,"start_time":0,"end_time":null,`+
			`"entered_zones":[%q],"current_zones":[%q],"has_snapshot":true,"has_clip":true,"false_positive":false}}`,
			id, cam, label, zone, zone))
	}
	e := NewIncidents(IncidentConfig{Adjacent: map[string][]string{
		"porch-east": {"porch-down"}, "backyard-gate": {"deck"},
	}})
	t0 := time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC)

	c1 := e.Observe(t0, "frigate/events", msg("p1", "porch-east", "person", "porch"))
	if len(c1) != 1 || c1[0].Op != OpStarted {
		t.Fatalf("person: %+v", c1)
	}
	porch := c1[0].Event.ID
	// The dog is in the back yard, unreachable from the porch: its own
	// incident, concurrently.
	c2 := e.Observe(t0.Add(5*time.Second), "frigate/events", msg("d1", "backyard-gate", "dog", "yard"))
	if len(c2) != 1 || c2[0].Op != OpStarted || c2[0].Event.ID == porch {
		t.Fatalf("dog should open its own incident: %+v", c2)
	}
	yard := c2[0].Event.ID
	if Describe(c2[0].Event) != "A dog in the yard" {
		t.Errorf("dog sentence: %q", Describe(c2[0].Event))
	}
	// An adjoining camera joins the porch incident...
	c3 := e.Observe(t0.Add(10*time.Second), "frigate/events", msg("p2", "porch-down", "person", "porch"))
	if len(c3) != 1 || c3[0].Op != OpUpdated || c3[0].Event.ID != porch {
		t.Fatalf("adjoining camera: %+v", c3)
	}
	// ...and a camera the graph never heard of folds rather than
	// isolates (joins the OLDEST reachable incident).
	c4 := e.Observe(t0.Add(15*time.Second), "frigate/events", msg("p3", "garage", "person", "driveway"))
	if len(c4) != 1 || c4[0].Event.ID != porch {
		t.Fatalf("unknown camera: %+v", c4)
	}
	// The gap closes both, separately.
	ended := e.Observe(t0.Add(2*time.Minute), "frigate/x/status/detect", []byte("ON"))
	got := map[string]Kind{}
	for _, c := range ended {
		if c.Op != OpEnded {
			t.Fatalf("close: %+v", ended)
		}
		got[c.Event.ID] = c.Event.Kind
	}
	if len(got) != 2 || got[porch] != KindActivity || got[yard] != KindActivity {
		t.Fatalf("ended: %+v", got)
	}
}

// Box evidence rides the ended activity: each message's box is kept
// (camera, time, pixels) so the follow view can score cameras, and
// intermediate revisions stay light.
func TestActivityCarriesBoxes(t *testing.T) {
	e := NewIncidents(IncidentConfig{})
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	msg := func(typ, id, cam, box string) []byte {
		return []byte(fmt.Sprintf(`{"type":%q,"after":{"id":%q,"camera":%q,"label":"person","start_time":%d,"end_time":null,`+
			`"entered_zones":["porch"],"current_zones":["porch"],"has_snapshot":true,"has_clip":true,"false_positive":false,"box":%s}}`,
			typ, id, cam, t0.Unix(), box))
	}
	c1 := e.Observe(t0, "frigate/events", msg("new", "p1", "porch-down", "[10,10,110,210]"))
	if len(c1) != 1 || len(c1[0].Event.Boxes) != 0 {
		t.Fatalf("started change carries boxes: %+v", c1)
	}
	e.Observe(t0.Add(5*time.Second), "frigate/events", msg("update", "p1", "porch-down", "[20,10,140,220]"))
	e.Observe(t0.Add(10*time.Second), "frigate/events", msg("end", "p1", "porch-down", "[30,10,150,220]"))
	ended := e.Observe(t0.Add(2*time.Minute), "frigate/x/status/detect", []byte("ON"))
	if len(ended) != 1 || ended[0].Op != OpEnded {
		t.Fatalf("end: %+v", ended)
	}
	b := ended[0].Event.Boxes
	if len(b) != 3 || b[0].Camera != "porch-down" || b[1].At != t0.Add(5*time.Second) ||
		b[2].Box != [4]float32{30, 10, 150, 220} {
		t.Fatalf("boxes: %+v", b)
	}
}

// At the cap the sample set thins -- every second sample dropped --
// so a long incident keeps whole-timeline coverage.
func TestBoxSamplesThinAtTheCap(t *testing.T) {
	inc := &incident{}
	t0 := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for i := range maxBoxSamples + 1 {
		inc.addBox("cam", t0.Add(time.Duration(i)*time.Second), []float64{0, 0, 1, 1})
	}
	if n := len(inc.boxes); n != maxBoxSamples/2+1 {
		t.Fatalf("%d samples after thinning, want %d", n, maxBoxSamples/2+1)
	}
	if !inc.boxes[0].At.Equal(t0) || !inc.boxes[len(inc.boxes)-1].At.Equal(t0.Add(maxBoxSamples*time.Second)) {
		t.Fatalf("coverage lost: %v .. %v", inc.boxes[0].At, inc.boxes[len(inc.boxes)-1].At)
	}
}

// The dog walk out: 26 Frigate objects across six cameras become one
// activity that ends, with the right people, dog, and path.
func TestWalkOutIsOneActivity(t *testing.T) {
	eng := NewIncidents(IncidentConfig{})
	changes, final := run(t, eng, fixture.WalkOut)
	acts := activities(final)
	if len(acts) != 1 {
		for _, a := range acts {
			t.Logf("activity: %s", Describe(a))
		}
		t.Fatalf("%d activities, want 1", len(acts))
	}
	a := acts[0]
	t.Logf("final: %s | objects=%v cameras=%v sources=%d", Describe(a), a.Objects, a.Cameras, len(a.SourceIDs))
	if a.Running() {
		t.Error("the walk never ended")
	}
	// Two people: porch-north saw one in the yard a second before
	// porch-west saw the other step onto the porch, so the recorded
	// order is yard, porch.  What is stable: those two first, the
	// driveway last.
	if len(a.Path) != 3 || a.Path[2] != "driveway" || !slices.Contains(a.Path[:2], "porch") || !slices.Contains(a.Path[:2], "yard") {
		t.Errorf("path = %v, want {porch, yard} then driveway", a.Path)
	}
	if a.Objects["person"] != 2 || a.Objects["dog"] != 1 {
		t.Errorf("objects = %v, want 2 persons and a dog", a.Objects)
	}
	// The first camera is the first ZONED sighting (porch-north's yard,
	// 16:04:51); porch-west saw an unzoned person two seconds earlier
	// and rides along without anchoring.
	if len(a.Cameras) < 5 || a.Camera != "porch-north" {
		t.Errorf("cameras = %v (first %s)", a.Cameras, a.Camera)
	}
	if !a.HasSnapshot || a.Clip != ClipFinal || a.Label != "person" {
		t.Errorf("snapshot=%v clip=%v label=%s", a.HasSnapshot, a.Clip, a.Label)
	}
	if d := a.EndedAt.Sub(a.StartedAt); d < 60*time.Second || d > 3*time.Minute {
		t.Errorf("duration %v", d)
	}
	// It evolved: one Started, some Updated, one Ended, all the same id;
	// the sentence at each step is a refinement of the last.
	var ops []Op
	var texts []string
	for _, c := range changes {
		if c.Event.Kind != KindActivity {
			continue
		}
		ops = append(ops, c.Op)
		texts = append(texts, Describe(c.Event))
		if c.Event.ID != a.ID {
			t.Errorf("a second activity id in the walk: %s", c.Event.ID)
		}
	}
	if ops[0] != OpStarted || ops[len(ops)-1] != OpEnded || len(ops) < 4 {
		t.Errorf("ops = %v", ops)
	}
	for _, tx := range texts {
		t.Log(tx)
	}
	if texts[0] == texts[len(texts)-1] {
		t.Error("the sentence never evolved")
	}
	// The one-second car misclassifications during the walk are plain
	// detections, not news.
	for _, e := range final {
		if e.Kind == KindDetection && e.Label != "car" {
			t.Errorf("a %s leaked through as a detection: %+v", e.Label, e)
		}
	}
}

func TestWalkBackReversesThePath(t *testing.T) {
	_, final := run(t, NewIncidents(IncidentConfig{}), fixture.WalkBack)
	acts := activities(final)
	if len(acts) != 1 {
		t.Fatalf("%d activities, want 1", len(acts))
	}
	a := acts[0]
	t.Logf("final: %s | objects=%v cameras=%v", Describe(a), a.Objects, a.Cameras)
	if !slices.Equal(a.Path, []string{"driveway", "yard", "porch"}) {
		t.Errorf("path = %v, want driveway -> yard -> porch", a.Path)
	}
	if a.Objects["dog"] != 1 || a.Objects["person"] < 1 || a.Running() {
		t.Errorf("objects=%v running=%v", a.Objects, a.Running())
	}
}

// Two walks 22 minutes apart, fed through one engine, are two
// activities; the two hours of driveway traffic before them produce
// none (nothing in a named zone was a person or a dog).
func TestSeparateWalksAndQuietMorning(t *testing.T) {
	eng := NewIncidents(IncidentConfig{})
	_, morning := run(t, eng, fixture.Driveway)
	if n := len(activities(morning)); n != 0 {
		for _, a := range activities(morning) {
			t.Logf("morning activity: %s cameras=%v", Describe(a), a.Cameras)
		}
		t.Errorf("morning: %d activities, want 0", n)
	}
	_, out := run(t, eng, fixture.WalkOut)
	_, back := run(t, eng, fixture.WalkBack)
	ids := map[string]bool{}
	for _, a := range activities(out) {
		ids[a.ID] = true
	}
	for _, a := range activities(back) {
		ids[a.ID] = true
	}
	if len(ids) != 2 {
		t.Errorf("%d distinct activities across both walks, want 2", len(ids))
	}
}

// The solo walk: a parked car misclassified as a person (unzoned,
// with a snapshot, 40 s before the real walk) folds into the activity
// but must not anchor it.
func TestSoloWalkAnchorsOnZonedMembers(t *testing.T) {
	_, final := run(t, NewIncidents(IncidentConfig{}), fixture.WalkSolo)
	acts := activities(final)
	if len(acts) != 1 {
		t.Fatalf("%d activities, want 1", len(acts))
	}
	a := acts[0]
	t.Logf("final: %s | started=%s camera=%s snap-src=%s cameras=%v", Describe(a), a.StartedAt.Format("15:04:05"), a.Camera, a.SourceID, a.Cameras)
	const misclassified = "1788116806.144034-ov0cf6" // driveway-winchester "person", the parked car
	if a.SourceID == misclassified {
		t.Error("the parked car owns the snapshot")
	}
	if a.Camera == "driveway-winchester" {
		t.Errorf("the parked car's camera leads: %v", a.Cameras)
	}
	// The walk's public start is the first zoned sighting (19:07:29,
	// porch-north), not the misclassification at 19:06:46.
	if a.StartedAt.Before(time.Date(2026, 8, 30, 19, 7, 20, 0, time.UTC)) {
		t.Errorf("started at %s, anchored by the misclassification", a.StartedAt.Format("15:04:05"))
	}
	if !slices.Contains(a.SourceIDs, misclassified) {
		t.Error("the misclassification should still be a member (supporting cast)")
	}
	if !a.HasSnapshot || a.Objects["person"] < 1 {
		t.Errorf("snapshot=%v objects=%v", a.HasSnapshot, a.Objects)
	}
	// One span per member, each with a camera and a start; ended
	// members have ends.
	if len(a.Spans) != len(a.SourceIDs) {
		t.Errorf("%d spans for %d members", len(a.Spans), len(a.SourceIDs))
	}
	for _, sp := range a.Spans {
		if sp.Camera == "" || sp.Label == "" || sp.Start.IsZero() {
			t.Errorf("bad span %+v", sp)
		}
	}
	// This fixture predates the disjoint porch/yard geometry, so the
	// path still opens with yard; the geometry fix owns that half.
	if len(a.Path) == 0 || a.Path[len(a.Path)-1] != "driveway" {
		t.Errorf("path = %v", a.Path)
	}
}

func msgAt(typ, id, camera, label string, zones []string, start float64) []byte {
	z := "[]"
	if len(zones) > 0 {
		z = `["` + zones[0] + `"]`
	}
	return []byte(fmt.Sprintf(`{"type":%q,"after":{"id":%q,"camera":%q,"label":%q,"start_time":%f,"end_time":null,"entered_zones":%s,"current_zones":%s,"has_snapshot":true,"has_clip":true,"false_positive":false}}`,
		typ, id, camera, label, start, z, z))
}

func TestIncidentRules(t *testing.T) {
	eng := NewIncidents(IncidentConfig{Gap: 60 * time.Second, Overlap: 3 * time.Second})
	t0 := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	unix := func(s float64) float64 { return float64(t0.Unix()) + s }

	// A person with no zone: nothing yet.
	if c := eng.Observe(at(0), "frigate/events", msgAt("new", "p1", "porch-west", "person", nil, unix(0))); c != nil {
		t.Fatalf("unzoned person -> %+v", c)
	}
	// A zone appears: Started, one person on the porch.
	c := eng.Observe(at(2), "frigate/events", msgAt("update", "p1", "porch-west", "person", []string{"porch"}, unix(0)))
	if len(c) != 1 || c[0].Op != OpStarted || Describe(c[0].Event) != "Person on the porch" {
		t.Fatalf("zoned -> %+v", c)
	}
	// Same person on another camera: a camera update, still one person.
	c = eng.Observe(at(10), "frigate/events", msgAt("new", "p2", "porch-east", "person", []string{"yard"}, unix(10)))
	if len(c) != 1 || c[0].Op != OpUpdated || c[0].Event.Objects["person"] != 1 || Describe(c[0].Event) != "Person started on the porch, moved to the yard" {
		t.Fatalf("second camera -> %+v %s", c, Describe(c[0].Event))
	}
	// A second person on porch-east, overlapping p2 for 5 s: two persons.
	eng.Observe(at(11), "frigate/events", msgAt("new", "p3", "porch-east", "person", []string{"yard"}, unix(11)))
	c = eng.Observe(at(16), "frigate/events", msgAt("update", "p3", "porch-east", "person", []string{"yard"}, unix(11)))
	if len(c) != 1 || c[0].Event.Objects["person"] != 2 {
		t.Fatalf("overlap -> %+v", c)
	}
	// A dog: the sentence grows.
	c = eng.Observe(at(20), "frigate/events", msgAt("new", "d1", "porch-east", "dog", []string{"yard"}, unix(20)))
	if len(c) != 1 || Describe(c[0].Event) != "2 persons and a dog started on the porch, moved to the yard" {
		t.Fatalf("dog -> %s", Describe(c[0].Event))
	}
	// A car during the walk is a detection, not part of it.
	c = eng.Observe(at(21), "frigate/events", msgAt("new", "c1", "porch-north", "car", nil, unix(21)))
	if len(c) != 1 || c[0].Event.Kind != KindDetection {
		t.Fatalf("car -> %+v", c)
	}
	// Quiet for the gap: a heartbeat on any topic ends it.
	c = eng.Observe(at(90), "frigate/porch-east/status/detect", []byte("ON"))
	if len(c) != 1 || c[0].Op != OpEnded || c[0].Event.Running() {
		t.Fatalf("gap -> %+v", c)
	}
	// A straggling update for a member of the ended incident is ignored;
	// a new person starts a new incident with a new id.
	if c := eng.Observe(at(95), "frigate/events", msgAt("end", "p1", "porch-west", "person", []string{"porch"}, unix(0))); c != nil {
		t.Errorf("straggler -> %+v", c)
	}
	c = eng.Observe(at(100), "frigate/events", msgAt("new", "p9", "porch-west", "person", []string{"porch"}, unix(100)))
	if len(c) != 1 || c[0].Op != OpStarted || c[0].Event.ID == MintID("activity/p1") {
		t.Fatalf("new incident -> %+v", c)
	}
}
