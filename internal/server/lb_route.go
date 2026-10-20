package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/lb"
)

// Path-based load balancing (feature 4): a group with a PathPrefix is
// addressable by path, not only by the X-LB-Group header.
//
//   - Uploads (PUT /api/fs/put) whose File-Path falls under the prefix are
//     routed through the group: the prefix is replaced by the selected
//     member's mount path, everything below it is preserved. The header
//     keeps precedence when both apply, and keeps its legacy shape
//     (<mount><PathPrefix>/<name> — source directories dropped).
//   - Reads/removes under the prefix (POST /api/fs/get, /api/fs/list,
//     /api/fs/remove) are re-pointed at the physical path that actually
//     holds the file: the upload record pins files uploaded through the
//     group; anything else (the prefix root, directories, pre-existing
//     files) falls back to the group's selection strategy with the same
//     prefix→mount remap. This is what makes a group prefix behave like a
//     virtual mount: clients (e.g. pshare) address files by prefix path
//     and never see the physical layout behind it.
//
// Direct download routes (/d, /p, ...) are NOT rewritten: OpenList mints
// those URLs from fs/get's raw_url, which already carries the physical
// path after the body rewrite, and the TTL feature keys on physical paths
// for LB uploads — rewriting would break both. Route resolution here is
// per-user (groups are owned by a user), so it only engages for requests
// with a resolved identity (API key).

// lbRoute is a resolved path-activated load-balance decision for one
// request, computed once and cached in ctxLBRoute.
type lbRoute struct {
	group  lb.Group
	prefix string // NormalizePrefix'd PathPrefix that matched
	// record pins the addressed file to the member its upload landed on
	// (reads/removes); nil when there is no record and strategy-based
	// routing applies.
	record *lb.UploadRecord
}

// lbPathRoute resolves and caches the path-activated route for the current
// request, or nil when the request's target path is not under any of the
// user's group prefixes (or no user / no target path is resolvable). The
// nil result is cached too, so the DB lookups happen at most once per
// request no matter how many guards and handlers consult the route.
func (s *Server) lbPathRoute(c *gin.Context) *lbRoute {
	if v, ok := c.Get(ctxLBRoute); ok {
		if v == nil {
			return nil
		}
		return v.(*lbRoute)
	}
	var r *lbRoute
	if uid, ok := c.Get(ctxUserID); ok {
		if target := lbTargetPath(c); target != "" {
			if g, found := lb.MatchGroupByPath(uid.(uint), target); found {
				r = &lbRoute{group: g, prefix: lb.NormalizePrefix(g.PathPrefix)}
				// Reads/removes follow the upload record when one exists —
				// the file lives on the member it was uploaded to, which is
				// not necessarily what the strategy would pick now.
				// Uploads must not do this: they are creating the file, and
				// the strategy decides where it lands.
				if !isUploadPut(c) {
					if rec, err := lb.FindUploadByPath(uid.(uint), target); err == nil {
						r.record = rec
					}
				}
			}
		}
	}
	c.Set(ctxLBRoute, r)
	return r
}

// isUploadPut reports whether the request is a PUT /api/fs/put.
func isUploadPut(c *gin.Context) bool {
	return c.Request.Method == http.MethodPut && strings.HasSuffix(relPathSafe(c), "/api/fs/put")
}

// lbGroupName returns the requested group name from the X-LB-Group header
// or ?lb_group= query.
func lbGroupName(c *gin.Context) string {
	if n := c.GetHeader("X-LB-Group"); n != "" {
		return n
	}
	return c.Query("lb_group")
}

// relPathSafe is relPath without the receiver (helpers above are free
// functions); the middleware always publishes ctxRelPath before routing,
// so reading it directly is exact.
func relPathSafe(c *gin.Context) string {
	if v, ok := c.Get(ctxRelPath); ok {
		return v.(string)
	}
	return c.Request.URL.Path
}

// lbTargetPath extracts the logical path the request addresses — the same
// decoded view OpenList itself routes on — or "" when it carries none.
// Uploads carry it in the File-Path header (URL-decoded, as OpenList's put
// handler decodes it); fs/get, fs/list, and fs/remove carry it in the JSON
// body ("path" / "dir"), which is peeked at, not consumed.
func lbTargetPath(c *gin.Context) string {
	rel := relPathSafe(c)
	switch {
	case strings.HasSuffix(rel, "/api/fs/put"):
		fp := c.GetHeader("File-Path")
		if fp == "" {
			return ""
		}
		if dec, err := url.PathUnescape(fp); err == nil {
			fp = dec
		}
		return "/" + strings.TrimPrefix(fp, "/")
	case strings.HasPrefix(rel, "/api/fs/get"),
		strings.HasPrefix(rel, "/api/fs/list"),
		strings.HasPrefix(rel, "/api/fs/remove"):
		return bodyPathField(c)
	}
	return ""
}

