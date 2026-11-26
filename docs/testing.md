# Testing and hardening

Zymple holds the Docker socket and sits on the deploy path of production
apps. It is only worth having if it is more trustworthy than the tools it
replaces, so tests are part of the design, not an afterthought.

## Rules

1. `just ci` runs the same checks locally as GitHub runs. A change that does
   not pass it is not merged.
2. Every step of the deploy sequence has a failure-injection test before the
   step is merged.
3. Every bug fix comes with a test that fails without the fix.
4. Every input that crosses a trust boundary has a fuzz target.
5. New code needs at least 80% coverage, enforced by the quality gate.

## Layers

| Layer | What it proves | Runs |
|---|---|---|
| Static analysis | No vet findings, no `unc` issues at major or above, quality gate passes, no known-vulnerable dependencies | Every push |
| Unit | Pure logic: app and compose validation, token checks, release bookkeeping, the deploy state machine against fakes | Every push, with the race detector and shuffled order |
| Fuzz | Parsers and decoders never panic and never accept what validation rejects | Short run every push, long run nightly |
| Integration | Real Docker with the containerd image store and real Caddy on the CI runner | Every push |
| End to end | A fresh Debian VM, installed the way production is | Nightly and before a release |

## What the tests must cover

### Deploy state machine

- For each step, inject a failure and assert: the old release is still
  serving, no container or Caddy snippet from the failed attempt remains, and
  the lock is released.
- Kill the server process at each step, restart it, and assert it converges
  to one healthy release with nothing orphaned.
- Two deploys for the same app at once: exactly one runs.

### Zero downtime

A load generator runs against the app throughout. Zero failed requests
during: a successful deploy, a deploy that fails its health check, a
rollback, and a `zymple reload`.

### Authentication and isolation

- A full matrix of token against operation. App A's token must fail every
  registry and API call for app B, including cross-repository blob mounts.
- No token, a malformed token and a revoked token fail every route.
- Operator commands are unreachable from the network listener.
- Pushes are tested with `docker push`, `docker buildx --push` and `crane`,
  single and multi-arch.

### Compose validation

A corpus of escape attempts that must all be rejected: privileged mode, host
networking and PID namespace, host bind mounts, the Docker socket, added
capabilities, devices, `build:`, published ports, and YAML anchors or merges
that hide any of them. New bypass ideas are added to the corpus first.

### Releases and rollback

- After `keep_releases + n` deploys, exactly `keep_releases` images remain
  pinned and a system-wide Docker prune removes none of them.
- Rollback to the oldest kept release works with the network unplugged from
  the registry path.

### Postgres

- An automated PITR drill against a local S3 server: write timestamped rows,
  restore to a chosen time, assert exactly the rows before it exist.
- Restore into a second cluster while the first keeps serving.
- A backup taken by version N restores with version N+1.

### Upgrades and host faults

- Upgrade Zymple from the previous release with apps running: zero failed
  requests, releases intact.
- Host reboot: every app comes back on its recorded release.
- Disk full during a push and during a deploy: the old release keeps
  serving and the error is reported.

## Tooling

- [`unc`](https://github.com/gokaykucuk/uncomplicated) is the static
  analyser, locally and in CI, with its default quality gate plus
  `--fail-on major`.
- `go vet`, `gofmt`, `go mod tidy -diff`, and `go test -race -shuffle=on`.
- `govulncheck` for dependencies.
- GitHub Actions are pinned by commit SHA. Tool versions are pinned in
  `scripts/ci.sh` and the workflow.
