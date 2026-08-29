#!/bin/sh
# Every local check, in the order CI runs them.  `sh lifecycle/presubmit.sh`
# before committing; lifecycle/install-hooks.sh wires the secret scan
# into the pre-commit hook.  Checks that need a tool not on PATH are
# SKIPPED with a note, never silently passed.
set -eu
cd "$(dirname "$0")/.."
sh lifecycle/check-ascii.sh
u=$(gofmt -l .); if [ -n "$u" ]; then echo "gofmt needed on: $u" >&2; exit 1; fi; echo "gofmt: OK"
GOFLAGS=-mod=readonly go vet ./... && echo "go vet: OK"
GOFLAGS=-mod=readonly go test ./... >/dev/null && echo "go test: OK"
if command -v buf >/dev/null 2>&1; then
  (cd proto && buf lint) && echo "buf lint: OK"
else
  echo "buf lint: SKIPPED (install buf: https://buf.build/docs/installation)"
fi
git diff "$(git hash-object -t tree /dev/null)" HEAD | bash lifecycle/presubmit-check.sh >/dev/null && echo "secret scan (tree): OK"
bash lifecycle/presubmit-check-test.sh >/dev/null && echo "secret scan tests: OK"
