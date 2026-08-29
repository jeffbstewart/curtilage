# curtilage

*Curtilage* (n.): the enclosed ground immediately around a house --
the driveway, the porch, the yard.

iOS notifications and event views for [Frigate](https://frigate.video)
in a homelab: a small server that decides which camera events are
news, and an app that shows them -- fast, with a picture, a clip that
plays now, and a live view one tap away -- without exposing Frigate or
Home Assistant to the internet.

Status: design.  See [docs/DESIGN.md](docs/DESIGN.md).

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
