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
there to serve the real UI.

### Configuration

| Flag            | Env              | Default                  | Meaning                                              |
|-----------------|------------------|--------------------------|------------------------------------------------------|
| `-listen`       | `LISTEN_ADDR`    | `:5245`                  | Address the embedded server binds                    |
| `-loopback`     | `LOOPBACK_ADDR`  | `http://127.0.0.1:5245`  | http form of listen, for in-process privileged calls |
| `-data-dir`     | `DATA_DIR`       | `data`                   | OpenList data dir (config.json + its database)       |
| `-db`           | `DB_PATH`        | `data/openlist-ext.db`   | Extension's own SQLite database                      |
| `-admin-token`  | `ADMIN_TOKEN`    | (empty)                  | OpenList admin token for privileged in-process ops   |
| `-reaper-interval` | —             | `60`                     | TTL reaper interval (seconds)                        |
| `-reaper-batch` | —                | `100`                    | TTL reaper batch size                                |

## Admin API

All admin routes are under `/ext/admin` and require the `Authorization:
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

48 test functions across 7 packages. The e2e tests (`internal/server`)
exercise every feature end-to-end through the engine's HTTP surface using a
stub stand-in for OpenList's routes; the integration test boots the **real**
OpenList in-process and verifies `/ping` and `/ext/healthz` coexist on one
engine.

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
