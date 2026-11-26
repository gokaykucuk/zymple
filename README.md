# zymple

An opinionated single-host deploy tool: push an image, deploy a digest, get
zero-downtime switches, Postgres with point-in-time recovery, and nothing
else. No web UI, no builds on the server, no database of its own.

Status: design stage. Nothing here runs yet.

- [Spec](docs/spec.md)
- [Testing and hardening](docs/testing.md)

## Development

```bash
just ci
```

runs formatting, vet, tests with the race detector, the `unc` static
analyser and `govulncheck`. GitHub runs the same script.
