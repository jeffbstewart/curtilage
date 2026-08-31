# To do

Ideas waiting on their prerequisites; docs/DESIGN.md is the decision
record, this is the wish list.  Date each entry.

## Frigate+ (2026-08-31: base model live, new labels tracked)

- **Known plates suppress, then rescind** (2026-08-31).  Frigate's
  known_plates roster (homenet, canonical config) turns a recognized
  plate into the car event's sub_label ("Jeff's BMW").  Policy: an
  arrival by a known plate is household noise, not news -- suppress
  the notification.  The catch is ordering: the plate often resolves
  seconds AFTER the arrival would have notified, so the notifier
  needs rescind.  APNs cannot unsend, but apns-collapse-id lets a
  follow-up replace the alert in place and a background push can
  clear it; design this into the notifier when that phase lands.
  Start with the one plate, expand if it earns it.
- **Bear pierces do-not-disturb** (2026-08-31).  bear is tracked on
  every camera (there are bears in town).  A bear detection is an
  alarm-class event that must blast through DND: iOS critical alerts
  (needs Apple's entitlement -- apply early, approval is slow) or at
  minimum time-sensitive interruption level.  Until the app exists it
  surfaces on the house page like everything else.
- **Packages** (2026-08-31).  package is tracked on porch-east/down/
  west, driveway-down and driveway-winchester.  Zones' object lists
  must include package before those events get zoned (UI edit, then
  pull); the house page elides unzoned events by design.

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
  The detector arrived 2026-08-31: the Frigate+ base model's
  waste_bin label is tracked on driveway-winchester.  What remains is
  a curb zone on that view (UI, then pull), one occupancy entry
  (zone: curb, labels: waste_bin -- the ledger was built
  label-generic for exactly this), and the scheduled-assertion policy
  shape: a claim about the world's state at a time, not a reaction to
  an event.  The notifier and quiet rules it needs arrive with the
  policy phase either way.  Bonus signal: garbage_truck is tracked on
  the same view -- a Monday-morning sighting confirms collection
  actually happened, distinguishing "missed pickup" from "cans out
  late".
