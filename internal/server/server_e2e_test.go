package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/config"
	"github.com/sagerenn/openlist/internal/db"
	"github.com/sagerenn/openlist/internal/domain"
	"github.com/sagerenn/openlist/internal/lb"
	"github.com/sagerenn/openlist/internal/listperm"
	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/sagerenn/openlist/internal/ttl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupE2E starts a stub OpenList + the extension server (with OpenList's
// real routes NOT mounted; a stub forwarder stands in for them), returns the
// extension's base URL, the stub, and a cleanup. The admin token is
// "test-admin-token".
//
// In production the forwarder is c.Next() into OpenList's own routes mounted
// via server.Init; here we substitute a reverse proxy to a stub so the e2e
// tests run without booting real OpenList. The feature middleware logic under
// test is identical either way.
func setupE2E(t *testing.T) (extURL string, stub *testutil.StubOpenList, cleanup func()) {
	t.Helper()
	dbCleanup := testutil.SetupDB(t)
	stub = testutil.NewStubOpenList("test-admin-token")

	cfg := config.Default()
	cfg.LoopbackAddr = stub.URL()
	cfg.AdminToken = "test-admin-token"
	cfg.DBPath = "" // already initialized

	srv, err := New(cfg)
	require.NoError(t, err)
	// Stand in for OpenList's real routes: proxy non-intercepted file-op
	// requests to the stub. (Production uses c.Next() into server.Init.)
	target, err := url.Parse(stub.URL())
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Director: func(r *http.Request) {
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host
		r.Host = target.Host
	}}
	srv.SetForwarder(func(c *gin.Context) { proxy.ServeHTTP(c.Writer, c.Request) })

	engine := srv.Engine(false)
	ts := httptest.NewServer(engine)

	cleanup = func() {
		ts.Close()
		stub.Close()
		dbCleanup()
	}
	return ts.URL, stub, cleanup
}

func adminDo(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "test-admin-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func respBody(t *testing.T, resp *http.Response) map[string]interface{} {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	var m map[string]interface{}
	if len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, &m), "body: %s", string(b))
	}
	return m
}

// ---- Feature 1: per-user domain ----

func TestE2E_DomainRouting(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// Bind alice.example.com to user 10.
	resp := adminDo(t, "POST", extURL+"/ext/admin/domain", `{"user_id":10,"host":"alice.example.com"}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	// A request with that Host header should resolve to user 10 (recorded
	// in context). We verify via the list-permission guard: set user 10's
	// CanList=false, then a /fs/list request from that host should be 403.
	resp = adminDo(t, "PUT", extURL+"/ext/admin/listperm/10", `{"can_list":false,"can_read":true}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	req, _ := http.NewRequest("GET", extURL+"/api/fs/list?path=/", nil)
	req.Host = "alice.example.com"
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode, "user 10 has listing disabled; domain request should be blocked")
	respBody(t, resp)

	// A different host (no binding) is a guest and should pass through.
	req, _ = http.NewRequest("GET", extURL+"/api/fs/list?path=/", nil)
	req.Host = "unknown.example.com"
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, "guest should pass through to OpenList")
	respBody(t, resp)
}

// ---- Feature 2: TTL ----

