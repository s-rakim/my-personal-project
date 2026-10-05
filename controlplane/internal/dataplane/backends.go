package dataplane

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/s-rakim/my-personal-project/controlplane/internal/fsutil"
)

// DryRun renders what it would apply and writes it to disk, touching nothing.
//
// This is the default, and it is how you review a change before trusting it to
// live subscribers: diff two ticks' output and you can see exactly which
// subscriber's rate moved and why.
type DryRun struct {
	cfg     Config
	outDir  string
	log     *slog.Logger
	classes *classAllocator

	mu   sync.RWMutex
	last Rendered
}

// NewDryRun returns a dry-run backend writing into outDir.
func NewDryRun(cfg Config, outDir string, log *slog.Logger) *DryRun {
	return &DryRun{cfg: cfg, outDir: outDir, log: log, classes: newClassAllocator()}
}

// Apply renders the state and writes it out.
func (d *DryRun) Apply(_ context.Context, s State) error {
	r, err := render(d.cfg, s, d.classes)
	if err != nil {
		return err
	}

	files := map[string]string{
		"nftables.conf":  r.NFT,
		"tc-downlink.tc": r.TCAccess,
		"tc-uplink.tc":   r.TCUplink,
		"setup-ifb.sh":   r.SetupIFB,
	}
	for name, body := range files {
		perm := os.FileMode(0o640)
		if strings.HasSuffix(name, ".sh") {
			perm = 0o750
		}
		if err := fsutil.WriteAtomic(filepath.Join(d.outDir, name), []byte(body), perm); err != nil {
			return fmt.Errorf("dataplane: write %s: %w", name, err)
		}
	}

	d.mu.Lock()
	d.last = r
	d.mu.Unlock()

	d.log.Debug("dataplane rendered (dry run, nothing applied)",
		"epoch", s.Epoch, "subscribers", r.Subscriber, "dir", d.outDir)
	return nil
}

// Describe returns the last rendered scripts.
func (d *DryRun) Describe() map[string]string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return map[string]string{
		"backend":        "dryrun",
		"output_dir":     d.outDir,
		"nftables.conf":  d.last.NFT,
		"tc-downlink.tc": d.last.TCAccess,
		"tc-uplink.tc":   d.last.TCUplink,
	}
}

// Close is a no-op.
func (d *DryRun) Close() error { return nil }

// Linux applies rules with nft and tc.
type Linux struct {
	cfg     Config
	outDir  string
	log     *slog.Logger
	classes *classAllocator

	mu   sync.RWMutex
	last Rendered
}

// NewLinux returns a backend that programs the kernel. The caller is
// responsible for having verified config.Dataplane.Apply, and for running
// setup-ifb.sh once before the first Apply.
func NewLinux(cfg Config, outDir string, log *slog.Logger) *Linux {
	return &Linux{cfg: cfg, outDir: outDir, log: log, classes: newClassAllocator()}
}

// Apply programs nftables and tc.
//
// nftables goes first and as one transaction, so the firewall is never in a
// half-configured state. tc goes second: a stale rate limit is a billing
// annoyance, while a missing firewall rule is an outage or a security hole, so if
// only one of the two can succeed it should be the firewall.
func (l *Linux) Apply(ctx context.Context, s State) error {
	r, err := render(l.cfg, s, l.classes)
	if err != nil {
		return err
	}

	// Keep a copy on disk regardless, so what the kernel is running can always be
	// read back and diffed after the fact.
	_ = fsutil.WriteAtomic(filepath.Join(l.outDir, "nftables.conf"), []byte(r.NFT), 0o640)
	_ = fsutil.WriteAtomic(filepath.Join(l.outDir, "tc-downlink.tc"), []byte(r.TCAccess), 0o640)
	_ = fsutil.WriteAtomic(filepath.Join(l.outDir, "tc-uplink.tc"), []byte(r.TCUplink), 0o640)

	if err := run(ctx, l.log, r.NFT, "nft", "-f", "-"); err != nil {
		return fmt.Errorf("dataplane: apply nftables: %w", err)
	}
	// tc -force keeps going past the expected failure of deleting a qdisc that is
	// not there yet, which otherwise aborts the batch on first run.
	if err := run(ctx, l.log, r.TCAccess, "tc", "-force", "-batch", "-"); err != nil {
		return fmt.Errorf("dataplane: apply downlink shaping: %w", err)
	}
	if err := run(ctx, l.log, r.TCUplink, "tc", "-force", "-batch", "-"); err != nil {
		return fmt.Errorf("dataplane: apply uplink shaping: %w", err)
	}

	l.mu.Lock()
	l.last = r
	l.mu.Unlock()

	l.log.Info("dataplane applied", "epoch", s.Epoch, "subscribers", r.Subscriber)
	return nil
}

// Describe returns the last applied scripts.
func (l *Linux) Describe() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return map[string]string{
		"backend":        "linux",
		"output_dir":     l.outDir,
		"nftables.conf":  l.last.NFT,
		"tc-downlink.tc": l.last.TCAccess,
		"tc-uplink.tc":   l.last.TCUplink,
	}
}

// Close leaves the rules in place. Tearing them down on shutdown would drop
// every subscriber during a daemon restart, which is the opposite of what a
// restart should cost.
func (l *Linux) Close() error { return nil }

func run(ctx context.Context, log *slog.Logger, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		log.Error("dataplane command failed", "command", name, "error", msg)
		return fmt.Errorf("%s: %s", name, msg)
	}
	return nil
}
