// Package config loads the control plane's configuration.
//
// JSON rather than YAML, because a dependency-free daemon is worth more on a
// tower site than the convenience of unquoted keys. Every field has a working
// default, so an empty file produces a daemon that runs in dry-run mode without
// touching the network.
package config

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"
)

// Config is the whole daemon's configuration.
type Config struct {
	// NodeID identifies this PoP controller in logs, metrics and RADIUS
	// Class attributes. Set it per site when you run more than one.
	NodeID string `json:"node_id"`

	// DataDir holds the inventory, IP allocations and rendered dataplane
	// scripts. It must survive restarts: losing it means every subscriber gets
	// a new IP address.
	DataDir string `json:"data_dir"`

	Scheduler SchedulerConfig `json:"scheduler"`
	IPAM      IPAMConfig      `json:"ipam"`
	RADIUS    RADIUSConfig    `json:"radius"`
	API       APIConfig       `json:"api"`
	Dataplane DataplaneConfig `json:"dataplane"`
	Radio     RadioConfig     `json:"radio"`
}

// SchedulerConfig controls the allocation tick.
type SchedulerConfig struct {
	// SolverPath is the airtime solver binary. It runs as a subprocess so a
	// crash in it cannot take AAA down with it.
	SolverPath string `json:"solver_path"`

	// TickSeconds is how often the allocation is recomputed. Starlink reassigns
	// terminals on 15-second boundaries; fixed towers have predictable geometry
	// and can safely use longer, which costs less CPU and causes less churn.
	TickSeconds float64 `json:"tick_seconds"`

	SolverTimeoutSeconds float64 `json:"solver_timeout_seconds"`

	HandoffGainThreshold  float64 `json:"handoff_gain_threshold"`
	MinDwellSeconds       float64 `json:"min_dwell_seconds"`
	Stickiness            float64 `json:"stickiness"`
	ReserveCommittedFirst bool    `json:"reserve_committed_first"`
}

// IPAMConfig describes the address pools.
type IPAMConfig struct {
	// CGNATPool is the shared-address space handed to terminals. 100.64.0.0/10
	// is reserved for exactly this (RFC 6598) and is what Starlink uses for
	// residential service. Do not use RFC 1918 here: subscribers have their own
	// 192.168 and 10.0 networks behind the CPE and the collision is miserable
	// to debug.
	CGNATPool string `json:"cgnat_pool"`

	// IPv6Pool is the prefix delegated from, normally the /32 an RIR assigned
	// you. Native IPv6 matters more here than on a wired network: it is the one
	// way a CGNAT'd subscriber gets a real inbound-reachable address.
	IPv6Pool string `json:"ipv6_pool"`

	// IPv6DelegationBits is the prefix length handed to each subscriber. A /56
	// gives them 256 subnets, costs nothing, and is the usual choice.
	IPv6DelegationBits int `json:"ipv6_delegation_bits"`
}

// RADIUSClient is one NAS permitted to talk to the AAA server.
type RADIUSClient struct {
	// CIDR is the network the NAS sources packets from.
	CIDR string `json:"cidr"`
	// Secret is the RADIUS shared secret. RADIUS authenticates and encrypts
	// with this alone, using MD5, so treat it as a password: long, random, and
	// different for every NAS.
	Secret string `json:"secret"`
	Name   string `json:"name"`
}

// RADIUSConfig configures the AAA server.
type RADIUSConfig struct {
	Enabled    bool           `json:"enabled"`
	AuthListen string         `json:"auth_listen"`
	AcctListen string         `json:"acct_listen"`
	Clients    []RADIUSClient `json:"clients"`

	// SessionTimeoutSeconds forces periodic reauthentication. It is how a
	// suspended account actually stops passing traffic, so do not set it to
	// days.
	SessionTimeoutSeconds int `json:"session_timeout_seconds"`

	// InterimIntervalSeconds is how often the NAS reports usage. Shorter means
	// better data-cap accounting and more accounting traffic.
	InterimIntervalSeconds int `json:"interim_interval_seconds"`
}

