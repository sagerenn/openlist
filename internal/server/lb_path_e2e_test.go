package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sagerenn/openlist/internal/apikey"
	"github.com/sagerenn/openlist/internal/ttl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lbPathSetup provisions the fixture the path-activation tests share: a
// round-robin group with prefix /pshare and members /s3 and /oss for the
// given user, an API key for that user, and the group's DB id.
func lbPathSetup(t *testing.T, extURL string, userID uint) (secret string, groupID uint64) {
	t.Helper()
	resp := adminDo(t, "POST", extURL+"/ext/admin/lb/group",
		`{"name":"pshare-group","user_id":`+itoa(uint64(userID))+`,"strategy":"round_robin","path_prefix":"/pshare"}`)
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	groupID = uint64(m["data"].(map[string]interface{})["id"].(float64))
	resp = adminDo(t, "POST", extURL+"/ext/admin/lb/group/"+itoa(groupID)+"/member", `{"mount_path":"/s3","weight":1}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)
	resp = adminDo(t, "POST", extURL+"/ext/admin/lb/group/"+itoa(groupID)+"/member", `{"mount_path":"/oss","weight":1}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)
	ck, err := apikey.Create(userID, "pathuser", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)
	return ck.Secret, groupID
}

// lbPutUpload issues a PUT /api/fs/put with the given key, File-Path, and
// optional extra headers.
func lbPutUpload(t *testing.T, extURL, key, filePath string, extra map[string]string) map[string]interface{} {
	t.Helper()
	req, err := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("payload")))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("File-Path", filePath)
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "upload %s: %s", filePath, string(b))
	var env map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &env), "body: %s", string(b))
	return env
}

// lbFsPost issues a POST /api/fs/<op> with a JSON body, as the frontend
// and pshare do.
func lbFsPost(t *testing.T, extURL, key, op, body string) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest("POST", extURL+"/api/fs/"+op, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var env map[string]interface{}
	if len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, &env), "body: %s", string(b))
	}
	return resp.StatusCode, env
}

// TestE2E_LBPathActivationUpload covers the headline feature: an upload
// whose File-Path falls under a group's PathPrefix is load-balanced with
// NO X-LB-Group header. The prefix is replaced by the selected member's
// mount path and everything below it is preserved; the header still works
// alongside and keeps its legacy shape; a path OUTSIDE the prefix is not
// intercepted.
func TestE2E_LBPathActivationUpload(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	secret, _ := lbPathSetup(t, extURL, 50)

	// Two path-activated uploads. Assertions are relative to the returned
	// target mounts, not fixed ones: the in-process rr counter carries
	// across tests (group IDs restart per test DB), so which member is
	// "first" is not stable — but equal-weight alternation is.
	env := lbPutUpload(t, extURL, secret, "/pshare/2024/note.txt", nil)
	data := env["data"].(map[string]interface{})
	m1 := data["target_mount"].(string)
	assert.Equal(t, m1+"/2024/note.txt", data["target_path"])

	env = lbPutUpload(t, extURL, secret, "/pshare/2024/report.pdf", nil)
	data = env["data"].(map[string]interface{})
	m2 := data["target_mount"].(string)
	assert.Equal(t, m2+"/2024/report.pdf", data["target_path"])
	assert.NotEqual(t, m1, m2, "equal-weight round robin must alternate")

	// URL-encoded File-Path (as pshare sends it) decodes before routing.
	env = lbPutUpload(t, extURL, secret, url.PathEscape("/pshare/a b/c.txt"), nil)
	data = env["data"].(map[string]interface{})
	assert.Equal(t, m1+"/a b/c.txt", data["target_path"])

	// A path outside the prefix passes through untouched (the stub sees the
	// original path).
	lbPutUpload(t, extURL, secret, "/plain/file.txt", nil)
	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 4)
	assert.Equal(t, "/plain/file.txt", uploads[3].Path)

	// The header form still works and keeps its legacy shape:
	// <mount><PathPrefix>/<name> — source dirs dropped.
	env = lbPutUpload(t, extURL, secret, "/orig/x.txt", map[string]string{"X-LB-Group": "pshare-group"})
	data = env["data"].(map[string]interface{})
	assert.Equal(t, m2, data["target_mount"])
	assert.Equal(t, m2+"/pshare/x.txt", data["target_path"])
}

