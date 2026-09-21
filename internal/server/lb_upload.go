package server

import (
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/lb"
)

// lbUpload handles a load-balanced upload (feature 4).
//
// The client requests an upload through an LB group by setting
// X-LB-Group=<name> (or ?lb_group=<name>) on a PUT /fs/put request. The
// extension selects a backend member from the group, rewrites the
// File-Path to land under that member's mount path, and streams the body
// to OpenList via the admin client.
//
// The original file name is preserved; only the directory prefix is
// rewritten to the chosen member's mount path (plus the group's optional
// PathPrefix).
func (s *Server) lbUpload(c *gin.Context) {
	uidAny, ok := c.Get(ctxUserID)
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "message": "lb upload requires a resolved user"})
		return
	}
	userID := uidAny.(uint)

	groupName := c.GetHeader("X-LB-Group")
	if groupName == "" {
		groupName = c.Query("lb_group")
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

	// Determine the target path: <member.MountPath><group.PathPrefix>/<originalName>
	origPath := c.GetHeader("File-Path")
	if origPath == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing File-Path header"})
		return
	}
	origPath = "/" + strings.TrimPrefix(origPath, "/")
	name := origPath
	if idx := strings.LastIndex(origPath, "/"); idx >= 0 {
		name = origPath[idx+1:]
	}
	targetDir := strings.TrimRight(member.MountPath, "/") + g.PathPrefix
	targetPath := strings.TrimRight(targetDir, "/") + "/" + name

	// Stream the request body to OpenList at the target path.
	size := c.Request.ContentLength
	if err := s.Client.UploadFile(c.Request.Context(), targetPath, c.Request.Body, size); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"code": 502, "message": err.Error()})
		return
	}
	// Drain any remaining body so the connection can be reused.
	_, _ = io.Copy(io.Discard, c.Request.Body)
	_ = c.Request.Body.Close()
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success", "data": gin.H{
		"target_mount": member.MountPath,
		"target_path":  targetPath,
	}})
}
