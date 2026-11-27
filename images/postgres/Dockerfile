# zymple-postgres: the official Postgres image plus WAL-G.
# Postgres needs WAL-G inside its own container, because the archive command
# runs there.
#
# WAL-G is the release binary, pinned by version and checksum. The build for
# Ubuntu 22.04 is the one whose C library is old enough for every Debian
# release the official image is based on.
#
# ca-certificates is not optional: the official image has no certificate
# authorities, and without them no TLS connection to an S3 endpoint is
# trusted, so no backup or WAL segment reaches a bucket.
ARG PG_MAJOR=18
FROM postgres:${PG_MAJOR}
ARG WALG_VERSION=v3.0.10
ARG WALG_SHA256_AMD64=ef16ca5fc079feab65c79cfdb75d80ce0d84622a3ab25b1fd2c8a3b9f1e0d9e5
ARG WALG_SHA256_ARM64=b137200f2adef0787ac5b460d0c2be56be0284811bbb42f5f8819daa82058223
RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
      amd64) asset=amd64; sum="$WALG_SHA256_AMD64" ;; \
      arm64) asset=aarch64; sum="$WALG_SHA256_ARM64" ;; \
      *) echo "no WAL-G build for this architecture" >&2; exit 1 ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    curl -fsSL -o /usr/local/bin/wal-g \
      "https://github.com/wal-g/wal-g/releases/download/${WALG_VERSION}/wal-g-pg-22.04-${asset}"; \
    echo "${sum}  /usr/local/bin/wal-g" | sha256sum -c -; \
    chmod 0755 /usr/local/bin/wal-g; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*; \
    mkdir -p /etc/wal-g /var/lib/wal-g; \
    chown postgres:postgres /var/lib/wal-g; \
    wal-g --version
