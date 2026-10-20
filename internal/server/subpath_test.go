package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// ---- base path normalization (config) ----

func TestNormalizeBasePath(t *testing.T) {
	cases := []struct {
		siteURL string
		want    string
	}{
		{"", ""},
		{"/", ""},
		{"/files", "/files"},
		{"/files/", "/files"},
		{"files", "/files"},
		// No trimming, on purpose: OpenList's bootstrap does not trim
		// site_url either, and one shared normalizer (utils.FixAndCleanPath)
		// must decide the prefix for BOTH halves of the process — so the
		// routes and the extension agree even on a whitespace-polluted
		// value (they agree on "/  /files  ", odd as that prefix is).
		{"  /files  ", "/  /files  "},
		{"https://example.com", ""},
		{"https://example.com/", ""},
		{"https://example.com/files", "/files"},
		{"https://example.com/files/", "/files"},
		{"http://example.com:8080/files", "/files"},
		{"https://example.com/a/../b", "/b"},
		{"/a//b", "/a/b"},
		{"/a/./b/", "/a/b"},
		{"/a/../b", "/b"},
		{`\files`, "/files"},
		// A scheme-less value is parsed for its path by OpenList's initURL
		// (after FixAndCleanPath), so query/fragment contribute nothing.
		{"/files?x=1", "/files"},
		{"/files#frag", "/files"},
	}
	for _, tc := range cases {
		got := config.NormalizeBasePath(tc.siteURL)
		assert.Equal(t, tc.want, got, "NormalizeBasePath(%q)", tc.siteURL)

		var cfg config.Config
		cfg.SiteURL = tc.siteURL
		assert.Equal(t, tc.want, cfg.BasePath(), "Config.BasePath() with SiteURL %q", tc.siteURL)
	}
}

// ---- construction: loopback client target ----

// TestSubpath_BasePathCollisionRefused pins the guard against base paths
// that ARE a top-level route segment: behind a prefix-stripping proxy the
// stripped form of such a route (e.g. /d/file.txt with base /d) already
// starts with the base, so the wrapper cannot tell it from a
// pass-through-form request and the engine 404s it. Any base whose first
// segment is not a route segment is fine, however deep.
func TestSubpath_BasePathCollisionRefused(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	for _, bad := range []string{
		"/d", "/api", "/ext", "/upload", "/sd", "/i", "/dav", "/ping",
		// Full-URL form: the collision is in the path component.
		"https://example.com/api",
		// Deeper paths are equally ambiguous when they start with a route
		// segment: a download of the logical file "/x/y" under base /d/x is
		// /d/x/d/x/y, whose stripped form /d/x/y starts with the base.
		"/d/more",
	} {
		cfg := config.Default()
		cfg.AdminToken = "tok"
		cfg.SiteURL = bad
		_, err := New(cfg)
		require.Error(t, err, "New must refuse base path %q", bad)
		assert.Contains(t, err.Error(), "collides", "error for base %q", bad)
	}
	for _, ok := range []string{
		"", "/", "/files", "/files/d", "/my-site", "/files/upload",
		"https://example.com/files",
	} {
		cfg := config.Default()
		cfg.AdminToken = "tok"
		cfg.SiteURL = ok
		_, err := New(cfg)
		require.NoError(t, err, "New must accept base path %q", ok)
	}
}

func TestSubpath_LoopbackClientIncludesPrefix(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	cfg := config.Default()
	cfg.LoopbackAddr = "http://127.0.0.1:5245"
	cfg.SiteURL = "/files"
	cfg.AdminToken = "tok"
	srv, err := New(cfg)
	require.NoError(t, err)
	assert.Equal(t, "/files", srv.basePath)
	assert.Equal(t, "http://127.0.0.1:5245/files", srv.Client.BaseURL,
		"loopback calls must hit the prefixed routes")

	// An operator-supplied loopback address that already carries the path
	// is not double-prefixed.
	cfg.LoopbackAddr = "http://127.0.0.1:5245/files/"
	srv2, err := New(cfg)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:5245/files", srv2.Client.BaseURL)
}

// ---- engine routing under the prefix ----

func TestSubpath_ExtensionRoutesUnderPrefix(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	cfg := config.Default()
	cfg.AdminToken = "tok"
	cfg.SiteURL = "https://example.com/files"
	srv, err := New(cfg)
	require.NoError(t, err)

	// The raw engine routes ONLY under the prefix (this is what the engine
	// looks like to a proxy that passes the subpath through).
	engine := srv.Engine(false)
	ts := httptest.NewServer(engine)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/files/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 404, resp.StatusCode, "without the wrapper, root-absolute paths must not match")
	resp.Body.Close()
}

// ---- full deployment behavior behind the wrapper ----

