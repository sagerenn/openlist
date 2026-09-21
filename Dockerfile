# Multi-stage, multi-arch Dockerfile for openlist-ext.
#
# Build (with buildx for multi-arch):
#   docker buildx build --platform linux/amd64,linux/arm64 -t openlist-ext .
#
# The image bundles the openlist-ext Go binary (which embeds OpenList
# in-process) together with a built OpenList-Frontend. At runtime the
# entrypoint seeds the frontend dist into the data directory on first
# start; BootOpenList then points OpenList's dist_dir at it so the manage
# panel (including the Extension section) is served from the same origin
# as the API on port 5245.
#
# Build context: the openlist-ext repo root, with the OpenList-Frontend
# source placed at ./frontend (the publish workflow checks out both repos
# and arranges this layout before invoking docker buildx).

# ---- Stage 1: build the frontend dist -------------------------------------
FROM node:24-alpine AS frontend
WORKDIR /frontend

# pnpm is shipped via corepack in the node image.
RUN corepack enable

# Copy only the frontend source (placed at ./frontend in the build context).
COPY frontend/ .

# Install with the official registry to avoid corrupted mirrors, then build.
RUN pnpm install --no-frozen-lockfile \
    && pnpm build

# ---- Stage 2: build the openlist-ext binary -------------------------------
FROM golang:1.25-alpine AS backend
WORKDIR /src

RUN apk add --no-cache git

# Copy only the backend module manifests first for layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the backend source (the .dockerignore excludes ./frontend here).
COPY . .

# buildx sets TARGETOS and TARGETARCH for the platform being built.
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}

RUN go build -trimpath -ldflags="-s -w" -o /out/openlist-ext .

# ---- Stage 3: runtime image -----------------------------------------------
FROM alpine:3.20 AS runtime

# ca-certificates for outbound TLS (storages, /api/me loopback is in-process
# but drivers may call remote endpoints); tzdata for sane default times.
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S openlist \
    && adduser -S -G openlist -h /opt/openlist-ext openlist

WORKDIR /opt/openlist-ext

# The binary.
COPY --from=backend /out/openlist-ext /usr/local/bin/openlist-ext

# The frontend dist, staged read-only; the entrypoint copies it into the
# (possibly volume-mounted) data directory on first start.
COPY --from=frontend /frontend/dist /opt/openlist-ext/seed-dist

# Entrypoint that seeds the frontend dist and execs the binary.
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENV DATA_DIR=/data \
    LISTEN_ADDR=:5245 \
    LOOPBACK_ADDR=http://127.0.0.1:5245

VOLUME ["/data"]
EXPOSE 5245

USER openlist

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
