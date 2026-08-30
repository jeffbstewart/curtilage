// Package fixture hands tests the recorded driveway traffic under
// testdata/: real Frigate messages, so parsers and the policy engine
// are tuned to what the household's cameras actually say
// (docs/DESIGN.md "Instrument before tuning").
//
// Fixtures are cut from a day's recording with `curtilage trim`; the
// command that made each one is in its entry below, so it can be
// re-cut from a newer day the same way.
package fixture

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/jeffbstewart/curtilage/internal/record"
)

// Driveway is two morning hours (2026-08-30 11:00-13:00 UTC), the
// busiest of that day: frigate/events, frigate/reviews, the per-camera
// and per-zone object counts, and frigate/available.  No garage (an
// interior view), no status heartbeats, no motion, no snapshots.
//
//	curtilage trim -file curtilage-20260830T002443Z.mcap \
//	  -out internal/fixture/testdata/driveway-20260830T11.mcap \
//	  -from 2026-08-30T11:00:00Z -to 2026-08-30T13:00:00Z \
//	  -keep '^frigate/(events|reviews|available)$|^frigate/[^/]+(/[^/]+)?/(car|person|package|dog|cat|bicycle|all)$' \
//	  -drop '^frigate/garage/'
const Driveway = "driveway-20260830T11.mcap"

// WalkOut and WalkBack are the household leaving for a dog walk
// (2026-08-30 16:04:49-16:06:14 UTC: porch-west -> porch-down/north ->
// porch-east -> driveway-corner -> driveway-winchester/fence; two
// people and a dog, 26 Frigate objects) and coming back 22 minutes
// later in reverse.  Each is ONE thing that happened; the engine's job
// is to say so.  Same topics as Driveway, cut from the same day:
//
//	curtilage trim -file curtilage-20260830T125953Z.mcap \
//	  -out internal/fixture/testdata/walk-out-20260830T1604.mcap \
//	  -from 2026-08-30T16:03:30Z -to 2026-08-30T16:08:00Z -keep '<as Driveway>'
//	curtilage trim ... -out walk-back-20260830T1627.mcap \
//	  -from 2026-08-30T16:27:30Z -to 2026-08-30T16:31:00Z ...
const (
	WalkOut  = "walk-out-20260830T1604.mcap"
	WalkBack = "walk-back-20260830T1627.mcap"
)

// Path is the on-disk path of a fixture.
func Path(t testing.TB, name string) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixture: cannot locate testdata")
	}
	return filepath.Join(filepath.Dir(here), "testdata", name)
}

// Records reads a whole fixture, in log-time order.
func Records(t testing.TB, name string) []*record.Record {
	t.Helper()
	recs, err := record.ReadAll(context.Background(), Path(t, name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return recs
}
