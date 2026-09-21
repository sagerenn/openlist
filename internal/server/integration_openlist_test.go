package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sagerenn/openlist/internal/config"
	"github.com/sagerenn/openlist/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegration_BootsOpenListInProcess is the keystone test for the
// standalone architecture: it boots the REAL OpenList in-process (via
// BootOpenList -> cmd.Init -> bootstrap.Init) and mounts OpenList's real
// routes on the extension engine (Engine(true) -> server.Init). It then
// verifies that OpenList's own /ping endpoint is served by the extension's
// single engine — proving the extension works alone, with no separate
// OpenList process and no HTTP proxy.
//
// This test is gated behind the OPENLIST_INTEGRATION env var because booting
// real OpenList creates files on disk (config.json, database) in a temp data
// dir. Run with: OPENLIST_INTEGRATION=1 go test ./internal/server/...
func TestIntegration_BootsOpenListInProcess(t *testing.T) {
	if os.Getenv("OPENLIST_INTEGRATION") == "" {
		t.Skip("set OPENLIST_INTEGRATION=1 to run the in-process OpenList boot test")
	}

	// Isolated temp data dir for OpenList (config.json + its database) and
	// the extension's own database.
	dataDir := t.TempDir()
	extDB := filepath.Join(dataDir, "ext.db")

	require.NoError(t, db.Init(extDB))
	defer db.Close()

	// Boot OpenList's internals in-process against the temp data dir.
	t.Log("booting openlist in-process")
	require.NoError(t, BootOpenList(dataDir))
	t.Log("openlist booted")

	cfg := config.Default()
	cfg.DataDir = dataDir
	cfg.DBPath = extDB
	cfg.AdminToken = "integration-admin"
	srv, err := New(cfg)
	require.NoError(t, err)
	t.Log("server constructed")

	// Mount OpenList's REAL routes (no stub forwarder).
	t.Log("mounting openlist routes")
	engine := srv.Engine(true)
	t.Log("routes mounted")
	ts := httptest.NewServer(engine)
	defer ts.Close()
	t.Log("test server started at", ts.URL)

	// OpenList's server.Init registers GET /ping -> "pong". Hitting it through
	// the extension engine proves OpenList is embedded and serving in-process.
	resp, err := http.Get(ts.URL + "/ping")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, "pong", string(body), "OpenList /ping should be served by the embedded engine")

	// The extension's own healthz must coexist on the same engine.
	resp, err = http.Get(ts.URL + "/ext/healthz")
	require.NoError(t, err)
	assert.Equal(t, 200, resp.StatusCode)
	resp.Body.Close()
}
