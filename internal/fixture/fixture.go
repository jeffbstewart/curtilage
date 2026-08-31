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

// WalkSolo is one person's quick loop (2026-08-30 19:07:26-19:09:33
// UTC), PRECEDED at 19:06:46 by a parked car on driveway-winchester
// misclassified as a person for 33 s, never in any zone, with a
// snapshot.  The engine folds that prelude into the walk (it is
// within the gap), and the fixture pins what must NOT happen: the
// unzoned misclassification owning the event's snapshot, start time,
// or first camera.  Cut as the walks above, window
// 2026-08-30T19:05:00Z .. 19:12:00Z of curtilage-20260830T190448Z.
const WalkSolo = "walk-solo-20260830T1906.mcap"

// RedCar is the red car leaving the garage (2026-08-30 22:20 UTC:
// its 6.5-hour right_parking_space track ends, a 2-minute maneuver
// track follows, a driveway transit at 22:21:42) and parking behind
// the minivan on the side lot -- where, with the polygons of that
// day, it never registered in side_parking (only unzoned blips on
// driveway-corner): the occlusion case the occupancy ledger's
// departure grace exists for.  Window 22:05-22:45 UTC.
const RedCar = "redcar-20260830T2205.mcap"

// SideArrival is the same morning's side-lot arrival (11:30 UTC): a
// car takes side_parking, seen by BOTH driveway-corner and
// driveway-winchester at once -- one car, two cameras, so a correct
// count is max-per-camera, not a sum.  Window 10:55-11:45 UTC.
const SideArrival = "sidearrival-20260830T1055.mcap"

// ClassifiersLunch and ClassifiersLandscapers are the state
// classifiers' first real day (2026-08-31, classification topics +
// garage heartbeats for clock ticks).  Lunch (13:45-15:11 UTC): the
// household leaves -- bmw_garage_door open 13:53:04, closed 13:54:24,
// bmw_ix_not_present 13:53:49 -- and returns an hour later in
// reverse; every transition is real.  Landscapers (15:11-15:25, fed
// AFTER lunch into the same engine): yellow-shirted landscapers walk
// the scene and two models lie at once -- sienna_2020_present for
// 35 s, bins_not_at_curb for ~4 minutes (its false start is in the
// lunch window at 15:10:15) -- and every one of those transitions
// must be held out of the believed state.
const (
	ClassifiersLunch       = "classifiers-lunch-20260831T1345.mcap"
	ClassifiersLandscapers = "classifiers-landscapers-20260831T1511.mcap"
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
