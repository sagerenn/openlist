package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/OpenListTeam/OpenList/v4/cmd"
	"github.com/OpenListTeam/OpenList/v4/cmd/flags"
	olserver "github.com/OpenListTeam/OpenList/v4/server"
	"github.com/gin-gonic/gin"
	"github.com/sagerenn/openlist/internal/config"
)

// BootOpenList initializes OpenList's internals in-process (config, database,
// storages, seed data) against the given data directory. It must be called
// once before Engine(true) mounts OpenList's routes. Tests that supply a
// stub forwarder call Engine(false) and do not need to boot OpenList.
//
// siteURL is the public site URL (full URL or bare path) the service is
// served under; its path component is the base path. Pass "" for a root
// deployment. It is written into OpenList's config.json as site_url, which
// is OpenList's own supported mechanism for subpath deployments: bootstrap
// parses it into conf.URL, server.Init mounts every route under
// e.Group(conf.URL.Path), and the static handler injects base_path into
// index.html so the frontend requests <base>/api/... . The extension layer
// (feature middleware, /ext and /upload routes, loopback client, link
// building) derives its view of the setting through OpenList's own
// utils.FixAndCleanPath normalizer (see config.CanonicalSiteURL), so both
// halves stay in sync for any input form.
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
func BootOpenList(dataDir, siteURL string) error {
	flags.DataDir = dataDir
	// Mirror all OpenList logging to stdout as well as the log file. Log()
	// (called by cmd.Init below) otherwise redirects logrus and the standard
	// log package to <dataDir>/log/log.log only, so a startup log.Fatal
	// (ours or OpenList's) would exit the container with no visible error
	// in docker logs.
	flags.LogStd = true

	if err := checkSiteURLEnvConflict(siteURL); err != nil {
		return err
	}

	// Phase 1: generate the default config.json and initialize OpenList
	// (database, admin user, settings). This does NOT mount routes, so the
	// static-frontend fatal in server.Init is not yet reached.
	cmd.Init()

	// Phase 2: point dist_dir at a placeholder frontend so server.Init's
	// static route registration succeeds, disable OpenList's built-in HTTP
	// listener (the extension serves via its own engine), set the site URL
	// for subpath deployments, then re-run cmd.Init to reload the patched
	// config into OpenList's in-memory config. cmd.Init is idempotent
	// (admin/guest users are only created when absent).
	if err := ensurePlaceholderDist(dataDir, siteURL); err != nil {
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

// checkSiteURLEnvConflict guards against an inherited OPENLIST_SITE_URL.
// OpenList's bootstrap reads that variable from the environment (its
// confFromEnv runs after config.json is loaded, so env wins) and would
// override the site_url this extension writes — mounting OpenList's routes
// under a different prefix than the extension's basePath, every route
// 404ing, silently. Refuse to boot instead of serving a half-routed site;
// matching base paths (e.g. the same value in full-URL and bare-path form)
// are fine.
func checkSiteURLEnvConflict(siteURL string) error {
	v := os.Getenv("OPENLIST_SITE_URL")
	if v == "" || config.NormalizeBasePath(v) == config.NormalizeBasePath(siteURL) {
		return nil
	}
	return fmt.Errorf("OPENLIST_SITE_URL=%q conflicts with SITE_URL=%q: unset OPENLIST_SITE_URL and configure the subpath via SITE_URL only (it is written into OpenList's config.json for you)", v, siteURL)
}

// ensurePlaceholderDist creates <dataDir>/dist/index.html (if absent), sets
// "dist_dir" and "site_url" in <dataDir>/config.json, and disables
// OpenList's built-in HTTP listener so the extension's own engine is the
// sole server.
//
// The http_port = -1 patch is what lets cmd.OutOpenListInit (called from
// BootOpenList) run OpenList's storage-loading path without spawning a
// duplicate HTTP server on OpenList's default port. See BootOpenList's doc
// comment for the full rationale.
//
// site_url (from the extension's SiteURL setting) selects the base path:
// OpenList's bootstrap parses it into conf.URL and server.Init mounts all
// routes under e.Group(conf.URL.Path). siteURL "" (root deployment) is
// written verbatim: OpenList normalizes it to "/" — the same mount its
// default "https://example.com" would produce — but empty keeps link
// building request-derived (common.GetApiUrlFromRequest falls back to the
// request's host when site_url has no scheme), whereas the example.com
// placeholder would pin every generated link to a nonexistent host.
func ensurePlaceholderDist(dataDir, siteURL string) error {
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

	// Patch dist_dir, site_url and scheme.http_port in config.json.
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
	cfg["site_url"] = siteURL
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
