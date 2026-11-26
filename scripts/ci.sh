#!/usr/bin/env bash
# The whole CI gate. Run locally with `just ci`; GitHub runs this same file.
set -euo pipefail
cd "$(dirname "$0")/.."

GOVULNCHECK_VERSION=v1.1.4
NEW_CODE_REF="${NEW_CODE_REF:-origin/main}"

step() { printf '\n== %s\n' "$1"; }

step gofmt
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "not gofmt-formatted:"
  echo "$unformatted"
  exit 1
fi

step "go mod tidy"
go mod tidy -diff

step "go vet"
go vet ./...

step "go test"
go test -race -shuffle=on -coverprofile=coverage.out ./...

step unc
unc_args=(analyze --format "${UNC_FORMAT:-text}" --go-coverage coverage.out --fail-on major)
# The first push has no base branch to compare against.
if git rev-parse --verify --quiet "$NEW_CODE_REF" >/dev/null; then
  unc_args+=(--new-code "$NEW_CODE_REF")
fi
unc "${unc_args[@]}"

step govulncheck
go run "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" ./...
