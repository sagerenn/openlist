package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/lb"
	"github.com/sagerenn/openlist/internal/ttl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- curl upload + download link ----

// doUpload performs a PUT /upload/<path> with the given auth header.
func doUpload(t *testing.T, method, extURL, path, authHeader, body string, extraHeaders map[string]string) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(method, extURL+"/upload"+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", authHeader)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var m map[string]interface{}
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &m), "body: %s", string(raw))
	}
	return resp.StatusCode, m
}

func uploadData(t *testing.T, m map[string]interface{}) map[string]interface{} {
	t.Helper()
	d, ok := m["data"].(map[string]interface{})
	require.True(t, ok, "response missing data: %v", m)
	return d
}

// ttlSetEnabled enables a 1-hour fixed TTL for the user, mirroring what
// the admin API does.
func ttlSetEnabled(userID uint) error {
	return ttl.SetConfig(ttl.UserTTLConfig{
		UserID:   userID,
		Enabled:  true,
		Mode:     ttl.ModeFixed,
		Duration: 3600,
	})
}

func ttlHasRecord(path string) (bool, error) {
	return ttl.HasRecord(path)
}

// hostOf extracts scheme://host from a base URL.
func hostOf(t *testing.T, base string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	return u.Scheme + "://" + u.Host
}

