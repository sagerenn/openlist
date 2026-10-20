# openlist-ext

A forward-compatible extension of [OpenList](https://github.com/OpenListTeam/OpenList)
that adds six features. **It runs standalone**: a single binary that embeds
and boots OpenList in-process and layers the features on top of OpenList's
own HTTP routes — no separate OpenList instance, no reverse proxy.

## How it works

OpenList v4 exposes enough of its public surface to be embedded without
forking:

- `cmd.Init()` boots OpenList's internals (config, database, storages, seed
  data) in-process.
- `cmd/flags.DataDir` / `cmd/flags.ConfigPath` are settable package vars.
- `server.Init(e *gin.Engine)` mounts OpenList's **entire** route tree
  (`/api/*`, `/d/*`, `/p/*`, webdav, s3, mcp, auth, admin) onto a gin engine
  we create.
- `server/handles.*`, `server/middlewares.*`, `server/common.*` are public.

`main` sets `flags.DataDir`, calls `server.BootOpenList` (which calls
`cmd.Init`), opens the extension's own SQLite database, then builds one gin
engine, calls `server.Init(engine)` to mount OpenList's routes, and registers
the extension's feature middleware + admin API on the same engine. One
process, one port.

### Storage loading (why `BootOpenList` calls `OutOpenListInit`)

OpenList's `StoragesLoaded` middleware blocks every non-whitelisted path
(`/api/*`, `/d/*`, `/p/*`, …) on a "storages loaded" signal that is only sent
by `bootstrap.Start` → `LoadStorages`. Because the extension mounts OpenList's
routes on its **own** gin engine (via `server.Init`) instead of calling
`bootstrap.Start`, that signal would never fire and every API call — including
login — would hang forever.

`BootOpenList` solves this with public packages only: it disables OpenList's
built-in HTTP listener (`scheme.http_port = -1` in `config.json`) and invokes
the public `cmd.OutOpenListInit` hook in a goroutine. `OutOpenListInit` is
OpenList's sanctioned "start from an external embedder" entry point (used by
OpenList-Mobile); with the listener disabled it runs `LoadStorages` +
`InitTaskManager` + `InitOfflineDownloadTools` — sending the signal and making
the embedded backend fully functional — **without** spawning a duplicate HTTP
server.

The feature middleware (`apiKeyAuth` → `domainAuth` → `listPermGuard` →
`applyTTL`) runs as global gin middleware **before** OpenList's route
handlers. For requests it doesn't govern it calls `c.Next()` and OpenList's
own handlers serve the response. Privileged in-process operations (TTL reaper
deletes, load-balanced uploads) are performed by an HTTP client pointed at the
engine's own loopback address with the admin token — a real in-process call,
not a second OpenList.

### Forward compatibility

Only OpenList **public** packages are imported (`cmd`, `cmd/flags`, `server`,
`server/handles`, `server/middlewares`, `server/common`). No `internal/`
package is imported. The extension keeps its own state in a **separate SQLite
database** keyed by OpenList user ID, never touching OpenList's schema.
OpenList may change its internals freely; as long as `server.Init(e)` and
`cmd.Init()` keep their signatures, the extension keeps working.

## The six features

1. **Per-user domain routing** — bind a host (e.g. `alice.example.com`) to a
   user; requests with that `Host` header resolve to that user so per-user
   policy applies.
2. **Per-user file TTL with auto-delete** — enable TTL per user; expiry by
   fixed time (`uploaded_at + duration`) or sliding access window (refreshed
   on each download). A background reaper deletes expired files. Enable/disable
   per user.
3. **Per-user list-permission control** — a user may read/write files but be
   forbidden from listing directories, or be a write-only drop box. Per-user
   `can_list` / `can_read`.
4. **Upload load-balancing** — route uploads through a named LB group of
   backend mount paths; strategies `round_robin`, `least_used`, `random`
   (weighted).
5. **API-key auth + scoped operations** — issue per-user API keys with scopes
   (`fs.list`, `fs.get`, `fs.put`, `fs.rm`, `fs.mkdir`, or `*`). Keys are
   SHA-256 hashed with a prefix for lookup and constant-time comparison.
