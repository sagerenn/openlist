// Package config holds the extension's runtime configuration.
package config

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
