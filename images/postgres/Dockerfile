# zymple-postgres: the official Postgres image plus pgBackRest.
# Postgres needs pgBackRest inside its own container, because the archive
# command runs there.
#
# ca-certificates is not optional: the official image has no certificate
# authorities, and without them pgBackRest refuses the TLS certificate of
# every S3 endpoint, so no backup or WAL segment reaches a bucket.
ARG PG_MAJOR=17
FROM postgres:${PG_MAJOR}
RUN apt-get update \
 && apt-get install -y --no-install-recommends pgbackrest ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/pgbackrest /var/lib/pgbackrest \
 && chown postgres:postgres /var/lib/pgbackrest
