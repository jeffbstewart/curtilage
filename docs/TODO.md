# To do

Ideas waiting on their prerequisites; docs/DESIGN.md is the decision
record, this is the wish list.  Date each entry.

## Waiting on Frigate+ (a model that knows our objects)

- **Garage door state** (2026-08-31).  Open or shut, from the garage
  camera via a Frigate state classifier (tilt sensors were tried and
  disappointed).  The left/right_garage_door zones only see objects
  crossing them, not the door itself.  When the classifier exists its
  present/absent topic feeds the policy engine like the package
  classifier the design already plans for.

- **Trash and recycling cans** (2026-08-30).  The cans go out Sunday
  night and come in Monday night.  Two scheduled checks, each worth an
  alarm to the household:
  - Sunday 19:00: the cans are NOT at the curb -> "the cans are not
    out yet".
  - Monday 19:00: the cans ARE still at the curb -> "the cans are
    still out".
  Needs a detector that recognizes the cans (Frigate+ fine-tune or a
  state classifier on the curb view), a curb zone, and a new policy
  shape: a scheduled assertion about the world's state at a time, not
  a reaction to an event.  The notifier and quiet rules it needs
  arrive with the policy phase either way.  The occupancy ledger
  (policy/occupancy.go: per zone-and-label counts with arrival dwell
  and departure grace, built for parked cars) is deliberately
  label-generic: once Frigate emits a can label in a curb zone, the
  cans are one config entry, and the Sunday/Monday checks read the
  same ledger.