// bodyPathField peeks the "path" (or "dir", for remove) field out of a
// JSON request body WITHOUT consuming it: the peeked bytes are cached in
// ctxBodyPeek and the body is re-armed so the downstream handler reads the
// original stream in full. "" when the body carries no usable path.
func bodyPathField(c *gin.Context) string {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return ""
	}
	v, ok := c.Get(ctxBodyPeek)
	if !ok {
		// fs bodies are tiny; 1 MiB is a generous ceiling that still keeps
		// a pathological body from being slurped into memory.
		peek, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
		if err != nil {
			return ""
		}
		v = peek
		c.Set(ctxBodyPeek, peek)
		c.Request.Body = &replayBody{buf: peek, under: c.Request.Body}
	}
	var parsed struct {
		Path string `json:"path"`
		Dir  string `json:"dir"`
	}
	if err := json.Unmarshal(v.([]byte), &parsed); err != nil {
		return ""
	}
	if parsed.Path != "" {
		return "/" + strings.TrimPrefix(parsed.Path, "/")
	}
	if parsed.Dir != "" {
		return "/" + strings.TrimPrefix(parsed.Dir, "/")
	}
	return ""
}

// ctxBodyPeek caches the peeked request body ([]byte) for fs requests.
const ctxBodyPeek = "ext.body_peek"

// replayBody serves the peeked bytes first, then continues with whatever
// remains unread in the original stream, so a peek does not consume the
// body for downstream handlers.
type replayBody struct {
	buf   []byte
	under io.ReadCloser
}

func (r *replayBody) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	return r.under.Read(p)
}

func (r *replayBody) Close() error { return r.under.Close() }

// rewriteBodyPath replaces the JSON body's path-carrying field with
// newPath, preserving every other field byte-for-byte (json.RawMessage
// round-trip). The cached peek is refreshed so later reads of ctxBodyPeek
// agree with what the downstream handler will see.
func rewriteBodyPath(c *gin.Context, key, newPath string) bool {
	v, ok := c.Get(ctxBodyPeek)
	if !ok {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(v.([]byte), &m); err != nil {
		return false
	}
	enc, err := json.Marshal(newPath)
	if err != nil {
		return false
	}
	m[key] = enc
	nb, err := json.Marshal(m)
	if err != nil {
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(nb))
	c.Request.ContentLength = int64(len(nb))
	c.Request.Header.Set("Content-Length", strconv.Itoa(len(nb)))
	c.Set(ctxBodyPeek, nb)
	return true
}

// lbReadTarget maps a logical path under a group prefix to the physical
// path that holds it: the upload record's target when one exists, else the
// strategy-picked member with the prefix replaced by its mount.
func lbReadTarget(r *lbRoute, logical string) (string, error) {
	if r.record != nil && r.record.TargetPath != "" {
		return r.record.TargetPath, nil
	}
	member, err := lb.PickForRead(r.group)
	if err != nil {
		return "", err
	}
	return lb.RemapPath(member.MountPath, r.prefix, logical), nil
}

// lbPurge names the upload records to drop after a successful remove: the
// files dir/names were deleted, so their records must go too.
type lbPurge struct {
	userID uint
	dir    string
	names  []string
}

// rewriteLBRead rewrites the JSON body of a POST /api/fs/get, /api/fs/list,
// or /api/fs/remove whose target path falls under a group prefix for the
// resolved user, pointing it at the physical path that actually holds the
// file. It returns a purge spec for a rewritten remove (records of the
// removed files are dropped by the caller once the remove succeeds), and
// nil in every other case — including "no route", which is the common case
// and costs nothing beyond the (cached) route lookup.
func (s *Server) rewriteLBRead(c *gin.Context) *lbPurge {
	if c.Request.Method != http.MethodPost {
		return nil
	}
	rel := s.relPath(c)
	var key string
	switch {
	case strings.HasPrefix(rel, "/api/fs/get"), strings.HasPrefix(rel, "/api/fs/list"):
		key = "path"
	case strings.HasPrefix(rel, "/api/fs/remove"):
		key = "dir"
	default:
		return nil
	}
	r := s.lbPathRoute(c)
	if r == nil {
		return nil
	}
	logical := lbTargetPath(c)
	if logical == "" {
		return nil
	}
	physical, err := lbReadTarget(r, logical)
	if err != nil {
		// No members: fall through to OpenList, which will fail with its
		// usual "storage not found" for the prefix path.
		return nil
	}
	if !rewriteBodyPath(c, key, physical) {
		return nil
	}
	if key != "dir" {
		return nil
	}
	// A remove: collect the removed names for the record purge. They ride
	// in the same body ("names"), already peeked; the dir rewrite left
	// them untouched.
	uid := c.MustGet(ctxUserID).(uint)
	var body struct {
		Names []string `json:"names"`
	}
	if v, ok := c.Get(ctxBodyPeek); ok {
		_ = json.Unmarshal(v.([]byte), &body)
	}
	return &lbPurge{userID: uid, dir: logical, names: body.Names}
}

// purgeUploadRecords drops the upload records for files removed through a
// group prefix (called after the remove response is a 2xx).
func purgeUploadRecords(p *lbPurge) {
	if p == nil {
		return
	}
	for _, name := range p.names {
		_, _ = lb.DeleteUploadsFor(p.userID, p.dir, name)
	}
}
