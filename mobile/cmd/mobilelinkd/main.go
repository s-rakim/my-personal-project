// Command mobilelinkd chooses which WAN a vehicle or a site uses, moment to
// moment, and defers the traffic that can wait until a link worth using appears.
//
// It exists because the alternative, plain failover, treats a free corridor
// radio and a metered cellular link as interchangeable. They are not: one costs
// nothing and the other costs by the gigabyte, and most traffic does not need to
// travel the instant it is generated.
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
	"sync"
	"syscall"
	"time"

	"github.com/s-rakim/my-personal-project/mobile/internal/link"
	"github.com/s-rakim/my-personal-project/mobile/internal/policy"
	"github.com/s-rakim/my-personal-project/mobile/internal/route"
	"github.com/s-rakim/my-personal-project/mobile/internal/syncq"
)

// Config is the daemon's configuration.
type Config struct {
	// ProbeSeconds is how often links are measured. Shorter reacts faster to
	// leaving coverage and costs more probes; on a vehicle 10 to 15 seconds is
	// about right, on a fixed site a minute is plenty.
	ProbeSeconds float64 `json:"probe_seconds"`

	Links  []link.Config `json:"links"`
	Policy policyConfig  `json:"policy"`

	QueuePath   string `json:"queue_path"`
	MaxAttempts int    `json:"queue_max_attempts"`

	// ApplyRoutes must be explicitly true before the kernel routing table is
	// touched. Changing the default route on a device you reach over the network
	// is a good way to lose it.
	ApplyRoutes bool `json:"apply_routes"`

	Listen string `json:"listen"`
}

type policyConfig struct {
	SwitchGainThreshold float64 `json:"switch_gain_threshold"`
	MinDwellSeconds     float64 `json:"min_dwell_seconds"`
	Stickiness          float64 `json:"stickiness"`
	CostWeight          float64 `json:"cost_weight"`
	BulkMinMbps         float64 `json:"bulk_min_mbps"`
	ReserveCapFraction  float64 `json:"reserve_cap_fraction"`
}

func (p policyConfig) toPolicy() policy.Policy {
	out := policy.Default()
	if p.SwitchGainThreshold > 0 {
		out.SwitchGainThreshold = p.SwitchGainThreshold
	}
	if p.MinDwellSeconds > 0 {
		out.MinDwell = time.Duration(p.MinDwellSeconds * float64(time.Second))
	}
	if p.Stickiness > 0 {
		out.Stickiness = p.Stickiness
	}
	if p.CostWeight > 0 {
		out.CostWeight = p.CostWeight
	}
	if p.BulkMinMbps > 0 {
		out.BulkMinMbps = p.BulkMinMbps
	}
	if p.ReserveCapFraction > 0 {
		out.ReserveCapFraction = p.ReserveCapFraction
	}
	return out
}

func main() {
	configPath := flag.String("config", "config.json", "configuration file")
	flag.Parse()

	if err := run(*configPath); err != nil {
		fmt.Fprintf(os.Stderr, "mobilelinkd: %v\n", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", configPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	if cfg.ProbeSeconds <= 0 {
		cfg.ProbeSeconds = 15
	}
	if cfg.QueuePath == "" {
		cfg.QueuePath = filepath.Join("var", "syncq.json")
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8075"
	}

	registry, err := link.NewRegistry(cfg.Links)
	if err != nil {
		return err
	}
	engine, err := policy.New(cfg.Policy.toPolicy())
	if err != nil {
		return err
	}
	queue, err := syncq.Open(syncq.Options{Path: cfg.QueuePath, MaxAttempts: cfg.MaxAttempts})
	if err != nil {
		return err
	}

	var applier route.Applier
	if cfg.ApplyRoutes {
		log.Warn("route changes will be applied to the kernel")
		applier = route.NewLinux(log)
	} else {
		log.Info("dry run: route changes will be logged, not applied")
		applier = route.NewDryRun(log)
	}

	d := &daemon{
		cfg: cfg, registry: registry, engine: engine, queue: queue,
		applier: applier, prober: link.NewProber(), log: log,
		client: &http.Client{Timeout: 30 * time.Minute},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("status server stopped", "error", err)
		}
	}()

	log.Info("mobilelinkd running",
		"links", len(cfg.Links), "probe_interval", cfg.ProbeSeconds, "listen", cfg.Listen)

	d.tick(ctx)
	ticker := time.NewTicker(time.Duration(cfg.ProbeSeconds * float64(time.Second)))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = srv.Shutdown(shutdown)
			cancel()
			wg.Wait()
			log.Info("stopped")
			return nil
		case <-ticker.C:
			d.tick(ctx)
		}
	}
}

