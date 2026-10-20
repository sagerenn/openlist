// Package server is the extension's public face: it embeds OpenList in the
// same process and layers six features on top of OpenList's own gin routes.
//
// The extension does NOT run as a reverse proxy in front of a separate
// OpenList. Instead:
//
//   - main boots OpenList's internals in-process via cmd.Init() (which runs
//     bootstrap.Init: config, DB, storages, data).
//   - Engine() creates a gin engine, calls OpenList's public server.Init(e)
//     to mount OpenList's full route tree (/api/*, /d/*, /p/*, ...), then
//     registers the extension's feature middleware and admin API on the same
//     engine.
//   - The feature middleware (apiKeyAuth -> domainAuth -> listPermGuard ->
//     ttlGuard) runs as global middleware BEFORE OpenList's route handlers.
//     For requests it does not care about it simply calls c.Next() and
//     OpenList's own handlers serve the response. There is no HTTP proxy.
//   - Privileged in-process operations (TTL reaper deletes, load-balanced
//     uploads) are performed by the openlist.Client pointed at the engine's
//     own loopback address (http://127.0.0.1:<listen-port>) using the admin
//     token — a real in-process call, not a second OpenList instance.
//
// The admin API (under /ext/admin) lets an admin configure domains, TTL,
// list permissions, LB groups, and API keys per user.
//
// Forward compatibility: only OpenList public packages are imported
// (cmd, cmd/flags, pkg/utils, server, server/handles, server/middlewares,
// server/common). No internal/ package is imported, so OpenList may change
// its internals freely; as long as server.Init(e) and cmd.Init() keep their
// signatures, the extension keeps working.
package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/config"
	"github.com/sagerenn/openlist/internal/openlist"
)

// Server holds the extension server's dependencies.
type Server struct {
	Config config.Config
	Client *openlist.Client
	// basePath is the URL path prefix the whole service is served under
	// ("" for root deployments, "/prefix" otherwise). It mirrors the path
	// component of OpenList's site_url: server.Init mounts OpenList's routes
	// under the same prefix, so the extension's routes, path checks, and
	// generated links must all include it too.
	basePath string
	// forward is the terminal handler invoked after the feature middleware
	// chain passes a request through. In production it is c.Next(), letting
	// OpenList's own routes (mounted via server.Init) serve the request. In
	// tests it may be a proxy to a stub OpenList. It is never an HTTP call
	// to a separate running OpenList in production.
	forward gin.HandlerFunc

	// tokens mints and caches a real OpenList admin JWT for privileged
	// in-process operations (reaper deletes, LB uploads, and forwarding
	// API-key-authenticated file ops to OpenList's core as admin). When
	// disabled (no admin password), callers fall back to Config.AdminToken,
	// which must then itself be a valid admin JWT.
	tokens *openlist.TokenManager
}

// New constructs a Server. The loopback client is built from the config's
// LoopbackAddr (the address the engine will listen on) and the admin token;
// when a base path is configured the client targets the prefixed location,
// because OpenList's routes are mounted under it. When AdminPassword is set,
// a TokenManager is created so the extension can obtain and refresh a real
// admin JWT for forwarding to OpenList's core (which only accepts JWTs, not
// the static AdminToken).
func New(cfg config.Config) (*Server, error) {
	base := cfg.BasePath()
	if base != "" {
		if err := validateBasePath(base); err != nil {
			return nil, err
		}
	}
	loopback := strings.TrimRight(cfg.LoopbackAddr, "/")
	if base != "" {
		// Routes live under the base path, so the in-process loopback
		// calls (/api/me, /api/fs/put, ...) must hit the prefixed paths
		// (e.g. http://127.0.0.1:5245/files/api/me). An operator-supplied
		// loopback address that already carries the path is not
		// double-prefixed.
		loopback = strings.TrimRight(strings.TrimSuffix(loopback, base), "/") + base
	}
	s := &Server{
		Config:   cfg,
		basePath: base,
		Client:   openlist.New(loopback, cfg.AdminToken),
	}
	if cfg.AdminPassword != "" {
		s.tokens = openlist.NewTokenManager(s.Client, cfg.AdminUser, cfg.AdminPassword)
		s.Client.SetTokenManager(s.tokens)
	}
	s.forward = func(c *gin.Context) { c.Next() }
	return s, nil
}

// reservedBaseSegments are the top-level route segments served under the
// base path: OpenList's own (server/router.go: ping, favicon.ico, robots.txt,
// manifest.json, i, dav, s3, mcp, d, p, ad, ap, ae, sd, sad, api — debug only
// under debug/dev flags) plus the extension's (ext, upload). Kept in sync
// with the route surface validateBasePath guards against.
var reservedBaseSegments = map[string]struct{}{
	"ping": {}, "favicon.ico": {}, "robots.txt": {}, "manifest.json": {},
	"i": {}, "dav": {}, "s3": {}, "mcp": {}, "debug": {},
	"d": {}, "p": {}, "ad": {}, "ap": {}, "ae": {}, "sd": {}, "sad": {},
	"api": {},
	"ext": {}, "upload": {},
}