// TestE2E_LBPathActivationReadRemove covers read/remove routing under a
// group prefix: the JSON body's path is rewritten to the physical path the
// file was uploaded to (per the upload record); a remove also purges the
// record; a get on a path with no record falls back to strategy routing.
func TestE2E_LBPathActivationReadRemove(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	secret, _ := lbPathSetup(t, extURL, 51)

	// Upload two files under the prefix: with two equal-weight members the
	// round-robin alternates, so they land on DIFFERENT mounts. The tests
	// assert against the returned target paths rather than fixed mounts —
	// the in-process rr counter carries across tests (group IDs restart per
	// test DB), so which member is "first" is not stable.
	env := lbPutUpload(t, extURL, secret, "/pshare/one.txt", nil)
	mountA := env["data"].(map[string]interface{})["target_mount"].(string)
	require.Equal(t, mountA+"/one.txt", env["data"].(map[string]interface{})["target_path"])
	env = lbPutUpload(t, extURL, secret, "/pshare/two.txt", nil)
	mountB := env["data"].(map[string]interface{})["target_mount"].(string)
	require.NotEqual(t, mountA, mountB, "equal-weight round robin must alternate")

	// fs/get on the first file must be rewritten to its physical path.
	code, _ := lbFsPost(t, extURL, secret, "get", `{"path":"/pshare/one.txt"}`)
	assert.Equal(t, 200, code)
	gets := stub.SnapshotGets()
	require.Len(t, gets, 1)
	assert.Equal(t, mountA+"/one.txt", gets[0])

	// fs/get on the second (recorded on the other member) hits its own
	// physical path — the record pins the file, not the round-robin.
	code, _ = lbFsPost(t, extURL, secret, "get", `{"path":"/pshare/two.txt"}`)
	assert.Equal(t, 200, code)
	gets = stub.SnapshotGets()
	require.Len(t, gets, 2)
	assert.Equal(t, mountB+"/two.txt", gets[1])

	// A get with no record (never uploaded) falls back to strategy routing.
	// Reads PEEK at the round-robin position without consuming it (reads
	// must not distort the upload balancing), so after the two uploads the
	// fallback resolves to the member that would serve the NEXT upload:
	// mountA.
	code, _ = lbFsPost(t, extURL, secret, "get", `{"path":"/pshare/never-uploaded.txt"}`)
	assert.Equal(t, 200, code)
	gets = stub.SnapshotGets()
	require.Len(t, gets, 3)
	assert.Equal(t, mountA+"/never-uploaded.txt", gets[2])

	// fs/list of the prefix root is also rewritten, to the bare mount — not
	// the prefix. Another record-less read: it peeks at the same
	// unconsumed position, so it resolves to mountA again.
	code, _ = lbFsPost(t, extURL, secret, "list", `{"path":"/pshare"}`)
	assert.Equal(t, 200, code)
	lists := stub.SnapshotLists()
	require.Len(t, lists, 1)
	assert.Equal(t, mountA, lists[0])

	// fs/remove of the files: the body's dir is rewritten to the physical
	// parent, and the records are purged after success.
	code, _ = lbFsPost(t, extURL, secret, "remove", `{"dir":"/pshare","names":["one.txt"]}`)
	assert.Equal(t, 200, code)
	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, mountA, removes[0].Dir)
	assert.Equal(t, []string{"one.txt"}, removes[0].Names)

	// The record for one.txt is gone; two.txt's survives.
	resp := adminDo(t, "GET", extURL+"/ext/admin/lb/uploads?user_id=51", "")
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	list, ok := m["data"].([]interface{})
	require.True(t, ok, "data should be an array: %v", m["data"])
	require.Len(t, list, 1)
	rec := list[0].(map[string]interface{})
	assert.Equal(t, "/pshare/two.txt", rec["source_path"])
	assert.Equal(t, mountB+"/two.txt", rec["target_path"])
	assert.Equal(t, mountB, rec["target_mount"])
	assert.Equal(t, "pshare-group", rec["group_name"])
}

