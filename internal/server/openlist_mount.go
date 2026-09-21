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
//
// Storage loading: OpenList's StoragesLoaded middleware blocks every
// non-whitelisted path (notably /api/*, /d/*, /p/*) on a "storages loaded"
// signal that is only sent by bootstrap.Start -> LoadStorages. The extension
// mounts OpenList's routes on its OWN gin engine (via server.Init) rather
// than calling bootstrap.Start, so without an explicit nudge that signal is
// never sent and every API call — including login — hangs forever. To send
// it through a public entry point, BootOpenList disables OpenList's built-in
// HTTP listener (scheme.http_port = -1 in config.json) and invokes the
// public cmd.OutOpenListInit in a goroutine. OutOpenListInit is OpenList's
// sanctioned "start the server from an external embedder" hook (used by
// OpenList-Mobile); with the listener disabled it runs LoadStorages +
// InitTaskManager + InitOfflineDownloadTools — sending the signal and making
// the embedded backend fully functional — without spawning a duplicate HTTP
// server. The goroutine blocks on OutOpenListInit's signal wait for the
// lifetime of the process, which is exactly what we want.
func BootOpenList(dataDir string) error {
	flags.DataDir = dataDir

	// Phase 1: generate the default config.json and initialize OpenList
	// (database, admin user, settings). This does NOT mount routes, so the
	// static-frontend fatal in server.Init is not yet reached.
	cmd.Init()

	// Phase 2: point dist_dir at a placeholder frontend so server.Init's
	// static route registration succeeds, disable OpenList's built-in HTTP
	// listener (the extension serves via its own engine), then re-run
	// cmd.Init to reload the patched config into OpenList's in-memory
	// config. cmd.Init is idempotent (admin/guest users are only created
	// when absent).
	if err := ensurePlaceholderDist(dataDir); err != nil {
		return err
	}
	cmd.Init()

	// Phase 3: trigger OpenList's storage loading via the public external
	// start hook. See the function doc above for why this is necessary and
	// why it is safe (no duplicate server, idempotent re-init). OutOpenListInit
	// blocks until process shutdown, so it must run in its own goroutine.
	go cmd.OutOpenListInit()
	return nil
}

// ensurePlaceholderDist creates <dataDir>/dist/index.html (if absent), sets
// "dist_dir" in <dataDir>/config.json to that directory, and disables
// OpenList's built-in HTTP listener so the extension's own engine is the
// sole server.
//
// The http_port = -1 patch is what lets cmd.OutOpenListInit (called from
// BootOpenList) run OpenList's storage-loading path without spawning a
// duplicate HTTP server on OpenList's default port. See BootOpenList's doc
// comment for the full rationale.
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

	// Patch dist_dir and scheme.http_port in config.json.
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
	// Disable OpenList's own HTTP listener: the extension serves everything
	// through the gin engine built by Server.Engine. http_port = -1 makes
	// bootstrap.Start skip its ListenAndServe while still running
	// LoadStorages (the reason OutOpenListInit is invoked).
	scheme, ok := cfg["scheme"].(map[string]interface{})
	if !ok {
		scheme = map[string]interface{}{}
	}
	scheme["http_port"] = -1
	cfg["scheme"] = scheme
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
