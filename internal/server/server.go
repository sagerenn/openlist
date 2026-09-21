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
}

// New constructs a Server. The loopback client is built from the config's
// LoopbackAddr (the address the engine will listen on) and the admin token.
func New(cfg config.Config) (*Server, error) {
	s := &Server{
		Config: cfg,
		Client: openlist.New(cfg.LoopbackAddr, cfg.AdminToken),
	}
	s.forward = func(c *gin.Context) { c.Next() }
	return s, nil
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

	// Mount OpenList's full route tree onto this engine. In production this
	// is what makes the extension "work alone" — OpenList runs in-process.
	if mountOpenList {
		olInit(r)
	}

	// Extension health + admin API (explicit routes; the feature middleware
	// below skips /ext/* paths so these are unaffected).
	r.GET("/ext/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	admin := r.Group("/ext/admin", s.adminAuth())
	s.registerAdminRoutes(admin)

	// Feature middleware runs globally, before OpenList's route handlers. It
	// only acts on file-operation paths (/api/fs/*, /d/*, /p/*) and skips
	// /ext/* internal paths. For everything else it calls c.Next() into
	// OpenList's own handlers (or the test stub forwarder).
	r.Use(s.featureMiddleware())

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

// adminAuth guards the admin API with the configured admin token.
func (s *Server) adminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		tok := c.GetHeader("Authorization")
		if tok == "" {
			tok = c.Query("token")
		}
		if tok == "" || tok != s.Config.AdminToken {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "unauthorized"})
			return
		}
		c.Next()
	}
}