// APIConfig configures the HTTP control and telemetry interface.
type APIConfig struct {
	Listen string `json:"listen"`

	// AdminToken guards every administrative route. Empty disables those
	// routes entirely, which is the right setting if you have not set up TLS.
	AdminToken string `json:"admin_token"`

	TLSCertFile string `json:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file"`
}

// DataplaneConfig controls how grants reach the kernel.
type DataplaneConfig struct {
	// Backend is "dryrun" or "linux". Dry-run renders the scripts it would
	// apply into DataDir and changes nothing, which is how you inspect a change
	// before trusting it to live subscribers.
	Backend string `json:"backend"`

	// Apply must be explicitly true before the linux backend touches the
	// kernel. Two separate switches, because an accidental nftables flush on a
	// PoP router takes everyone offline at once.
	Apply bool `json:"apply"`

	WANInterface    string `json:"wan_interface"`
	AccessInterface string `json:"access_interface"`

	// NFTTable is the nftables table name owned by this daemon. It is flushed
	// and rebuilt on every apply, so it must not be shared with hand-written
	// rules.
	NFTTable string `json:"nft_table"`

	// PublicIPv4 is the address CGNAT translates to.
	PublicIPv4 string `json:"public_ipv4"`
}

// RadioConfig holds defaults used when predicting link quality for a terminal
// that has not reported anything yet.
type RadioConfig struct {
	NoiseFigureDB      float64 `json:"noise_figure_db"`
	ProtocolEfficiency float64 `json:"protocol_efficiency"`

	// DefaultDemandMbps is the offered load assumed for a terminal with no
	// telemetry. Allocating to plan ceilings instead would reserve the whole
	// network for subscribers who are asleep.
	DefaultDemandMbps float64 `json:"default_demand_mbps"`

	// DemandHalfLifeSeconds smooths measured demand. Too short and the
	// scheduler chases every burst; too long and it never notices the evening
	// peak.
	DemandHalfLifeSeconds float64 `json:"demand_half_life_seconds"`

	// TelemetryStaleSeconds is how long a terminal's reported SINR is trusted.
	// After that the predicted value is used, because a stale measurement from
	// before a storm is worse than an honest estimate.
	TelemetryStaleSeconds float64 `json:"telemetry_stale_seconds"`
}

// Default returns a configuration that runs safely out of the box: dry-run
// dataplane, RADIUS off, admin routes disabled.
func Default() Config {
	return Config{
		NodeID:  "pop-1",
		DataDir: "var",
		Scheduler: SchedulerConfig{
			SolverPath:            "scheduler/build/bswisp-solver",
			TickSeconds:           15,
			SolverTimeoutSeconds:  10,
			HandoffGainThreshold:  1.25,
			MinDwellSeconds:       60,
			Stickiness:            0.15,
			ReserveCommittedFirst: true,
		},
		IPAM: IPAMConfig{
			CGNATPool:          "100.64.0.0/10",
			IPv6Pool:           "2001:db8::/32",
			IPv6DelegationBits: 56,
		},
		RADIUS: RADIUSConfig{
			Enabled:                false,
			AuthListen:             ":1812",
			AcctListen:             ":1813",
			SessionTimeoutSeconds:  3600,
			InterimIntervalSeconds: 300,
		},
		API: APIConfig{
			Listen: "127.0.0.1:8080",
		},
		Dataplane: DataplaneConfig{
			Backend:         "dryrun",
			Apply:           false,
			WANInterface:    "eth0",
			AccessInterface: "eth1",
			NFTTable:        "bswisp",
			PublicIPv4:      "198.51.100.1",
		},
		Radio: RadioConfig{
			NoiseFigureDB:         6,
			ProtocolEfficiency:    0.75,
			DefaultDemandMbps:     8,
			DemandHalfLifeSeconds: 120,
			TelemetryStaleSeconds: 180,
		},
	}
}

