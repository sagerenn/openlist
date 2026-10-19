package server

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/lb"
	"github.com/sagerenn/openlist/internal/ttl"
)

// curlUpload implements the curl-friendly streaming upload endpoint
// (PUT/POST /upload/<path>). It exists so a file can be uploaded and shared
// in one command:
//
//	curl -T file.txt -H "X-Api-Key: <key>" http://host:5245/upload/file.txt
//	=> {"code":200,"data":{"path":"/file.txt","url":"http://host:5245/d/file.txt",...}}
//
// OpenList's native upload (PUT /api/fs/put) carries the target in the
// File-Path header, which curl's -T cannot set; this endpoint takes the
// target path from the URL instead, so the same command shape works for
// any filename and nested directories. Both PUT (curl -T / --upload-file)
// and POST (curl --data-binary @file) are accepted.
//
// Authentication (either form):
//   - an extension API key (X-Api-Key or Authorization: Bearer) with the
//     fs.put scope — the upload is attributed to the key's user, so TTL
//     and load-balance policies apply exactly as they do for /api/fs/put;
//   - the configured static AdminToken (Authorization: <token>) — a
//     superuser path with no API key, useful for smoke-testing.
//
// Guests are rejected: an open upload endpoint on a multi-user file server
// would let anyone write to the storages.
//
// The body is streamed (never buffered) to OpenList in-process via the
// loopback admin client — the same code path load-balanced uploads use —
// and the feature headers keep their /api/fs/put meaning: X-LB-Group
// routes through an LB group, X-Ttl sets a per-file TTL override, and a
// TTL record is created when the user has TTL enabled.
//
// The response carries a same-origin download link (GET /d/<path>) built
// from the request's own host, so it works behind proxies and for any
// storage driver. When OpenList requires signed downloads (sign-all,
// storage sign, or a password-protected meta), the sign is fetched via
// /api/fs/get — it is derived from the path alone, so the admin-fetched
// value equals what any user would receive — and embedded as ?sign=.
func (s *Server) curlUpload(c *gin.Context) {
	// Resolve the identity: API key first, then the static admin token.
	var (
		userID    uint
		hasKey    bool
		tokHeader = c.GetHeader("Authorization")
	)
	if secret, err := apikey.FromHeader(c.GetHeader); err == nil {
		k, err := apikey.Verify(secret)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "invalid api key"})
			return
		}
		if !k.HasScope(apikey.ScopeWrite) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "api key scope denied: fs.put required"})
			return
		}
		userID, hasKey = k.UserID, true
	} else if tokHeader != "" && s.Config.AdminToken != "" && tokHeader == s.Config.AdminToken {
		// Static admin token: no per-user policy (no TTL record, no LB
		// group ownership), but OpenList's own write checks still run
		// server-side because the loopback call carries a real admin JWT.
		hasKey = false
	} else {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "upload requires an api key (X-Api-Key / Bearer) or the admin token"})
		return
	}

	// Target path from the URL. gin delivers wildcard params decoded
	// (e.g. "/upload/a%20b.txt" => "/a b.txt"), which is the logical path
	// OpenList stores — the same view of the path /api/fs/put has after
	// unescaping its File-Path header.
	targetPath := "/" + strings.TrimPrefix(c.Param("path"), "/")
	if targetPath == "/" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing target path: upload with PUT /upload/<dir>/<name>"})
		return
	}

	// Optional load balancing: with X-LB-Group (or ?lb_group=), rewrite the
	// directory prefix onto the selected member's mount path, exactly like
	// lbUpload does for /api/fs/put. The original file name is preserved.
	groupName := c.GetHeader("X-LB-Group")
	if groupName == "" {
		groupName = c.Query("lb_group")
	}
	if groupName != "" {
		if !hasKey {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": 400, "message": "load-balanced upload requires an api key (group ownership)"})
			return
		}
		g, err := lb.GetGroupByName(userID, groupName)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
			return
		}
		member, err := lb.Select(g)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": err.Error()})
			return
		}
		name := targetPath
		if idx := strings.LastIndex(targetPath, "/"); idx >= 0 {
			name = targetPath[idx+1:]
		}
		targetPath = strings.TrimRight(strings.TrimRight(member.MountPath, "/")+g.PathPrefix, "/") + "/" + name
	}

	// Stream the body to OpenList. UploadFile forwards with a real admin
	// JWT, so OpenList's own write-permission checks still run.
	size := c.Request.ContentLength
	if err := s.Client.UploadFile(c.Request.Context(), targetPath, c.Request.Body, size); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"code": 502, "message": err.Error()})
		return
	}
	// Drain any remaining body so the connection can be reused.
	_, _ = io.Copy(io.Discard, c.Request.Body)
	_ = c.Request.Body.Close()

	// Record TTL when the uploading user has TTL enabled (or an explicit
	// per-file override was requested via X-Ttl, mirroring applyTTL).
	// Admin-token uploads have no user, hence no TTL policy to apply.
	if hasKey {
		var override int64
		if raw := strings.TrimSpace(c.GetHeader("X-Ttl")); raw != "" {
			if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
				override = v
			}
		}
		_ = ttl.RecordUploadWithTTL(userID, targetPath, time.Now(), override)
	}

	// Best-effort sign lookup for the download link. Sign is empty (and
	// the URL works bare) unless OpenList's sign-all / storage-sign /
	// meta-password settings are in effect. Also yields the authoritative
	// stored size, which covers chunked uploads (ContentLength == -1).
	sign := ""
	if info, err := s.Client.GetFile(c.Request.Context(), targetPath); err == nil {
		sign = info.Sign
		if info.Size >= 0 {
			size = info.Size
		}
	}

	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": gin.H{
		"path": targetPath,
		"url":  downloadURL(c, targetPath, sign),
		"sign": sign,
		"size": size,
	}})
}

// downloadURL builds the /d/<path> link served by this engine. Scheme and
// host come from the request itself (X-Forwarded-Proto / X-Forwarded-Host
// when behind a proxy, else r.TLS / r.Host), matching how OpenList's own
// common.GetApiUrlFromRequest builds download URLs. The path is re-escaped
// per segment from the decoded logical path — the same encoding OpenList's
// utils.EncodePath(_, true) applies — so a round trip through /d/ resolves
// to the uploaded file.
func downloadURL(c *gin.Context, path, sign string) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	u := scheme + "://" + host + "/d" + escapeURLPath(path)
	if sign != "" {
		u += "?sign=" + url.QueryEscape(sign)
	}
	return u
}

// escapeURLPath percent-encodes each path segment of p, preserving "/"
// separators, so the result can be appended to a URL path.
func escapeURLPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
