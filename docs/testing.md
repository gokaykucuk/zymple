# Testing and hardening

Zymple holds the Docker socket and sits on the deploy path of production
apps. It is only worth having if it is more trustworthy than the tools it
replaces, so tests are part of the design.

## Rules

1. `just ci` runs the same script GitHub runs. A change that does not pass it
   is not merged.
2. Every step of the deploy sequence has a failure-injection test before the
   step is merged.
3. Every bug fix comes with a test that fails without the fix.
4. Every input that crosses a trust boundary has a fuzz target.
5. New code needs at least 80% coverage, enforced by the quality gate.

## Layers

| Layer | Where | Runs |
|---|---|---|
| Static analysis: `gofmt`, `go vet`, `unc`, `govulncheck` | `scripts/ci.sh` | Every push |
| Unit tests with the race detector and shuffled order | `internal/**` | Every push |
| Fuzzing, 5 seconds per target | `scripts/fuzz.sh` | Every push |
| Fuzzing, 5 minutes per target | `.github/workflows/nightly.yml` | Nightly |
| End to end | `test/e2e`, `scripts/e2e.sh` | Every push and nightly |

## How the end-to-end suite works

`just e2e` builds a throwaway "server": a privileged Docker-in-Docker
container with the containerd image store and Caddy. It installs the Linux
build of Zymple into it, then drives it from outside with the real CLI and
the real docker client, the way CI would. Nothing in the path is faked: image
layers travel through Caddy into the embedded registry and into containerd,
containers are started by docker compose, and Postgres is a real cluster
archiving to a pgBackRest repository.

Set `ZYMPLE_E2E_KEEP=1` to leave the host running for inspection.

## What is covered

### Deploy state machine

- Unit: a failure is injected into every step up to the switch. Each case
  asserts the old release still serves, the new container and its compose
  file are gone, the port is returned, the lock is released, and the next
  deploy works.
- Unit: a failure in every step after the switch does not fail the deploy.
- Unit: failing to record the release switches traffic back.
- End to end: the daemon is killed with SIGKILL while a new container waits
  for its health check, and again while the old one drains. After a restart
  it converges back in the first case and forward in the second, with no
  failed requests.
- Two operations on one app at once: exactly one runs.
- Unit: convergence points the proxy at the running current release before
  it removes any other web container, and removes nothing if that fails.
- Unit: a reload with a typo in a running app's definition, or with a new app
  claiming its domain, leaves the app loaded, proxied and running.
- Unit: cleanup leaves a freshly pushed tag it cannot account for alone for
  an hour, and removes a deploy's own push tag at once.
- Unit: a release command that never returns is aborted at its timeout, its
  container is removed, the lock is released and the next deploy works.

### Zero downtime

A load generator runs against the app through Caddy and every non-200 answer
is a test failure. Covered: a successful deploy, a rollback, a redeploy, a
reload, a deploy that fails its health check, both crash-recovery cases, and
an aggressive image prune followed by a reload.

The stress workflow (`.github/workflows/stress.yml`, run by hand or by
pushing a `stress/` branch) repeats the switch 40 times on each of six
runners while two load generators run: one outside the host, and one inside
it that opens a new connection per request. Each failure is logged with its
time, so it can be matched to a phase of a deploy. This is how the dropped
connections caused by reloading Caddy were found and how the fix was proven.
It is too slow for every push; run it after any change to how traffic is
switched.

### Authentication and isolation

- A matrix of token against route, in unit tests and end to end: no token, a
  wrong token, another app's token, an unknown app, own token.
- App A's token cannot push to or deploy app B, read any manifest or blob,
  list the catalog, or push outside the `zymple/` namespace.
- The operator routes return 404 on the network listener.
- Fuzzing asserts that no accepted registry path leaves the app's repository
  and that no read is ever authorised. The repository is the one the
  registry's own router resolves the path to, not a prefix of the text.
- A path the registry would read as a repository nested under the app's own
  is refused, and no image is created from it.
- An app can hold only a bounded number of log streams open.
- A push cannot move a `release-<n>` tag.
- The client does not follow a redirect, so the token is not sent on.
- The CLI replaces terminal control sequences in output it prints.
- A caller-supplied digest whose content exists only under another app is
  refused.

### Registry

The storage backend runs in unit tests against containerd's real content,
image and lease stores on a temporary directory, with no daemon. A whole push
is made over HTTP through the token guard: chunked and single-request
uploads, existence checks, a manifest, a tag. The tests then read the image
back from the image store and check the garbage-collection labels that keep
its layers alive. They also cover what must be refused: content that does not
match its digest, a manifest that is malformed or references blobs that were
never uploaded, reads, deletes, other repositories and cross-repository
mounts. One app replaying another app's manifest, or putting its own config
on another app's layers, is refused until it has uploaded every blob itself,
and a lookup of a layer as a manifest is answered without reading it. An
empty or junk upload under another app's digest is refused against a store
that, like the containerd daemon, reports "already exists" without checking
the bytes. An index naming a manifest that was only uploaded as a blob is
refused. An upload sent in several requests holds one lease, not one per
request.
Fuzzing asserts that a repository which owns no blobs can never store a
manifest.

