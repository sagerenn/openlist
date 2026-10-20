package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/domain"
)

// context keys for the acting identity resolved by the extension, and for
// the request's path normalization computed once by the feature
// middleware (see Server.relPath / Server.pathKind).
const (
	ctxUserID   = "ext.user_id"
	ctxAPIKey   = "ext.api_key"
	ctxAuthSrc  = "ext.auth_src"  // "apikey" | "domain" | "token" | "guest"
	ctxRelPath  = "ext.rel_path"  // base-path-relative request path
	ctxPathKind = "ext.path_kind" // pathKind of the request
)

// apiKeyAuth resolves an API key presented in the request to a user. On
// success it stores the user ID and the key in the context and sets the
// Authorization header to the admin token so the downstream OpenList call
// executes as admin (the extension enforces the key's scopes itself). If
// no key is present it does nothing (the next middleware may resolve a
// domain or fall through to guest).
//
// It is a pure guard, not a standalone middleware: it must only return
// (or Abort) — never call c.Next(). featureMiddleware invokes it directly,
// and a c.Next() from inside it would advance the engine's handler chain
// into the route handler BEFORE the remaining guards (domain, listPerm)
// have run: a user denied listing would still receive the full listing
// body, with the 403 appended after it. Only the orchestrator advances
// the chain (via applyTTL's forward).
func (s *Server) apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, err := apikey.FromHeader(c.GetHeader)
		if err != nil {
			return
		}
		k, err := apikey.Verify(secret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid api key"})
			return
		}
		c.Set(ctxUserID, k.UserID)
		c.Set(ctxAPIKey, &k)
		c.Set(ctxAuthSrc, "apikey")
		// Forward as admin so OpenList performs the actual file op; the
		// extension enforces the key's scope in listPermGuard / a scope
		// check below. OpenList's core Auth middleware only accepts real
		// admin JWTs (not the static AdminToken), so use the refreshable
		// admin JWT from the TokenManager when available.
		c.Request.Header.Set("Authorization", s.adminToken(c.Request.Context()))
		if !s.checkScope(c, k) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "api key scope denied"})
			return
		}
	}
}

// scopeForPath maps a request path + method to the required API key scope.
// Paths are OpenList's own (/api/fs/*).
func scopeForPath(method, path string) apikey.Scope {
	switch {
	case strings.HasPrefix(path, "/api/fs/list"):
		return apikey.ScopeList
	case strings.HasPrefix(path, "/api/fs/get"):
		return apikey.ScopeRead
	case method == http.MethodPut && (strings.HasSuffix(path, "/fs/put") || strings.HasSuffix(path, "/fs/form")):
		return apikey.ScopeWrite
	case strings.HasPrefix(path, "/api/fs/remove"):
		return apikey.ScopeRemove
	case strings.HasPrefix(path, "/api/fs/mkdir"):
		return apikey.ScopeMkdir
	default:
		return apikey.ScopeAll
	}
}

// checkScope verifies the resolved API key has the scope required by the
// current request. Returns true if no key is in play. The path is the
// base-path-relative one the feature middleware published, so subpath
// deployments (<base>/api/fs/...) map to the same scopes.
func (s *Server) checkScope(c *gin.Context, k apikey.APIKey) bool {
	required := scopeForPath(c.Request.Method, s.relPath(c))
	if required == apikey.ScopeAll {
		return true
	}
	if k.HasScope(required) {
		return true
	}
	return false
}

// domainAuth resolves the request Host to a user via the domain binding
// when no API key was applied. The resolved user ID is stored in the
// context; the request is forwarded as guest (no token) so OpenList
// applies guest permissions, but the extension's listPermGuard and
// ttlGuard use the resolved user ID to enforce per-user policy.
//
// Like apiKeyAuth it is a pure guard orchestrated by featureMiddleware:
// it never calls c.Next() (see apiKeyAuth's doc comment for why that
// would run the route handler before the remaining guards).
func (s *Server) domainAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Get(ctxUserID); ok {
			return
		}
		host := c.Request.Host
		if host == "" {
			host = c.GetHeader("Host")
		}
		uid, ok := domain.LookupUserID(host)
		if !ok {
			c.Set(ctxAuthSrc, "guest")
			return
		}
		c.Set(ctxUserID, uid)
		c.Set(ctxAuthSrc, "domain")
		// Do NOT set admin token: forward as guest so OpenList enforces
		// guest visibility. The extension still tracks per-user policy
		// by uid.
	}
}
