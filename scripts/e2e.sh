#!/usr/bin/env bash
# End-to-end tests: a throwaway privileged Docker-in-Docker "server" with
# Caddy, driven from outside with the real CLI. Needs a local Docker daemon.
set -euo pipefail
cd "$(dirname "$0")/.."
exec go test -tags e2e -count=1 -timeout 40m "$@" ./test/e2e
