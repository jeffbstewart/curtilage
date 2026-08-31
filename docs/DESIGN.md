# Curtilage -- design

*Curtilage* (n.): the enclosed ground immediately around a house --
the driveway, the porch, the yard -- that the law treats as part of
the home.  It is exactly what a homelab's cameras watch, and this
project is the part that tells the household what happened there.

Status: design, 2026-08-29.  Nothing built yet.  This records the
decisions and the reasoning so the server, the iOS app and the
household deployment can proceed in phases.

## Problem

[Frigate](https://frigate.video) sees things: a car in the driveway,
a person on the porch, a package on the mat.  Getting that to a
phone, fast, with a picture and a way to watch, turned out to be the
hard part -- and the obvious plumbing (Frigate -> MQTT -> Home
Assistant -> companion app -> browser link) failed on four fronts at
once in the household this was built for:

1. **Home Assistant must not be internet-reachable.**  It controls
   physical things and holds credentials to everything else; exposing
   it to fetch a thumbnail is the wrong trade.
2. **Frigate must not be internet-reachable.**  It *is* the camera
   archive.
3. **Clips arrive late.**  Frigate cuts an event's clip after the
   event ends; a notification tapped while the person is still in
   frame has nothing to play.
4. **Alerts do not distinguish news from state.**  "There is a car in
   the driveway" every twenty minutes is not "a car just arrived."
   Frigate reports what is in frame; nobody decides whether that is
   new.

And an ask from the household: one notification when a package
arrives, one when a car arrives, none when the parked car is still
parked; a tap that plays *now*; a live view one tap away; a "keep
this" button that archives the clip beyond the house.  All of it
without anyone having to turn on a VPN.

## The shape

Four layers, each with one owner:

| layer | owner | job |
|---|---|---|
| perception | Frigate | objects with identity, stationary tracking, zones, sub-labels, classifier states |
| **policy** | **curtilage server** | is this news?  who should hear?  what should they see? |
| delivery | APNs (via a blind relay) | the push |
| presentation | **curtilage iOS app** (web page as fallback) | the picture, the clip, the live view, the buttons |

Home Assistant keeps the dashboards and automations; it is off the
time-critical path.

```
cameras -> Frigate --MQTT--> curtilage (policy) --APNs--> phone (app)
                                  ^   |                     |
                                  |   +--MQTT (state)--> Home Assistant (sensors)
                                  +----- app: gRPC over HTTPS (events, media, live, actions)
```

## Perception: what Frigate actually provides

More than "car or no car":

- **Tracked objects with identity.**  An object gets an id when first
  seen and keeps it while tracked; a Frigate *event* is one object's
  lifetime, published as `new` / `update` / `end` on
  `frigate/events`.
- **Stationary detection.**  An object that stops moving is flagged
  stationary and its event keeps running quietly.  A parked car is
  one long event, not a stream of new ones.
- **Per-camera and per-zone counts as retained MQTT state**
  (`frigate/<cam>/car`, `frigate/<cam>/<zone>/car`).
- **Sub-labels** (a classifier's "blue minivan") on the object, and
  **state classifiers** with their own topics
  (`frigate/<cam>/classification/<model>`: `present` / `absent`).
- `frigate/available` as a last-will, and the recording API for
  time-range clips that exist within seconds of an event starting.

Curtilage consumes all of it and never talks to a camera.

## Policy: the state machine

Per `(camera, zone, label)`, fed every event message and count change:

- **Arrival** = a *new* tracked object that stays present for N
  seconds (hysteresis kills headlight and shadow blips) in a zone
  that matters (the driveway pad, not the road).  One push per object
  id, ever.  A parked car re-acquired under a new id after a lost
  track is suppressed unless the zone count actually went 0 -> 1:
  that is the "newly there vs still there" distinction, and the
  count topic is its signal.
- **Departure** = count 1 -> 0 held for M seconds.  With a sub-label
  it names who left.
- **Package** = the state classifier's `absent -> present`; one push.
  `present -> absent` optionally ("package picked up").
- **Quiet rules** that belong nowhere else: schedules ("our own cars
  07:00-08:00 are not news"), presence, per-person routing (an arrival
  goes to the parents, not to the arriver).

Why not Home Assistant automations: a single "count went 0 -> 1"
trigger works for one camera and falls apart at hysteresis,
identity, sub-labels, quiet hours and "did we already say so."  That
is a state machine, and it belongs in one Go package with tests, fed
by recorded streams.

**Instrument before tuning.**  Phase 1 ships a `record` mode that
writes the raw topic stream to files.  Those recordings are the test
fixtures; the rules are tuned to the household's driveway, not to
guesses.

Recordings are **MCAP** files (Foxglove's container for timestamped,
typed messages): one MCAP channel per MQTT topic, each message a
`curtilage.v1.Record` (received time, topic, retained flag, QoS, and
the broker's payload bytes verbatim -- never re-encoded, so a
recording made against one Frigate version still says exactly what
that version said), with the protobuf schema embedded so `mcap`
tooling decodes them without our code; zstd-compressed chunks, CRCs,
index, one file per day (UTC).  Chosen over length-delimited protobuf
for the index, the compression, the self-description, and the
tooling -- and to learn the format.  The writer's interface is a
channel: the MQTT client is a producer, the recorder drains, and
closing the channel is the shutdown signal.

**Clean shutdown is a requirement, not a nicety.**  An MCAP file is
complete only once its summary and footer are written, so SIGTERM
(kubernetes, `docker stop`) cancels the root context, the MQTT
client publishes `offline`, disconnects and closes the channel, the
recorder drains and closes the file, and only then does the process
exit -- ~300 ms, proven against a real broker.  A file that still
gets torn (SIGKILL, node loss) is read back in file order as far as
it goes and reported as truncated; every fully written chunk
survives.

**Configuration is protobuf text format** (`curtilage.textproto`,
schema in `proto/curtilage/v1/config.proto`): human-editable,
commented, typed against the schema -- a typo is a parse error, not a
silently ignored key -- and evolved under the same `buf breaking`
guard as the API.  Credentials are never in the file.

## Ingest: MQTT directly

Curtilage is an MQTT client with its own broker account (read
`frigate/#`, write only `curtilage/#`).  Not through Home Assistant:

- The policy engine must not depend on the box that wedges.  A
  wedged HA must not mean missed deltas, silently.
- Fidelity: retained topics on reconnect (the state machine
  cold-starts correct), ordering, last-will.
- One fewer place for logic to leak.

The retained conclusions go back out on `curtilage/#`
(`curtilage/driveway/car_present`, `curtilage/porch/package`,
`curtilage/last_arrival`, `curtilage/available` as last-will) and Home
Assistant picks them up via MQTT discovery as sensors -- dashboards
and light-switch automations for free.

## Delivery: APNs, through a blind relay

The push goes to APNs directly; Home Assistant is not in the path.
APNs only accepts pushes signed with the app owner's key for the
app's bundle id, which has a consequence the moment a second
household installs this: their server cannot hold that key.  So the
design is relay-shaped from the first commit, with the relay being a
no-op for the house:

- The server encrypts the notification payload to the phone's key
  (established at enrollment).  A relay -- whoever runs it -- forwards
  `{device token, opaque blob}` and learns nothing else.  The app's
  Notification Service Extension decrypts and fetches the thumbnail
  from the *user's own* server with a capability token.
- The notifier is an interface.  Implementations: **Home Assistant
  REST** (`notify.mobile_app_*`, so alerts work the week before the
  app ships) and **APNs** (the app).  Same policy engine under both.
- No free-text from servers through a relay; the phone renders from
  structured fields.  Per-device rate limits.  Pushes are the
  operator's signature; abuse would be the operator's violation.
- **The relay is a private club** (decided 2026-08-31).  It accepts
  push requests only from household servers holding a per-household
  bearer credential the operator mints and hands out personally --
  the capability-keyring pattern (two keys, current and prior, so
  rotation never breaks a household; revocation is deleting a row).
  Phones never talk to the relay, so this one gate is the whole
  perimeter: the server being open source costs nothing, because a
  stranger's install works entirely locally and simply cannot push.
  Per-household rate limits ride the credential.

Delivery results are synchronous and counted (`push_failures_total`),
which is how the household learns Home Assistant -- or APNs -- is
unwell before it notices missing alerts.

## Presentation: the app, and the page

**App-first**, because it removes every "tap did nothing" failure
mode at once: native rich notifications (thumbnail fetched by the
extension before the banner shows), native action buttons (Live,
Clip, Preserve, Ignore this vehicle), deep links that land on the
event screen with no browser and no login prompt, AVPlayer on
go2rtc's HLS for live (~2 s, no library), and a server stream for
"the clip is growing" so nothing polls.

The API is **gRPC over HTTP/2** behind the household's reverse proxy
(the established house pattern, as MediaManager: native gRPC on the
one port, grpc-swift on the phone): `GetServerInfo`, `ListEvents`,
`WatchEvents`, `GetMedia` now (`proto/curtilage/v1/curtilage.proto`);
enrollment, preserve and feedback arrive with their phases rather than
as placeholders.  Events are presentation-shaped, so the policy engine
can change its mind without the app changing its screens.  QUIC is a
later config change, not a v1 feature.

The **web page** is the fallback and the reviewer's first view: a
capability link opens the event -- snapshot with bounding box, the
recording-range clip playing while the event is still running and
swapping to the finished clip when it exists, a Live button, a
Preserve button.

**Plays immediately** is a hard requirement: the server never waits
for Frigate's event clip.  It serves the recording-range clip for
`[start - 5 s, now]`, extends it on request, and swaps in the
finished clip when Frigate has cut it.

## Identity

- **Notification links are capability URLs.**  The server signs
  `{event id, camera, expiry}`; the URL authorises that event's media
  for a few hours and nothing else.  A leaked screenshot leaks one
  event.  No session, no login; works in Safari, in a webview, on a
  watch.
- **Devices enroll once** (QR code on the LAN) and hold a key in the
  Keychain.  Live view, preserve and browsing require an enrolled
  device; Face ID gates the sensitive actions.  Passkeys (WebAuthn)
  are the v2 form of the same thing.  Enrollment issues the second
  bearer token of the system (the first is server-to-relay): the
  device's credential to its own household server.
- **Official builds only, eventually; a human gate meanwhile.**  The
  goal is App Attest (DeviceCheck): enrollment succeeds only for an
  unmodified, App Store-signed instance of our app -- the honest form
  of "built from our repo", since only our Apple account can sign the
  bundle id.  Initially we are relaxed: a device that fails (or
  predates) attestation lands in a human-curated approval queue and
  the operator admits it by hand.  Never a secret embedded in the app
  binary; anything shipped in the app is extractable and proves
  nothing.
- **No accounts.**  There is nothing to create or delete, which keeps
  App Store guideline 5.1.1(v) out of scope.  A household single
  sign-on is worth building only when a second service wants the same
  identities; then a small OIDC provider fronting passkeys is the
  thing, and curtilage becomes its first client.

## Credentials, by tier

- **MQTT account**: committed in the household's private config repo
  (worthless off-LAN; worst case is disrupted camera events).
- **Home Assistant long-lived token** (interim notifier): operates the
  house, and with HA Cloud in place operates it from the internet.
  Never committed; a dedicated non-admin HA user; Secret by ceremony.
- **APNs key**: an Apple-account credential.  Never committed; Secret
  by ceremony.
- **Relay household credential**: minted by the relay operator,
  handed to a household in person, held by that household's server
  (Secret by ceremony there); two-key rotation as capability links.
- **Capability-link signing keys**: configured, not generated, so a
  pool of instances behind a load balancer signs and verifies alike.
  Two Secrets: `CURTILAGE_MEDIA_KEY` (current: mints and verifies) and
  `CURTILAGE_MEDIA_KEY_PRIOR` (verifies only; empty outside a
  rotation).  Tokens name their key by id.  Rotation is prior <-
  current, current <- new, rolling restart; the prior key is dropped
  once the link lifetime has passed, so no outstanding link ever
  breaks.

## Exposure

One door: `curtilage.<house domain>` on the household's reverse
proxy, on the LAN and on the internet.  It proxies only what a valid
token authorises, by event id: never a general Frigate or Home
Assistant proxy, no path passthrough, unsigned ids 404 identically
(no enumeration), per-IP rate limits, a structured audit log of every
media fetch.  Frigate and Home Assistant stay exactly as unexposed as
before.  It is a new public surface and gets a written assessment
before the first public listing.

## Observability

A Prometheus `/metrics` endpoint, non-negotiable:

- events consumed (camera, label, type); state transitions (camera,
  zone, label, transition) -- the arrivals and departures themselves;
- pushes sent / failed (device); capability links minted / opened /
  rejected (invalid, expired) -- the abuse signal for the public door;
- media proxied (kind, bytes); MQTT connected; Frigate available;
  Home Assistant reachable; relay reachable.

Alerts follow: `CurtilageDown`, `CurtilageMQTTDisconnected`,
`CurtilagePushFailing`, `CurtilageRejectedLinksSpiking`.  The
`curtilage/available` last-will is the MQTT-side heartbeat.

## Review, demo, and strangers

**Apple's reviewer never touches the house.**  Demo mode lives **in
the app**: bundled canned events, snapshots and clips, a "Try the
demo" button on the sign-in screen, no server.  The server's own
`--demo`/replay mode remains as the policy engine's fixture harness.
TestFlight internal testers (the household) need no review at all;
external testers and the App Store do, and the in-app demo satisfies
guideline 2.1.

**Distribution** (decided 2026-08-31): an **unlisted** App Store app
-- fully reviewed and hosted by Apple, installs never expire (no
TestFlight 90-day treadmill), but reachable only by direct link,
invisible to search.  **Free, no in-app purchases, no revenue, no
LLC.**  The audience is the immediate family, plus friends at the
operator's discretion; the enrollment QR and the relay credential are
the club, so a leaked install link yields only an app with nothing to
enroll against.  If by some quirk real demand appears, THAT is the
trigger to form the LLC and open a paid cloud gateway at deliberately
silly monthly rates -- a decision to be made then, not engineered for
now beyond what the relay abstraction already provides.

**If this goes viral** -- decisions made cheap now so they never have
to be made expensively later:

- Encrypted push envelopes with a relay abstraction (above).
- In-app demo mode, so no household server is ever a public target.
- `buf breaking` in CI from the first commit; additive-only API
  changes; `GetServerInfo` so the app can say "your server is too
  old" gracefully.
- A supported-Frigate-versions contract, tested against recorded
  streams per version; Frigate churn is the support burden.
- A truthful privacy manifest -- "Data Not Collected" -- and no
  analytics SDK, ever.  Standard-algorithm encryption still means
  answering the export question and filing the annual
  self-classification.
- Free, forever, for the club.  Charging turns it into a business;
  the only version of that worth having is the demand-triggered
  gateway above, priced to deter.
- No accounts (above); no Frigate branding in the name or icon.

## Repository layout

One repo: `proto/` (buf, generates both sides), Go module at the
root (`cmd/curtilage`, `internal/...`), `ios/` beside it, CI with
path filters (Go on Linux runners, Xcode on macOS runners).  One tag
is one release: a server image and a TestFlight build.  Household
configuration (camera names, zones, who hears what, credentials)
lives in the household's config repo, never here.

Go, Apache-2.0, 7-bit ASCII, LF line endings, `go vet`, `go test`,
`govulncheck`, a secret scan, and supply-chain pins (actions by SHA,
images by digest, `-mod=readonly`) from the first commit, as in the
sibling projects.

## Phases

1. **Server core**: MQTT ingest, `record` mode, capability links,
   snapshot + growing clip, web page, Home Assistant REST notifier,
   `/metrics`.  LAN only.  Recorded streams become fixtures.
2. **Policy**: the state machine on the fixtures; zones; quiet rules;
   MQTT state out to Home Assistant.
3. **Public door**: the exposure assessment, then the proxy listing.
4. **App**: enrollment, rich notifications via APNs, event screen,
   live (HLS), in-app demo; TestFlight internal.
5. **Preserve**: the archive pipeline and its button.
6. **Later**: passkeys, the blind relay as a real service, QUIC,
   household SSO if a second client appears.

## Name

Crowsnest was the first choice (the lookout's perch on a frigate)
and is heavily contested on the App Store.  Curtilage has zero App
Store results and says precisely what is watched.  Frigate does not
appear in the name or the icon: the project is MIT, the mark is not
ours.
