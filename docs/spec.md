# Zymple v1 spec

Status: implemented. This document describes what the code does. Where the
implementation departed from the original design, the reason is given under
[Decisions made while building](#decisions-made-while-building).

"The infra repo" means the operator's own infrastructure repository: the
Ansible code that configures hosts and holds app definitions and encrypted
secrets. Zymple knows nothing about it beyond the files it renders onto the
host.

Related: [operations](operations.md), [testing and hardening](testing.md).

## Purpose

Zymple runs applications on a single host. It is an opinionated deploy tool:
an "advanced Dokku" with a far smaller feature set and attack surface than a
general PaaS.

Observability (analytics, errors, traces, uptime) is out of scope. Apps send
telemetry to whatever backend the operator runs elsewhere.

## Principles

1. **No state worth backing up.** Everything Zymple knows comes from files
   Ansible rendered plus the images in the host's container store. It keeps a
   small cache of release history and generated passwords, and a host can be
   rebuilt without either.
2. **No builds on the server.** Images are built with `docker buildx` in CI or
   on a devops machine and pushed to the host.
3. **A deploy names a digest.** Pushing an image does not deploy it. Tags are
   never trusted to stay put.
4. **Opinionated.** docker-compose is the only app format. Postgres and Redis
   are the only managed services. One image per app. One host per app.
5. **Rebuildable.** A host is fully described by the infra repo. Recovery is
   an Ansible run, a Postgres restore, and a re-deploy per app.

## Non-goals for v1

- Building images, buildpacks, preview environments.
- Multi-host apps, clustering, Swarm-style workers.
- Zero downtime across host reboots.
- A web console, teams, roles.
- Managed services other than Postgres and Redis.

## Components

| Piece | What it is |
|---|---|
| `zymple server` | Go daemon on each app host |
| `zymple` CLI | The same binary, used from CI, laptops and on the host |
| Caddy | Host package; TLS and routing |
| Docker | Docker's apt repo, with the containerd image store enabled |

The server contains an OCI registry whose storage is the host's containerd
image store, so pushed images land directly where Docker runs them. There is
no separate registry process, registry disk, or registry garbage collection.
The HTTP side is the `distribution` library; the storage backend is in
`internal/registry/containerdstore`, derived from
[unregistry](https://github.com/psviderski/unregistry) under its Apache 2.0
license.

## Network surface

Caddy is the only public listener (80/443). Zymple listens on a loopback port
and on a unix socket.

- Loopback port, forwarded by Caddy from one hostname:
  - `/v2/*`: the OCI registry routes.
  - `/api/v1/apps/<app>/*`: the deploy API.
- Unix socket, mode 0600, root only: the same deploy API without a token,
  plus the operator API under `/admin/*`. The operator routes do not exist on
  the loopback port.

The configuration loader refuses a `listen` address that is not loopback.

Web containers publish their port on `127.0.0.1` only. Postgres and Redis
publish nothing; apps reach them over the app's Docker network.

## Authentication

- Tokens are per app: 256 random bits with a `zym_` prefix, created with
  `zymple token generate <id>`. The host stores only SHA-256 hashes, in
  `/etc/zymple/apps/<app>/tokens`, one `<id> <hash>` per line. Several lines
  allow rotation. The id is recorded with each release.
- A token authorises exactly: pushing to repository `zymple/<app>`, and the
  deploy API for `<app>`.
- The registry accepts it as basic auth (`docker login`, user = app name), the
  API as a bearer token.
- An unknown app and a wrong token get the same answer.
- The registry guard allows only what a push needs: the version check, blob
  and manifest existence checks, and uploads. Reading blobs or manifests,
  listing, deleting and cross-repository mounts are refused.
- A blob belongs to the repositories that uploaded its bytes. Existence
  checks and manifests see only the app's own blobs, so a manifest cannot
  name content another app pushed, even when the digest is known.

## App definition

Lives in the infra repo and is rendered by Ansible to
`/etc/zymple/apps/<app>/`: `app.yml`, `compose.yml`, `env` (optional,
`KEY=value` lines, passed to containers literally) and `tokens`.

```yaml
name: myapp
domains: [app.example.com]
compose: compose.yml          # default
web:
  service: web
  port: 3000
  healthcheck: /healthz       # default /
  health_timeout: 60s         # default
  drain: 30s                  # default
release: ["bin/migrate"]      # optional, runs before the switch
keep_releases: 5              # default
postgres: { version: 17, pitr_days: 14 }
redis: { persistent: false }
```

Unknown keys are an error. A definition that fails validation is reported by
`zymple reload` and ignored; the previously loaded app keeps running on its
last good definition and other apps are unaffected. Its tokens file is still
re-read, so a token can be revoked while the definition is broken. An app is
removed, and its containers stopped, only when its directory no longer
exists. If the path is still there but is not a plain directory (a symlink
or a file), or cannot be inspected, the app keeps its previous definition
and `zymple reload` reports it. When two definitions claim the same domain,
the app already in use wins: one that is loaded, or, after a daemon restart,
one that has a current release on record. The other is not loaded, or stays
on its own previous definition. Between two apps that are both new, the one
whose name sorts first wins. Reloads run one at a time.

### The compose subset

The compose file is checked against an allow-list. Anything not on it is
rejected.

- Top level: `services` and `volumes` only.
- Every service's `image` must be exactly `${IMAGE}`.
- Service keys: `image`, `command`, `entrypoint`, `environment`,
  `healthcheck`, `restart`, `stop_grace_period`, `stop_signal`,
  `working_dir`, `user`, `init`, `read_only`, `tmpfs`, `mem_limit`, `cpus`,
  `pids_limit`, `volumes`.
- Volumes are named volumes declared at the top level, with no options,
  mounted with the short `name:/path[:ro]` form. Host paths are not possible.
- `user` is `USER[:GROUP]`. A numeric uid is plain decimal from 1 to
  2147483647 and a numeric gid plain decimal from 0 to 2147483647; a name is
  a lowercase account name, and the user may not be `root`. The check is
  made on the text as written in the file, quoted or not, so `01000`,
  `0x3E8`, `0o17`, `0b11`, `+1` and `1_000` are refused rather than read as
  numbers. Omit the key to run the image's own user.
- Service names are reserved when Zymple uses them itself:
  `<web service>-r<number>` always, `postgres` and `postgres-drill-*` when
  the app declares Postgres, and `redis` when it declares Redis.
- Mapping keys must be strings.
- `$` is not allowed anywhere else, which disables compose interpolation.
- YAML anchors, aliases and merge keys are refused.
- `DATABASE_URL` and `REDIS_URL` may not be set by the app.

For each release Zymple renders the file it actually runs: the image pinned
by digest, `pull_policy: never`, `no-new-privileges`, the env file, labels,
the app's network, and, for the web service, a per-release service name and a
loopback port.

Exactly one service is the web service. Only it gets blue/green. Other
services (job workers, schedulers) are recreated on the new image after the
switch.

## Host layout

```
/etc/zymple/zymple.yml              server config
/etc/zymple/apps/<app>/             app.yml, compose.yml, env, tokens
/etc/caddy/zymple/<app>.caddy       generated by Zymple
/var/lib/zymple/apps/<app>/         releases.json, rendered compose files,
                                    postgres/, redis/
/run/zymple/zymple.sock             operator socket
```

## Deploy

```
zymple deploy <app> --image <ref>
```

1. The CLI tags the local image for `<host>/zymple/<app>` and pushes it with
   the docker CLI, using a temporary docker config so the user's own login
   state is untouched. Layers the host already has are skipped.
2. The CLI calls `POST /api/v1/apps/<app>/deploys` with the digest. The
   response is a stream of events until the deploy ends. The CLI prints them
   and exits non-zero on failure.

`zymple deploy <app> --digest sha256:...` skips the push.

Server side, per deploy:

| Step | Action | On failure |
|---|---|---|
| verify | The digest exists in repository `zymple/<app>` | Reject |
| | Take the app's deploy lock | HTTP 409 |
| | Reserve a release number and a loopback port; start Postgres and Redis if they are not running | Abort |
| release | Run `release` in a one-off container | Abort, remove the one-off container, old release untouched |
| start | Start the new web container | Abort, remove it |
| health | Poll the health check until `health_timeout` | Abort, remove it |
| switch | Atomically replace the app's port file, which Caddy reads on every request | Abort |
| | **Commit**: record the release as current | Switch back, abort |
| drain | Wait `drain`, stop the old web container | Report, continue |
| services | Recreate the other compose services on the new image | Report, continue |
| cleanup | Pin the image, prune beyond `keep_releases` | Report, continue |

Until the commit the old release serves all traffic and a failure leaves
nothing behind. After the commit the new release is live and nothing can fail
the deploy. If the client disconnects, the deploy continues; `zymple status
<app>` reports the result.

A release number is never reused, including by failed attempts.

### Crash recovery and reload

At startup and on `zymple reload`, each app is converged to its recorded
state: web containers of releases that are not current are removed, the
current one is started if stopped or recreated if missing, the proxy is
pointed at it, and the other services are brought up. When the current
release is already running, the proxy is pointed at it first, before
anything is removed. One-off containers (`zymple run`, the release command)
are never counted as the web container. Those of a release that is not
current are removed, so a release command orphaned by a crash cannot keep
running beside the live release; a `zymple run` on the current release is
left alone. A crash before the
commit therefore converges back to the old release, and one after it
converges forward.

`zymple redeploy <app>` starts a new release of the current image through the
same blue/green sequence. This is how a changed `env` file or app definition
takes effect without downtime; a reload alone does not restart containers.

### How traffic is switched

Each app has two files in `/etc/caddy/zymple/`: `<app>.caddy`, a site block
with the app's domains, and `<app>.port`, the loopback port of the live
release. The site block reads the port file on every request, so a switch is
one atomic rename and Caddy is not reloaded.

Caddy is reloaded only when the site block itself changes: on an app's first
deploy, when its domains change, and when the app is removed. A reload makes
Caddy replace its listening sockets, and a connection that arrives in that
instant can be dropped, so domain changes are best made outside peak hours.

## Releases and rollback

- Each successful deploy is a numbered release: digest, port, time, who.
- The last `keep_releases` images are tagged `zymple/<app>:release-<n>`.
  Older release tags are removed after each deploy. Only a tag of the exact
  form `release-<n>` is Zymple's own. Any other tag is removed at once when
  its image is the release the deploy just made, and otherwise an hour after
  cleanup first saw it, so that an image another CI job has pushed but not
  yet deployed is not taken away under it. The first-seen times are kept in
  the app's release state (`pending_tags`), so a daemon restart does not
  restart the hour.
- `zymple releases <app>` lists them.
- `zymple rollback <app> [release]` runs the same sequence onto an image that
  is already on the host, skipping the `release` command. No transfer.
- Rollback reverts the image only. Migrations must stay compatible with the
  previous release (expand, deploy, contract).

**`docker image prune -a` destroys rollback targets.** The tags protect kept
releases from `docker image prune`, which removes only dangling images. The
`-a` form removes every image no container uses, tags included, and with the
containerd image store it also strips the name from images that running
containers do use. Zymple restores the names of running releases on the next
reload or rollback, and refuses a rollback whose image is gone with a clear
message. Do not run `prune -a` on a Zymple host; Zymple prunes its own images.

## Postgres

- One cluster per app, in a container managed by Zymple, with its data under
  `/var/lib/zymple/apps/<app>/postgres/data`.
- The image is `zymple-postgres:<major>`: the official Postgres image plus
  pgBackRest (`images/postgres/Dockerfile`). This repository publishes it;
  `postgres_images` in the server config overrides the reference.
- pgBackRest runs inside that container and ships base backups and WAL to
  the configured repository, one stanza per app. Repositories are S3
  compatible buckets or a host directory.
- The server schedules backups: full weekly, differential daily. Retention is
  `pitr_days`.
- A deploy never restarts the Postgres container. It is recreated only when
  its image or settings change. The first deploy or reload after the Zymple
  server starts repeats the role and backup-stanza setup on the running
  container, without restarting it, so a setup that was interrupted is
  finished.
- The app connects as role `app`, which owns database `app` and its `public`
  schema (on Postgres 14 Zymple hands the schema over explicitly; from 15 on
  the database owner has it) and is not a superuser: it cannot run programs or read files in the database container,
  where the backup credentials are. The cluster's superuser is `postgres`,
  with its own generated password that no app is given. Only trusted
  extensions can be created from the app; for anything else use
  `docker exec -it zymple-<app>-postgres psql -U postgres app`.
- Both passwords are generated on first use (`password` and `admin-password`
  in the app's `postgres/` state directory). The roles and the app's password
  are re-applied at every container start and once after each start of the
  Zymple server, so they survive a restore from another host.
- A cluster created before this split has `app` as its bootstrap superuser,
  which Postgres does not allow to be demoted. Zymple adds the `postgres`
  role, keeps the cluster working and logs a warning at every container start and
  once after each start of the Zymple server. To get
  the split, dump the database and restore it into a new cluster.
- A database container that has to be replaced is stopped first, with up to
  two minutes to shut down cleanly.
- Without a backup repository, Postgres runs with archiving off.

Operator commands, on the host:

- `zymple pg info <app>`
- `zymple pg backup <app> [--type full|diff|incr]`
- `zymple pg restore <app> --yes [--time <ts>]`: in place. Stops the app,
  restores, starts it again, holding the deploy lock throughout.
- `zymple pg restore <app> --into <name> [--time <ts>]`: a drill cluster
  beside the live one, with archiving off, reachable on the app's network as
  `postgres-drill-<name>`.
- `zymple pg drill-rm <app> <name>`
- `zymple pg psql <app>`

Before a restore, the latest WAL segment is archived so that any time up to
now is reachable. A target time later than the last recorded transaction
makes Postgres refuse to start; omit `--time` to restore to the end.

A major version upgrade is a manual procedure in v1.

## Redis

- `persistent: false`: cache only, no volume, no backup.
- `persistent: true`: AOF on a host directory, snapshotted daily to the
  backup repository with restic. `zymple redis backup|restore <app>`. Redis
  is stopped for a restore and started again whether or not it succeeds.

## CLI summary

From CI or a laptop, with `ZYMPLE_HOST` and `ZYMPLE_TOKEN`:

```
zymple deploy <app> --image <ref> | --digest <sha256:...>
zymple rollback <app> [release]
zymple releases <app>
zymple status <app>
zymple logs <app> [service] [-f] [--tail n]
```

On the host, as root (uses the operator socket):

```
zymple apps
zymple reload
zymple redeploy <app>
zymple run <app> -- <command...>
zymple pg info|backup|restore|psql|drill-rm <app>
zymple redis backup|restore <app>
```

Anywhere: `zymple token generate <id>`, `zymple server`, `zymple version`.

Plain HTTP is accepted only for localhost, so a token cannot be sent
unencrypted by mistake.

## What the infra repo provides

- A role that installs a pinned Zymple release, its systemd unit and server
  config, and enables the containerd image store in Docker.
- A Caddy role with a base Caddyfile that imports `/etc/caddy/zymple/*`.
- App definitions, env files and token hashes.

App deploys run from CI with the app token. Host configuration is applied by
the operator with Ansible.

## Security model

- Zymple holds the Docker and containerd sockets, so it is root-equivalent.
  Its network input is limited to registry pushes and five API routes, all
  behind a per-app token.
- A leaked app token can push images to that app, deploy and roll back that
  app, and read its status and logs. It cannot read images or secrets, reach
  other apps, or run commands on the host.
- App definitions are untrusted input to the validator, but they are also
  reviewed in the infra repo. The compose allow-list is the second line of
  defence, not the first.
- Secrets exist in the infra repo (encrypted) and on the host's disk. The
  server config holds backup credentials and must be mode 0600.

Known limits:

- The container content store is shared between apps, but the registry
  answers for a blob only to an app that uploaded it. The price is that each
  app uploads a layer once, even when another app's image already has it.
- There is no per-token upload quota. A token holder can fill the disk with
  uploads; unreferenced uploads are collected after 24 hours. Limit request
  sizes and source addresses in Caddy.
- `docker inspect` on the host shows the Postgres superuser password. Anyone
  who can run docker there is already root-equivalent. Secrets are never put
  on a command line, where any local user could read them.
- Long streams are bounded: past 10000 undelivered events, or 16 MiB of
  undelivered output, the oldest are dropped, and output with no line ending
  is cut into lines of at most 64 KiB, never inside a UTF-8 character.

## Decisions made while building

- **The registry storage is carried in this repository** instead of imported
  from unregistry. unregistry's public API only starts its own HTTP listener,
  and it is built against a `distribution` release with known
  vulnerabilities that it cannot be compiled without. Carrying the roughly
  500 lines lets Zymple use patched dependencies, serve the registry from its
  own authenticated handler with no extra port, and test the storage against
  in-process containerd stores.
- **Manifests are verified before they become images.** The inherited code
  stored any manifest and tagged it. Zymple refuses one with the wrong schema
  version, with no content, or that references a blob that was never
  uploaded, and answers a digest mismatch with a client error.
- **Repository names are `zymple/<app>`**, not `<app>`, so an app named
  `postgres` or `redis` cannot overwrite a library image on the host.
- **Compose validation is an allow-list** and bind mounts are not supported
  at all. A deny-list would have to track every new compose feature.
- **Caddy runs as a host package** and web containers publish on loopback
  ports, which stay stable across container restarts. Zymple needs no access
  to Caddy beyond one directory and a reload command.
- **A deploy does not reload Caddy.** The first version rewrote the site
  block and reloaded Caddy on every switch. Under load on CI runners about
  one request in 100,000 then failed with a closed connection, always at the
  moment of a reload. Reading the port from a file removed the reload, and
  the same stress run then lost none of 1.05 million requests.
- **Each release's web service has its own compose service name**
  (`web-r<n>`) inside one compose project, so old and new run side by side
  while sharing the project's named volumes.
- **The registry does not serve downloads.** A push never needs them, and
  with a shared content store they would let one app fetch another's image.
- **A caller's digest is never re-pinned by image id.** Restoring a lost
  image name is only done for digests from the app's own release history.
- **Blob ownership is recorded by Zymple, not by the content store.** Each
  committed upload labels the blob with the repository it was pushed to
  (`zymple.io/pushed-to.<repository>`). Zymple compares the uploaded bytes
  with the claimed digest itself first: containerd's content service answers
  "already exists" for a digest the host has without looking at the upload.
- **An index may only name manifests the registry verified.** A manifest
  stored through the manifest route is labelled
  `zymple.io/manifest-of.<repository>`; an index is accepted only if every
  entry carries it, and an image manifest may not name a manifest as a blob.
  Otherwise a manifest uploaded as a plain blob, which nothing checks, could
  be reached through an index. Images pushed before this existed carry no
  label, so the first push after an upgrade uploads every layer again.
  Releases already on the host are unaffected.
- **Manifest lookups are bounded.** A lookup by digest answers only for the
  app's own blobs of at most 4 MiB; anything larger is not a manifest and is
  never read into memory.

## Open items

1. **The registry storage is Zymple's to maintain.** Fixes made upstream in
   unregistry have to be ported by hand.
2. **S3 repositories are not covered by the end-to-end tests**, which use a
   directory repository. The generated pgBackRest and restic settings for S3
   are unit-tested only.
3. **The systemd unit and the Ansible role have not run on a real host.**
   The release workflow's provenance attestation has not run either.
4. **Long release commands.** A migration longer than CI's job timeout still
   completes on the server; CI has to learn the result from `zymple status`.
5. **Postgres major upgrades** are manual.