// validateBasePath refuses a base path whose first segment is one of the
// top-level route segments above. The Handler wrapper decides between the
// two proxy forms by asking "does the path already start with the base?":
// a stripped request /d/file.txt (public /d/d/file.txt behind a
// prefix-stripping proxy) starts with the base /d, so the wrapper would
// route it as an already-prefixed path and the engine would 404 — the two
// proxy forms are indistinguishable exactly when the base begins with a
// route segment. Any other base (/files, /files/d, /my-site) is
// unambiguous, because no stripped route path (/d/..., /api/..., /ext/...)
// starts with it.
func validateBasePath(base string) error {
	first := strings.TrimPrefix(base, "/")
	if i := strings.Index(first, "/"); i >= 0 {
		first = first[:i]
	}
	if _, bad := reservedBaseSegments[first]; bad {
		return fmt.Errorf("SITE_URL base path %q collides with the top-level route segment %q: behind a prefix-stripping proxy, %s requests are indistinguishable from already-prefixed ones and would 404 — choose a base path that is not itself a route segment (e.g. /files)", base, first, "/"+first)
	}
	return nil
}

// stripBase removes the configured base path prefix from p, returning the
// path the feature middleware and guards match on (OpenList's own route
// paths). Paths that do not carry the prefix are returned unchanged.
func (s *Server) stripBase(p string) string {
	if s.basePath == "" {
		return p
	}
	if p == s.basePath {
		return "/"
	}
	if strings.HasPrefix(p, s.basePath+"/") {
		return p[len(s.basePath):]
	}
	return p
}

// relPath returns the current request's URL path relative to the base path
// — the path OpenList's own routes see. The feature middleware computes it
// once per request and publishes it via ctxRelPath; when that is absent
// (the middleware has not run yet) it is derived from the request.
func (s *Server) relPath(c *gin.Context) string {
	if v, ok := c.Get(ctxRelPath); ok {
		return v.(string)
	}
	return s.stripBase(c.Request.URL.Path)
}

// pathKind returns the path classification the feature middleware published
// for this request; kindOther before the middleware has run.
func (s *Server) pathKind(c *gin.Context) pathKind {
	if v, ok := c.Get(ctxPathKind); ok {
		return v.(pathKind)
	}
	return kindOther
}

// adminToken returns a valid admin JWT for forwarding to OpenList's core.
// It prefers the refreshable TokenManager (logging in if needed); when that
// is disabled it falls back to the static Config.AdminToken (which must then
// be a valid admin JWT). On error it returns the static token as a
// best-effort so the request still proceeds (and fails with a clear 401 if
// the token is invalid).
func (s *Server) adminToken(ctx context.Context) string {
	if s.tokens != nil && s.tokens.Enabled() {
		if tok, err := s.tokens.Get(ctx); err == nil {
			return tok
		}
	}
	return s.Config.AdminToken
}

// SetForwarder overrides the terminal forward handler. Used by tests to
// direct non-intercepted requests to a stub instead of OpenList's real
// routes. Production code does not call this.
func (s *Server) SetForwarder(fn gin.HandlerFunc) {
	if fn != nil {
		s.forward = fn
	}
}

// Engine builds and returns the configured gin engine with OpenList's routes
// (mounted via server.Init) plus the extension's feature middleware and admin
// API. If mountOpenList is false, OpenList's routes are not mounted (used by
// tests that supply a stub forwarder).
func (s *Server) Engine(mountOpenList bool) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// Feature middleware must be registered on the engine BEFORE OpenList's
	// routes are mounted. gin's RouterGroup captures a snapshot of the
	// parent's handler chain at Group()/registration time (combineHandlers
	// copies the slice), so middleware added via Use() AFTER olInit(r) never
	// reaches OpenList's already-registered routes — which would leave API
	// keys, list-permission, and TTL enforcement silently inactive on every
	// /api/fs/* call. Registering it first ensures it is copied into every
	// route group OpenList creates. The middleware itself is a no-op
	// (c.Next()) for paths it does not govern (/ext/*, auth, admin, ...).
	r.Use(s.featureMiddleware())

	// Mount OpenList's full route tree onto this engine. In production this
	// is what makes the extension "work alone" — OpenList runs in-process.
	// With a base path configured, OpenList's server.Init mounts everything
	// under e.Group(conf.URL.Path) (from site_url), i.e. under the same
	// prefix as the extension's own routes below.
	if mountOpenList {
		olInit(r)
	}

	// Extension health + admin API + curl upload, mounted under the same
	// base path as OpenList's routes (a no-op group at root deployments).
	// The feature middleware above skips /ext/* paths, so those are
	// unaffected by it.
	g := r.Group(s.basePath)
	g.GET("/ext/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	admin := g.Group("/ext/admin", s.adminAuth())
	s.registerAdminRoutes(admin)

	// curl-friendly streaming upload + download link (PUT/POST /upload/<path>).
	// It is NOT under /ext/ so it stays outside the admin guard; it performs
	// its own API-key/admin-token auth. It is also not a file-op path, so the
	// feature middleware passes it straight through to this handler.
	upload := s.curlUpload
	g.PUT("/upload", upload)
	g.PUT("/upload/*path", upload)
	g.POST("/upload", upload)
	g.POST("/upload/*path", upload)

	return r
}