6. **Forward compatibility** — separate module + DB, public-packages-only
   dependency (see above).

## curl upload

One command to upload and share a file:

```bash
curl -T file.txt -H "X-Api-Key: <secret>" http://localhost:5245/upload/dir/file.txt
```

```json
{"code":200,"message":"success","data":{
  "path":"/dir/file.txt",
  "url":"http://localhost:5245/d/dir/file.txt?sign=<sign>",
  "sign":"<sign>","size":1234}}
```

`PUT /upload/<path>` (curl `-T` / `--upload-file`) and `POST /upload/<path>`
(`--data-binary @file`) both work. The body is streamed to the storage
never buffered, and the response's `url` is a same-origin `/d/<path>` link
that works in a browser or `curl` — with the sign OpenList requires already
embedded as `?sign=` when signed downloads are enabled (sign-all, storage
sign, or a password-protected meta). The link is built from the request's
own host, honoring `X-Forwarded-Proto` / `X-Forwarded-Host` behind a proxy.

**Authentication** (guests are rejected): an API key with the `fs.put` scope
(`X-Api-Key: <secret>` or `Authorization: Bearer <secret>`), or the static
admin token (`Authorization: <admin-token>`). API-key uploads are attributed
to the key's user, so the user's other features apply:

- `X-LB-Group: g1` (or `?lb_group=g1`) — route through one of the user's LB
  groups (see feature 4); the path is rewritten to the chosen member's
  mount, the file name preserved, and `data.path` / `data.url` reflect the
  final location. Not available with the admin token (no owning user).
- A TTL record is created when the user has TTL enabled (feature 2), and
  `X-Ttl: <seconds>` sets a per-file override, exactly as with
  `/api/fs/put`.
- OpenList's own write-permission checks still run server-side.

Errors: `400` missing path / bad LB usage, `401` missing or invalid key,
`403` key without `fs.put`, `404` unknown LB group, `502` storage failure,
`503` LB group has no members.

## Build & run

```bash
# Build (Go 1.25+). The binary embeds OpenList and all its storage drivers.
go build -o openlist-ext .

# Run with defaults (listens on :5245, OpenList data in ./data).
ADMIN_TOKEN=<your-admin-token> ./openlist-ext

# Run with explicit flags.
./openlist-ext \
  -listen :5245 \
  -data-dir ./data \
  -db ./data/openlist-ext.db \
  -admin-token <admin-token> \
  -reaper-interval 60 -reaper-batch 100
```

On first run, OpenList creates `./data/config.json`, its own `./data/data.db`,
and an `admin` user (the initial password is printed to stdout). A placeholder
web UI is created at `./data/dist/index.html`; drop a built OpenList frontend
there to serve the real UI. To set a deterministic admin password (useful for
containers and CI), set `OPENLIST_ADMIN_PASSWORD`; the initial `admin` user's
password is then that value on first run.

## Docker

A multi-stage, multi-arch Dockerfile bundles the binary with a built
OpenList-Frontend. The entrypoint seeds the frontend dist into the data
directory on first start, so the manage panel (including the Extension
section) is served from the same origin as the API on port 5245.

```bash
# Local single-arch build.
docker build -t openlist-ext .

# Multi-arch build + push (requires buildx + QEMU).
docker buildx build --platform linux/amd64,linux/arm64 -t ghcr.io/<owner>/openlist-ext:latest --push .

# Run. State lives in the mounted /data volume.
docker run -p 5245:5245 -v openlist-data:/data \
  -e OPENLIST_ADMIN_PASSWORD=changeme ghcr.io/<owner>/openlist-ext:latest

# Run behind a reverse proxy at https://example.com/files/.
docker run -p 5245:5245 -v openlist-data:/data \
  -e OPENLIST_ADMIN_PASSWORD=changeme \
  -e SITE_URL=https://example.com/files ghcr.io/<owner>/openlist-ext:latest
```

The container runs as the unprivileged `openlist` user (uid 100, gid 101).
Named volumes are seeded with that ownership on first use, but if you bind
mount a host directory (`-v /host/path:/data`), it must be writable by that
user — e.g. `chown -R 100:101 /host/path` — or run with
`--user $(id -u):$(id -g)` to match the directory's owner.

