package openlist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// LoginResp mirrors the relevant fields of OpenList's /api/auth/login
// response envelope: {code, message, data: {token}}.
type LoginResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Token string `json:"token"`
	} `json:"data"`
}

// Login authenticates against OpenList's /api/auth/login and returns the
// admin JWT. It does not mutate the client's Token.
func (c *Client) Login(ctx context.Context, username, password string) (string, error) {
	body := struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{Username: username, Password: password}
	payload, _ := json.Marshal(body)
	u := c.BaseURL + "/api/auth/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out LoginResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Code != 200 || out.Data.Token == "" {
		return "", errors.New("openlist: admin login failed: " + out.Message)
	}
	return out.Data.Token, nil
}

// TokenManager caches an admin JWT and refreshes it on demand. It is safe
// for concurrent use. The extension uses it so that privileged in-process
// operations (TTL reaper deletes, load-balanced uploads, and forwarding
// API-key-authenticated file ops to OpenList's core as admin) always carry
// a valid JWT, even after the cached token expires (default 48h).
//
// When AdminPassword is empty the manager is a no-op: callers fall back to
// a static token (which must then itself be a valid admin JWT).
type TokenManager struct {
	client   *Client
	user     string
	password string

	mu     sync.Mutex
	cached string
}

// NewTokenManager builds a manager that logs in as user/password via client.
func NewTokenManager(client *Client, user, password string) *TokenManager {
	return &TokenManager{client: client, user: user, password: password}
}

// Enabled reports whether the manager can refresh (i.e. has credentials).
func (m *TokenManager) Enabled() bool {
	return m != nil && m.password != ""
}

// Get returns a cached admin JWT, logging in first if none is cached.
func (m *TokenManager) Get(ctx context.Context) (string, error) {
	if !m.Enabled() {
		return "", errors.New("openlist: token manager disabled (no admin password)")
	}
	m.mu.Lock()
	tok := m.cached
	m.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	return m.Refresh(ctx)
}

// Refresh logs in as admin and caches the resulting JWT, regardless of any
// cached value. Used to recover after a 401 ("token is invalidated").
func (m *TokenManager) Refresh(ctx context.Context) (string, error) {
	if !m.Enabled() {
		return "", errors.New("openlist: token manager disabled (no admin password)")
	}
	tok, err := m.client.Login(ctx, m.user, m.password)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.cached = tok
	m.mu.Unlock()
	return tok, nil
}

// timeNow is a small indirection; the real time package is used directly
// elsewhere. Kept as a named reference to make future test injection easy.
var timeNow = time.Now
