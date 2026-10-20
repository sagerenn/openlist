package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/lb"
)

// Virtual root mounts (feature 4, path activation). A group's PathPrefix
// behaves like a mount: uploads under it are load-balanced, reads under it
// are routed to the member that holds the file. But the prefix is virtual —
// it has no storage behind it, so OpenList's own "/" listing does not show
// it and the manage file manager gives no way to reach it (typing /pshare
// blind is not discoverability). rewriteRootList amends the ROOT listing
// response with a directory entry per visible prefix, making the virtual
// mounts first-class in the file browser; clicking one lists the prefix,
// which lbPathRoute routes to a member exactly as an API-key client's
// /pshare reads are.

// rewriteRootList intercepts a root directory listing (fs/list of "/"),
// forwards it through the normal chain with the response buffered, and
// appends a virtual directory entry for each LB group prefix the requester
// may see. It returns true when it handled the request (the caller must not
// forward again), false when the request is not an amendable root listing
// (the common case — nothing is paid beyond a path check).
func (s *Server) rewriteRootList(c *gin.Context) bool {
	if listRootPath(c) != "/" {
		return false
	}
	prefixes := s.visibleLBPrefixes(c)
	if len(prefixes) == 0 {
		return false
	}
	rec := &bufferRecorder{ResponseWriter: c.Writer, status: 200}
	c.Writer = rec
	s.applyTTL(c, s.forward)
	body := rec.buf.Bytes()
	status := rec.status
	if status < 200 || status > 299 {
		// OpenList refused (auth, permission): pass the refusal through
		// untouched rather than decorating an error body.
		s.emitBuffered(c, rec, status, body)
		return true
	}
	out, ok := injectRootEntries(body, prefixes)
	if !ok {
		// Not an amendable listing body (unexpected shape): never break the
		// response — pass it through exactly as captured.
		s.emitBuffered(c, rec, status, body)
		return true
	}
	c.Writer = rec.ResponseWriter
	c.Writer.Header().Set("Content-Length", strconv.Itoa(len(out)))
	c.Writer.WriteHeader(status)
	_, _ = c.Writer.Write(out)
	return true
}

// emitBuffered releases a buffered response to the real writer unchanged.
func (s *Server) emitBuffered(c *gin.Context, rec *bufferRecorder, status int, body []byte) {
	c.Writer = rec.ResponseWriter
	if status == 0 {
		status = 200
	}
	c.Writer.WriteHeader(status)
	_, _ = c.Writer.Write(body)
}

// listRootPath returns the path an fs/list request targets ("" when the
// request is not a listing), normalized with a leading slash. The frontend
// file browser issues POST with a JSON body; OpenList also serves GET with
// a query parameter.
func listRootPath(c *gin.Context) string {
	if !strings.HasPrefix(relPathSafe(c), "/api/fs/list") {
		return ""
	}
	switch c.Request.Method {
	case http.MethodPost:
		return bodyPathField(c)
	case http.MethodGet:
		p := c.Query("path")
		if p == "" {
			return ""
		}
		return "/" + strings.TrimPrefix(p, "/")
	}
	return ""
}

// visibleLBPrefixes returns the LB group path prefixes the requester may
// see as virtual root directories: the resolved user's own groups, or — for
// an OpenList session caller (the manage pages' file browser) — that
// session's scope, admins seeing every group (the same cross-user view
// /ext/admin/lb/uploads gives them). nil when the caller has no visible
// groups (guests, unprovable tokens) or the lookup fails; the caller then
// forwards the listing untouched.
func (s *Server) visibleLBPrefixes(c *gin.Context) []string {
	scope, ok := c.Get(ctxUserID)
	if !ok {
		su := s.resolveSessionUser(c)
		if su == nil {
			return nil
		}
		if su.admin {
			scope = lb.UserAny
		} else {
			scope = su.userID
		}
	}
	gs, err := lb.ListGroups(scope.(uint))
	if err != nil {
		return nil
	}
	seen := make(map[string]bool, len(gs))
	var prefixes []string
	for _, g := range gs {
		p := lb.NormalizePrefix(g.PathPrefix)
		// Header-only groups (empty prefix) have no virtual mount, and a "/"
		// prefix routes every path — neither yields a distinct directory.
		if p == "" || p == "/" || seen[p] {
			continue
		}
		seen[p] = true
		prefixes = append(prefixes, p)
	}
	return prefixes
}

// injectRootEntries appends a virtual directory entry per prefix to an
// fs/list response body, bumping the listing's total. The envelope and
// every data field other than content/total round-trip untouched
// (json.RawMessage), so the frontend sees exactly what OpenList produced
// plus the injected entries. Prefix names already present in the listing
// are skipped: a prefix colliding with a real root entry needs no virtual
// duplicate. ok is false when the body is not a successful listing — the
// caller passes the original bytes through then, so an unexpected shape
// degrades to the un-amended listing, never a broken response.
func injectRootEntries(body []byte, prefixes []string) (out []byte, ok bool) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, false
	}
	var code int
	if raw, has := env["code"]; !has || json.Unmarshal(raw, &code) != nil || code != 200 {
		return nil, false
	}
	dataRaw, has := env["data"]
	if !has {
		return nil, false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		return nil, false
	}
	var content []json.RawMessage
	if raw, has := data["content"]; has && string(raw) != "null" {
		if err := json.Unmarshal(raw, &content); err != nil {
			return nil, false
		}
	}
	seen := make(map[string]bool, len(content))
	for _, raw := range content {
		var e struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Name != "" {
			seen[e.Name] = true
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	added := 0
	for _, p := range prefixes {
		name := strings.Trim(p, "/")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		entry, err := json.Marshal(map[string]interface{}{
			"name":     name,
			"is_dir":   true,
			"size":     0,
			"modified": now,
			"sign":     "",
		})
		if err != nil {
			continue
		}
		content = append(content, json.RawMessage(entry))
		added++
	}
	if added == 0 {
		return nil, false
	}
	nb, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	data["content"] = nb
	if raw, has := data["total"]; has {
		var total int
		if json.Unmarshal(raw, &total) == nil {
			if tb, err := json.Marshal(total + added); err == nil {
				data["total"] = tb
			}
		}
	}
	nd, err := json.Marshal(data)
	if err != nil {
		return nil, false
	}
	env["data"] = nd
	out, err = json.Marshal(env)
	if err != nil {
		return nil, false
	}
	return out, true
}

// bufferRecorder captures everything a forwarded handler writes so a
// response can be amended before it reaches the client. Header emission is
// deferred: WriteHeader/Write are held in memory (headers set via
// c.Writer.Header() land on the real writer's shared map), Flush is a no-op
// because nothing has reached the client yet, and the caller replays the
// captured status+body on the real writer when done.
type bufferRecorder struct {
	gin.ResponseWriter
	buf    bytes.Buffer
	status int
}

func (r *bufferRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *bufferRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.buf.Write(b)
}

func (r *bufferRecorder) WriteString(s string) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	return r.buf.WriteString(s)
}

// Flush is a no-op: the buffered body is emitted once, at the end.
func (r *bufferRecorder) Flush() {}
