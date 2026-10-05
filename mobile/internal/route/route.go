// Package route installs the default route for the selected link.
//
// Two backends, for the same reason the control plane has two: a dry run that
// shows exactly what it would do, and a real one that only acts when explicitly
// enabled. Changing the default route on a device you reach over the network is
// a good way to lose it.
package route

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// Applier installs routes.
type Applier interface {
	// Apply makes the given link the default route. Must be idempotent.
	Apply(ctx context.Context, iface, gateway string) error

	// Describe returns the last command issued, for the status endpoint.
	Describe() string
}

// DryRun records what it would run and changes nothing.
type DryRun struct {
	log *slog.Logger

	mu   sync.RWMutex
	last string
}

// NewDryRun returns a dry-run applier.
func NewDryRun(log *slog.Logger) *DryRun { return &DryRun{log: log} }

// Apply logs the command it would run.
func (d *DryRun) Apply(_ context.Context, iface, gateway string) error {
	cmd := command(iface, gateway)

	d.mu.Lock()
	d.last = cmd
	d.mu.Unlock()

	d.log.Info("route (dry run, nothing applied)", "command", cmd)
	return nil
}

// Describe returns the last rendered command.
func (d *DryRun) Describe() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.last == "" {
		return "(nothing applied yet)"
	}
	return "dry run: " + d.last
}

// Linux installs routes with iproute2.
type Linux struct {
	log *slog.Logger

	mu   sync.RWMutex
	last string
}

// NewLinux returns an applier that changes the kernel routing table.
func NewLinux(log *slog.Logger) *Linux { return &Linux{log: log} }

// Apply replaces the default route.
func (l *Linux) Apply(ctx context.Context, iface, gateway string) error {
	args := routeArgs(iface, gateway)

	cmdline := "ip " + strings.Join(args, " ")
	l.mu.Lock()
	l.last = cmdline
	l.mu.Unlock()

	// "route replace" rather than delete-then-add: the gap between the two would
	// leave the device with no default route at all, and on a remote box that
	// gap is the thing that strands it.
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		l.log.Error("route change failed", "command", cmdline, "error", msg)
		return fmt.Errorf("route: %s: %s", cmdline, msg)
	}

	l.log.Info("default route changed", "interface", iface, "gateway", gateway)
	return nil
}

// Describe returns the last command run.
func (l *Linux) Describe() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.last == "" {
		return "(nothing applied yet)"
	}
	return l.last
}

func routeArgs(iface, gateway string) []string {
	if gateway != "" {
		return []string{"route", "replace", "default", "via", gateway, "dev", iface}
	}
	// A point-to-point link such as a cellular modem often has no usable
	// gateway address; a device route is correct there.
	return []string{"route", "replace", "default", "dev", iface}
}

func command(iface, gateway string) string {
	return "ip " + strings.Join(routeArgs(iface, gateway), " ")
}