func TestE2E_CurlUploadReturnsDownloadLink(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	// A user key with the fs.put scope.
	ck, err := apikey.Create(60, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	code, m := doUpload(t, "PUT", extURL, "/docs/hello.txt", "Bearer "+ck.Secret, "hello world", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	d := uploadData(t, m)

	// The upload landed at the requested logical path...
	assert.Equal(t, "/docs/hello.txt", d["path"])
	// ...and the response carries a same-origin download link with the stub's
	// sign and the authoritative size.
	assert.Equal(t, extURL+"/d/docs/hello.txt?sign=sign%3A%2Fdocs%2Fhello.txt", d["url"])
	assert.Equal(t, "sign:/docs/hello.txt", d["sign"])
	assert.Equal(t, float64(len("hello world")), d["size"])

	// The body was streamed to OpenList unmodified.
	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/docs/hello.txt", uploads[0].Path)
	assert.Equal(t, "hello world", uploads[0].Body)
}

func TestE2E_CurlUploadFilenameWithSpaceAndUnicode(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	ck, err := apikey.Create(61, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	// Space + CJK + % in the name: exercises the decoded-param handling and
	// the re-escaping when building the download link.
	code, m := doUpload(t, "PUT", extURL, "/a%20b%20%E4%B8%AD%25.txt", "Bearer "+ck.Secret, "x", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	d := uploadData(t, m)

	assert.Equal(t, "/a b 中%.txt", d["path"])
	assert.Contains(t, d["url"], "/d/a%20b%20%E4%B8%AD%25.txt", "download link must re-escape the path per segment")

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/a b 中%.txt", uploads[0].Path)
}

func TestE2E_CurlUploadWithAdminToken(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	code, m := doUpload(t, "PUT", extURL, "/admin/file.txt", "test-admin-token", "admin-bytes", nil)
	require.Equal(t, 200, code, "admin token upload should succeed: %v", m)
	d := uploadData(t, m)
	assert.Equal(t, "/admin/file.txt", d["path"])
	assert.Contains(t, d["url"], "/d/admin/file.txt")

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/admin/file.txt", uploads[0].Path)
	assert.Equal(t, "admin-bytes", uploads[0].Body)
}

func TestE2E_CurlUploadAuthFailures(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// No auth => 401.
	code, m := doUpload(t, "PUT", extURL, "/x.txt", "", "x", nil)
	assert.Equal(t, 401, code)
	assert.NotEmpty(t, m["message"])

	// Invalid key => 401.
	code, _ = doUpload(t, "PUT", extURL, "/x.txt", "Bearer notarealkey12345678", "x", nil)
	assert.Equal(t, 401, code)

	// Valid key without fs.put scope => 403.
	ckRO, err := apikey.Create(62, "ro", []apikey.Scope{apikey.ScopeRead})
	require.NoError(t, err)
	code, _ = doUpload(t, "PUT", extURL, "/x.txt", "Bearer "+ckRO.Secret, "x", nil)
	assert.Equal(t, 403, code)

	// Disabled key => 401 (Verify only returns enabled keys).
	ck, err := apikey.Create(62, "dis", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)
	require.NoError(t, apikey.SetEnabled(62, ck.ID, false))
	code, _ = doUpload(t, "PUT", extURL, "/x.txt", "Bearer "+ck.Secret, "x", nil)
	assert.Equal(t, 401, code)
}

func TestE2E_CurlUploadMissingPath(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	ck, err := apikey.Create(63, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	// PUT /upload (no path) => 400.
	code, m := doUpload(t, "PUT", extURL, "", "Bearer "+ck.Secret, "x", nil)
	assert.Equal(t, 400, code)
	assert.NotEmpty(t, m["message"])
}

func TestE2E_CurlUploadTTLRecorded(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	// TTL enabled for user 64, plus a key.
	require.NoError(t, ttlSetEnabled(64))
	ck, err := apikey.Create(64, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	code, m := doUpload(t, "PUT", extURL, "/u64/t.txt", "Bearer "+ck.Secret, "data", nil)
	require.Equal(t, 200, code)
	d := uploadData(t, m)
	assert.Equal(t, "/u64/t.txt", d["path"])

	// The TTL record must exist for the uploaded path (same policy as
	// /api/fs/put uploads).
	has, err := ttlHasRecord("/u64/t.txt")
	require.NoError(t, err)
	assert.True(t, has, "curl upload must record a TTL entry for a TTL-enabled user")
}

func TestE2E_CurlUploadLBGroup(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	// A group for user 65 with two members.
	require.NoError(t, lb.CreateGroup(&lb.Group{Name: "g65", UserID: 65, Strategy: lb.StrategyRoundRobin}))
	g, err := lb.GetGroupByName(65, "g65")
	require.NoError(t, err)
	require.NoError(t, lb.AddMember(&lb.GroupMember{GroupID: g.ID, MountPath: "/s3", Weight: 1}))
	require.NoError(t, lb.AddMember(&lb.GroupMember{GroupID: g.ID, MountPath: "/oss", Weight: 1}))

	ck, err := apikey.Create(65, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	// Two uploads with X-LB-Group should alternate backends.
	for i := 0; i < 2; i++ {
		code, m := doUpload(t, "PUT", extURL, "/orig/f"+itoa(uint64(i))+".txt", "Bearer "+ck.Secret, "p", map[string]string{"X-LB-Group": "g65"})
		require.Equal(t, 200, code, "lb upload %d should succeed", i)
		d := uploadData(t, m)
		// path + url reflect the rewritten target, and the name is preserved.
		wantDir := []string{"/s3", "/oss"}[i]
		assert.Equal(t, wantDir+"/f"+itoa(uint64(i))+".txt", d["path"])
		assert.Contains(t, d["url"], "/d"+wantDir+"/f")
	}

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 2)
	assert.Equal(t, "/s3/f0.txt", uploads[0].Path)
	assert.Equal(t, "/oss/f1.txt", uploads[1].Path)

	// LB group owned by another user is invisible.
	ckOther, err := apikey.Create(66, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)
	code, m := doUpload(t, "PUT", extURL, "/x.txt", "Bearer "+ckOther.Secret, "p", map[string]string{"X-LB-Group": "g65"})
	assert.Equal(t, 404, code, "group owned by another user must not resolve: %v", m)

	// Admin token cannot use LB groups (no owning user).
	code, _ = doUpload(t, "PUT", extURL, "/x.txt", "test-admin-token", "p", map[string]string{"X-LB-Group": "g65"})
	assert.Equal(t, 400, code)
}

// TestE2E_CurlUploadLBByPath covers the one-command flow under path
// activation, no X-LB-Group header: curl -T under a group's path_prefix
// must be routed through the group, land on a member, return a download
// link for the physical file, AND record the upload so the logical path
// reads back through the group afterwards.
func TestE2E_CurlUploadLBByPath(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	// Group with prefix /pshare and members /s3 + /oss for user 69.
	secret, _ := lbPathSetup(t, extURL, 69)

	code, m := doUpload(t, "PUT", extURL, "/pshare/2026/plan.txt", "Bearer "+secret, "plan-bytes", nil)
	require.Equal(t, 200, code, "path-activated curl upload should succeed: %v", m)
	d := uploadData(t, m)

	// Routed through the group: the target is the member's mount with the
	// prefix replaced, and the response names both sides of the mapping.
	mount, _ := d["target_mount"].(string)
	assert.Contains(t, []string{"/s3", "/oss"}, mount, "must land on a group member: %v", d)
	assert.Equal(t, mount+"/2026/plan.txt", d["path"], "path must be the physical target")
	assert.Equal(t, "/pshare/2026/plan.txt", d["source_path"], "source_path must be the logical path under the prefix")

	// The stream went to the physical path.
	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, mount+"/2026/plan.txt", uploads[0].Path)
	assert.Equal(t, "plan-bytes", uploads[0].Body)

	// The download link is GETable and serves the stored bytes (sign and
	// all; the stub's /d/ answers file-bytes:<path>).
	link := strings.Replace(d["url"].(string), hostOf(t, extURL), extURL, 1)
	resp, err := http.Get(link)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "download link must be GETable: %s", link)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(b), "file-bytes:"), "GET on the returned link should serve the file, got: %s", string(b))

	// The upload was recorded (admin uploads view / read routing pin).
	respA := adminDo(t, "GET", extURL+"/ext/admin/lb/uploads?user_id=69", "")
	require.Equal(t, 200, respA.StatusCode)
	recs := respBody(t, respA)["data"].([]interface{})
	require.Len(t, recs, 1)
	rec := recs[0].(map[string]interface{})
	assert.Equal(t, "/pshare/2026/plan.txt", rec["source_path"])
	assert.Equal(t, mount, rec["target_mount"])
	assert.Equal(t, mount+"/2026/plan.txt", rec["target_path"])

	// Reading the file back through the LOGICAL path routes to the member
	// that holds it — the full upload-and-share loop in one prefix. The stub
	// records every GET, and this test already made two (the response's sign
	// lookup on the physical path, and the download-link GET on /d/...), so
	// assert on the LAST get: the logical read rewritten to the physical file.
	lbCode, _ := lbFsPost(t, extURL, secret, "get", `{"path":"/pshare/2026/plan.txt"}`)
	assert.Equal(t, 200, lbCode)
	gets := stub.SnapshotGets()
	require.NotEmpty(t, gets)
	assert.Equal(t, mount+"/2026/plan.txt", gets[len(gets)-1])
}

func TestE2E_CurlUploadPOSTForm(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	ck, err := apikey.Create(67, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	// POST with --data-binary style body also works.
	code, m := doUpload(t, "POST", extURL, "/via/post.txt", "Bearer "+ck.Secret, "posted", nil)
	require.Equal(t, 200, code, "POST upload should succeed: %v", m)
	d := uploadData(t, m)
	assert.Equal(t, "/via/post.txt", d["path"])

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/via/post.txt", uploads[0].Path)
	assert.Equal(t, "posted", uploads[0].Body)
}

func TestE2E_CurlUploadLinkIsDownloadable(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	ck, err := apikey.Create(68, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	code, m := doUpload(t, "PUT", extURL, "/dl/file.txt", "Bearer "+ck.Secret, "the-content", nil)
	require.Equal(t, 200, code)
	d := uploadData(t, m)
	url := d["url"].(string)

	// Replace the stub host with the extension host (the link is built from
	// the request's Host, which is the extension's).
	link := strings.Replace(url, hostOf(t, extURL), extURL, 1)

	// GET the link: the stub's /d/ passthrough must serve the file.
	resp, err := http.Get(link)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	// The stub /d/ handler answers "file-bytes:<path>".
	assert.True(t, strings.HasPrefix(string(b), "file-bytes:"), "GET on the returned link should serve the file, got: %s", string(b))
}