// TestE2E_LBUploadsAdminEndpoints covers the uploads-view endpoints:
// listing (per-user and all-users), and deleting one record.
func TestE2E_LBUploadsAdminEndpoints(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	secret, _ := lbPathSetup(t, extURL, 52)
	lbPutUpload(t, extURL, secret, "/pshare/a.txt", nil)
	lbPutUpload(t, extURL, secret, "/pshare/b.txt", nil)

	// All-users listing (no user_id).
	resp := adminDo(t, "GET", extURL+"/ext/admin/lb/uploads", "")
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	list := m["data"].([]interface{})
	require.Len(t, list, 2)
	// Newest first (b before a).
	assert.Equal(t, "/pshare/b.txt", list[0].(map[string]interface{})["source_path"])

	// Per-user listing.
	resp = adminDo(t, "GET", extURL+"/ext/admin/lb/uploads?user_id=52", "")
	require.Equal(t, 200, resp.StatusCode)
	m = respBody(t, resp)
	require.Len(t, m["data"].([]interface{}), 2)

	// Another user sees none.
	resp = adminDo(t, "GET", extURL+"/ext/admin/lb/uploads?user_id=53", "")
	require.Equal(t, 200, resp.StatusCode)
	m = respBody(t, resp)
	assert.Empty(t, m["data"])

	// Delete one record: metadata only — the stub recorded no remove.
	id := uint64(list[0].(map[string]interface{})["id"].(float64))
	resp = adminDo(t, "DELETE", extURL+"/ext/admin/lb/uploads/"+itoa(id), "")
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	resp = adminDo(t, "GET", extURL+"/ext/admin/lb/uploads?user_id=52", "")
	require.Equal(t, 200, resp.StatusCode)
	m = respBody(t, resp)
	require.Len(t, m["data"].([]interface{}), 1)
	assert.Equal(t, "/pshare/a.txt", m["data"].([]interface{})[0].(map[string]interface{})["source_path"])
}

// TestE2E_LBUploadRecordsTTL covers the TTL gap this work closed: an LB
// upload (header OR path form) now records TTL against the PHYSICAL target
// path, so the reaper deletes what the strategy actually wrote.
func TestE2E_LBUploadRecordsTTL(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	require.NoError(t, ttl.SetConfig(ttl.UserTTLConfig{UserID: 54, Enabled: true, Mode: ttl.ModeFixed, Duration: 60}))
	secret, _ := lbPathSetup(t, extURL, 54)

	// Path-activated upload with a per-file TTL override. Assert against
	// the returned target, not a fixed mount: the rr counter carries across
	// tests, so which member is "first" is not stable.
	env := lbPutUpload(t, extURL, secret, "/pshare/gone.txt", map[string]string{"X-Ttl": "1"})
	data := env["data"].(map[string]interface{})
	targetPath := data["target_path"].(string)
	targetMount := data["target_mount"].(string)

	has, err := ttl.HasRecord(targetPath)
	require.NoError(t, err)
	assert.True(t, has, "physical path should carry the TTL record")

	// Wait past expiry and sweep: the file is deleted at its physical path.
	time.Sleep(1100 * time.Millisecond)
	resp := adminDo(t, "POST", extURL+"/ext/admin/ttl/sweep", "")
	require.Equal(t, 200, resp.StatusCode)
	m := respBody(t, resp)
	assert.Equal(t, float64(1), m["data"].(map[string]interface{})["deleted"])
	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, targetMount, removes[0].Dir)
	name := targetPath[strings.LastIndex(targetPath, "/")+1:]
	assert.Equal(t, []string{name}, removes[0].Names)
}

// TestE2E_CORSOnFeatureChainResponses pins the regression where a
// cross-origin browser upload (the static pshare site) succeeded at the
// server but the browser refused to hand the response to the page: the
// feature chain short-circuits before OpenList's engine-level cors
// middleware, so responses it writes itself shipped without
// Access-Control-Allow-Origin and fetch threw — surfacing in the frontend
// as "could not reach OpenList". Every governed response must carry the
// header: the LB-path upload success AND the guard denials, so a browser
// client sees the real error instead of an opaque network failure.
func TestE2E_CORSOnFeatureChainResponses(t *testing.T) {
	extURL, _, cleanup := setupE2E(t)
	defer cleanup()

	secret, _ := lbPathSetup(t, extURL, 55)

	origin := map[string]string{"Origin": "https://sagerenn.github.io"}

	// LB-path upload (handled entirely by the extension's lbUpload): the
	// 200 must be readable cross-origin.
	req, err := http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("payload")))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", secret)
	req.Header.Set("File-Path", "/pshare/cors/note.txt")
	req.Header.Set("Origin", origin["Origin"])
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, "body: %s", string(b))
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"),
		"LB upload response must be readable by the cross-origin frontend")

	// Guard denial written by apiKeyAuth: same requirement.
	req, err = http.NewRequest("PUT", extURL+"/api/fs/put", bytes.NewReader([]byte("payload")))
	require.NoError(t, err)
	req.Header.Set("X-Api-Key", "not-a-real-key")
	req.Header.Set("File-Path", "/pshare/cors/note.txt")
	req.Header.Set("Origin", origin["Origin"])
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, 401, resp.StatusCode)
	assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"),
		"guard denials must be readable by the cross-origin frontend")
}
