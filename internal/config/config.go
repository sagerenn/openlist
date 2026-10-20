// Package config holds the extension's runtime configuration.
package config

import (
	"net/url"
	"strings"

	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// Config is the extension configuration, loaded from flags / environment.
type Config struct {
	// ListenAddr is the address the embedded server binds. This is the only
	// port the extension exposes; OpenList runs in-process on the same
	// engine.
	ListenAddr string `json:"listen_addr" env:"LISTEN_ADDR"`
	// LoopbackAddr is the http://host:port form of ListenAddr, used by the
	// in-process client to call the engine's own routes for privileged
	// operations (TTL reaper deletes, load-balanced uploads). It must
	// resolve to the same listener as ListenAddr.
	LoopbackAddr string `json:"loopback_addr" env:"LOOPBACK_ADDR"`
	// AdminToken is a static token accepted by the extension's own /ext/admin
	// guard (adminAuth). It may be any opaque string. It is NOT used to
	// authenticate to OpenList's core API — for that, the extension logs in
	// as the admin user (see AdminUser/AdminPassword) and forwards a real
	// admin JWT, because OpenList's Auth middleware only accepts JWTs.
	AdminToken string `json:"admin_token" env:"ADMIN_TOKEN"`
	// AdminUser is the OpenList admin username used to obtain a real admin
	// JWT for privileged in-process operations (TTL reaper deletes, LB
	// uploads, and forwarding API-key-authenticated file ops as admin).
	// Defaults to "admin".
	AdminUser string `json:"admin_user" env:"ADMIN_USER"`
	// AdminPassword is the password for AdminUser, read from the
	// OPENLIST_ADMIN_PASSWORD env var (the same var OpenList's own bootstrap
	// reads to seed the admin password). When set, the extension logs in at
	// startup and refreshes the JWT on expiry; when empty, the extension
	// falls back to AdminToken (which must then be a valid admin JWT).
	AdminPassword string `json:"admin_password" env:"OPENLIST_ADMIN_PASSWORD"`
	// DBPath is the path to the extension's SQLite database (separate from
	// OpenList's own DB, for forward compatibility).
	DBPath string `json:"db_path" env:"DB_PATH"`
	// DataDir is OpenList's data directory (where its config.json and own
	// database live). The extension boots OpenList in-process against this
	// directory.
	DataDir string `json:"data_dir" env:"DATA_DIR"`
	// ReaperInterval is how often the TTL reaper scans for expired files.
	// Defaults to 60s.
	ReaperIntervalSeconds int `json:"reaper_interval_seconds" env:"REAPER_INTERVAL_SECONDS"`
	// ReaperBatch is the max number of expired files deleted per scan.
	ReaperBatch int `json:"reaper_batch" env:"REAPER_BATCH"`
	// SiteURL is the public site URL of the whole service, used to deploy
	// under a subpath behind a reverse proxy (caddy, nginx, httpd, ...).
	// It may be a full URL ("https://example.com/files") or just the path
	// ("/files"); the path component is the base path every route —
	// OpenList's own (/api, /d, /p, the web UI) and the extension's (/ext,
	// /upload) — is served under. Empty (default) or "/" means the service
	// is served at the root, exactly as before. BootOpenList writes it into
	// OpenList's config.json as site_url, which is OpenList's own supported
	// way of mounting under a subpath (it also makes the frontend request
	// /files/api/... via the injected base_path). Link building follows
	// OpenList's site_url semantics: a full URL pins generated links to
	// that origin, a bare path leaves links derived from the request's own
	// host (X-Forwarded-* aware).
	SiteURL string `json:"site_url" env:"SITE_URL"`
}

// BasePath returns the normalized URL path prefix the service is served
// under, derived from SiteURL: "" for root deployments, otherwise
// "/prefix" (leading slash, no trailing slash). See NormalizeBasePath.
func (c Config) BasePath() string {
	return NormalizeBasePath(c.SiteURL)
}

// CanonicalSiteURL returns the site URL in the exact form OpenList's
// bootstrap normalizes it to in memory (internal/bootstrap/config.go
// initURL): a value containing "://" is kept verbatim, anything else goes
// through OpenList's own utils.FixAndCleanPath (leading slash added,
// "." / ".." resolved, backslashes turned into slashes — so "" and "/"
// both become "/"). Deriving the extension's view of the setting through
// the SAME upstream normalizer guarantees the extension and OpenList
// always agree on the prefix and on generated links.
func (c Config) CanonicalSiteURL() string {
	return CanonicalSiteURL(c.SiteURL)
}

// CanonicalSiteURL is the package-level form of Config.CanonicalSiteURL.
func CanonicalSiteURL(siteURL string) string {
	if !strings.Contains(siteURL, "://") {
		return utils.FixAndCleanPath(siteURL)
	}
	return siteURL
}

// NormalizeBasePath extracts the path component of the canonical site URL
// and returns it as "" (root) or "/prefix". It mirrors, step for step, what
// OpenList's bootstrap ends up mounting routes under: initURL runs
// utils.FixAndCleanPath on scheme-less values, then url.Parse — which also
// drops any query or fragment, so "/files?x=1" mounts at "/files" — and
// server/router.go mounts e.Group(conf.URL.Path). The final FixAndCleanPath
// of the parsed path matters only for full URLs with unclean paths
// (e.g. "/a/../b"): gin cleans a group prefix while registering routes
// (joinPaths uses path.Join), and OpenList's link building cleans too, so
// the extension's prefix must be the cleaned form to match both.
func NormalizeBasePath(siteURL string) string {
	u, err := url.Parse(CanonicalSiteURL(siteURL))
	if err != nil {
		// OpenList's bootstrap log.Fatals on an unparsable site_url at
		// boot, so production never serves with one; treat it as root.
		return ""
	}
	if p := utils.FixAndCleanPath(u.Path); p != "/" {
		return p
	}
	return ""
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		ListenAddr:            ":5245",
		LoopbackAddr:          "http://127.0.0.1:5245",
		AdminToken:            "",
		AdminUser:             "admin",
		AdminPassword:         "",
		DBPath:                "data/openlist-ext.db",
		DataDir:               "data",
		ReaperIntervalSeconds: 60,
		ReaperBatch:           100,
	}
}
