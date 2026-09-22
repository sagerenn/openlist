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
// (cmd, cmd/flags, server, server/handles, server/middlewares,
// server/common). No internal/ package is imported, so OpenList may change
// its internals freely; as long as server.Init(e) and cmd.Init() keep their
// signatures, the extension keeps working.
package server

import (
	"context"
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
// LoopbackAddr (the address the engine will listen on) and the admin token.
// When AdminPassword is set, a TokenManager is created so the extension can
// obtain and refresh a real admin JWT for forwarding to OpenList's core
// (which only accepts JWTs, not the static AdminToken).
func New(cfg config.Config) (*Server, error) {
	s := &Server{
		Config: cfg,
		Client: openlist.New(cfg.LoopbackAddr, cfg.AdminToken),
	}
	if cfg.AdminPassword != "" {
		s.tokens = openlist.NewTokenManager(s.Client, cfg.AdminUser, cfg.AdminPassword)
		s.Client.SetTokenManager(s.tokens)
	}
	s.forward = func(c *gin.Context) { c.Next() }
	return s, nil
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
	if mountOpenList {
		olInit(r)
	}

	// Extension health + admin API. The feature middleware above skips
	// /ext/* paths, so these are unaffected by it.
	r.GET("/ext/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	admin := r.Group("/ext/admin", s.adminAuth())
	s.registerAdminRoutes(admin)

	return r
}

// featureMiddleware is the ordered feature chain. It short-circuits
// (Abort) when a policy denies; otherwise it records TTL events around the
// forwarder (OpenList's routes in production).
func (s *Server) featureMiddleware() gin.HandlerFunc {
	apiKey := s.apiKeyAuth()
	domain := s.domainAuth()
	listPerm := s.listPermGuard()
	return func(c *gin.Context) {
		// Never touch extension-internal paths.
		if strings.HasPrefix(c.Request.URL.Path, "/ext/") {
			c.Next()
			return
		}
		// Only engage the feature chain on file-operation paths. Other
		// OpenList routes (auth, admin, webdav, etc.) pass straight through.
		if !isFileOpPath(c.Request.URL.Path) {
			c.Next()
			return
		}
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
		// Load-balanced uploads are handled entirely in-process by lbUpload
		// (it writes the response and aborts); otherwise apply TTL wrapping
		// around the forwarder.
		if isLBUpload(c) {
			s.lbUpload(c)
			return
		}
		s.applyTTL(c, s.forward)
	}
}

// isFileOpPath reports whether a path is a file operation the feature chain
// governs: /api/fs/* (list/get/put/remove/mkdir/...), /d/* and /p/*
// downloads.
func isFileOpPath(p string) bool {
	if strings.HasPrefix(p, "/api/fs/") {
		return true
	}
	if strings.HasPrefix(p, "/d/") || strings.HasPrefix(p, "/p/") {
		return true
	}
	return false
}

// isLBUpload reports whether the current request is an upload that should
// be load-balanced. The caller signals this by setting the
// "X-LB-Group" header (or query param) on the upload request.
func isLBUpload(c *gin.Context) bool {
	if c.Request.Method != http.MethodPut {
		return false
	}
	if !strings.HasSuffix(c.Request.URL.Path, "/fs/put") {
		return false
	}
	return c.GetHeader("X-LB-Group") != "" || c.Query("lb_group") != ""
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
