# curtilage

*Curtilage* (n.): the enclosed ground immediately around a house --
the driveway, the porch, the yard.

iOS notifications and event views for [Frigate](https://frigate.video)
in a homelab: a small server that decides which camera events are
news, and an app that shows them -- fast, with a picture, a clip that
plays now, and a live view one tap away -- without exposing Frigate or
Home Assistant to the internet.

Status: phase 1 (ingest + record).  See [docs/DESIGN.md](docs/DESIGN.md).

    curtilage run -config curtilage.textproto     # watch the broker, record to MCAP, serve the API
    curtilage run -config F -replay G.mcap -speed 10   # serve the API from a recording; no broker needed
    curtilage replay -file curtilage-<start>.mcap  # summarize a recording (-topic T -dump N prints payloads)
    curtilage trim -file F -out G -from T -to T -keep RE -drop RE   # cut a fixture from a recording
    curtilage version

Configuration is protobuf text format (`examples/curtilage.textproto`,
schema `proto/curtilage/v1/config.proto`); credentials come from
`CURTILAGE_MQTT_USER` / `CURTILAGE_MQTT_PASSWORD`.  Recordings are
MCAP files, one channel per MQTT topic, `curtilage.v1.Record`
messages with the schema embedded -- `mcap info` and Foxglove read
them directly.  On `listen`: `/metrics` (Prometheus), `/healthz`, and
`POST /admin/rotate`, which closes the current recording immediately
so it is complete and indexed (unauthenticated; the listener is LAN
only).  Files also rotate on `rotate_every`, idle or not.

Shutdown: SIGTERM/SIGINT publishes `offline`, disconnects, finishes
the current MCAP (summary + footer) and exits; a file torn by SIGKILL
is still readable up to the last complete chunk (`replay` says so).
Chunks are small (64 KiB) so a file still being written lags the live
stream by little.

## Development

Go 1.26 (the `toolchain` directive pins the release), protobuf schema
under `proto/` (buf, STANDARD lint, breaking changes against `main`
fail CI), the iOS app under `ios/` when it exists.  7-bit ASCII, LF
line endings, no dependencies that a reader cannot audit.

    sh lifecycle/presubmit.sh        # every local check, in CI order
    sh lifecycle/install-hooks.sh    # secret scan as the pre-commit hook

CI (`verify`): ASCII, gofmt, vet, test, govulncheck, buf lint/format/
breaking, the secret scan and its self-test, and forge-lint (the
merge-path protections).  Actions are pinned by commit SHA, go.sum is
enforced (`-mod=readonly`), images (later) by digest.  Work happens on
`agent/` branches via pull request; only the operator merges.

Apache-2.0.
