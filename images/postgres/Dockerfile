# zymple-postgres: the official Postgres image plus pgBackRest.
# Postgres needs pgBackRest inside its own container, because the archive
# command runs there.
ARG PG_MAJOR=17
FROM postgres:${PG_MAJOR}
RUN apt-get update \
 && apt-get install -y --no-install-recommends pgbackrest \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/pgbackrest /var/lib/pgbackrest \
 && chown postgres:postgres /var/lib/pgbackrest
