// Command cached serves content from a local disk so it does not have to cross
// a slow, metered or intermittent link more than once.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/s-rakim/my-personal-project/cache/internal/catalog"
	"github.com/s-rakim/my-personal-project/cache/internal/server"
	"github.com/s-rakim/my-personal-project/cache/internal/store"
)

// Config is the daemon's configuration.
type Config struct {
	DataDir string `json:"data_dir"`
	Listen  string `json:"listen"`

	// BudgetGB is the disk allowance for cached content. Eviction keeps the
	// store under it, never discarding pinned entries.
	BudgetGB float64 `json:"budget_gb"`

	// FetchOnMiss makes an uncached request fetch upstream immediately. Off by
	// default: behind a slow or metered link a silent fetch is just a slow
	// request with extra steps, and the point is to decide deliberately when
	// bytes cross the link.
	FetchOnMiss bool `json:"fetch_on_miss"`

	// SaveSeconds is how often the catalog is flushed to disk.
	SaveSeconds float64 `json:"save_seconds"`
}

func main() {
	configPath := flag.String("config", "config.json", "configuration file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "cached: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := Config{DataDir: "var/cache", Listen: "127.0.0.1:8078", BudgetGB: 20, SaveSeconds: 60}
	if raw, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parse %s: %w", configPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", configPath, err)
	} else {
		log.Info("no config file; using defaults", "path", configPath)
	}

	blobs, err := store.Open(filepath.Join(cfg.DataDir, "blobs"))
	if err != nil {
		return err
	}
	cat, err := catalog.Open(filepath.Join(cfg.DataDir, "catalog.json"))
	if err != nil {
		return err
	}

	// The filesystem is the source of truth. An entry whose content vanished
	// would otherwise report a hit and then fail to serve it.
	if dropped := cat.Reconcile(blobs); dropped > 0 {
		log.Warn("dropped catalog entries whose content was missing", "entries", dropped)
	}

	budget := int64(cfg.BudgetGB * 1e9)
	srv, err := server.New(server.Options{
		Store: blobs, Catalog: cat, Log: log,
		BudgetBytes: budget, FetchOnMiss: cfg.FetchOnMiss,
		Client:      &http.Client{Timeout: 30 * time.Minute},
	})
	if err != nil {
		return err
	}

	stats := cat.Stats(blobs)
	log.Info("cache ready",
		"entries", stats.Entries, "blobs", stats.Blobs,
		"stored_gb", float64(stats.StoredBytes)/1e9, "budget_gb", cfg.BudgetGB,
		"fetch_on_miss", cfg.FetchOnMiss, "listen", cfg.Listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server stopped", "error", err)
		}
	}()

	if cfg.SaveSeconds <= 0 {
		cfg.SaveSeconds = 60
	}
	ticker := time.NewTicker(time.Duration(cfg.SaveSeconds * float64(time.Second)))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = httpSrv.Shutdown(shutdown)
			cancel()
			// Save on the way out, so a clean stop never loses the index.
			if err := cat.Save(); err != nil {
				log.Error("could not save catalog", "error", err)
			}
			final := cat.Stats(blobs)
			log.Info("stopped",
				"hit_rate", final.HitRate(), "bytes_saved", final.BytesSaved(),
				"multiplier", final.Multiplier())
			return nil
		case <-ticker.C:
			if err := cat.Save(); err != nil {
				log.Error("could not save catalog", "error", err)
			}
		}
	}
}
