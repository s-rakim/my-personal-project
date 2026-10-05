// Package link models the WAN connections a vehicle can use, and measures them.
//
// The premise is that a vehicle has several ways to reach the internet at any
// moment and they differ enormously in what they cost. A corridor radio is free
// and fast but only works where you have built coverage. Cellular works almost
// everywhere and is billed by the gigabyte. Home Wi-Fi is free and very fast and
// available for ten minutes a day.
//
// Treating these as interchangeable, which is what a plain failover setup does,
// wastes the cheap ones and overspends on the expensive one. Treating them as a
// ranked, measured set is the whole idea here.
package link

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Kind is the sort of transport a link uses. It determines the defaults that
// matter: whether it is metered, and roughly what to expect before any
// measurement exists.
type Kind string

const (
	// KindCorridor is a radio link to infrastructure you own, such as a 900 MHz
	// sector covering a route you drive. Free, moderate bandwidth, and available
	// only inside coverage you built.
	KindCorridor Kind = "corridor"

	// KindWiFi is a high-speed access point at a place you stop: home, a garage,
	// a workplace. Free and fast, and available for minutes at a time.
	KindWiFi Kind = "wifi"

	// KindCellular is a carrier SIM. Available almost everywhere, billed by the
	// gigabyte, and the thing this whole design exists to use sparingly.
	KindCellular Kind = "cellular"

	// KindSatellite is a mobility satellite service. Available almost anywhere
	// with sky, expensive, and usually subject to its own fair-use limits.
	KindSatellite Kind = "satellite"
)

// Metered reports whether traffic on this kind of link costs money by volume.
func (k Kind) Metered() bool {
	return k == KindCellular || k == KindSatellite
}

// Config describes one link as the operator declares it.
type Config struct {
	Name      string `json:"name"`
	Kind      Kind   `json:"kind"`
	Interface string `json:"interface"`

	// Gateway is the next hop to install when this link is selected.
	Gateway string `json:"gateway"`

	// CostPerGB is what a gigabyte costs on this link, in whatever currency you
	// think in. Zero means free. This is what stops the policy engine from
	// treating a cellular link as equivalent to a free one.
	CostPerGB float64 `json:"cost_per_gb"`

	// MonthlyCapGB is the allowance before the link throttles or bills overage.
	// Zero means unmetered.
	MonthlyCapGB float64 `json:"monthly_cap_gb"`

	// ExpectedMbps seeds the estimate before any measurement. Replaced by
	// measured throughput as soon as there is one.
	ExpectedMbps float64 `json:"expected_mbps"`

	// Priority breaks ties between links that score equally. Higher wins.
	Priority int `json:"priority"`

	// ProbeTarget is the host to probe for reachability and latency. Prefer
	// something on the far side of the link's own infrastructure, so a probe
	// failure means the internet is unreachable rather than that one host is
	// down.
	ProbeTarget string `json:"probe_target"`

	Enabled bool `json:"enabled"`
}

// Validate rejects a link that cannot be used.
func (c Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("link: name is required")
	}
	switch c.Kind {
	case KindCorridor, KindWiFi, KindCellular, KindSatellite:
	default:
		return fmt.Errorf("link %q: unknown kind %q", c.Name, c.Kind)
	}
	if c.Interface == "" {
		return fmt.Errorf("link %q: interface is required", c.Name)
	}
	if c.CostPerGB < 0 {
		return fmt.Errorf("link %q: cost_per_gb cannot be negative", c.Name)
	}
	if c.Kind.Metered() && c.CostPerGB == 0 {
		// Not fatal, but it means the policy engine will treat an expensive link
		// as free and drain bulk transfers over it.
		return fmt.Errorf("link %q is %s, which is metered, but cost_per_gb is 0; "+
			"set it or bulk traffic will be sent over it as though it were free",
			c.Name, c.Kind)
	}
	return nil
}

