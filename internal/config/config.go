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
	// AdminToken is the OpenList admin token, used by the extension to
	// perform privileged in-process operations (TTL reaper deletes, LB
	// uploads) via the loopback client.
	AdminToken string `json:"admin_token" env:"ADMIN_TOKEN"`
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
		DBPath:                "data/openlist-ext.db",
		DataDir:               "data",
		ReaperIntervalSeconds: 60,
		ReaperBatch:           100,
	}
}
