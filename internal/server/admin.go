package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/domain"
	"github.com/sagerenn/openlist/internal/lb"
	"github.com/sagerenn/openlist/internal/listperm"
	"github.com/sagerenn/openlist/internal/ttl"
)

// registerAdminRoutes wires up the /ext/admin API for all six features.
func (s *Server) registerAdminRoutes(g *gin.RouterGroup) {
	// ---- Feature 1: per-user domain ----
	g.POST("/domain", setDomain)
	g.DELETE("/domain", unbindDomain)
	g.GET("/domain/:user_id", listDomains)

	// ---- Feature 2: TTL ----
	g.PUT("/ttl/:user_id", setTTL)
	g.GET("/ttl/:user_id", getTTL)
	g.POST("/ttl/sweep", s.sweepNow)

	// ---- Feature 3: list permission ----
	g.PUT("/listperm/:user_id", setListPerm)
	g.GET("/listperm/:user_id", getListPerm)

	// ---- Feature 4: load balance ----
	g.POST("/lb/group", createLBGroup)
	g.POST("/lb/group/:id/member", addLBMember)
	g.GET("/lb/group", listLBGroups)
	g.GET("/lb/group/:id", getLBGroup)
	g.DELETE("/lb/group/:id", deleteLBGroup)

	// ---- Feature 5: API keys ----
	g.POST("/apikey", createAPIKey)
	g.GET("/apikey/:user_id", listAPIKeys)
	g.DELETE("/apikey/:user_id/:id", deleteAPIKey)
	g.PUT("/apikey/:user_id/:id/enable", setAPIKeyEnabled)
}

func userIDParam(c *gin.Context) (uint, bool) {
	id, err := strconv.ParseUint(c.Param("user_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid user_id"})
		return 0, false
	}
	return uint(id), true
}

// ---- domain handlers ----

type setDomainReq struct {
	UserID uint   `json:"user_id" binding:"required"`
	Host   string `json:"host" binding:"required"`
}

func setDomain(c *gin.Context) {
	var req setDomainReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	if err := domain.Set(req.UserID, req.Host); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

func unbindDomain(c *gin.Context) {
	host := c.Query("host")
	if host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "missing host"})
		return
	}
	if err := domain.Unbind(host); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

func listDomains(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	uds, err := domain.ListByUser(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": uds})
}

// ---- ttl handlers ----

type ttlConfigReq struct {
	Enabled          bool   `json:"enabled"`
	Mode             string `json:"mode"`
	DurationSeconds  int64  `json:"duration_seconds"`
}

func setTTL(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	var req ttlConfigReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	cfg := ttl.UserTTLConfig{
		UserID:   uid,
		Enabled:  req.Enabled,
		Mode:     ttl.Mode(req.Mode),
		Duration: req.DurationSeconds,
	}
	if err := ttl.SetConfig(cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

func getTTL(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	cfg, err := ttl.GetConfig(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": cfg})
}

func (s *Server) sweepNow(c *gin.Context) {
	r := ttl.NewReaper(s.Client, time.Second, s.Config.ReaperBatch)
	n := r.SweepOnce(c.Request.Context())
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": gin.H{"deleted": n}})
}

// ---- listperm handlers ----

type listPermReq struct {
	CanList bool `json:"can_list"`
	CanRead bool `json:"can_read"`
}

func setListPerm(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	var req listPermReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	p := listperm.UserListPerm{UserID: uid, CanList: req.CanList, CanRead: req.CanRead}
	if err := listperm.Set(p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

func getListPerm(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	p, err := listperm.Get(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": p})
}

// ---- lb handlers ----

type createGroupReq struct {
	Name       string `json:"name" binding:"required"`
	UserID     uint   `json:"user_id" binding:"required"`
	Strategy   string `json:"strategy"`
	PathPrefix string `json:"path_prefix"`
}

func createLBGroup(c *gin.Context) {
	var req createGroupReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	g := &lb.Group{
		Name:       req.Name,
		UserID:     req.UserID,
		Strategy:   lb.Strategy(req.Strategy),
		PathPrefix: req.PathPrefix,
	}
	if err := lb.CreateGroup(g); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": g})
}

type addMemberReq struct {
	MountPath string `json:"mount_path" binding:"required"`
	Weight    int    `json:"weight"`
}

func addLBMember(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid id"})
		return
	}
	var req addMemberReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	m := &lb.GroupMember{GroupID: id, MountPath: req.MountPath, Weight: req.Weight}
	if err := lb.AddMember(m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": m})
}

func listLBGroups(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	gs, err := lb.ListGroups(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": gs})
}

func getLBGroup(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid id"})
		return
	}
	var g lb.Group
	g, err = lb.GetGroup(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "group not found"})
		return
	}
	members, err := lb.GroupMembers(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": gin.H{"group": g, "members": members}})
}

func deleteLBGroup(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid id"})
		return
	}
	if err := lb.DeleteGroup(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

// ---- apikey handlers ----

type createAPIKeyReq struct {
	UserID uint            `json:"user_id" binding:"required"`
	Name   string          `json:"name"`
	Scopes []apikey.Scope  `json:"scopes"`
}

func createAPIKey(c *gin.Context) {
	var req createAPIKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	ck, err := apikey.Create(req.UserID, req.Name, req.Scopes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": ck})
}

func listAPIKeys(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	ks, err := apikey.ListByUser(uid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 500, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "data": ks})
}

func deleteAPIKey(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid id"})
		return
	}
	if err := apikey.Delete(uid, uint(id)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}

type enableKeyReq struct {
	Enabled bool `json:"enabled"`
}

func setAPIKeyEnabled(c *gin.Context) {
	uid, ok := userIDParam(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "invalid id"})
		return
	}
	var req enableKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": err.Error()})
		return
	}
	if err := apikey.SetEnabled(uid, uint(id), req.Enabled); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"code": 200, "message": "success"})
}