// Load reads a configuration file, applying defaults for anything absent. An
// empty path returns the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	// Decode over the defaults so a partial file only overrides what it names.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate rejects configurations that would fail later in a confusing place.
func (c Config) Validate() error {
	if c.NodeID == "" {
		return fmt.Errorf("config: node_id is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("config: data_dir is required")
	}
	if c.Scheduler.TickSeconds <= 0 {
		return fmt.Errorf("config: scheduler.tick_seconds must be positive")
	}
	if c.Scheduler.SolverTimeoutSeconds <= 0 {
		return fmt.Errorf("config: scheduler.solver_timeout_seconds must be positive")
	}
	if c.Scheduler.SolverTimeoutSeconds >= c.Scheduler.TickSeconds {
		// Otherwise a slow solve overlaps the next tick and the two fight over
		// the dataplane.
		return fmt.Errorf("config: scheduler.solver_timeout_seconds (%.1f) must be "+
			"less than tick_seconds (%.1f)",
			c.Scheduler.SolverTimeoutSeconds, c.Scheduler.TickSeconds)
	}
	if c.Scheduler.HandoffGainThreshold < 1 {
		return fmt.Errorf("config: scheduler.handoff_gain_threshold must be at least 1.0, " +
			"or terminals will chase every marginal improvement and flap")
	}

	pool, err := netip.ParsePrefix(c.IPAM.CGNATPool)
	if err != nil {
		return fmt.Errorf("config: ipam.cgnat_pool: %w", err)
	}
	if !pool.Addr().Is4() {
		return fmt.Errorf("config: ipam.cgnat_pool must be IPv4")
	}

	v6, err := netip.ParsePrefix(c.IPAM.IPv6Pool)
	if err != nil {
		return fmt.Errorf("config: ipam.ipv6_pool: %w", err)
	}
	if !v6.Addr().Is6() {
		return fmt.Errorf("config: ipam.ipv6_pool must be IPv6")
	}
	if c.IPAM.IPv6DelegationBits <= v6.Bits() || c.IPAM.IPv6DelegationBits > 64 {
		return fmt.Errorf("config: ipam.ipv6_delegation_bits must be between %d and 64",
			v6.Bits()+1)
	}

	switch c.Dataplane.Backend {
	case "dryrun", "linux":
	default:
		return fmt.Errorf("config: dataplane.backend must be \"dryrun\" or \"linux\", got %q",
			c.Dataplane.Backend)
	}
	if c.Dataplane.Backend == "linux" && c.Dataplane.Apply {
		if c.Dataplane.WANInterface == "" || c.Dataplane.AccessInterface == "" {
			return fmt.Errorf("config: dataplane.wan_interface and access_interface are " +
				"required when applying rules")
		}
		if _, err := netip.ParseAddr(c.Dataplane.PublicIPv4); err != nil {
			return fmt.Errorf("config: dataplane.public_ipv4: %w", err)
		}
	}

	for i, cl := range c.RADIUS.Clients {
		if _, err := netip.ParsePrefix(cl.CIDR); err != nil {
			return fmt.Errorf("config: radius.clients[%d].cidr: %w", i, err)
		}
		if len(cl.Secret) < 16 {
			return fmt.Errorf("config: radius.clients[%d] (%s): secret must be at least "+
				"16 characters; RADIUS protects it with MD5 alone", i, cl.CIDR)
		}
	}
	if c.RADIUS.Enabled && len(c.RADIUS.Clients) == 0 {
		return fmt.Errorf("config: radius is enabled but no clients are configured")
	}
	return nil
}

// Tick is the scheduler interval as a duration.
func (c Config) Tick() time.Duration {
	return time.Duration(c.Scheduler.TickSeconds * float64(time.Second))
}

// SolverTimeout is the solver subprocess deadline.
func (c Config) SolverTimeout() time.Duration {
	return time.Duration(c.Scheduler.SolverTimeoutSeconds * float64(time.Second))
}
