# zymple

An opinionated single-host deploy tool. Push an image, deploy a digest, get
zero-downtime switches and Postgres with point-in-time recovery, and nothing
else. No web UI, no builds on the server, no database of its own.

```bash
zymple deploy myapp --image myapp:1.4.2
```

- **Blue/green deploys.** The new container must pass its health check before
  Caddy switches to it. A failed deploy leaves the old release serving.
- **Images go straight into the host's container store.** No registry to run
  or clean up. Only layers the host lacks are uploaded.
- **Per-app deploy tokens.** A CI secret can deploy one app and nothing else.
- **Rollback without a push.** The last few releases stay on the host.
- **Postgres with pgBackRest.** WAL archiving, scheduled backups, restore to a
  point in time, and drill restores beside the live database.
- **App definitions are files.** They live in your infrastructure repo, so a
  host can be rebuilt from git.

Status: v1 is implemented and covered by unit, fuzz and end-to-end tests. It
has not run a production workload yet.

## Documentation

- [Operations](docs/operations.md): install, define an app, deploy, restore.
- [Spec](docs/spec.md): what it does and why.
- [Testing and hardening](docs/testing.md): what is tested, and what is not.

## Development

```bash
just ci
```

runs formatting, vet, unit tests with the race detector, short fuzzing, the
`unc` static analyser and `govulncheck`. GitHub runs the same script.

```bash
just e2e
```

runs the end-to-end suite against a throwaway Docker-in-Docker host. It needs
a local Docker daemon and takes several minutes.

## Third-party code

`internal/registry/containerdstore` is derived from
[unregistry](https://github.com/psviderski/unregistry), Apache License 2.0.
The license text is in that directory.
