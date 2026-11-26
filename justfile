set shell := ["bash", "-euo", "pipefail", "-c"]

default:
    @just --list

# The gate GitHub CI runs: format, vet, tests, short fuzzing, unc, govulncheck.
ci:
    ./scripts/ci.sh

# End-to-end tests against a throwaway Docker-in-Docker host. Needs Docker.
e2e *args="":
    ./scripts/e2e.sh {{ args }}

# Unit tests only.
test *args="./...":
    go test -race -shuffle=on {{ args }}

# Fuzz every target for a while (default one minute each).
fuzz time="1m":
    FUZZTIME={{ time }} ./scripts/fuzz.sh

# Static analysis only.
lint:
    unc analyze --fail-on major

build:
    go build -o bin/zymple ./cmd/zymple
