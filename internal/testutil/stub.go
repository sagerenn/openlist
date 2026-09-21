package testutil

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

// StubOpenList is a minimal stand-in for OpenList's HTTP API used in e2e
// tests. It records calls and responds to the handful of endpoints the
// extension talks to: /api/me, /api/fs/remove, /api/fs/put, and generic
// passthrough. The real OpenList is not required for extension e2e tests.
type StubOpenList struct {
	Server *httptest.Server

	mu       sync.Mutex
	Removes  []StubRemove
	Uploads  []StubUpload
	Gets     []string
	Token    string // expected admin token
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
	mux.HandleFunc("/api/fs/list", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, "success", ginH{"content": []interface{}{}, "total": 0})
	})
	mux.HandleFunc("/api/fs/get", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.Gets = append(s.Gets, r.URL.Path)
		s.mu.Unlock()
		writeJSON(w, 200, "success", ginH{"name": "file"})
	})

	s.Server = httptest.NewServer(mux)
	return s
}

// Close shuts down the stub server.
func (s *StubOpenList) Close() { s.Server.Close() }

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

func writeJSON(w http.ResponseWriter, code int, msg string, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ginH{"code": code, "message": msg, "data": data})
}

type ginH map[string]interface{}