// State is what measurement says about a link right now.
type State struct {
	Config Config `json:"config"`

	// Up means the last probe succeeded.
	Up bool `json:"up"`

	// LatencyMs and LossPct come from probing. Loss matters more than latency on
	// a moving vehicle: a corridor link at the edge of coverage degrades by
	// dropping packets long before it stops answering entirely.
	LatencyMs float64 `json:"latency_ms"`
	LossPct   float64 `json:"loss_pct"`

	// ThroughputMbps is the last measured rate, falling back to ExpectedMbps.
	ThroughputMbps float64 `json:"throughput_mbps"`

	// UsedGBThisMonth tracks consumption against the cap.
	UsedGBThisMonth float64 `json:"used_gb_this_month"`

	LastProbe   time.Time `json:"last_probe"`
	LastUpAt    time.Time `json:"last_up_at"`
	FlapCount   int       `json:"flap_count"`
	ConsecFails int       `json:"consecutive_failures"`
}

// Usable reports whether the link is in a state worth selecting.
//
// A link that answers but drops a third of its packets is worse than useless for
// interactive traffic: TCP collapses, and the user experiences it as a hang
// rather than as an outage, which is harder to diagnose and more annoying.
func (s State) Usable() bool {
	const maxTolerableLoss = 30.0
	return s.Up && s.LossPct < maxTolerableLoss && s.ThroughputMbps > 0
}

// CapExhausted reports whether the link has spent its monthly allowance.
func (s State) CapExhausted() bool {
	return s.Config.MonthlyCapGB > 0 && s.UsedGBThisMonth >= s.Config.MonthlyCapGB
}

// Registry holds the live state of every configured link.
type Registry struct {
	mu    sync.RWMutex
	state map[string]*State
	order []string
}

// NewRegistry builds a registry from configuration.
func NewRegistry(configs []Config) (*Registry, error) {
	r := &Registry{state: make(map[string]*State)}

	for _, c := range configs {
		if !c.Enabled {
			continue
		}
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.state[c.Name]; dup {
			return nil, fmt.Errorf("link: duplicate name %q", c.Name)
		}
		r.state[c.Name] = &State{
			Config:         c,
			ThroughputMbps: c.ExpectedMbps,
		}
		r.order = append(r.order, c.Name)
	}
	if len(r.state) == 0 {
		return nil, fmt.Errorf("link: no enabled links configured")
	}
	sort.Strings(r.order)
	return r, nil
}

// Update folds a probe result into a link's state.
func (r *Registry) Update(name string, up bool, latencyMs, lossPct, throughputMbps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.state[name]
	if !ok {
		return
	}
	now := time.Now()

	// Count a flap only on a down-to-up transition, so a link that is simply out
	// of coverage for a while is not mistaken for an unstable one.
	if up && !s.Up && !s.LastUpAt.IsZero() {
		s.FlapCount++
	}
	if up {
		s.LastUpAt = now
		s.ConsecFails = 0
	} else {
		s.ConsecFails++
	}

	s.Up = up
	s.LatencyMs = latencyMs
	s.LossPct = lossPct
	if throughputMbps > 0 {
		s.ThroughputMbps = throughputMbps
	} else if !up {
		// A down link has no throughput. Leaving the last good figure in place
		// would let the policy engine keep scoring it as though it were fast.
		s.ThroughputMbps = 0
	}
	s.LastProbe = now
}

// AddUsage records consumed volume, for cap tracking.
func (r *Registry) AddUsage(name string, gb float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.state[name]; ok {
		s.UsedGBThisMonth += gb
	}
}

// ResetUsage clears monthly counters, to be called on the billing boundary.
func (r *Registry) ResetUsage() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.state {
		s.UsedGBThisMonth = 0
	}
}

// Get returns a copy of one link's state.
func (r *Registry) Get(name string) (State, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.state[name]
	if !ok {
		return State{}, false
	}
	return *s, true
}

// All returns copies of every link's state, in stable name order.
func (r *Registry) All() []State {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]State, 0, len(r.state))
	for _, name := range r.order {
		out = append(out, *r.state[name])
	}
	return out
}

// Usable returns the links currently worth selecting.
func (r *Registry) Usable() []State {
	all := r.All()
	out := make([]State, 0, len(all))
	for _, s := range all {
		if s.Usable() {
			out = append(out, s)
		}
	}
	return out
}
