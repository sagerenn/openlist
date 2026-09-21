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

mkdir -p "${DATA_DIR}"

# Seed the frontend dist on first start only: if the target index.html does
# not exist, copy the bundled dist over. This preserves any dist an operator
# has dropped in or a previous start has written.
if [ ! -f "${DIST_DIR}/index.html" ]; then
    echo "entrypoint: seeding frontend dist into ${DIST_DIR}"
    mkdir -p "${DIST_DIR}"
    cp -a "${SEED_DIST}/." "${DIST_DIR}/"
fi

exec openlist-ext "$@"
