// Package main is the entry point for the openlist-ext binary.
//
// It is a forward-compatible extension of OpenList
// (https://github.com/OpenListTeam/OpenList). The extension runs STANDALONE:
// it embeds and boots OpenList in the same process (via OpenList's public
// cmd.Init + server.Init entry points) and layers six features on top of
// OpenList's own gin routes — no separate OpenList instance, no reverse
// proxy.
//
//	1. Per-user custom domain routing
//	2. Per-user file TTL with auto-delete (fixed-time or sliding access)
//	3. Per-user file-listing permission control
//	4. Upload load-balancing across multiple backend storages
//	5. API-key authentication and scoped operations
//	6. Forward compatibility (separate module + DB; depends on OpenList as
//	   a library via its public packages only)
//
// Usage:
//
//	openlist-ext                       # run with defaults
//	ADMIN_TOKEN=... openlist-ext
//
// The extension listens on LISTEN_ADDR (default :5245). OpenList's data
// directory is DATA_DIR (default "data"); its config.json and database are
// created there on first run.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sagerenn/openlist/internal/config"
	"github.com/sagerenn/openlist/internal/db"
	"github.com/sagerenn/openlist/internal/server"
	"github.com/sagerenn/openlist/internal/ttl"

	// Feature packages self-register their gorm models via init().
	_ "github.com/sagerenn/openlist/internal/apikey"
	_ "github.com/sagerenn/openlist/internal/domain"
	_ "github.com/sagerenn/openlist/internal/lb"
	_ "github.com/sagerenn/openlist/internal/listperm"
)

func main() {
	cfg := config.Default()
	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "address the embedded server binds")
	flag.StringVar(&cfg.LoopbackAddr, "loopback", cfg.LoopbackAddr, "http://host:port form of listen, for in-process privileged calls")
	flag.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "OpenList admin token for privileged in-process ops")
	flag.StringVar(&cfg.DBPath, "db", cfg.DBPath, "path to the extension SQLite database")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "OpenList data directory (config.json + its database)")
	flag.IntVar(&cfg.ReaperIntervalSeconds, "reaper-interval", cfg.ReaperIntervalSeconds, "TTL reaper interval (seconds)")
	flag.IntVar(&cfg.ReaperBatch, "reaper-batch", cfg.ReaperBatch, "TTL reaper batch size")
	flag.Parse()

	// Allow env overrides for non-flag usage.
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("LOOPBACK_ADDR"); v != "" {
		cfg.LoopbackAddr = v
	}
	if v := os.Getenv("ADMIN_TOKEN"); v != "" {
		cfg.AdminToken = v
	}
	if v := os.Getenv("DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		cfg.DataDir = v
	}

	// Boot OpenList's internals in-process (config, DB, storages, data).
	if err := server.BootOpenList(cfg.DataDir); err != nil {
		log.Fatalf("failed to boot openlist: %v", err)
	}

	// Open the extension's own database (separate schema, forward compat).
	if err := db.Init(cfg.DBPath); err != nil {
		log.Fatalf("failed to init extension db: %v", err)
	}
	defer db.Close()

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("failed to create server: %v", err)
	}

	// Start the TTL reaper. It deletes expired files via the in-process
	// loopback client (same engine, admin token).
	reaper := ttl.NewReaper(srv.Client, time.Duration(cfg.ReaperIntervalSeconds)*time.Second, cfg.ReaperBatch)
	reaper.Start()
	defer reaper.Stop()

	// Mount OpenList's routes + extension features on one engine.
	engine := srv.Engine(true)
	httpSrv := &http.Server{Addr: cfg.ListenAddr, Handler: engine}

	go func() {
		log.Printf("openlist-ext listening on %s (OpenList embedded in-process)", cfg.ListenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down openlist-ext...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}
