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

### An app with more than one image

List the extra images in `app.yml` (`images: [ocr]`) and give each a compose
service whose image is `${IMAGE_OCR}`. Push them with the same token:

```bash
zymple deploy myapp --image myapp:$GIT_SHA --image ocr=myapp-ocr:$GIT_SHA
```

The first deploy after declaring an image has to include it. After that a
deploy changes only the images it names, so the usual pipeline can go on
deploying `--image myapp:$GIT_SHA` alone, and the job that builds the
document processor deploys `--image ocr=myapp-ocr:$GIT_SHA` when it changes.
Each release pins the whole set, and a rollback brings the set back.

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
postgres_images: { 18: zymple-postgres:18 }
```

## Database backup and restore

Backups run on their own once `backup` is configured. To check them:

```bash
zymple pg info myapp
```

The list shows each backup's name and time; a name with `_D_` in it is a
delta. Take one by hand with `zymple pg backup myapp --type full`.

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

## Upgrading from a release that used pgBackRest

Releases up to v0.1.5 backed up with pgBackRest. From the next release the
image `zymple-postgres:<major>` contains WAL-G instead, under the same tag,
and the two cannot read each other's repositories.

1. Upgrade Zymple.
2. On the host, pull the image: `docker pull ghcr.io/gokaykucuk/zymple-postgres:<major>`.
   Docker does not replace a tag it already has, and a database container
   started from the old image would have no `wal-g` to archive with.
3. Run `zymple reload`. The database container is recreated with the new
   settings; the app loses its database for the few seconds that takes.
4. Take a full backup: `zymple pg backup <app> --type full`. Point-in-time
   recovery starts from this backup; nothing before it is reachable with the
   new release.
5. The pgBackRest repository is in the same prefix, in `archive/` and
   `backup/`, beside WAL-G's `basebackups_005/` and `wal_005/`. Delete those
   two folders once the first WAL-G backup has been restored into a drill
   cluster.

`--type diff` and `--type incr` are gone; the types are `full` and `delta`.

## Upgrading from a release before the hardening changes

- The release command is now stopped after `release_timeout`, one hour
  unless `app.yml` sets it (up to 24h). Set it before deploying an app whose
  migrations can take longer.
- `zymple redis restore <app>` now needs `--yes`.
- CI must not push tags of the form `release-<n>`; the registry refuses
  them. `zymple deploy --image` never did.

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

## Moving a database in from elsewhere

Take a custom-format dump on the source (`pg_dump -Fc`), copy it to the host,
deploy the app once so its database and roles exist, then:

```bash
zymple pg import myapp /root/myapp.pgdump --yes
```

This replaces the app's whole database. Read "Importing a dump" in the
[spec](spec.md#importing-a-dump) first: it explains how ownership is mapped
and what `after-import.sql` is for. If the dump is a copy of production, set
whatever stops the app from sending mail or calling other services before you
import, because the import starts the app.

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
- Do not edit anything in `/etc/caddy/zymple/` or under `/var/lib/zymple`.
  Both are overwritten, and the `.port` files there decide where live
  traffic goes.
- Do not give anyone docker access on the host who should not be root.
