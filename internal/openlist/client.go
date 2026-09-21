// Package openlist is a thin HTTP client wrapper around OpenList's public
// REST API. The extension uses it to perform privileged operations on
// behalf of users — primarily file removal (TTL reaper) and uploads
// (load-balance routing) — authenticating as the admin.
//
// It talks only to OpenList's documented HTTP endpoints, so it is
// forward-compatible: it does not depend on OpenList's internal packages.
package openlist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client calls OpenList's HTTP API as the admin.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a Client targeting baseURL with the given admin token.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// apiResp mirrors OpenList's standard response envelope.
type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do performs an authenticated request and decodes the envelope.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (apiResp, error) {
	var out apiResp
	u := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", c.Token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("openlist: decode response: %w", err)
	}
	if out.Code != 200 {
		return out, fmt.Errorf("openlist: api error code=%d msg=%s", out.Code, out.Message)
	}
	return out, nil
}

// RemoveFile deletes a single file at the given logical path. It calls
// POST /api/fs/remove with the admin token. The path is split into dir and
// name as OpenList expects.
func (c *Client) RemoveFile(ctx context.Context, fullPath string) error {
	fullPath = strings.TrimPrefix(fullPath, "/")
	// OpenList RemoveReq takes {dir, names[]}. Split into dir + name.
	idx := strings.LastIndex(fullPath, "/")
	var dir, name string
	if idx < 0 {
		dir = "/"
		name = fullPath
	} else {
		dir = "/" + fullPath[:idx]
		name = fullPath[idx+1:]
	}
	if name == "" {
		return fmt.Errorf("openlist: empty file name in path %q", fullPath)
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"dir":   dir,
		"names": []string{name},
	})
	_, err := c.do(ctx, http.MethodPost, "/api/fs/remove", bytes.NewReader(payload), "application/json")
	return err
}

// UploadFile streams a file to OpenList at the given logical path via
// PUT /api/fs/put (the streaming upload endpoint). The reader is consumed.
func (c *Client) UploadFile(ctx context.Context, fullPath string, r io.Reader, size int64) error {
	fullPath = "/" + strings.TrimPrefix(fullPath, "/")
	u := c.BaseURL + "/api/fs/put"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("File-Path", url.PathEscape(fullPath))
	req.Header.Set("As-Task", "false")
	req.Header.Set("Overwrite", "true")
	if size > 0 {
		req.ContentLength = size
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out apiResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("openlist: decode upload response: %w", err)
	}
	if out.Code != 200 {
		return fmt.Errorf("openlist: upload error code=%d msg=%s", out.Code, out.Message)
	}
	return nil
}

// Ping checks that OpenList is reachable and the token is valid by hitting
// /api/me (admin current user). Returns nil on success.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/api/me", nil, "")
	return err
}