type daemon struct {
	cfg      Config
	registry *link.Registry
	engine   *policy.Engine
	queue    *syncq.Queue
	applier  route.Applier
	prober   *link.Prober
	client   *http.Client
	log      *slog.Logger

	mu        sync.RWMutex
	decisions map[policy.Class]policy.Decision
	draining  bool
}

// tick measures every link, re-decides, and drains deferred work if it can.
func (d *daemon) tick(ctx context.Context) {
	for _, s := range d.registry.All() {
		res := d.prober.Probe(ctx, s.Config)
		// Throughput is not probed synthetically. It is measured from real
		// transfers, so the figure reflects what the link actually delivers for
		// work rather than for a test file.
		d.registry.Update(s.Config.Name, res.Up, res.LatencyMs, res.LossPct, 0)
	}

	states := d.registry.All()
	now := time.Now()

	decisions := make(map[policy.Class]policy.Decision, 3)
	for _, c := range []policy.Class{policy.ClassLive, policy.ClassBulk, policy.ClassIdle} {
		decisions[c] = d.engine.Decide(c, states, now)
	}

	d.mu.Lock()
	d.decisions = decisions
	d.mu.Unlock()

	// The live decision owns the default route: it is the class that cannot wait.
	live := decisions[policy.ClassLive]
	if live.Changed {
		d.log.Info("link selection changed", "class", "live",
			"link", live.Link, "reason", live.Reason)
	}
	if live.Link != "" {
		if s, ok := d.registry.Get(live.Link); ok {
			if err := d.applier.Apply(ctx, s.Config.Interface, s.Config.Gateway); err != nil {
				d.log.Error("could not install default route", "error", err)
			}
		}
	} else {
		d.log.Warn("no link is usable for live traffic", "reason", live.Reason)
	}

	go d.drain(ctx, decisions)
}

// drain runs deferred transfers over whichever link the policy allows.
//
// One at a time, and never concurrently with itself: a corridor link shared with
// other users is not improved by opening eight parallel downloads on it.
func (d *daemon) drain(ctx context.Context, decisions map[policy.Class]policy.Decision) {
	d.mu.Lock()
	if d.draining {
		d.mu.Unlock()
		return
	}
	d.draining = true
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.draining = false
		d.mu.Unlock()
	}()

	for _, class := range []policy.Class{policy.ClassBulk, policy.ClassIdle} {
		decision := decisions[class]
		if decision.Link == "" {
			continue
		}
		state, ok := d.registry.Get(decision.Link)
		if !ok {
			continue
		}

		for _, item := range d.queue.Pending(string(class)) {
			if ctx.Err() != nil {
				return
			}
			started := time.Now()
			d.log.Info("starting deferred transfer",
				"item", item.ID, "class", class, "link", decision.Link)

			err := d.queue.Transfer(ctx, item.ID, decision.Link, d.client)
			moved := d.bytesMoved(item.ID, item.BytesDone)

			if moved > 0 {
				elapsed := time.Since(started).Seconds()
				if elapsed > 0.5 {
					mbps := float64(moved) * 8 / 1e6 / elapsed
					// Feed the measured rate back so the next decision is made on
					// what this link really delivers, not on a configured guess.
					d.registry.Update(decision.Link, true, state.LatencyMs, state.LossPct, mbps)
				}
				d.registry.AddUsage(decision.Link, float64(moved)/1e9)
			}

			if err != nil {
				d.log.Warn("deferred transfer did not finish",
					"item", item.ID, "error", err)
				// Leaving coverage mid-transfer is normal. Stop draining this
				// class and pick up where we left off when a link returns.
				break
			}
			d.log.Info("deferred transfer complete",
				"item", item.ID, "bytes", moved, "link", decision.Link)
		}
	}
}

// bytesMoved reports how much a transfer advanced in this attempt.
func (d *daemon) bytesMoved(id string, before int64) int64 {
	for _, it := range d.queue.All() {
		if it.ID == id {
			if delta := it.BytesDone - before; delta > 0 {
				return delta
			}
			return 0
		}
	}
	return 0
}

func (d *daemon) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		d.mu.RLock()
		decisions := d.decisions
		d.mu.RUnlock()

		writeJSON(w, http.StatusOK, map[string]any{
			"links":     d.registry.All(),
			"decisions": decisions,
			"queue":     d.queue.Stats(),
			"route":     d.applier.Describe(),
		})
	})

	mux.HandleFunc("GET /v1/queue", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.queue.All())
	})

	mux.HandleFunc("POST /v1/queue", func(w http.ResponseWriter, r *http.Request) {
		var item syncq.Item
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&item); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := d.queue.Add(item); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "queued",
			"note":   "will transfer when a link allowed for " + item.Class + " traffic is available",
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
