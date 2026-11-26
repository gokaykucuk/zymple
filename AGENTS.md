# zymple

Single-host deploy tool. The design is in `docs/spec.md`; how it is tested is
in `docs/testing.md`. Read both before changing behaviour.

## Conventions

- `just ci` is the only gate and runs `scripts/ci.sh`, the same script GitHub
  runs. Run it before committing.
- The static analyser is `unc` (uncomplicated). Do not silence a finding with
  `// NOSONAR` without a reason on the same line.
- Follow the rules at the top of `docs/testing.md`: failure-injection tests
  for deploy steps, a regression test per bug, a fuzz target per untrusted
  input.
- Keep dependencies few. Each new one needs a reason in the PR.
- Pin GitHub Actions by commit SHA and tools by version.
- This repository is public. No hostnames, app names or secrets from any
  real deployment.
