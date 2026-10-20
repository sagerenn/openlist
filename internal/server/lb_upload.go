package server

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/lb"
	"github.com/sagerenn/openlist/internal/ttl"
)

// lbUpload handles a load-balanced upload (feature 4).
//
// Activation: X-LB-Group=<name> (or ?lb_group=<name>) on a PUT /fs/put
// request — the original semantics — or, new, a File-Path under a group's
// PathPrefix, no header needed. In both forms the extension selects a
// backend member from the group, rewrites the target to land under that
// member's mount path, streams the body to OpenList via the admin client,
// records the upload (so later reads under the prefix find it), and
// applies the uploader's TTL policy (which applyTTL never sees, since this
// handler bypasses it).
//
// The original file name is preserved; only the directory prefix is
// rewritten to the chosen member's mount path.
func (s *Server) lbUpload(c *gin.Context) {
	uidAny, ok := c.Get(ctxUserID)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "lb upload requires a resolved user"})
		return
	}
	userID := uidAny.(uint)

	// Header activation resolves the group by name (legacy semantics,
	// source directories dropped); path activation arrives via ctxLBRoute.
	var g lb.Group
	var pathActivated bool
	if name := lbGroupName(c); name != "" {
		var err error
		g, err = lb.GetGroupByName(userID, name)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
			return
		}
	} else if route := s.lbPathRoute(c); route != nil {
		g = route.group
		pathActivated = true
	} else {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": 400, "message": "no lb group resolved for upload"})
		return
	}
	member, err := lb.Select(g)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"code": 503, "message": err.Error()})
		return
	}

	// Target path. File-Path is URL-decoded first: OpenList's own put
	// handler unescapes it, and the recorded SourcePath must be the decoded
	// logical path so reads/removes under the prefix match it again.
	origPath := c.GetHeader("File-Path")
	if origPath == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing File-Path header"})
		return
	}
	if dec, err := url.PathUnescape(origPath); err == nil {
		origPath = dec
	}
	origPath = "/" + strings.TrimPrefix(origPath, "/")
	name := origPath
	if idx := strings.LastIndex(origPath, "/"); idx >= 0 {
		name = origPath[idx+1:]
	}
	var targetPath string
	if pathActivated {
		// Preserve the logical hierarchy below the prefix: /pshare/2024/x.txt
		// lands at <mount>/2024/x.txt.
		targetPath = lb.RemapPath(member.MountPath, lb.NormalizePrefix(g.PathPrefix), origPath)
	} else {
		// Header form keeps its legacy shape: <mount><PathPrefix>/<name>.
		targetDir := strings.TrimRight(member.MountPath, "/") + g.PathPrefix
		targetPath = strings.TrimRight(targetDir, "/") + "/" + name
	}

	// Stream the request body to OpenList at the target path.
	size := c.Request.ContentLength
	if err := s.Client.UploadFile(c.Request.Context(), targetPath, c.Request.Body, size); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"code": 502, "message": err.Error()})
		return
	}
	// Drain any remaining body so the connection can be reused.
	_, _ = io.Copy(io.Discard, c.Request.Body)
	_ = c.Request.Body.Close()

	// Record the upload: pins later reads under the group prefix to this
	// member, and powers the admin uploads view.
	_ = lb.RecordUpload(&lb.UploadRecord{
		UserID:      userID,
		GroupID:     g.ID,
		GroupName:   g.Name,
		SourcePath:  origPath,
		TargetMount: member.MountPath,
		TargetPath:  targetPath,
		Size:        size,
	})
	// TTL for LB uploads, mirroring applyTTL (which this handler bypasses):
	// X-Ttl override when present and > 0, else the user's default. The
	// record keys on the PHYSICAL target path — the reaper deletes that
	// path, and /d/ downloads (whose URLs fs/get mints from the physical
	// path) refresh the same record.
	var ttlOverride int64
	if raw := strings.TrimSpace(c.GetHeader("X-Ttl")); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			ttlOverride = v
		}
	}
	_ = ttl.RecordUploadWithTTL(userID, targetPath, time.Now(), ttlOverride)
	c.AbortWithStatusJSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": gin.H{
		"target_mount": member.MountPath,
		"target_path":  targetPath,
	}})
}
