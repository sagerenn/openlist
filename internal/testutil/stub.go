package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sync"
)

// StubOpenList is a minimal stand-in for OpenList's HTTP API used in e2e
// tests. It records calls and responds to the handful of endpoints the
// extension talks to: /api/me, /api/fs/remove, /api/fs/put, /api/share/get,
// the download routes (/d, /p, /sd), and generic passthrough. The real
// OpenList is not required for extension e2e tests.
type StubOpenList struct {
	Server *httptest.Server

	mu sync.Mutex
	// Shares maps a share id to its root paths, served by /api/share/get.
	Shares  map[string][]string
	Removes []StubRemove
	Uploads []StubUpload
	Gets    []string
	Lists   []string
	Token   string // expected admin token
	// listContent, when non-nil, is served as the content array of every
	// /api/fs/list response instead of the default empty listing.
	listContent []interface{}
}

// StubRemove records a remove call.
type StubRemove struct {
	Dir   string   `json:"dir"`
	Names []string `json:"names"`
}

// StubUpload records an upload call.
type StubUpload struct {
	Path   string
	Size   int64
	Body   string
	Token  string
}

// NewStubOpenList starts a stub OpenList server on a random port.
func NewStubOpenList(token string) *StubOpenList {
	s := &StubOpenList{Token: token}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/me", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != s.Token {
			writeJSON(w, 401, "unauthorized", nil)
			return
		}
		writeJSON(w, 200, "success", ginH{"id": 1, "username": "admin", "role": 2})
	})

	mux.HandleFunc("/api/fs/remove", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != s.Token {
			writeJSON(w, 401, "unauthorized", nil)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var rm StubRemove
		_ = json.Unmarshal(body, &rm)
		s.mu.Lock()
		s.Removes = append(s.Removes, rm)
		s.mu.Unlock()
		writeJSON(w, 200, "success", nil)
	})

	mux.HandleFunc("/api/fs/put", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != s.Token {
			writeJSON(w, 401, "unauthorized", nil)
			return
		}
		body, _ := io.ReadAll(r.Body)
		// OpenList's FsStream does url.PathUnescape on File-Path; mirror
		// that so recorded paths are the logical paths.
		path := r.Header.Get("File-Path")
		if dec, err := url.PathUnescape(path); err == nil {
			path = dec
		}
		s.mu.Lock()
		s.Uploads = append(s.Uploads, StubUpload{
			Path:  path,
			Size:  r.ContentLength,
			Body:  string(body),
			Token: r.Header.Get("Authorization"),
		})
		s.mu.Unlock()
		writeJSON(w, 200, "success", nil)
	})

	// Generic passthrough for /d/* and /p/* downloads.
	mux.HandleFunc("/d/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Gets = append(s.Gets, r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("file-bytes:" + r.URL.Path))
	})
	mux.HandleFunc("/p/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Gets = append(s.Gets, r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("proxy-bytes:" + r.URL.Path))
	})
	// Share downloads (/sd/<sid>/<path>): the path behind the share id is
	// share-relative, mirroring OpenList's SharingDown routing.
	mux.HandleFunc("/sd/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Gets = append(s.Gets, r.URL.Path)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("share-bytes:" + r.URL.Path))
	})
	// Share metadata (GET /api/share/get?id=<sid>): serves s.Shares. Real
	// OpenList returns the flattened SharingResp; only "files" matters here.
	mux.HandleFunc("/api/share/get", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != s.Token {
			writeJSON(w, 401, "unauthorized", nil)
			return
		}
		s.mu.Lock()
		files, ok := s.Shares[r.URL.Query().Get("id")]
		s.mu.Unlock()
		if !ok {
			writeJSON(w, 404, "sharing not found", nil)
			return
		}
		writeJSON(w, 200, "success", ginH{"files": files})
	})
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Path == "" {
			req.Path = r.URL.Query().Get("path")
		}
		s.mu.Lock()
		s.Lists = append(s.Lists, req.Path)
		content := s.listContent
		s.mu.Unlock()
		if content == nil {
			content = []interface{}{}
		}
		writeJSON(w, 200, "success", ginH{"content": content, "total": len(content)})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Path == "" {
			req.Path = r.URL.Query().Get("path")
		}
		s.mu.Lock()
		s.Gets = append(s.Gets, req.Path)
		// Size of the most recent upload at this path, if any, so callers
		// see the authoritative stored size.
		size := int64(0)
		for _, u := range s.Uploads {
			if u.Path == req.Path {
				size = int64(len(u.Body))
			}
		}
		s.mu.Unlock()
		writeJSON(w, 200, "success", ginH{
			"name": path.Base(req.Path),
			"size": size,
			// Echo a deterministic sign so tests can assert it is
			// propagated into download links. OpenList computes the sign
			// from the path; ours is just "sign:<path>".
			"sign": "sign:" + req.Path,
		})
	})

	s.Server = httptest.NewServer(mux)
	return s
}

// Close shuts down the stub server.
func (s *StubOpenList) Close() { s.Server.Close() }

// SetShare registers a share id with its root paths, served by the
// /api/share/get handler.
func (s *StubOpenList) SetShare(sid string, files []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Shares == nil {
		s.Shares = map[string][]string{}
	}
	s.Shares[sid] = files
}

// SetListContent makes every /api/fs/list response carry the given content
// array instead of an empty listing. Test hook for code that inspects
// listings (e.g. the TTL reaper's emptiness check).
func (s *StubOpenList) SetListContent(content []map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]interface{}, len(content))
	for i, c := range content {
		out[i] = c
	}
	s.listContent = out
}

// URL returns the stub server's base URL.
func (s *StubOpenList) URL() string { return s.Server.URL }

// SnapshotRemoves returns a copy of recorded removes.
func (s *StubOpenList) SnapshotRemoves() []StubRemove {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubRemove, len(s.Removes))
	copy(out, s.Removes)
	return out
}

// SnapshotUploads returns a copy of recorded uploads.
func (s *StubOpenList) SnapshotUploads() []StubUpload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StubUpload, len(s.Uploads))
	copy(out, s.Uploads)
	return out
}

// SnapshotGets returns a copy of recorded read paths.
func (s *StubOpenList) SnapshotGets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Gets))
	copy(out, s.Gets)
	return out
}

// SnapshotLists returns a copy of recorded list paths.
func (s *StubOpenList) SnapshotLists() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Lists))
	copy(out, s.Lists)
	return out
}

func writeJSON(w http.ResponseWriter, code int, msg string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ginH{"code": code, "message": msg, "data": data})
}

type ginH map[string]interface{}
