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

## Waiting on Frigate+ (a model that knows our objects)

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
  arrive with the policy phase either way.
