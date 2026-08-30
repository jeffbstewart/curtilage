# To do

Ideas waiting on their prerequisites; docs/DESIGN.md is the decision
record, this is the wish list.  Date each entry.

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
