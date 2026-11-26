set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

# Everything GitHub CI runs: format, vet, tests, unc, govulncheck.
ci:
    ./scripts/ci.sh

# Tests only.
test *args="./...":
    go test -race -shuffle=on {{ args }}

# Static analysis only.
lint:
    unc analyze --fail-on major

build:
    go build -o bin/zymple ./cmd/zymple