// Handler returns the http.Handler for the whole service: the gin engine
// built by Engine(mountOpenList), wrapped for subpath deployments.
//
// Reverse proxies differ in whether they pass the subpath through or strip
// it: nginx "location /files/ { proxy_pass http://backend; }" forwards
// /files/api/... unchanged, while a trailing-slash proxy_pass, caddy's
// handle_path, and Apache's ProxyPass all strip the prefix and deliver
// /api/... . The engine routes are registered under the base path either
// way, so the wrapper re-adds the prefix to requests that arrive without
// it before the engine routes them — both proxy styles then work with the
// same SITE_URL setting, and URLs the service generates (download links,
// redirects) always carry the prefix.
func (s *Server) Handler(mountOpenList bool) http.Handler {
	return s.subpathHandler(s.Engine(mountOpenList))
}

// subpathHandler wraps the engine for subpath deployments; root deployments
// get the engine untouched. Split from Handler so tests can wrap an engine
// carrying their own routes.
func (s *Server) subpathHandler(engine http.Handler) http.Handler {
	if s.basePath == "" {
		return engine
	}
	prefix := s.basePath
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// gin's redirectTrailingSlash prepends X-Forwarded-Prefix to the
		// redirect target it builds, and every route here is ALREADY under
		// that prefix — so a proxy that sets the header (Traefik, some
		// nginx setups) would produce /files/files/ping from a /files/ping/
		// request. The header is a proxy→app signal this engine has no use
		// for, so drop it on a clone of the request before routing. A
		// clone, because handlers must not modify the request they are
		// given (the same rule the re-prefixing branch below follows).
		if r.Header.Get("X-Forwarded-Prefix") != "" {
			r2 := r.Clone(r.Context())
			r2.Header.Del("X-Forwarded-Prefix")
			r = r2
		}
		if r.RequestURI == "*" ||
			r.URL.Path == prefix ||
			strings.HasPrefix(r.URL.Path, prefix+"/") {
			engine.ServeHTTP(w, r)
			return
		}
		// The proxy stripped the subpath; re-add it before routing — on a
		// CLONE of the request, mirroring http.StripPrefix: net/http's
		// contract says handlers must not modify the provided *http.Request,
		// and readers of r.RequestURI (OpenList's static asset cache
		// headers, logging, dumps) expect a request-target consistent with
		// r.URL. Only origin-form targets get a rewritten RequestURI;
		// absolute-form requests (rare, from misconfigured proxies) keep
		// theirs, since it is not the path being routed. Path and RawPath
		// get the same constant prefix so escaped forms stay valid (RawPath
		// is only set when the request used non-canonical escapes, e.g.
		// %2F).
		r2 := r.Clone(r.Context())
		r2.URL.Path = prefix + r.URL.Path
		if r.URL.RawPath != "" {
			r2.URL.RawPath = prefix + r.URL.RawPath
		}
		if r2.RequestURI != "" && r2.RequestURI[0] == '/' {
			r2.RequestURI = prefix + r2.RequestURI
		}
		engine.ServeHTTP(w, r2)
	})
}

