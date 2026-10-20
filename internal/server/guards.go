package server

import (
	"net/http"
	"net/url"
	"strconv"
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
		// The feature middleware publishes the base-path-relative path in
		// the context; matching on it (rather than the raw request path)
		// makes the guard fire identically on subpath deployments, where
		// requests arrive as <base>/api/fs/... .
		path := s.relPath(c)

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
		// Read endpoints (file info and every direct download form).
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
	return strings.HasPrefix(p, "/api/fs/get") || isDownloadPath(p)
}

// applyTTL records TTL events around a forwarded file operation (feature 2).
//
//   - On download (/d, /p, the archive and share download routes, /fs/get
//     with link): refresh the access time for the path (ModeAccess sliding
//     window), done before forwarding.
//   - On upload (PUT /fs/put, /fs/form): after a successful forwarded upload,
//     record a TTL entry for the uploaded path (if the user has TTL on).
//
// forward is the terminal handler that actually serves the request
// (OpenList's own route in production, a stub in tests).
func (s *Server) applyTTL(c *gin.Context, forward gin.HandlerFunc) {
	// The feature middleware publishes the base-path-relative path
	// (OpenList's route path): requests arrive as <base>/d/... on subpath
	// deployments.
	path := s.relPath(c)
	method := c.Request.Method

	// Download: refresh access time before forwarding. No-op if the file has
	// no TTL record.
	if isDownloadPath(path) {
		fp := downloadPath(path)
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
				// OpenList's put handler URL-decodes File-Path before storing
				// the file, so the TTL record must store the decoded path too
				// — otherwise the reaper later tries to remove the still-
				// escaped path and silently misses the file.
				if decoded, err := url.PathUnescape(filePath); err == nil {
					filePath = decoded
				}
				if filePath != "" {
					// Optional per-file TTL override (seconds). When present
					// and > 0, the uploaded file's expiry uses this duration
					// instead of the user's configured default; when absent
					// or <= 0, the user's default applies. The header is
					// URL-decoded-safe (a plain integer).
					var override int64
					if raw := strings.TrimSpace(c.GetHeader("X-Ttl")); raw != "" {
						if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
							override = v
						}
					}
					_ = ttl.RecordUploadWithTTL(uid.(uint), filePath, time.Now(), override)
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

// downloadPrefixes enumerates the direct download routes OpenList's
// server/router.go registers: plain download /d, proxy download /p, archive
// download /ad, archive proxy /ap, in-archive extract /ae, share download
// /sd, and share archive extract /sad. One list feeds every path check
// (classifyPath, isDownloadPath, downloadPath); adding a prefix here brings
// a future OpenList download route under the feature chain wholesale.
var downloadPrefixes = []string{"/d", "/p", "/ad", "/ap", "/ae", "/sd", "/sad"}

// isDownloadPath reports whether the path targets a direct file download
// (or its API equivalent, /api/fs/get).
func isDownloadPath(p string) bool {
	if strings.HasPrefix(p, "/api/fs/get") {
		return true
	}
	for _, prefix := range downloadPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// downloadPath extracts the logical file path from a download request URL.
// p must be the base-path-relative path (OpenList's route path), not the
// full request path. Share downloads (/sd/:sid/<path>, /sad/:sid/<path>)
// carry a leading share-id segment that is not part of the file's path and
// is skipped.
func downloadPath(p string) string {
	switch {
	case strings.HasPrefix(p, "/api/fs/get"):
		return downloadFilePath(strings.TrimPrefix(p, "/api/fs/get"))
	case strings.HasPrefix(p, "/sd/") || strings.HasPrefix(p, "/sad/"):
		// /sd/:sid/<path> — the share id is not part of the file path.
		rest := strings.TrimPrefix(strings.TrimPrefix(p, "/sad"), "/sd")
		if i := strings.Index(rest[1:], "/"); i >= 0 {
			return downloadFilePath(rest[i+1:])
		}
		return ""
	}
	for _, prefix := range downloadPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return downloadFilePath(strings.TrimPrefix(p, prefix))
		}
	}
	return ""
}

// downloadFilePath turns the remainder after a download route prefix into
// the logical file path ("/a/b.txt"), or "" when nothing follows.
func downloadFilePath(rest string) string {
	rest = "/" + strings.TrimPrefix(rest, "/")
	if rest == "/" {
		return ""
	}
	return rest
}