// TestSubpath_Deployment exercises the whole service as it behaves behind a
// reverse proxy at https://host/files/: API, admin routes, guards, and the
// curl upload endpoint, each through BOTH request forms a proxy produces.
func TestSubpath_Deployment(t *testing.T) {
	extURL, stub, cleanup := setupE2EBase(t, "/files")
	defer cleanup()

	// --- healthz under both forms ---
	for _, p := range []string{"/files/ext/healthz", "/ext/healthz"} {
		resp, err := http.Get(extURL + p)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode, "GET %s", p)
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.JSONEq(t, `{"ok":true}`, string(b))
	}

	// --- admin API under the subpath ---
	resp := adminDo(t, "PUT", extURL+"/files/ext/admin/listperm/70", `{"can_list":false,"can_read":true}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)
	resp = adminDo(t, "PUT", extURL+"/ext/admin/listperm/70", `{"can_list":false,"can_read":true}`)
	require.Equal(t, 200, resp.StatusCode, "admin API via the stripped form too")
	respBody(t, resp)

	// --- API list through both proxy forms ---
	for _, p := range []string{"/files/api/fs/list?path=/", "/api/fs/list?path=/"} {
		req, _ := http.NewRequest("GET", extURL+p, nil)
		req.Header.Set("Authorization", "test-admin-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode, "GET %s (stub passthrough)", p)
		respBody(t, resp)
	}

	// --- feature guards still fire under the subpath ---
	ck, err := apikey.Create(70, "curl", []apikey.Scope{apikey.ScopeList, apikey.ScopeRead, apikey.ScopeWrite})
	require.NoError(t, err)

	// listperm: user 70 may not list.
	req, _ := http.NewRequest("GET", extURL+"/files/api/fs/list?path=/", nil)
	req.Header.Set("Authorization", "Bearer "+ck.Secret)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode, "list-permission guard must fire on <base>/api/fs/list")
	respBody(t, resp)
	// ... and via the stripped form.
	req, _ = http.NewRequest("GET", extURL+"/api/fs/list?path=/", nil)
	req.Header.Set("Authorization", "Bearer "+ck.Secret)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, 403, resp.StatusCode, "list-permission guard must fire via the stripped form as well")
	respBody(t, resp)

	// --- curl upload through the subpath: link must carry the prefix ---
	code, m := doUpload(t, "PUT", extURL, "/docs/hello.txt", "Bearer "+ck.Secret, "hello world", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	d := uploadData(t, m)
	assert.Equal(t, "/docs/hello.txt", d["path"], "the file's logical path must not include the base path")
	assert.Equal(t,
		extURL+"/files/d/docs/hello.txt?sign=sign%3A%2Fdocs%2Fhello.txt",
		d["url"], "download link must include the base path")

	// The upload was streamed to OpenList at the logical path.
	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/docs/hello.txt", uploads[0].Path)

	// --- download through the subpath reaches OpenList at the right path ---
	resp, err = http.Get(extURL + "/files/d/docs/hello.txt")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "file-bytes:/d/docs/hello.txt", string(b))
	assert.Contains(t, stub.SnapshotGets(), "/d/docs/hello.txt",
		"the stub (standing in for OpenList's group-relative routes) must see the un-prefixed /d/ path")
}

// TestSubpath_XForwardedHostLink checks the generated download link honors
// X-Forwarded-Host/Proto from the proxy while keeping the base path.
func TestSubpath_XForwardedHostLink(t *testing.T) {
	extURL, _, cleanup := setupE2EBase(t, "/files")
	defer cleanup()

	ck, err := apikey.Create(71, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	req, err := http.NewRequest("PUT", extURL+"/upload/x.txt", strings.NewReader("x"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+ck.Secret)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "files.example.com")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body := respBody(t, resp)
	d := uploadData(t, body)
	assert.Equal(t, "https://files.example.com/files/d/x.txt?sign=sign%3A%2Fx.txt", d["url"])
}

// TestSubpath_FullSiteURLPinsLinkOrigin verifies the other half of
// OpenList's site_url link semantics: when SITE_URL is a full URL, links
// are pinned to that origin — the declared public address — instead of
// being derived from the request's (here loopback) host. Routing still
// uses only the path component.
func TestSubpath_FullSiteURLPinsLinkOrigin(t *testing.T) {
	extURL, _, cleanup := setupE2EBase(t, "https://files.example.com/files")
	defer cleanup()

	// Routes are under /files, as with a bare-path SITE_URL.
	resp, err := http.Get(extURL + "/files/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	ck, err := apikey.Create(74, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	code, m := doUpload(t, "PUT", extURL, "/x.txt", "Bearer "+ck.Secret, "x", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	d := uploadData(t, m)
	assert.Equal(t, "https://files.example.com/files/d/x.txt?sign=sign%3A%2Fx.txt", d["url"])
}

// TestSubpath_RootUnchanged verifies a root deployment (no SiteURL) keeps
// the exact request shapes the rest of the suite (and the README) rely on.
func TestSubpath_RootUnchanged(t *testing.T) {
	extURL, stub, cleanup := setupE2E(t)
	defer cleanup()

	resp, err := http.Get(extURL + "/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	ck, err := apikey.Create(72, "curl", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)
	code, m := doUpload(t, "PUT", extURL, "/hello.txt", "Bearer "+ck.Secret, "hello", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	d := uploadData(t, m)
	assert.Equal(t, extURL+"/d/hello.txt?sign=sign%3A%2Fhello.txt", d["url"])

	resp, err = http.Get(extURL + "/d/hello.txt")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()
	assert.Contains(t, stub.SnapshotGets(), "/d/hello.txt")
}

// TestSubpath_DownloadRecordsAccess proves applyTTL's path extraction
// works with the prefix and actually SLIDES the window: with TTL in
// ModeAccess, a TTL record created (and backdated) for the logical
// (un-prefixed) path is refreshed by a <base>/d/... download, moving both
// LastAccess and ExpiresAt forward.
func TestSubpath_DownloadRecordsAccess(t *testing.T) {
	extURL, _, cleanup := setupE2EBase(t, "/files")
	defer cleanup()

	ck, err := apikey.Create(73, "curl", []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite})
	require.NoError(t, err)

	// ModeAccess: the whole point is that a download slides ExpiresAt.
	require.NoError(t, ttl.SetConfig(ttl.UserTTLConfig{
		UserID: 73, Enabled: true, Mode: ttl.ModeAccess, Duration: 3600,
	}))
	code, m := doUpload(t, "PUT", extURL, "/ttlfile.txt", "Bearer "+ck.Secret, "ttl", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	uploadData(t, m)

	rec, err := ttl.Record("/ttlfile.txt")
	require.NoError(t, err)
	require.NotNil(t, rec, "TTL record created for the logical path (no base path)")

	// Backdate the record two hours so a slide is unmistakable.
	backdated := time.Now().Add(-2 * time.Hour)
	require.NoError(t, db.DB().Model(&ttl.FileTTLRecord{}).
		Where("path = ?", "/ttlfile.txt").
		Updates(map[string]interface{}{"last_access": backdated, "expires_at": backdated}).Error)

	// A download through the subpath serves the file and refreshes the
	// record (RecordAccess is keyed by the extracted logical path).
	resp, err := http.Get(extURL + "/files/d/ttlfile.txt")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "file-bytes:/d/ttlfile.txt", string(b))

	rec, err = ttl.Record("/ttlfile.txt")
	require.NoError(t, err)
	require.NotNil(t, rec, "TTL record survives the subpath download")
	assert.True(t, rec.LastAccess.After(backdated.Add(time.Hour)),
		"LastAccess must slide on download: got %v, backdated %v", rec.LastAccess, backdated)
	assert.True(t, rec.ExpiresAt.After(backdated.Add(time.Hour)),
		"ModeAccess ExpiresAt must slide on download: got %v, backdated %v", rec.ExpiresAt, backdated)
}

// TestSubpath_ShareDownloadRecordsAccess proves share downloads (/sd) also
// refresh the TTL window — at the file's LOGICAL path. The share URL
// carries a share id where other download routes carry the path, so the
// guard resolves the id through the share API (as the admin, since share
// links are anonymous) and joins the share-relative remainder onto the
// share's root(s) exactly like OpenList's own op.GetSharingUnwrapPath.
func TestSubpath_ShareDownloadRecordsAccess(t *testing.T) {
	extURL, stub, cleanup := setupE2EBase(t, "/files")
	defer cleanup()

	ck, err := apikey.Create(76, "curl", []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite})
	require.NoError(t, err)
	require.NoError(t, ttl.SetConfig(ttl.UserTTLConfig{
		UserID: 76, Enabled: true, Mode: ttl.ModeAccess, Duration: 3600,
	}))

	// Single-root share "sh9": /srv9/kept.txt is shared as /sd/sh9/kept.txt.
	stub.SetShare("sh9", []string{"/srv9"})
	code, m := doUpload(t, "PUT", extURL, "/srv9/kept.txt", "Bearer "+ck.Secret, "kept", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	uploadData(t, m)

	// Multi-root share "shM": two storages, roots /mntA/photos and
	// /mntB/videos; the share's first segment names the root (by base
	// name), as in OpenList's multi-file shares.
	stub.SetShare("shM", []string{"/mntA/photos", "/mntB/videos"})
	code, m = doUpload(t, "PUT", extURL, "/mntA/photos/pic.jpg", "Bearer "+ck.Secret, "pic", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	uploadData(t, m)
	// A second file under the same root, downloaded by nobody: its record
	// staying backdated is the proof that an unresolvable share URL slides
	// nothing.
	code, m = doUpload(t, "PUT", extURL, "/mntA/photos/other.jpg", "Bearer "+ck.Secret, "other", nil)
	require.Equal(t, 200, code, "upload should succeed: %v", m)
	uploadData(t, m)

	// Backdate all records so a slide is unmistakable.
	backdated := time.Now().Add(-2 * time.Hour)
	require.NoError(t, db.DB().Model(&ttl.FileTTLRecord{}).
		Where("path IN ?", []string{"/srv9/kept.txt", "/mntA/photos/pic.jpg", "/mntA/photos/other.jpg"}).
		Updates(map[string]interface{}{"last_access": backdated, "expires_at": backdated}).Error)

	// --- single-root share download slides the window at the logical path.
	resp, err := http.Get(extURL + "/files/sd/sh9/kept.txt")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "share-bytes:/sd/sh9/kept.txt", string(b))

	rec, err := ttl.Record("/srv9/kept.txt")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.ExpiresAt.After(backdated.Add(time.Hour)),
		"single-root share download must slide the TTL window at the resolved path: got %v", rec.ExpiresAt)

	// --- multi-root share download maps the first share segment to the
	// matching root (photos => /mntA/photos, by base name, like OpenList).
	resp, err = http.Get(extURL + "/files/sd/shM/photos/pic.jpg")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()

	rec, err = ttl.Record("/mntA/photos/pic.jpg")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.ExpiresAt.After(backdated.Add(time.Hour)),
		"multi-root share download must slide the TTL window at the mapped path: got %v", rec.ExpiresAt)

	// --- a share URL whose first segment matches no root resolves to
	// nothing: the download still serves, and no record slides (the
	// untouched other.jpg proves it — a slide would have moved it, since
	// "nope" maps to no root at all).
	resp, err = http.Get(extURL + "/files/sd/shM/nope/pic.jpg")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode, "unresolvable share path must not break the download")
	resp.Body.Close()
	rec, err = ttl.Record("/mntA/photos/other.jpg")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, rec.ExpiresAt.After(backdated.Add(90*time.Minute)),
		"a no-match share path must not refresh any record")

	// --- an unknown share id likewise resolves to nothing.
	resp, err = http.Get(extURL + "/files/sd/noshare/kept.txt")
	require.NoError(t, err)
	resp.Body.Close()
	rec, err = ttl.Record("/srv9/kept.txt")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.True(t, rec.ExpiresAt.After(backdated.Add(time.Hour)),
		"the earlier slide must persist (the unknown-share download must not touch the record)")
}

// TestUnwrapSharePath pins the share-root resolution against OpenList's
// op.GetSharingUnwrapPath semantics: single root joins, multi root maps the
// first share segment onto a root by base name (first match wins).
func TestUnwrapSharePath(t *testing.T) {
	cases := []struct {
		roots []string
		rel   string
		want  string
	}{
		{[]string{"/srv"}, "/a.txt", "/srv/a.txt"},
		{[]string{"/srv"}, "/", "/srv"},
		{[]string{"/srv/nested"}, "/deep/a.txt", "/srv/nested/deep/a.txt"},
		{[]string{"/mntA/photos", "/mntB/videos"}, "/photos/pic.jpg", "/mntA/photos/pic.jpg"},
		{[]string{"/mntA/photos", "/mntB/videos"}, "/videos/x.mp4", "/mntB/videos/x.mp4"},
		{[]string{"/mntA/photos", "/mntB/videos"}, "/docs/x", ""},
		{[]string{"/mntA/photos", "/mntB/videos"}, "/", ""},
		// Same base name on two roots: the first root in Files order wins.
		{[]string{"/mntA/dup", "/mntB/dup"}, "/dup/a.txt", "/mntA/dup/a.txt"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, unwrapSharePath(tc.roots, tc.rel),
			"unwrapSharePath(%v, %q)", tc.roots, tc.rel)
	}
}

// TestSubpath_EnsurePlaceholderDistWritesSiteURL verifies the config.json
// patcher records the site URL OpenList's bootstrap will read.
func TestSubpath_EnsurePlaceholderDistWritesSiteURL(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644))
	require.NoError(t, ensurePlaceholderDist(dir, "/files"))

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	assert.Equal(t, "/files", cfg["site_url"])
	assert.NotEmpty(t, cfg["dist_dir"])

	// Empty site URL stays empty: OpenList normalizes it to "/" (root mount)
	// and derives links from the request host instead of a placeholder.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o644))
	require.NoError(t, ensurePlaceholderDist(dir, ""))
	raw, err = os.ReadFile(filepath.Join(dir, "config.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &cfg))
	assert.Equal(t, "", cfg["site_url"], "empty site URL must stay empty (request-derived links)")
}

// TestSubpath_SiteURLOwnership pins who owns OpenList's config.json
// site_url. The extension pins its SITE_URL there on every boot, but must
// not clobber a value it did not write: a pre-existing OpenList config, or
// an operator's direct edit, is refused loudly (like the
// OPENLIST_SITE_URL env guard) instead of being silently overwritten. The
// sidecar marker (config.json cannot carry one — OpenList rewrites the file
// from its struct each boot) distinguishes "we wrote this value" (SITE_URL
// change wins) from "someone else did".
func TestSubpath_SiteURLOwnership(t *testing.T) {
	// boot builds a data dir whose config.json carries the given site_url,
	// optionally pre-seeded with a marker from a previous extension boot.
	boot := func(siteURL, marker string) string {
		dir := t.TempDir()
		body := `{}`
		if siteURL != "" {
			body = `{"site_url": ` + strconv.Quote(siteURL) + `}`
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644))
		if marker != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, siteURLMarkerFile), []byte(marker), 0o644))
		}
		return dir
	}
	siteURLOf := func(dir string) string {
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		require.NoError(t, err)
		var cfg map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &cfg))
		v, _ := cfg["site_url"].(string)
		return v
	}
	markerOf := func(dir string) string {
		raw, err := os.ReadFile(filepath.Join(dir, siteURLMarkerFile))
		if err != nil {
			return "<absent>"
		}
		return strings.TrimSpace(string(raw))
	}

	t.Run("fresh default adopts SITE_URL", func(t *testing.T) {
		dir := boot("", "")
		require.NoError(t, ensurePlaceholderDist(dir, "/files"))
		assert.Equal(t, "/files", siteURLOf(dir))
		assert.Equal(t, "/files", markerOf(dir))
	})

	t.Run("steady state is untouched", func(t *testing.T) {
		dir := boot("/files", "/files")
		require.NoError(t, ensurePlaceholderDist(dir, "/files"))
		assert.Equal(t, "/files", siteURLOf(dir))
	})

	t.Run("SITE_URL change wins over the value we wrote", func(t *testing.T) {
		dir := boot("/files", "/files")
		require.NoError(t, ensurePlaceholderDist(dir, "/docs"))
		assert.Equal(t, "/docs", siteURLOf(dir))
		assert.Equal(t, "/docs", markerOf(dir))
	})

	t.Run("unmarked config carrying the value is adopted (migration)", func(t *testing.T) {
		// A deployment of the pre-marker releases wrote SITE_URL into
		// config.json without a marker; upgrading must not refuse.
		dir := boot("/files", "")
		require.NoError(t, ensurePlaceholderDist(dir, "/files"))
		assert.Equal(t, "/files", siteURLOf(dir))
		assert.Equal(t, "/files", markerOf(dir))
	})

	t.Run("operator edit is refused, not clobbered", func(t *testing.T) {
		dir := boot("https://example.com/files", "/files")
		err := ensurePlaceholderDist(dir, "/files")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "site_url")
		assert.Contains(t, err.Error(), "https://example.com/files")
		assert.Equal(t, "https://example.com/files", siteURLOf(dir),
			"a refused boot must leave the operator's value in place")
	})

	t.Run("pre-existing OpenList config is refused", func(t *testing.T) {
		dir := boot("/their-path", "")
		err := ensurePlaceholderDist(dir, "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "SITE_URL")
		assert.Equal(t, "/their-path", siteURLOf(dir))
	})

	t.Run("reset-to-default is adopted", func(t *testing.T) {
		// The operator reset site_url to OpenList's pristine "": SITE_URL
		// takes over again.
		dir := boot("", "/files")
		require.NoError(t, ensurePlaceholderDist(dir, "/files"))
		assert.Equal(t, "/files", siteURLOf(dir))
	})
}

// ---- download-route coverage (archive + share downloads) ----

// TestClassifyPath pins the path classifier to OpenList's route surface:
// every direct download form (plain, proxy, archive, share) engages the
// feature chain; auth/admin/webdav/s3 paths do not.
func TestClassifyPath(t *testing.T) {
	cases := []struct {
		path string
		want pathKind
	}{
		{"/api/fs/list", kindAPIFS},
		{"/api/fs/get", kindAPIFS},
		{"/api/fs/put", kindAPIFS},
		{"/api/fs/mkdir", kindAPIFS},
		{"/api/fs", kindOther},
		{"/d/file.txt", kindDownload},
		{"/d", kindDownload},
		{"/p/file.txt", kindDownload},
		{"/ad/dir/file.zip", kindDownload},
		{"/ap/dir/file.zip", kindDownload},
		{"/ae/dir/file.zip/inner.txt", kindDownload},
		{"/sd/share1/dir/file.txt", kindDownload},
		{"/sad/share1/file.zip/inner.txt", kindDownload},
		{"/ext/healthz", kindExt},
		{"/ext/admin/domain", kindExt},
		{"/api/auth/login", kindOther},
		{"/api/public/settings", kindOther},
		{"/api/admin/user/list", kindOther},
		{"/dav/file.txt", kindOther}, // WebDAV: OpenList applies its own auth
		{"/s3/file.txt", kindOther},
		{"/ping", kindOther},
		{"/", kindOther},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, classifyPath(tc.path), "classifyPath(%q)", tc.path)
	}
}

// TestDownloadPath covers the logical-path extraction for every download
// route family, including the share-id segment skip.
func TestDownloadPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/d/file.txt", "/file.txt"},
		{"/d/a/b.txt", "/a/b.txt"},
		{"/d", ""},
		{"/d/", ""},
		{"/p/file.txt", "/file.txt"},
		{"/ad/dir/file.zip", "/dir/file.zip"},
		{"/ap/dir/file.zip", "/dir/file.zip"},
		{"/ae/dir/file.zip/inner.txt", "/dir/file.zip/inner.txt"},
		{"/sd/share1/dir/file.txt", "/dir/file.txt"},
		{"/sd/share1", ""},
		{"/sd/share1/", ""},
		{"/sad/share1/file.zip/inner.txt", "/file.zip/inner.txt"},
		{"/api/fs/get", ""},
		// fs/get carries its path in the POST body; the URL never has one
		// to extract (and no such route exists anyway).
		{"/api/fs/get/x", ""},
		{"/api/fs/list", ""},
		{"/upload/file.txt", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, downloadPath(tc.path), "downloadPath(%q)", tc.path)
	}
}

// TestIsDownloadPath covers the route-family membership check that decides
// whether the feature chain engages (and TTL access recording fires).
func TestIsDownloadPath(t *testing.T) {
	for _, p := range []string{
		"/d", "/d/", "/d/file.txt",
		"/p", "/p/file.txt",
		"/ad", "/ad/dir/file.zip", "/ap/dir/file.zip", "/ae/dir/file.zip/inner.txt",
		"/sd/share1", "/sd/share1/dir/file.txt", "/sad/share1/file.zip",
		"/api/fs/get",
	} {
		assert.True(t, isDownloadPath(p), "isDownloadPath(%q)", p)
	}
	for _, p := range []string{
		"", "/", "/dav/file.txt", "/s3/file.txt",
		"/api/fs/list", "/api/fs/put", "/api/auth/login", "/upload/file.txt",
		"/debug/pprof/",
	} {
		assert.False(t, isDownloadPath(p), "isDownloadPath(%q)", p)
	}
}

// TestSubpath_ArchiveAndShareDownloadsGuarded proves the feature chain
// engages on OpenList's archive (/ad, /ap) and share (/sd) download routes
// under a subpath deployment: a user with reads disabled is denied on
// every form, prefixed and stripped alike.
func TestSubpath_ArchiveAndShareDownloadsGuarded(t *testing.T) {
	extURL, _, cleanup := setupE2EBase(t, "/files")
	defer cleanup()

	resp := adminDo(t, "PUT", extURL+"/files/ext/admin/listperm/75", `{"can_list":true,"can_read":false}`)
	require.Equal(t, 200, resp.StatusCode)
	respBody(t, resp)

	ck, err := apikey.Create(75, "curl", []apikey.Scope{apikey.ScopeRead, apikey.ScopeList})
	require.NoError(t, err)

	for _, p := range []string{
		"/files/ad/dir/file.zip",
		"/files/ap/dir/file.zip",
		"/files/ae/dir/file.zip/inner.txt",
		"/files/sd/share1/dir/file.txt",
		"/files/sad/share1/file.zip/inner.txt",
		// ... and the stripped form a prefix-stripping proxy produces.
		"/ad/dir/file.zip",
		"/sd/share1/dir/file.txt",
	} {
		req, err := http.NewRequest("GET", extURL+p, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+ck.Secret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, 403, resp.StatusCode, "read guard must fire on GET %s", p)
		respBody(t, resp)
	}
}

// TestSubpath_OpenListEnvSiteURLConflict verifies the guard against an
// inherited OPENLIST_SITE_URL: OpenList's bootstrap lets that variable
// override the config.json site_url this extension writes, so a value
// pointing at a different base path would silently 404 every route. The
// guard must reject it (and accept matching base paths in other forms).
func TestSubpath_OpenListEnvSiteURLConflict(t *testing.T) {
	for _, tc := range []struct {
		name        string
		siteURL     string
		envSiteURL  string
		expectError bool
	}{
		{"env unset", "/files", "", false},
		{"same base path, other form", "/files", "https://example.com/files", false},
		{"different prefix", "/files", "/docs", true},
		{"env set, extension at root", "", "/files", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPENLIST_SITE_URL", tc.envSiteURL)
			err := checkSiteURLEnvConflict(tc.siteURL)
			if tc.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "OPENLIST_SITE_URL")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestSubpath_WrapperDoesNotMutateRequest verifies the re-prefixing wrapper
// follows http.StripPrefix's precedent and clones the request instead of
// mutating it in place: non-origin-form request targets (absolute-form
// proxies, CONNECT) must not end up with a corrupted r.RequestURI, and the
// caller's request must be untouched.
func TestSubpath_WrapperDoesNotMutateRequest(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	cfg := config.Default()
	cfg.AdminToken = "tok"
	cfg.SiteURL = "/files"
	srv, err := New(cfg)
	require.NoError(t, err)

	handler := srv.Handler(false)

	// Origin-form: routed and RequestURI consistent with the path.
	originReq := httptest.NewRequest("GET", "/api/fs/list?path=/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, originReq)
	assert.Equal(t, "/api/fs/list?path=/", originReq.RequestURI,
		"caller's request must not be mutated")
	assert.Equal(t, "/api/fs/list", originReq.URL.Path)

	// Absolute-form: routing uses URL.Path, and the request target must
	// not be mangled into a prefix+URL hybrid.
	absReq := httptest.NewRequest("GET", "http://backend:5245/api/fs/list?path=/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, absReq)
	assert.Equal(t, "http://backend:5245/api/fs/list?path=/", absReq.RequestURI,
		"absolute-form request target must survive the wrapper intact")
	assert.Equal(t, "/api/fs/list", absReq.URL.Path,
		"the caller's request is untouched; the prefix lives on the internal clone")
}

// TestSubpath_WrapperStripsForwardedPrefix verifies the wrapper drops
// X-Forwarded-Prefix before the engine routes the request. gin's
// redirectTrailingSlash prepends that header to the redirect target it
// builds, and every route is already under the base path — leaving the
// header in place doubles the prefix on trailing-slash redirects
// (/files/pingx/ => /files/files/pingx), which a Traefik or
// X-Forwarded-Prefix-setting nginx in front would otherwise trigger. Both
// proxy forms are covered, and the caller's request must keep its header.
func TestSubpath_WrapperStripsForwardedPrefix(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	cfg := config.Default()
	cfg.AdminToken = "tok"
	cfg.SiteURL = "/files"
	srv, err := New(cfg)
	require.NoError(t, err)

	engine := srv.Engine(false)
	engine.GET("/files/pingx", func(c *gin.Context) { c.String(200, "pong") })
	handler := srv.subpathHandler(engine)

	// A client that does NOT follow the redirect, so Location is inspectable.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, path := range []string{
		"/files/pingx/", // proxy passes the subpath through
		"/pingx/",       // proxy strips the subpath
	} {
		req, err := http.NewRequest("GET", "http://backend"+path, nil)
		require.NoError(t, err)
		req.Header.Set("X-Forwarded-Prefix", "/files")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		// gin absolutizes the redirect Location against the request host;
		// assert on the parsed path so the assertion is host-agnostic.
		loc, err := url.Parse(rec.Header().Get("Location"))
		require.NoError(t, err)
		assert.Equal(t, "/files/pingx", loc.Path,
			"trailing-slash redirect from %s must carry the base path exactly once", path)
		assert.Equal(t, "/files", req.Header.Get("X-Forwarded-Prefix"),
			"the caller's request must not be mutated")
	}

	// Without the header the redirect is the same single-prefix target.
	req, err := http.NewRequest("GET", "http://backend/files/pingx/", nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/files/pingx", loc.Path)

	// And through a real server round trip (net/http normalizes request
	// forms), the redirect is still single-prefix and the plain route
	// serves.
	ts := httptest.NewServer(handler)
	defer ts.Close()
	req, err = http.NewRequest("GET", ts.URL+"/files/pingx/", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-Prefix", "/files")
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	loc, err = url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "/files/pingx", loc.Path)
}

// TestSubpath_LBUploadAbortsBeforeRoute reproduces the production shape for
// a load-balanced upload: real route handlers exist behind the feature
// middleware (a stand-in for OpenList's PUT /fs/put route), and lbUpload
// must ABORT after writing its response. Without the abort, gin's chain
// continues into the route handler on a request whose body was already
// consumed, appending a second response.
func TestSubpath_LBUploadAbortsBeforeRoute(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	stub := testutil.NewStubOpenList("test-admin-token")
	defer stub.Close()

	cfg := config.Default()
	cfg.LoopbackAddr = stub.URL()
	cfg.AdminToken = "test-admin-token"
	cfg.DBPath = "" // already initialized
	srv, err := New(cfg)
	require.NoError(t, err)

	var leakRuns int32
	engine := srv.Engine(false)
	engine.PUT("/api/fs/put", func(c *gin.Context) {
		atomic.StoreInt32(&leakRuns, 1)
		c.JSON(200, gin.H{"code": 200, "message": "route-leak"})
	})
	ts := httptest.NewServer(engine)
	defer ts.Close()

	// least_used, not round_robin: the round-robin counter is a
	// process-global keyed by group ID, and every test's first group gets
	// ID 1 in its fresh DB — one extra tick here would shift the start
	// position of the round-robin tests that run later in the package.
	require.NoError(t, lb.CreateGroup(&lb.Group{Name: "g90", UserID: 90, Strategy: lb.StrategyLeastUsed}))
	groups, err := lb.ListGroups(90)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.NoError(t, lb.AddMember(&lb.GroupMember{GroupID: groups[0].ID, MountPath: "/mnt90"}))
	ck, err := apikey.Create(90, "u90", []apikey.Scope{apikey.ScopeWrite})
	require.NoError(t, err)

	req, err := http.NewRequest("PUT", ts.URL+"/api/fs/put", strings.NewReader("payload"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+ck.Secret)
	req.Header.Set("File-Path", "/orig.txt")
	req.Header.Set("X-LB-Group", "g90")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, 200, resp.StatusCode)
	assert.True(t, json.Valid(b), "response must be a single JSON document, got: %s", b)
	assert.Equal(t, int32(0), atomic.LoadInt32(&leakRuns), "the /api/fs/put route handler must not run")
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	d, ok := m["data"].(map[string]interface{})
	require.True(t, ok, "response missing data: %v", m)
	assert.Equal(t, "/mnt90/orig.txt", d["target_path"], "the LB member's mount path is the upload target")

	// The upload itself landed in the (stub) storage at the rewritten path.
	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/mnt90/orig.txt", uploads[0].Path)
}

// TestSubpath_GuardsRunBeforeRouteHandlers reproduces the PRODUCTION shape,
// which the stub-proxy e2e harness cannot: real route handlers are mounted on
// the engine after the feature middleware, and the forwarder is the default
// c.Next(). The feature guards must therefore finish — and deny — BEFORE the
// route handler runs, and an allowed request must reach the route exactly
// once. (Regression test: apiKeyAuth and domainAuth used to end with
// c.Next(); invoked as nested calls from featureMiddleware that advanced the
// engine's chain into the route handler while the orchestrator was still
// running guards, so a list-denied user received the full listing body with
// status 200 and the 403 appended after it.)
func TestSubpath_GuardsRunBeforeRouteHandlers(t *testing.T) {
	dbCleanup := testutil.SetupDB(t)
	defer dbCleanup()

	cfg := config.Default()
	cfg.AdminToken = "tok"
	cfg.SiteURL = "/files"
	srv, err := New(cfg)
	require.NoError(t, err)

	// A stand-in for OpenList's /api/fs routes, mounted under the base path
	// like server.Init does in production. Its body is the secret the
	// guards must keep off the wire for denied users.
	engine := srv.Engine(false)
	leak := func(c *gin.Context) {
		c.JSON(200, gin.H{"code": 200, "data": []string{"secret.txt"}})
	}
	engine.POST("/files/api/fs/list", leak)
	engine.POST("/files/api/fs/get", leak)
	ts := httptest.NewServer(engine)
	defer ts.Close()

	do := func(path, authz, host string) (*http.Response, string) {
		req, err := http.NewRequest("POST", ts.URL+path, nil)
		require.NoError(t, err)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		resp.Body.Close()
		return resp, string(b)
	}

	// Denied listing (feature 3 via API key, feature 5): the user's scope
	// check passes, so only listPermGuard denies — after every guard, before
	// the route. One 403, no listing body, no concatenated JSON.
	require.NoError(t, listperm.Set(listperm.UserListPerm{UserID: 80, CanList: false, CanRead: true}))
	ck, err := apikey.Create(80, "u80", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)
	resp, body := do("/files/api/fs/list?path=/", "Bearer "+ck.Secret, "")
	assert.Equal(t, 403, resp.StatusCode, "listing must be denied for user 80")
	assert.NotContains(t, body, "secret.txt", "the route handler must not have run")
	assert.True(t, json.Valid([]byte(body)), "response must be a single JSON document, got: %s", body)

	// Same user, allowed read: the route handler runs and its body arrives.
	resp, body = do("/files/api/fs/get?path=/secret.txt", "Bearer "+ck.Secret, "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "secret.txt")
	assert.Equal(t, 1, strings.Count(body, "secret.txt"), "the route must have served exactly one response")

	// Denied read, allowed listing (write-only-drop-box inverse).
	require.NoError(t, listperm.Set(listperm.UserListPerm{UserID: 81, CanList: true, CanRead: false}))
	ck, err = apikey.Create(81, "u81", []apikey.Scope{apikey.ScopeAll})
	require.NoError(t, err)
	resp, body = do("/files/api/fs/get?path=/secret.txt", "Bearer "+ck.Secret, "")
	assert.Equal(t, 403, resp.StatusCode)
	assert.NotContains(t, body, "secret.txt", "the route handler must not have run")
	assert.True(t, json.Valid([]byte(body)), "response must be a single JSON document, got: %s", body)
	resp, body = do("/files/api/fs/list?path=/", "Bearer "+ck.Secret, "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "secret.txt")

	// Scope denial (feature 5) is a 403 from apiKeyAuth, before the route.
	ck, err = apikey.Create(82, "u82", []apikey.Scope{apikey.ScopeRead})
	require.NoError(t, err)
	resp, body = do("/files/api/fs/list?path=/", "Bearer "+ck.Secret, "")
	assert.Equal(t, 403, resp.StatusCode, "listing requires the list scope")
	assert.NotContains(t, body, "secret.txt", "the route handler must not have run")
	assert.True(t, json.Valid([]byte(body)), "response must be a single JSON document, got: %s", body)

	// Domain-resolved user (feature 1) with listing denied: no API key, the
	// Host header resolves the user, and the denial precedes the route.
	require.NoError(t, domain.Set(83, "denied.example.com"))
	require.NoError(t, listperm.Set(listperm.UserListPerm{UserID: 83, CanList: false, CanRead: true}))
	resp, body = do("/files/api/fs/list?path=/", "", "denied.example.com")
	assert.Equal(t, 403, resp.StatusCode, "listing must be denied for the domain user")
	assert.NotContains(t, body, "secret.txt", "the route handler must not have run")
	assert.True(t, json.Valid([]byte(body)), "response must be a single JSON document, got: %s", body)

	// A guest (no key, no binding) still reaches the route unchanged.
	resp, body = do("/files/api/fs/list?path=/", "", "")
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, body, "secret.txt")
}