func TestE2E_TTLReaperDeletesExpired(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	// Enable TTL for user 20: fixed, 1 second.
	resp := adminDo(t, "PUT", extURL+"/ext/admin/ttl/20", `{"enabled":true,"mode":"fixed","duration_seconds":1}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	// Create an API key for user 20 so we can upload as that user.
	resp = adminDo(t, "POST", extURL+"/ext/admin/apikey", `{"user_id":20,"name":"u20","scopes":["*"]}`)
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	secret := m["data"].(map[string]interface{})["secret"].(string)

	// Upload a file via the extension (PUT /fs/put) with the API key.
	uploadReq, _ := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("hello")))
	uploadReq.Header.Set("X-Api-Key", secret)
	uploadReq.Header.Set("File-Path", "/u20/file.txt")
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(uploadReq)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "upload should succeed")
	respBody(t, resp)

	// A TTL record should exist.
	has, err := ttl.HasRecord("/u20/file.txt")
	require.NoError(t, err)
	assert.True(t, has)

	// Wait past expiry and trigger a sweep.
	time.Sleep(1100 * time.Millisecond)
	resp = adminDo(t, "POST", extURL+"/ext/admin/ttl/sweep", "")
	require.Equal(t, 200, resp.StatusCode)
	m = respBody(t, resp)
	assert.Equal(t, float64(1), m["data"].(map[string]interface{})["deleted"], "one file should be deleted")

	// The stub should have received a remove for /u20/file.txt.
	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, "/u20", removes[0].Dir)
	assert.Equal(t, []string{"file.txt"}, removes[0].Names)

	// Record should be gone.
	has, err = ttl.HasRecord("/u20/file.txt")
	require.NoError(t, err)
	assert.False(t, has)
}

func TestE2E_TTLAccessModeSlidesOnDownload(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	require.NoError(t, ttl.SetConfig(ttl.UserTTLConfig{UserID: 21, Enabled: true, Mode: ttl.ModeAccess, Duration: 60}))

	// Create API key + record an upload directly via the package.
	ck, err := apikey.Create(21, "u21", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)
	require.NoError(t, ttl.RecordUpload(21, "/u21/sliding.txt", time.Now()))

	// Download the file via /d/ path with the API key; this should refresh
	// access time.
	var r ttl.FileTTLRecord
	require.NoError(t, db.DB().Where("path = ?", "/u21/sliding.txt").First(&r).Error)
	firstExpiry := r.ExpiresAt

	time.Sleep(50 * time.Millisecond)
	req, _ := http.NewRequest("GET", extURL+"/d/u21/sliding.txt", nil)
	req.Header.Set("X-Api-Key", ck.Secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	require.NoError(t, db.DB().Where("path = ?", "/u21/sliding.txt").First(&r).Error)
	assert.True(t, r.ExpiresAt.After(firstExpiry), "access-mode expiry should slide forward on download")
}

// ---- Feature 3: list permission ----

func TestE2E_ListPermissionBlocksListing(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// User 30: can read but not list.
	resp := adminDo(t, "PUT", extURL+"/ext/admin/listperm/30", `{"can_list":false,"can_read":true}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	ck, err := apikey.Create(30, "u30", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)

	// Listing is blocked.
	req, _ := http.NewRequest("GET", extURL+"/api/fs/list?path=/", nil)
	req.Header.Set("X-Api-Key", ck.Secret)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode)
	respBody(t, resp)

	// Direct read is allowed.
	req, _ = http.NewRequest("GET", extURL+"/api/fs/get?path=/file.txt", nil)
	req.Header.Set("X-Api-Key", ck.Secret)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, "read should be allowed when can_read=true")
	respBody(t, resp)
}

