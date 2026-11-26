#!/usr/bin/env bash
# Runs every fuzz target for $FUZZTIME (default 5s). One list, used by both
# the CI gate and the nightly job.
set -euo pipefail
cd "$(dirname "$0")/.."
FUZZTIME="${FUZZTIME:-5s}"
while read -r target pkg; do
  echo "-- $target ($pkg)"
  go test -run '^$' -fuzz "^${target}\$" -fuzztime "$FUZZTIME" "$pkg"
done <<'TARGETS'
FuzzParseCompose ./internal/appdef
FuzzParse ./internal/appdef
FuzzParse ./internal/auth
FuzzAuthorize ./internal/registry
FuzzValidDigest ./internal/deploy
FuzzManifestPut ./internal/registry/containerdstore
FuzzRestoreOptions ./internal/services
FuzzParseReleaseTag ./internal/runtime
TARGETS