The publish workflow (below) automates the multi-arch build + push to GHCR.
It runs on pushes to main (refreshing `:latest`) and on version tags (which
also publish `:<version>`).

### Configuration

| Flag            | Env              | Default                  | Meaning                                              |
|-----------------|------------------|--------------------------|------------------------------------------------------|
| `-listen`       | `LISTEN_ADDR`    | `:5245`                  | Address the embedded server binds                    |
| `-loopback`     | `LOOPBACK_ADDR`  | `http://127.0.0.1:5245`  | http form of listen, for in-process privileged calls |
| `-data-dir`     | `DATA_DIR`       | `data`                   | OpenList data dir (config.json + its database)       |
| `-db`           | `DB_PATH`        | `data/openlist-ext.db`   | Extension's own SQLite database                      |
| `-admin-token`  | `ADMIN_TOKEN`    | (empty)                  | OpenList admin token for privileged in-process ops   |
| `-site-url`     | `SITE_URL`       | (empty = root)           | Public site URL or path prefix for subpath deploy    |
| `-reaper-interval` | —             | `60`                     | TTL reaper interval (seconds)                        |
| `-reaper-batch` | —                | `100`                    | TTL reaper batch size                                |
| —               | `OPENLIST_ADMIN_PASSWORD` | (random)      | Initial admin password on first run (OpenList core)  |

## Subpath deployment (behind caddy / nginx / httpd)

Set `SITE_URL` to serve the whole service under a path prefix — e.g. behind
Caddy, nginx, or Apache httpd at `https://example.com/files/`:

```bash
SITE_URL=/files ./openlist-ext
# or with a full URL (host part is ignored for routing, only the path matters):
SITE_URL=https://example.com/files ./openlist-ext
```

Every route — OpenList's own (`/api/*`, `/d/*`, `/p/*`, WebDAV at `/dav`,
the web UI) and the extension's (`/ext/*`, `/upload/*`) — is then served
under `/files`:
- The value is persisted to OpenList's `config.json` as `site_url`, which is
  OpenList's own supported subpath mechanism: routes mount under the prefix
  and `base_path` is injected into the frontend's `index.html`, so the
  built UI requests `/files/api/...` and loads its assets from `/files/assets/...`.
- The extension's feature middleware (API keys, domains, list permissions,
  TTL), the admin API, and the curl upload endpoint all work relative to
  the prefix; download links returned by `/upload/*` carry it.
- The service tolerates **both** proxy styles: proxies that pass the subpath
  through unchanged (nginx `proxy_pass http://backend:5245;`) and proxies
  that strip it before forwarding (nginx trailing-slash
  `proxy_pass http://backend:5245/;`, Caddy `handle_path`, Apache
  `ProxyPass /files/ http://backend:5245/`). Requests arriving without the
  prefix get it re-added before routing, so one `SITE_URL` value works with
  either configuration.

Proxy configs (with X-Forwarded-Host/Proto set so generated download links
use the public host and scheme):

```nginx
# nginx — pass the subpath through
location /files/ {
    proxy_pass http://backend:5245;
    proxy_set_header X-Forwarded-Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
# bare /files (no trailing slash) never matches location /files/, so
# redirect it explicitly; absolute_redirect keeps the Location under the
# subpath instead of the proxy's own host.
absolute_redirect off;
location = /files {
    return 301 /files/;
}

# nginx — strip the subpath (note the trailing slash)
location /files/ {
    proxy_pass http://backend:5245/;
    proxy_set_header X-Forwarded-Host $http_host;
    proxy_set_header X-Forwarded-Proto $scheme;
    # The service itself already emits /files-prefixed Locations (it knows
    # its SITE_URL), so rewrite from the prefixed backend URL — matching
    # backend root here would double the prefix (/files/files/...).
    proxy_redirect http://backend:5245/files/ /files/;
}
location = /files {
    return 301 /files/;
}
```

```
# Caddy — handle_path strips the prefix
handle_path /files/* {
    reverse_proxy backend:5245
}
redir /files /files/ 301
```

