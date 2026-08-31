# To do

Ideas waiting on their prerequisites; docs/DESIGN.md is the decision
record, this is the wish list.  Date each entry.

## Phase 4 (the app and the relay)

- **The private club** (2026-08-30/31, DESIGN.md "Delivery",
  "Identity", "Distribution").  Unlisted App Store app, free, no IAP.
  Two bearer tokens: household server -> relay (operator-minted,
  captoken keyring pattern, per-household revocation and rate
  limits); device -> household server (issued at QR enrollment).
  App Attest at enrollment eventually; until then a human-curated
  approval queue for devices that fail or predate it.  No secrets in
  the app binary, ever.

## Landed sooner than expected

The state classifiers arrived 2026-08-31 without Frigate+: locally
trained models publishing frigate/<camera>/classification/<model>
with the current class as payload.  Doors, per-car presence and
bins_at_curb are live; policy/states.go holds their twitch out of
the believed state.  What remains of the old entries:

- **Scheduled assertions** (the bins).  Sunday 19:00: bins not out ->
  act (missed collections have happened; preventing a recurrence has
  very high WAF).  Monday 19:00: bins still out -> act.  Reads the
  HELD bins state, which the hold makes landscaper-proof.
- **MQTT out to Home Assistant** (design phase 2): publish the held
  states retained under curtilage/#.  First consumer: a "house
  occupied" bit composed in HA from car presence plus
  are-the-iphones-on-the-WiFi.
- **Semi-supervised improvement**: flag false positives/negatives
  back into the classifiers' training libraries as they occur.

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