func TestE2E_ListPermissionWriteOnly(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// User 31: write-only drop box (no list, no read).
	resp := adminDo(t, "PUT", extURL+"/ext/admin/listperm/31", `{"can_list":false,"can_read":false}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	ck, err := apikey.Create(31, "u31", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)

	// Read blocked.
	req, _ := http.NewRequest("GET", extURL+"/api/fs/get?path=/file.txt", nil)
	req.Header.Set("X-Api-Key", ck.Secret)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode)

	// Upload still allowed (write not gated by listperm).
	uploadReq, _ := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("data")))
	uploadReq.Header.Set("X-Api-Key", ck.Secret)
	uploadReq.Header.Set("File-Path", "/u31/drop.txt")
	resp, err = http.DefaultClient.Do(uploadReq)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

// ---- Feature 4: load balance ----

func TestE2E_LoadBalanceUpload(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	// Create a group for user 40 with two backends.
	resp := adminDo(t, "POST", extURL+"/ext/admin/lb/group", `{"name":"g40","user_id":40,"strategy":"round_robin"}`)
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	groupID := uint64(m["data"].(map[string]interface{})["id"].(float64))

	resp = adminDo(t, "POST", extURL+"/ext/admin/lb/group/"+itoa(groupID)+"/member", `{"mount_path":"/s3","weight":1}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)
	resp = adminDo(t, "POST", extURL+"/ext/admin/lb/group/"+itoa(groupID)+"/member", `{"mount_path":"/oss","weight":1}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	ck, err := apikey.Create(40, "u40", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)

	// Upload 4 files through the LB group; should alternate /s3, /oss, /s3, /oss.
	for i := 0; i < 4; i++ {
		req, _ := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("payload")))
		req.Header.Set("X-Api-Key", ck.Secret)
		req.Header.Set("X-LB-Group", "g40")
		req.Header.Set("File-Path", "/orig/file"+itoa(uint64(i))+".txt")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode, "lb upload %d should succeed", i)
		body := respBody(t, resp)
		_ = body
		resp.Body.Close()
	}

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 4)
	paths := []string{}
	for _, u := range uploads {
		paths = append(paths, u.Path)
	}
	// Expect alternating mount prefixes.
	assert.Contains(t, paths[0], "/s3/")
	assert.Contains(t, paths[1], "/oss/")
	assert.Contains(t, paths[2], "/s3/")
	assert.Contains(t, paths[3], "/oss/")
	// Original filename preserved.
	for _, u := range uploads {
		assert.True(t, strings.HasSuffix(u.Path, ".txt"), "filename preserved: %s", u.Path)
	}
}

// ---- Feature 5: API key auth ----

func TestE2E_APIKeyAuthAndScope(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// Create a read-only key for user 50.
	resp := adminDo(t, "POST", extURL+"/ext/admin/apikey", `{"user_id":50,"name":"ro","scopes":["fs.get","fs.list"]}`)
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	secret := m["data"].(map[string]interface{})["secret"].(string)

	// Read is allowed.
	req, _ := http.NewRequest("GET", extURL+"/api/fs/get?path=/x", nil)
	req.Header.Set("X-Api-Key", secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	// Write is denied by scope.
	uploadReq, _ := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("x")))
	uploadReq.Header.Set("X-Api-Key", secret)
	uploadReq.Header.Set("File-Path", "/x.txt")
	resp, err = http.DefaultClient.Do(uploadReq)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode, "read-only key must not write")

	// Bogus key is rejected.
	req, _ = http.NewRequest("GET", extURL+"/api/fs/get?path=/x", nil)
	req.Header.Set("X-Api-Key", "notarealkey12345678")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 401, resp.StatusCode)
}

func TestE2E_APIKeyBearerForm(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	resp := adminDo(t, "POST", extURL+"/ext/admin/apikey", `{"user_id":51,"name":"b","scopes":["*"]}`)
	require.Equal(t, 200, resp.StatusCode)
	secret := respBody(t, resp)["data"].(map[string]interface{})["secret"].(string)

	// Bearer form.
	req, _ := http.NewRequest("GET", extURL+"/api/fs/get?path=/x", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
}

// ---- Feature 6: forward compatibility (smoke) ----

func TestE2E_AdminAuthRequired(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// No token => 401.
	resp, err := http.Get(extURL + "/ext/admin/domain/1")
	require.NoError(t, err)
	assert.Equal(t, 401, resp.StatusCode)
	resp.Body.Close()

	// Wrong token => 401.
	req, _ := http.NewRequest("GET", extURL+"/ext/admin/domain/1", nil)
	req.Header.Set("Authorization", "wrong")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 401, resp.StatusCode)
	resp.Body.Close()
}

func TestE2E_Healthz(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()
	resp, err := http.Get(extURL + "/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()
}

// itoa is a tiny uint64->string helper to avoid strconv import noise.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// Ensure feature packages are linked into the test binary (their init()
// registers models). The imports are used by the tests above too, but this
// guarantees presence even if a subset of tests is selected.
var (
	_ = domain.Set
	_ = listperm.Set
	_ = lb.CreateGroup
	_ = apikey.Create
	_ = ttl.SetConfig
)
