package link

import (
	"context"
	"net"
	"time"
)

// Prober measures a link's reachability, latency and loss.
//
// TCP connect rather than ICMP, for two reasons. It needs no raw socket and so
// no root, which matters on a router appliance. And it measures something
// closer to what applications experience: a cellular link that answers pings
// while refusing to complete handshakes is a real and common failure, and a ping
// test calls it healthy.
type Prober struct {
	// Samples is how many connects make up one measurement. Loss on a marginal
	// radio link is bursty, so a single probe is close to meaningless; five
	// gives a usable loss figure without being slow.
	Samples int

	// Timeout bounds one connect attempt.
	Timeout time.Duration
}

// NewProber returns a prober with sensible defaults.
func NewProber() *Prober {
	return &Prober{Samples: 5, Timeout: 2 * time.Second}
}

// Result is one measurement.
type Result struct {
	Up        bool
	LatencyMs float64
	LossPct   float64
}

// Probe measures one link.
//
// The dial is bound to the link's own interface, so the measurement reflects
// that path rather than whichever one the host's routing table happens to
// prefer. Without binding, every link would report the health of the currently
// selected one, and the policy engine would never see a reason to switch.
func (p *Prober) Probe(ctx context.Context, cfg Config) Result {
	target := cfg.ProbeTarget
	if target == "" {
		// Public resolvers answer on 53 from anywhere and are about as close to
		// "is the internet reachable" as a single target gets.
		target = "1.1.1.1:53"
	}

	samples := p.Samples
	if samples <= 0 {
		samples = 3
	}

	dialer := &net.Dialer{Timeout: p.Timeout}
	if cfg.Interface != "" {
		if iface, err := net.InterfaceByName(cfg.Interface); err == nil {
			if addrs, err := iface.Addrs(); err == nil && len(addrs) > 0 {
				if ipnet, ok := addrs[0].(*net.IPNet); ok {
					dialer.LocalAddr = &net.TCPAddr{IP: ipnet.IP}
				}
			}
		}
		// A missing or address-less interface is not an error here. It means the
		// link is down, which the failed dials below will report anyway.
	}

	var ok int
	var total time.Duration

	for i := 0; i < samples; i++ {
		if ctx.Err() != nil {
			break
		}
		started := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			continue
		}
		total += time.Since(started)
		conn.Close()
		ok++
	}

	if ok == 0 {
		return Result{Up: false, LossPct: 100}
	}
	return Result{
		Up:        true,
		LatencyMs: float64(total.Milliseconds()) / float64(ok),
		LossPct:   float64(samples-ok) / float64(samples) * 100,
	}
}