// featureMiddleware is the ordered feature chain. It short-circuits
// (Abort) when a policy denies; otherwise it records TTL events around the
// forwarder (OpenList's routes in production).
func (s *Server) featureMiddleware() gin.HandlerFunc {
	apiKey := s.apiKeyAuth()
	domain := s.domainAuth()
	listPerm := s.listPermGuard()
	return func(c *gin.Context) {
		// Normalize the request path ONCE, relative to the base path —
		// under a subpath deployment every request arrives as
		// <base>/api/..., so matching OpenList's route paths requires the
		// prefix stripped first — and publish path + classification in the
		// context. Guards consume those (relPath / pathKind) instead of
		// each re-reading c.Request.URL.Path, so a future guard cannot
		// accidentally work on the prefixed form.
		rel := s.stripBase(c.Request.URL.Path)
		c.Set(ctxRelPath, rel)
		kind := classifyPath(rel)
		c.Set(ctxPathKind, kind)
		// Never touch extension-internal paths; engage the feature chain
		// only on file-operation paths. Other OpenList routes (auth,
		// admin, webdav, ...) pass straight through.
		if kind == kindExt || kind == kindOther {
			c.Next()
			return
		}
		// Cross-origin access for the governed paths: this extension exists
		// to serve browser frontends hosted on other origins (the static
		// pshare site) with bearer API keys, but responses the feature
		// chain writes itself — LB-path upload results, guard denials —
		// never reach OpenList's engine-level cors middleware, which is
		// registered later in the chain (inside olInit). A cross-origin
		// upload would succeed at the server yet be unreadable by the page:
		// fetch rejects on the missing Access-Control-Allow-Origin and the
		// frontend reports a network failure (the OPTIONS preflight passes
		// through to OpenList's cors, which is why only the real response's
		// header goes missing). Set it here so every governed response
		// carries it — including pass-through requests, on which OpenList's
		// own cors middleware then re-Sets the header per its configured
		// policy, so its configured value still governs there.
		c.Header("Access-Control-Allow-Origin", "*")
		apiKey(c)
		if c.IsAborted() {
			return
		}
		domain(c)
		if c.IsAborted() {
			return
		}
		listPerm(c)
		if c.IsAborted() {
			return
		}
		// Load-balanced uploads are handled entirely in-process by lbUpload,
		// which writes the response and aborts (aborting is what keeps gin
		// from continuing into the route handler behind it — the request
		// body is already consumed). Reads/removes under a group prefix get
		// their fs body rewritten before forwarding; a rewritten remove
		// purges the affected upload records once the remove succeeds.
		// Everything else applies TTL wrapping around the forwarder.
		if s.isLBUpload(c) {
			s.lbUpload(c)
			return
		}
		if purge := s.rewriteLBRead(c); purge != nil {
			rw := &statusRecorder{ResponseWriter: c.Writer, status: 200}
			c.Writer = rw
			s.applyTTL(c, s.forward)
			if rw.status >= 200 && rw.status < 300 {
				purgeUploadRecords(purge)
			}
			return
		}
		s.applyTTL(c, s.forward)
	}
}

// pathKind classifies a request's URL path — base-path-relative, i.e.
// OpenList's own route path — for the feature middleware. The zero value
// kindOther means "not a path the feature chain governs".
type pathKind int

const (
	kindOther    pathKind = iota
	kindExt               // the extension's own /ext/* routes
	kindAPIFS             // /api/fs/* (list/get/put/remove/mkdir/...)
	kindDownload          // direct file downloads: /d, /p, /ad, /ap, /ae, /sd, /sad
)

// classifyPath maps a base-path-relative request path to its pathKind. The
// download prefixes enumerate exactly the direct download routes OpenList's
// server/router.go registers: plain downloads /d and proxy downloads /p,
// archive downloads /ad (download), /ap (proxy), /ae (in-archive extract),
// and share downloads /sd and /sad — kept in sync with the same list as
// isDownloadPath. If OpenList adds a download route, this classifier (and
// only it) needs the new prefix.
func classifyPath(p string) pathKind {
	if strings.HasPrefix(p, "/ext/") {
		return kindExt
	}
	if strings.HasPrefix(p, "/api/fs/") {
		return kindAPIFS
	}
	if isDownloadPath(p) {
		return kindDownload
	}
	return kindOther
}

// isLBUpload reports whether the current request is an upload that should
// be load-balanced. Activation is either explicit — the X-LB-Group header
// (or query param) on the upload request — or by path: the (decoded)
// File-Path falls under one of the resolved user's group prefixes, in
// which case the group routes it with no header at all (see lb_route.go).
func (s *Server) isLBUpload(c *gin.Context) bool {
	if c.Request.Method != http.MethodPut {
		return false
	}
	if s.pathKind(c) != kindAPIFS || !strings.HasSuffix(s.relPath(c), "/fs/put") {
		return false
	}
	if lbGroupName(c) != "" {
		return true
	}
	return s.lbPathRoute(c) != nil
}

// adminAuth guards the admin API. It accepts either:
//   - the configured static admin token (Config.AdminToken), or
//   - a valid OpenList admin user's session token, validated in-process via
//     the loopback client's /api/me (role == ADMIN).
//
// The second path lets the OpenList-Frontend manage the extension features
// with the same token the logged-in admin already uses for OpenList, so no
// separate extension token needs to be configured or entered.
func (s *Server) adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := c.GetHeader("Authorization")
		if tok == "" {
			tok = c.Query("token")
		}
		if tok == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "unauthorized"})
			return
		}
		if tok == s.Config.AdminToken && s.Config.AdminToken != "" {
			c.Next()
			return
		}
		if s.Client != nil && s.Client.IsAdminToken(c.Request.Context(), tok) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "unauthorized"})
	}
}
