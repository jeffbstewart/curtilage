package policy

import (
	"fmt"
	"testing"
	"time"

	"github.com/jeffbstewart/curtilage/internal/fixture"
)

func TestMintID(t *testing.T) {
	a, b := MintID("1788049526.709964-hbpbil"), MintID("1788049526.709964-hbpbil")
	if a != b {
		t.Fatal("MintID is not deterministic")
	}
	if len(a) != 20 || a == "1788049526.709964-hbpbil" {
		t.Errorf("MintID = %q", a)
	}
	if MintID("x") == MintID("y") {
		t.Error("MintID collides")
	}
}

func msg(typ, id, camera string, zones []string, snapshot, clip bool, end float64) []byte {
	z := "[]"
	if len(zones) > 0 {
		z = "["
		for i, s := range zones {
			if i > 0 {
				z += ","
			}
			z += fmt.Sprintf("%q", s)
		}
		z += "]"
	}
	endJSON := "null"
	if end != 0 {
		endJSON = fmt.Sprintf("%f", end)
	}
	return []byte(fmt.Sprintf(`{"type":%q,"before":null,"after":{"id":%q,"camera":%q,"label":"car","sub_label":null,`+
		`"start_time":1788049526.709964,"end_time":%s,"entered_zones":%s,"current_zones":%s,"has_snapshot":%t,"has_clip":%t,"false_positive":false}}`,
		typ, id, camera, endJSON, z, z, snapshot, clip))
}

func ops(changes []Change) []Op {
	var out []Op
	for _, c := range changes {
		out = append(out, c.Op)
	}
	return out
}

func TestPassthroughLifecycle(t *testing.T) {
	p := NewPassthrough()
	at := time.Date(2026, 8, 30, 0, 25, 27, 0, time.UTC)
	var got []Change

	got = p.Observe(at, "frigate/events", msg("new", "obj1", "driveway-corner", nil, false, false, 0))
	if len(got) != 1 || got[0].Op != OpStarted || got[0].Event.Kind != KindDetection || got[0].Event.Clip != ClipNone {
		t.Fatalf("new -> %+v", got)
	}
	if got[0].Event.ID != MintID("obj1") || got[0].Event.SourceID != "obj1" || !got[0].Event.Running() {
		t.Errorf("started event = %+v", got[0].Event)
	}

	// Same state again (a stationary heartbeat): silence.
	if got = p.Observe(at, "frigate/events", msg("update", "obj1", "driveway-corner", nil, false, false, 0)); got != nil {
		t.Errorf("no-op update -> %+v", got)
	}
	// The snapshot and clip arrive, and a zone: one Updated.
	got = p.Observe(at, "frigate/events", msg("update", "obj1", "driveway-corner", []string{"pad"}, true, true, 0))
	if len(got) != 1 || got[0].Op != OpUpdated || !got[0].Event.HasSnapshot || got[0].Event.Clip != ClipGrowing || len(got[0].Event.Zones) != 1 {
		t.Fatalf("update -> %+v", got)
	}
	// Duplicate new after a reconnect replay: silence.
	if got = p.Observe(at, "frigate/events", msg("new", "obj1", "driveway-corner", []string{"pad"}, true, true, 0)); got != nil {
		t.Errorf("duplicate new -> %+v", got)
	}
	got = p.Observe(at, "frigate/events", msg("end", "obj1", "driveway-corner", []string{"pad"}, true, true, 1788049600))
	if len(got) != 1 || got[0].Op != OpEnded || got[0].Event.Running() || got[0].Event.Clip != ClipFinal {
		t.Fatalf("end -> %+v", got)
	}
	if len(p.Live()) != 0 {
		t.Errorf("still live after end: %+v", p.Live())
	}

	// Other topics and false positives are ignored; garbage is counted.
	if got = p.Observe(at, "frigate/porch-east/car", []byte("1")); got != nil {
		t.Errorf("count topic -> %+v", got)
	}
	if got = p.Observe(at, "frigate/events", []byte(`{"type":"new","after":{"id":"fp","camera":"c","false_positive":true}}`)); got != nil {
		t.Errorf("false positive -> %+v", got)
	}
	if got = p.Observe(at, "frigate/events", []byte("nope")); got != nil || p.Undecodable != 1 {
		t.Errorf("garbage -> %+v, undecodable=%d", got, p.Undecodable)
	}
}

func TestPassthroughJoinsMidStream(t *testing.T) {
	p := NewPassthrough()
	at := time.Now()
	// An update for an object we never saw start: it starts now.
	got := p.Observe(at, "frigate/events", msg("update", "late", "porch-east", nil, true, true, 0))
	if len(got) != 1 || got[0].Op != OpStarted {
		t.Fatalf("late update -> %v", ops(got))
	}
	// An end for an object we never saw at all: started, then ended.
	got = p.Observe(at, "frigate/events", msg("end", "ghost", "porch-east", nil, true, true, 1788049600))
	if len(got) != 2 || got[0].Op != OpStarted || got[1].Op != OpEnded || !got[0].Event.Running() || got[1].Event.Running() {
		t.Fatalf("orphan end -> %v %+v", ops(got), got)
	}
	if got[0].Event.Clip != ClipGrowing || got[1].Event.Clip != ClipFinal {
		t.Errorf("clip states = %v, %v", got[0].Event.Clip, got[1].Event.Clip)
	}
}

// The engine over two real hours: the invariants any engine must hold.
func TestPassthroughOnFixture(t *testing.T) {
	p := NewPassthrough()
	started := map[string]int{}
	ended := map[string]int{}
	var updated int
	for _, r := range fixture.Records(t, fixture.Driveway) {
		for _, c := range p.Observe(r.GetReceivedAt().AsTime(), r.GetTopic(), r.GetPayload()) {
			e := c.Event
			if e.ID == "" || e.Camera == "" || e.Label == "" || e.StartedAt.IsZero() || e.Kind != KindDetection {
				t.Fatalf("malformed change %+v", c)
			}
			switch c.Op {
			case OpStarted:
				started[e.ID]++
				if !e.Running() {
					t.Errorf("started %s already ended", e.SourceID)
				}
			case OpUpdated:
				updated++
				if started[e.ID] == 0 {
					t.Errorf("updated %s before it started", e.SourceID)
				}
			case OpEnded:
				ended[e.ID]++
				if started[e.ID] == 0 || e.Running() || e.EndedAt.Before(e.StartedAt) {
					t.Errorf("bad end %+v", e)
				}
			}
		}
	}
	for id, n := range started {
		if n != 1 {
			t.Errorf("event %s started %d times", id, n)
		}
	}
	for id, n := range ended {
		if n != 1 {
			t.Errorf("event %s ended %d times", id, n)
		}
	}
	t.Logf("started=%d updated=%d ended=%d still live=%d undecodable=%d",
		len(started), updated, len(ended), len(p.Live()), p.Undecodable)
	if p.Undecodable != 0 {
		t.Errorf("%d undecodable messages in the fixture", p.Undecodable)
	}
	if len(started) == 0 || len(ended) == 0 || len(ended) > len(started) {
		t.Fatalf("implausible: started=%d ended=%d", len(started), len(ended))
	}
	if updated > 3*len(started) {
		t.Errorf("too chatty: %d updates for %d events (stationary heartbeats leaking?)", updated, len(started))
	}
}
