package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/domain"
)

// context keys for the acting identity resolved by the extension.
const (
	ctxUserID  = "ext.user_id"
	ctxAPIKey  = "ext.api_key"
	ctxAuthSrc = "ext.auth_src" // "apikey" | "domain" | "token" | "guest"
)

// apiKeyAuth resolves an API key presented in the request to a user. On
// success it stores the user ID and the key in the context and sets the
// Authorization header to the admin token so the downstream OpenList call
// executes as admin (the extension enforces the key's scopes itself). If
// no key is present it does nothing (the next middleware may resolve a
// domain or fall through to guest).
func (s *Server) apiKeyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		secret, err := apikey.FromHeader(c.GetHeader)
		if err != nil {
			c.Next()
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
		// check below.
		c.Request.Header.Set("Authorization", s.Config.AdminToken)
		if !s.checkScope(c, k) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "api key scope denied"})
			return
		}
		c.Next()
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
// current request. Returns true if no key is in play.
func (s *Server) checkScope(c *gin.Context, k apikey.APIKey) bool {
	required := scopeForPath(c.Request.Method, c.Request.URL.Path)
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
func (s *Server) domainAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := c.Get(ctxUserID); ok {
			c.Next()
			return
		}
		host := c.Request.Host
		if host == "" {
			host = c.GetHeader("Host")
		}
		uid, ok := domain.LookupUserID(host)
		if !ok {
			c.Set(ctxAuthSrc, "guest")
			c.Next()
			return
		}
		c.Set(ctxUserID, uid)
		c.Set(ctxAuthSrc, "domain")
		// Do NOT set admin token: forward as guest so OpenList enforces
		// guest visibility. The extension still tracks per-user policy
		// by uid.
		c.Next()
	}
}
