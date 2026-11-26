# Operations

How to install Zymple on a host, define an app, and deploy to it. Example
files are in [`deploy/`](../deploy).

## Host requirements

- Debian or Ubuntu with systemd.
- Docker from Docker's own apt repository, with the compose plugin and the
  **containerd image store** enabled. Docker 29 enables it on new installs.
  On older or upgraded hosts, set it in `/etc/docker/daemon.json` and restart
  Docker:

  ```json
  { "features": { "containerd-snapshotter": true } }
  ```

  `docker info --format '{{.DriverStatus}}'` must mention
  `io.containerd.snapshotter`.
- Caddy, as a host package.
- Ports 80 and 443 open. Nothing else needs to be reachable.

## Install

1. Put the release binary at `/usr/local/bin/zymple`. Check it first: each
   release carries `SHA256SUMS` and a signed build provenance attestation.

   ```bash
   gh attestation verify zymple-linux-amd64 -R gokaykucuk/zymple
   ```

2. Copy `deploy/zymple.service` to `/etc/systemd/system/`.
3. Write `/etc/zymple/zymple.yml`, mode 0600. Every key is optional; see
   `deploy/zymple.yml.example`. Set `containerd.socket` to the socket your
   Docker uses (`docker info --format '{{.Containerd.Address}}'`).
4. Create `/etc/zymple/apps` and `/etc/caddy/zymple`.
5. Use `deploy/Caddyfile.example` as the base Caddyfile. It routes one
   hostname to Zymple and imports the per-app snippets.
6. `systemctl enable --now zymple`.

The unit file has not been run on a real host yet; treat its hardening
options as a starting point.

## Define an app

Create `/etc/zymple/apps/<app>/` with:

- `app.yml` and `compose.yml`, described in the [spec](spec.md#app-definition).
- `env` (optional): `KEY=value` lines, mode 0600. Values reach containers
  literally, including `$`.
- `tokens`: one line per deploy token.

Create a token anywhere the binary runs:

```bash
zymple token generate ci
```

It prints the token, which goes into your CI secrets, and the line for the
`tokens` file. The host never sees the token itself.

Then, on the host:

```bash
zymple reload
```

A definition that fails validation is reported and ignored. The reload exits
non-zero, and every other app is left as it was.

## Deploy from CI

```bash
docker buildx build --load -t myapp:$GIT_SHA .
```

```bash
ZYMPLE_HOST=https://deploy.example.com ZYMPLE_TOKEN=$TOKEN zymple deploy myapp --image myapp:$GIT_SHA
```

The command streams the deploy and exits non-zero if it fails. A failed
deploy leaves the previous release serving.

## Day to day

```bash
zymple status myapp
```

```bash
zymple releases myapp
```

```bash
zymple rollback myapp
```

```bash
zymple logs myapp worker -f
```

On the host, as root:

```bash
zymple apps
```

```bash
zymple redeploy myapp
```

Use `redeploy` after changing an app's `env` file or definition. It starts a
new release of the current image with no downtime.

```bash
zymple run myapp -- bin/console
```

`run` is not interactive: it runs the command in a one-off container and
prints its output. A `zymple run` still running on a release that is no
longer current is removed by the next `zymple reload` or daemon start.

## The Postgres image

Apps that declare `postgres` run `ghcr.io/gokaykucuk/zymple-postgres:<major>`,
built from `images/postgres/Dockerfile` by the "Postgres image" workflow on
every change and once a week. To use your own build, or before that workflow
has published one, build the Dockerfile on the host and point
`postgres_images` in `zymple.yml` at the result:

```yaml
postgres_images: { 17: zymple-postgres:17 }
```

## Database backup and restore

Backups run on their own once `backup` is configured. To check them:

```bash
zymple pg info myapp
```

Rehearse a restore without touching the live database:

```bash
zymple pg restore myapp --into drill --time "2026-01-02 15:04:05+00"
```

The drill cluster is reachable from the app's network as
`postgres-drill-drill`, or with
`docker exec -it zymple-myapp-postgres-drill-drill psql -U app app`. Remove
it with `zymple pg drill-rm myapp drill`.

Restore the live database, which stops the app for the duration:

```bash
zymple pg restore myapp --yes --time "2026-01-02 15:04:05+00"
```

Omit `--time` to restore to the latest archived state.

## Upgrading from a release before the hardening changes

- The first push of each app uploads every layer again. Blob ownership is
  now recorded per app and older images carry no record.
- With backups configured, each Postgres container is replaced once, with a
  clean shutdown, on the next deploy or reload: expect a short database
  restart. Redis containers are replaced once too, because the default image
  is now pinned by digest.
- Existing clusters keep `app` as a superuser and log a warning on the first
  deploy or reload after the upgrade, with or without a backup repository. See the
  spec's Postgres section for how to get the split.
- Replace the unit file: it now sets `KillMode=mixed`.
- `user` in a compose file now takes only a lowercase account name or a
  plain decimal id. A definition that no longer validates is not loaded when
  the daemon starts, so check definitions before restarting.

## Rebuilding a host

1. Provision the host and run the infra repo's roles, which install Docker,
   Caddy and Zymple and render the app definitions.
2. For each app with Postgres, deploy once so the cluster exists, then run
   `zymple pg restore <app> --yes`.
3. Deploy each app from CI. A new host has no images.

## Things not to do

- **Do not run `docker image prune -a` or `docker system prune -a`.** They
  delete the images rollback needs. Zymple removes old images itself.
- Do not replace an app directory with a symlink. Zymple loads plain
  directories only; it keeps the old definition and reports an error.
- Do not edit `/etc/caddy/zymple/*.caddy` or anything under
  `/var/lib/zymple`. Both are overwritten.
- Do not give anyone docker access on the host who should not be root.
