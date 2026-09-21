package server

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/OpenListTeam/OpenList/v4/cmd"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	olserver "github.com/OpenListTeam/OpenList/v4/server"
	"github.com/gin-gonic/gin"
)

// BootOpenList initializes OpenList's internals in-process (config, database,
// storages, seed data) against the given data directory. It must be called
// once before Engine(true) mounts OpenList's routes. Tests that supply a
// stub forwarder call Engine(false) and do not need to boot OpenList.
//
// Only OpenList public packages are used (cmd, cmd/flags), preserving
// forward compatibility.
//
// OpenList's web UI is served from a "dist" directory containing index.html.
// The library distribution does not embed a built frontend, so BootOpenList
// creates a minimal placeholder dist (a tiny index.html) inside the data
// directory and points OpenList's dist_dir at it. This lets server.Init
// register the static routes without fataling. Operators who want the real
// UI can drop a built frontend into <dataDir>/dist before starting.
func BootOpenList(dataDir string) error {
	flags.DataDir = dataDir

	// Phase 1: generate the default config.json and initialize OpenList
	// (database, admin user, settings). This does NOT mount routes, so the
	// static-frontend fatal in server.Init is not yet reached.
	cmd.Init()

	// Phase 2: point dist_dir at a placeholder frontend so server.Init's
	// static route registration succeeds, then re-run cmd.Init to reload the
	// patched config into OpenList's in-memory config. cmd.Init is
	// idempotent (admin/guest users are only created when absent).
	if err := ensurePlaceholderDist(dataDir); err != nil {
		return err
	}
	cmd.Init()
	return nil
}

// ensurePlaceholderDist creates <dataDir>/dist/index.html (if absent) and
// sets "dist_dir" in <dataDir>/config.json to that directory.
func ensurePlaceholderDist(dataDir string) error {
	distDir := filepath.Join(dataDir, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return err
	}
	indexHTML := filepath.Join(distDir, "index.html")
	if !fileExists(indexHTML) {
		placeholder := "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><title>openlist-ext</title></head>" +
			"<body><h1>openlist-ext</h1><p>OpenList API is embedded in this server. " +
			"Place a built frontend in the dist directory to serve the web UI.</p></body></html>"
		if err := os.WriteFile(indexHTML, []byte(placeholder), 0o644); err != nil {
			return err
		}
	}

	// Patch dist_dir in config.json.
	configPath := filepath.Join(dataDir, "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg["dist_dir"] = distDir
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, 0o644)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// olInit mounts OpenList's full route tree onto the given gin engine via the
// public server.Init entry point. After this call the engine serves
// OpenList's /api/*, /d/*, /p/*, webdav, s3, mcp, auth, and admin routes
// in-process.
func olInit(e *gin.Engine) {
	olserver.Init(e)
}
