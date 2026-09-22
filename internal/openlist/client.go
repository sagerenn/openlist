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
	// tokens, when set, supplies a refreshable admin JWT used for
	// authenticated calls. On a 401 ("token is invalidated") the client
	// refreshes the JWT once and retries. When nil, the static Token is
	// used as-is.
	tokens *TokenManager
}

// New returns a Client targeting baseURL with the given admin token.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// SetTokenManager attaches a refreshable admin-JWT manager. When set, the
// client uses the manager's JWT for authenticated calls and refreshes it on
// a 401.
func (c *Client) SetTokenManager(m *TokenManager) {
	c.tokens = m
}

// effectiveToken returns the token to send on a request, preferring a
// refreshable JWT from the manager when one is available.
func (c *Client) effectiveToken(ctx context.Context) string {
	if c.tokens != nil && c.tokens.Enabled() {
		if tok, err := c.tokens.Get(ctx); err == nil {
			return tok
		}
	}
	return c.Token
}

// apiResp mirrors OpenList's standard response envelope.
type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do performs an authenticated request and decodes the envelope. When a
// TokenManager is attached and the request fails with a 401 (the cached
// admin JWT has expired), it refreshes the JWT and retries once. The body
// is buffered so the retry can replay it.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (apiResp, error) {
	var buf []byte
	if body != nil {
		var err error
		buf, err = io.ReadAll(body)
		if err != nil {
			return apiResp{}, err
		}
	}
	doOnce := func(token string) (apiResp, int, error) {
		var out apiResp
		u := c.BaseURL + path
		var bodyReader io.Reader
		if buf != nil {
			bodyReader = bytes.NewReader(buf)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
		if err != nil {
			return out, 0, err
		}
		req.Header.Set("Authorization", token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return out, 0, err
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return out, resp.StatusCode, fmt.Errorf("openlist: decode response: %w", err)
		}
		return out, resp.StatusCode, nil
	}
	out, status, err := doOnce(c.effectiveToken(ctx))
	if err != nil {
		return out, err
	}
	// Retry once on a 401 from an expired/invalid admin JWT.
	if status == http.StatusUnauthorized && c.tokens != nil && c.tokens.Enabled() {
		if tok, rerr := c.tokens.Refresh(ctx); rerr == nil {
			out2, _, err2 := doOnce(tok)
			if err2 == nil {
				return out2, nil
			}
			return out, err2
		}
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
	req.Header.Set("Authorization", c.effectiveToken(ctx))
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

// meResp is the subset of /api/me's data the extension cares about.
type meResp struct {
	Role int `json:"role"`
}

// IsAdminToken reports whether the given token belongs to an OpenList admin
// user, by calling /api/me and checking the role. It returns false on any
// error (treat unprovable tokens as non-admin). Used by the extension's
// adminAuth to accept a logged-in admin's session token in addition to the
// configured static admin token, so the frontend can manage extension
// features with the same token it already uses for OpenList.
func (c *Client) IsAdminToken(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	// Probe the given token directly (not the client's cached admin JWT),
	// so adminAuth can validate a frontend-supplied session token.
	var out apiResp
	u := c.BaseURL + "/api/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	if out.Code != 200 {
		return false
	}
	var me meResp
	if err := json.Unmarshal(out.Data, &me); err != nil {
		return false
	}
	// OpenList UserRole: 0=GENERAL, 1=GUEST, 2=ADMIN.
	return me.Role == 2
}