```apache
# Apache httpd — ProxyPass strips the prefix
ProxyPass        /files/ http://backend:5245/
# The service itself already emits /files-prefixed Locations (it knows its
# SITE_URL), so reverse-map from the prefixed backend URL — matching the
# backend root here would double the prefix (/files/files/...).
ProxyPassReverse /files/ http://backend:5245/files/
# bare /files (no trailing slash) is not matched by ProxyPass; redirect it
RedirectMatch 301 ^/files$ /files/
```

Pick a base path that is not one of the service's own route prefixes
(`/api`, `/d`, `/p`, `/ad`, `/sd`, `/dav`, `/s3`, `/ext`, `/upload`, ...):
behind a prefix-stripping proxy a base path like `/d` is ambiguous — the
stripped request `/d/file.txt` is indistinguishable from a prefixed one.

With WebDAV clients, mount the prefixed URL (`https://example.com/files/dav/`).
Storages configured to 302-redirect downloads emit their own (absolute)
link URLs, which are unaffected by the base path; the proxy examples above
rewrite those with `proxy_redirect` / `ProxyPassReverse` where needed.

## Admin API

All admin routes are under `/ext/admin` (under the base path on subpath
deployments, e.g. `/files/ext/admin`) and require the `Authorization:
<admin-token>` header (or `?token=<admin-token>`).

### Domain (feature 1)
- `POST   /ext/admin/domain` — `{"user_id":10,"host":"alice.example.com"}`
- `DELETE /ext/admin/domain?host=alice.example.com`
- `GET    /ext/admin/domain/:user_id` — list a user's domains

### TTL (feature 2)
- `PUT  /ext/admin/ttl/:user_id` — `{"enabled":true,"mode":"fixed|access","duration_seconds":3600}`
- `GET  /ext/admin/ttl/:user_id`
- `POST /ext/admin/ttl/sweep` — run one reaper sweep now, returns `{"deleted":N}`

### List permission (feature 3)
- `PUT /ext/admin/listperm/:user_id` — `{"can_list":false,"can_read":true}`
- `GET /ext/admin/listperm/:user_id`

### Load balance (feature 4)
- `POST   /ext/admin/lb/group` — `{"name":"g1","user_id":10,"strategy":"round_robin","path_prefix":""}`
- `POST   /ext/admin/lb/group/:id/member` — `{"mount_path":"/s3","weight":1}`
- `GET    /ext/admin/lb/group?user_id=10` (list) · `GET /ext/admin/lb/group/:id` (one)
- `DELETE /ext/admin/lb/group/:id`

To upload through a group: `PUT /api/fs/put` with header `X-LB-Group: g1`
(and `File-Path: /orig/name.txt`). The extension picks a member and rewrites
the path to `<mount><path_prefix>/<name>`.

### API keys (feature 5)
- `POST   /ext/admin/apikey` — `{"user_id":10,"name":"ci","scopes":["fs.get","fs.list"]}` → returns `{"secret":"..."}`
- `GET    /ext/admin/apikey/:user_id`
- `DELETE /ext/admin/apikey/:user_id/:id`
- `PUT    /ext/admin/apikey/:user_id/:id/enable` — `{"enabled":false}`

Clients present a key via `X-Api-Key: <secret>` or
`Authorization: Bearer <secret>`.

## Tests

```bash
# Unit + integration + e2e (stub-based; fast, no real OpenList boot).
go test ./...

# In-process OpenList boot test (creates files in a temp dir).
# Proves the standalone embedding: real OpenList routes served on the engine.
OPENLIST_INTEGRATION=1 go test ./internal/server/... -run TestIntegration -v
```

76 test functions across 7 packages. The e2e tests (`internal/server`)
exercise every feature end-to-end through the engine's HTTP surface using a
stub stand-in for OpenList's routes; the subpath tests cover both proxy
forms (prefix passed through and stripped), every download route family
(plain, proxy, archive, share), link generation for both `SITE_URL`
forms, and the production guard shape (real routes mounted on the engine —
denials must arrive before the route handler runs); the integration test
boots the **real** OpenList in-process under a `/files` subpath and
verifies `/files/ping` and `/files/ext/healthz` coexist on one engine.

## CI/CD

Three GitHub workflows cover build, publish, and end-to-end testing.

