#!/bin/sh
# Entrypoint for the openlist-ext Docker image.
#
# openlist-ext embeds OpenList in-process. OpenList serves its web UI from a
# "dist" directory pointed to by the dist_dir setting in config.json.
# BootOpenList (in internal/server/openlist_mount.go) creates <dataDir>/dist
# with a placeholder index.html if it is absent, and sets dist_dir to that
# directory. To serve the real built frontend instead of the placeholder, we
# seed <dataDir>/dist from the read-only /opt/openlist-ext/seed-dist image
# layer on first start (when the directory is empty or missing). On subsequent
# starts the operator's dist is preserved.
set -eu

DATA_DIR="${DATA_DIR:-/data}"
SEED_DIST="/opt/openlist-ext/seed-dist"
DIST_DIR="${DATA_DIR}/dist"

mkdir -p "${DATA_DIR}" 2>/dev/null || true

# Seed the frontend dist on first start only: if the target index.html does
# not exist, copy the bundled dist over. This preserves any dist an operator
# has dropped in or a previous start has written.
if [ ! -f "${DIST_DIR}/index.html" ]; then
    if ! mkdir -p "${DIST_DIR}" 2>/dev/null; then
        echo "entrypoint: cannot create ${DIST_DIR}: ${DATA_DIR} is not writable by $(id -un 2>/dev/null || id -u)." >&2
        echo "entrypoint: if you bind-mounted ${DATA_DIR}, fix ownership on the host, e.g." >&2
        echo "entrypoint:   sudo chown -R 100:101 <host-dir>   (openlist is uid 100, gid 101 in this image)" >&2
        echo "entrypoint: or pass --user <uid>:<gid> matching the mounted directory." >&2
        # Fail rather than fall back to the placeholder dist: BootOpenList
        # would create one, and its index.html would then block seeding on
        # every later start.
        exit 1
    fi
    echo "entrypoint: seeding frontend dist into ${DIST_DIR}"
    # -R (not -a): the seed-dist files are owned by the build-stage user, and
    # cp -a would try to preserve that ownership, which fails as non-root.
    cp -R "${SEED_DIST}/." "${DIST_DIR}/"
fi

exec openlist-ext "$@"
