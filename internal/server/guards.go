package server

import (
	"context"
	"net/http"
	"net/url"
	stdpath "path"
	"strconv"
	"strings"
	"time"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
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
//   - On download (/d, /p, the archive and share download routes): refresh
//     the access time for the file's logical path (ModeAccess sliding
//     window), done before forwarding. Share downloads (/sd, /sad) carry a
//     share id instead of the file's path; the logical path is resolved
//     through the share API (see shareDownloadPath). /api/fs/get carries
//     its path in the POST body, so no access can be recorded for it; it
//     remains in isDownloadPath only so the read-permission guard covers
//     it.
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
		if isShareDownloadPath(path) {
			fp = s.shareDownloadPath(c, path)
		}
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

// downloadPath extracts the path a download request URL carries. p must be
// the base-path-relative path (OpenList's route path), not the full request
// path. For /d, /p, /ad, /ap, /ae that IS the file's logical path. For
// share downloads (/sd/:sid/<path>, /sad/:sid/<path>) it is only the
// share-relative path — shareDownloadPath resolves it to the logical path
// via the share API. "/api/fs/get" yields "" (isDownloadPath covers it for
// the read guard, but the file path travels in its POST body, so there is
// nothing to extract). "" when no path follows the route.
func downloadPath(p string) string {
	if isShareDownloadPath(p) {
		_, rel := splitSharePath(p)
		return downloadFilePath(rel)
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

// isShareDownloadPath reports whether p is a share download URL
// (/sd/:sid/<path> or /sad/:sid/<path>).
func isShareDownloadPath(p string) bool {
	return strings.HasPrefix(p, "/sd/") || strings.HasPrefix(p, "/sad/")
}

// shareDownloadPath resolves the logical file path a share download URL
// targets, best-effort: "" when it cannot be determined (bad share id,
// API error, or a path no share root matches), in which case no TTL access
// is recorded — the download itself is unaffected.
//
// A share URL carries the share id where every other download route carries
// the file's path, so the raw extracted remainder is share-relative, NOT
// the logical path TTL records are keyed by. The share's roots are fetched
// through the public share API (GET /api/share/get) with the admin loopback
// client — share downloads are typically anonymous, so the requester's own
// credentials cannot be relied on — and the remainder is joined exactly the
// way OpenList's own op.GetSharingUnwrapPath does it: a single-root share
// prefixes the root, a multi-root share maps the first remainder segment
// against each root's base name.
func (s *Server) shareDownloadPath(c *gin.Context, p string) string {
	sid, rel := splitSharePath(p)
	if sid == "" || rel == "" {
		return ""
	}
	// A short-bounded context: this sits on the hot download path, so an
	// unresponsive share API must not hold the download itself up.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	roots, err := s.Client.GetSharing(ctx, sid)
	if err != nil || len(roots) == 0 {
		return ""
	}
	return unwrapSharePath(roots, rel)
}

// splitSharePath splits a share download URL into its share id and the
// share-relative path ("/sd/a/b/file.txt" => "a", "/b/file.txt"); either
// part is empty when absent.
func splitSharePath(p string) (sid, rel string) {
	rest := strings.TrimPrefix(strings.TrimPrefix(p, "/sad"), "/sd")
	rest = strings.TrimPrefix(rest, "/")
	if rest == "" {
		return "", ""
	}
	sid, rel, _ = strings.Cut(rest, "/")
	return sid, "/" + rel
}

// unwrapSharePath resolves the share-relative path against the share's
// root paths, mirroring OpenList's op.GetSharingUnwrapPath: single root =>
// root joined with the path; multiple roots => the path's first segment
// names one of the roots (by base name) and the rest is joined onto it.
// "" when the segment matches no root.
func unwrapSharePath(roots []string, rel string) string {
	if len(roots) == 1 {
		return stdpath.Join(roots[0], rel)
	}
	cleaned := utils.FixAndCleanPath(rel)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return ""
	}
	child, rest, _ := strings.Cut(cleaned, "/")
	for _, root := range roots {
		if child == stdpath.Base(root) {
			return stdpath.Join(root, rest)
		}
	}
	return ""
}