### Compose validation

A corpus of escape attempts that must all be rejected: privileged mode,
host namespaces, capabilities, devices, the Docker socket, host bind mounts
in every syntax, volume driver options, `build:`, published ports, other
images, interpolation, root users in every spelling Docker resolves to uid 0,
service names that collide with Zymple's own, YAML anchors and merge keys, `extends`,
`include`, and more. Fuzzing asserts that whatever the validator accepts
renders to a file containing only allow-listed keys. New bypass ideas go into
the corpus first.

### Releases and rollback

- After more deploys than `keep_releases`, exactly that many images remain
  tagged, and `docker image prune` removes none of them.
- Rollback works from the host with no push.
- `docker image prune -a` is survivable: running releases keep serving, a
  reload restores their image names, and a rollback to a lost image either
  succeeds from surviving content or is refused cleanly.

### Extra images

- End to end, an app with a second image: the first deploy is refused until
  it brings both, each later deploy changes only the image it names while
  requests keep being answered, a rollback brings back the pair, a prune of
  dangling images leaves every kept release whole, and a push may not move
  the tag that pins a release's extra image.
- The engine is tested for the set as a unit: every image is verified before
  anything starts, a missing or undeclared one refuses the deploy, the tags
  of pruned releases go and those of kept ones stay, an image name lost to
  `prune -a` is restored, and a rollback across a change of the declared
  images takes what the old release lacks from the current one.
- A placeholder is accepted only as the exact text Zymple writes for a
  declared image; the variants compose would also interpolate (defaults,
  prefixes, suffixes, lower case) are in the rejection tests, and fuzzing
  asserts the same for arbitrary text and for release tags.

### Postgres

End to end, against a real cluster: rows are written before and after a
recorded time, the cluster is restored to that time, and exactly the earlier
rows exist. This runs twice: into a drill cluster while the live one keeps
serving, and in place. Afterwards the restored cluster accepts writes, takes
a backup and survives a deploy.

### Managed services

- Unit: no secret appears in the arguments of any command, for Postgres and
  Redis, S3 and directory repositories.
- Unit: role setup on a new cluster and on one whose superuser is still
  `app`; a failed Redis restore starts Redis again; database containers are
  stopped gracefully before they are replaced; a drill is removed only
  when the container carries the app's drill labels, and a container
  labelled as another app's is never replaced as this app's database.
- Unit: after a restore the role setup waits for the server to leave
  recovery. The end-to-end restore found this on a fast machine.
- Unit: a Redis backup pauses append-only file rewrites around the copy,
  waits for one under way, and copies nothing when it cannot.
- Fuzzing asserts that accepted restore options only ever produce arguments
  of the validated shape.
- By hand against real images, not yet in the suite: the role split on
  `postgres:17` (the app role cannot run programs or read server files) and
  the restic restore command.

### Importing a dump

Unit tests cover which roles are read from a dump, the order of the steps,
that only missing roles are created and that they are always dropped again,
and which failures leave the database untouched. End to end, a real dump with
objects owned by a foreign role, by the superuser and by a role the target
has is imported into another app: the test checks rows, the owner of every
kind of object, that the borrowed roles are gone, that `after-import.sql`
ran, that the app's role can write and is still not a superuser, and that a
file which is not an archive leaves the app serving.

### Host faults

- Host reboot: the test host is restarted; every app comes back on its
  recorded release and deploys normally afterwards.
- A Postgres container that keeps crashing fails the deploy quickly with the
  container's logs, instead of waiting out the readiness timeout.

## Not covered yet

- S3 backup repositories end to end. The tests use a directory repository;
  the S3 settings Zymple generates are unit-tested only.
- Redis backup and restore end to end.
- Upgrading Zymple from a previous release with apps running.
- Disk-full behaviour during a push or a deploy.
- Pushing with `buildx --push` or `crane`, and multi-architecture images. The
  CLI pushes with `docker push`, which is what the tests exercise.
- The systemd unit.

## Static analysis settings

[`unc`](https://github.com/gokaykucuk/uncomplicated) runs with its default
quality gate plus `--fail-on major`, so any issue of major severity or above
fails the build, not only issues in new code.

Two deliberate exceptions, both visible in the repository:

- `uncomplicated.toml` turns the cognitive-complexity rule off for
  `*_test.go`. SonarQube applies that rule to main sources only.
- `internal/execx/execx.go` and `internal/services/postgres.go` each carry
  one `// NOSONAR` with its reason: the single place commands are executed,
  and the mode of a mount point directory.

GitHub Actions are pinned by commit SHA. Tool versions are pinned in
`scripts/ci.sh` and the workflows.