| Workflow | Repo | Trigger | What it does |
|---|---|---|---|
| `ci.yml` | openlist-ext | push/PR to `main` | `go vet` + `go build` + `go test -race` |
| `publish.yml` | openlist-ext | tag `v*` / dispatch | Multi-arch (`linux/amd64,linux/arm64`) Docker build + push to GHCR |
| `e2e.yml` | OpenList-Frontend | push/PR to `main` | Builds the backend binary + frontend dist, boots the backend, runs Playwright E2E against it — twice: at the origin root and under a subpath (`SITE_URL=/files`) |

### `ci.yml` — build & test

Runs on every push and pull request to `main`. Sets up Go from `go.mod`,
downloads deps, then `go vet ./...`, `go build ./...`, and
`go test -race -count=1 ./...`. Because the extension embeds OpenList
in-process, a clean build + test here guards both the extension code and the
public OpenList API surface it depends on.

### `publish.yml` — multi-arch image to GHCR

Triggered by `v*` tags (or `workflow_dispatch`). Checks out both the backend
(openlist-ext) and the frontend (OpenList-Frontend), sets up QEMU + buildx,
logs in to the GitHub Container Registry, and builds a
`linux/amd64,linux/arm64` image via `docker/build-push-action@v6`. Tags are
derived from the git tag (`v1.2.3` → `1.2.3`), plus `latest` and the short
SHA. The Dockerfile's `TARGETOS`/`TARGETARCH` ARGs are auto-injected by
buildx per platform, so no explicit `build-args` are needed.

Override the frontend repo/ref with the `OPENLIST_FRONTEND_REPO` and
`OPENLIST_FRONTEND_REF` repository variables when forking.

### `e2e.yml` — frontend E2E vs the backend

Lives in the OpenList-Frontend repo and runs on push/PR to `main`. It is the
end-to-end check that the frontend works against a real openlist-ext backend:

1. Check out the frontend and the backend (into `./backend`).
2. Build the `openlist-ext` binary (`go build`).
3. `pnpm install` + `pnpm build` the frontend dist.
4. Seed the dist into the backend's data dir so `BootOpenList` serves the
   real UI (with the Extension section) instead of the placeholder.
5. Start the backend with `OPENLIST_ADMIN_PASSWORD=admin` (deterministic
   login: `admin` / `admin`).
6. Wait for `/ext/healthz` to return 200.
7. `pnpm e2e:install` (Playwright chromium) + `pnpm e2e`.

The whole matrix runs twice: at the origin root and under a base path
(`SITE_URL=/files`), matching a deployment behind caddy / nginx / Apache
httpd at `https://example.com/files/`. The subpath leg passes
`E2E_BASE_PATH=/files`, which prefixes every in-app path in the specs and
activates `e2e/subpath.spec.ts` (base-path injection into index.html,
prefixed asset loading, no request escaping the prefix, and the index.html
guard that redirects a direct visit to the host root into the prefix).

The Playwright suite (`e2e/extension.spec.ts`) verifies: `/ext/healthz`
returns 200; `/` serves the built frontend; the **Extension** menu group is
visible when `/ext/healthz` is reachable; and it is **hidden** when
`/ext/healthz` is intercepted as 404 (forward compatibility with stock
OpenList, which has no `/ext/*`). Selectors are English text-based, pinned by
forcing locale `en-US` and `localStorage("lang") = "en"`.

Override the backend repo/ref with the `OPENLIST_BACKEND_REPO` and
`OPENLIST_BACKEND_REF` repository variables when forking.

## Project layout

```
main.go                      entry point: boots OpenList, serves one engine
internal/
  config/                    flags + env config
  db/                        extension SQLite (separate schema), model registry
  domain/                    feature 1: per-user domain bindings
  ttl/                       feature 2: TTL config, file records, reaper
  listperm/                  feature 3: per-user list/read permission
  lb/                        feature 4: LB groups, members, selection
  apikey/                    feature 5: API keys, scopes, verify
  openlist/                  loopback HTTP client for privileged ops
  server/                    engine: server.Init + feature middleware + admin API
    openlist_mount.go        BootOpenList + olInit (public-packages-only embed)
    integration_openlist_test.go  real in-process OpenList boot test
  testutil/                  test DB + stub OpenList
```
