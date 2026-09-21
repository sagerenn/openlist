package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/listperm"
	"github.com/sagerenn/openlist/internal/ttl"
)

// listPermGuard enforces per-user file-listing permission (feature 3).
//
// For directory-listing requests (/fs/list) by a resolved user with
// CanList=false, it denies the request. For file-read requests (/fs/get,
// /d/*, /p/*) by a user with CanRead=false, it denies. Write operations
// are unaffected. Requests without a resolved user (true guests) are
// passed through to OpenList's own guest handling.
//
// It does NOT call c.Next(); the orchestrator (featureMiddleware) advances
// the chain. It returns nil to continue, or calls c.Abort and returns nil
// to deny.
func (s *Server) listPermGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := c.Get(ctxUserID)
		if !ok {
			return
		}
		userID := uid.(uint)
		p, err := listperm.Get(userID)
		if err != nil {
			return
		}
		path := c.Request.URL.Path

		// Listing endpoint.
		if strings.HasPrefix(path, "/api/fs/list") {
			if !p.CanList {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"code":    403,
					"message": "listing is disabled for this user",
				})
				return
			}
		}
		// Direct read endpoints.
		if isReadPath(path) && !p.CanRead {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"message": "read is disabled for this user",
			})
			return
		}
	}
}

// isReadPath reports whether the path targets a file read (not a write or
// listing).
func isReadPath(p string) bool {
	return strings.HasPrefix(p, "/api/fs/get") ||
		strings.HasPrefix(p, "/d/") ||
		strings.HasPrefix(p, "/p/")
}

// applyTTL records TTL events around a forwarded file operation (feature 2).
//
//   - On download (/d/*, /p/*, /fs/get with link): refresh the access time
//     for the path (ModeAccess sliding window), done before forwarding.
//   - On upload (PUT /fs/put, /fs/form): after a successful forwarded upload,
//     record a TTL entry for the uploaded path (if the user has TTL on).
//
// forward is the terminal handler that actually serves the request
// (OpenList's own route in production, a stub in tests).
func (s *Server) applyTTL(c *gin.Context, forward gin.HandlerFunc) {
	path := c.Request.URL.Path
	method := c.Request.Method

	// Download: refresh access time before forwarding. No-op if the file has
	// no TTL record.
	if isDownloadPath(path) {
		fp := downloadPath(c)
		if fp != "" {
			_ = ttl.RecordAccess(fp, time.Now())
		}
		forward(c)
		return
	}

	// Upload: wrap the ResponseWriter so we can record TTL only on 2xx.
	if method == http.MethodPut && (strings.HasSuffix(path, "/fs/put") || strings.HasSuffix(path, "/fs/form")) {
		rw := &statusRecorder{ResponseWriter: c.Writer, status: 200}
		c.Writer = rw
		forward(c)
		if rw.status >= 200 && rw.status < 300 {
			uid, ok := c.Get(ctxUserID)
			if ok {
				filePath := c.GetHeader("File-Path")
				if filePath == "" {
					filePath = c.Query("file_path")
				}
				if filePath != "" {
					_ = ttl.RecordUpload(uid.(uint), filePath, time.Now())
				}
			}
		}
		return
	}

	forward(c)
}

// statusRecorder captures the response status code.
type statusRecorder struct {
	gin.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.ResponseWriter.Write(b)
}

// isDownloadPath reports whether the path is a file download.
func isDownloadPath(p string) bool {
	return strings.HasPrefix(p, "/d/") || strings.HasPrefix(p, "/p/") || strings.HasPrefix(p, "/api/fs/get")
}

// downloadPath extracts the logical file path from a download request URL.
func downloadPath(c *gin.Context) string {
	p := c.Request.URL.Path
	for _, prefix := range []string{"/d/", "/p/", "/api/fs/get"} {
		if strings.HasPrefix(p, prefix) {
			rest := strings.TrimPrefix(p, prefix)
			if rest == "" {
				return ""
			}
			return "/" + strings.TrimPrefix(rest, "/")
		}
	}
	return ""
}
