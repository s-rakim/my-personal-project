// Package registry holds the network's persistent inventory: service plans,
// tower sites, sector radios, subscribers and their terminals.
//
// Sites form a tree. A site either backhauls straight to the PoP or relays
// through a parent site, which is this network's equivalent of Starlink's
// inter-satellite laser links: it buys reach into places fibre does not go, at
// the cost of every hop sharing one upstream pipe. The scheduler accounts
// capacity up that tree, because the usual way a relay network fails is a
// single congested trunk two hops away from the subscriber who is complaining.
package registry

import (
	"fmt"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/radio"
)

// BackhaulKind describes how a site reaches its parent or the PoP.
type BackhaulKind string

const (
	// BackhaulFiber is a wired circuit to the PoP. Treat its capacity as hard.
	BackhaulFiber BackhaulKind = "fiber"
	// BackhaulPTP is a point-to-point microwave or mmWave link. Its capacity
	// varies with weather, so configure it at the rate it sustains in rain,
	// not the rate it shows on a clear day.
	BackhaulPTP BackhaulKind = "ptp"
	// BackhaulRelay means this site's traffic rides a parent site's sectors,
	// sharing airtime with that parent's subscribers. The most fragile option
	// and the one worth monitoring hardest.
	BackhaulRelay BackhaulKind = "relay"

	// BackhaulCellular reaches the internet over a carrier's LTE or 5G network
	// through a SIM modem. It is the fastest way to light up a site with no
	// fibre anywhere near it, and it behaves unlike every other option here:
	//
	//   - Capacity is whatever the carrier's sector gives you this minute. You
	//     are a subscriber on someone else's oversubscribed tower, so configure
	//     BackhaulMbps at the rate it sustains at 8pm, not at noon.
	//   - It is usually metered. Set MonthlyCapGB and watch it, or the month
	//     ends with a throttle you did not plan for.
	//   - You sit behind the carrier's own CGNAT, so nothing inbound works and
	//     you cannot announce your own addresses over it. Fine for best-effort
	//     service; useless as the path for anything needing a static address.
	//   - Latency is higher and more variable than a microwave link of the same
	//     throughput.
	//
	// Reasonable as a first uplink or as failover. Reasonable as the permanent
	// trunk for a growing subscriber base only if the economics have been
	// checked, since you are reselling capacity you rent by the gigabyte.
	BackhaulCellular BackhaulKind = "cellular"
)

// PlanTier is a service plan. The distinction between Down/Up and Committed is
// the one that matters operationally: the first is a ceiling you advertise, the
// second is a floor you promise. The scheduler protects the floor and treats
// the ceiling as best-effort.
type PlanTier struct {
	Name              string  `json:"name"`
	DownMbps          float64 `json:"down_mbps"`
	UpMbps            float64 `json:"up_mbps"`
	CommittedDownMbps float64 `json:"committed_down_mbps"`
	CommittedUpMbps   float64 `json:"committed_up_mbps"`

	// Priority breaks ties when airtime is scarce. Higher wins. Keep the range
	// small and meaningful; a dozen priority levels is a dozen ways to be
	// surprised.
	Priority int `json:"priority"`

	// BurstMbps and BurstSeconds let a plan exceed DownMbps briefly, which is
	// what makes a connection feel fast on page loads without selling capacity
	// you do not have. Zero disables bursting.
	BurstMbps    float64 `json:"burst_mbps"`
	BurstSeconds float64 `json:"burst_seconds"`
}

// Validate checks a plan is internally coherent.
func (p PlanTier) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("plan: name is required")
	}
	if p.DownMbps <= 0 || p.UpMbps <= 0 {
		return fmt.Errorf("plan %q: down_mbps and up_mbps must be positive", p.Name)
	}
	if p.CommittedDownMbps > p.DownMbps {
		return fmt.Errorf("plan %q: committed_down_mbps %.1f exceeds down_mbps %.1f",
			p.Name, p.CommittedDownMbps, p.DownMbps)
	}
	if p.CommittedUpMbps > p.UpMbps {
		return fmt.Errorf("plan %q: committed_up_mbps %.1f exceeds up_mbps %.1f",
			p.Name, p.CommittedUpMbps, p.UpMbps)
	}
	if p.BurstMbps != 0 && p.BurstMbps < p.DownMbps {
		return fmt.Errorf("plan %q: burst_mbps %.1f is below down_mbps %.1f",
			p.Name, p.BurstMbps, p.DownMbps)
	}
	return nil
}

// Site is a tower or rooftop with radios on it.
type Site struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Position radio.Point `json:"position"`

	// ParentSiteID is the site this one relays through. Empty means it reaches
	// the PoP directly.
	ParentSiteID string `json:"parent_site_id,omitempty"`

	BackhaulKind BackhaulKind `json:"backhaul_kind"`
	BackhaulMbps float64      `json:"backhaul_mbps"`

	// MonthlyCapGB is the metered allowance on this site's uplink, used by
	// BackhaulCellular. Zero means unmetered. The scheduler does not enforce it;
	// it is reported so the overage is a decision rather than a surprise.
	MonthlyCapGB float64 `json:"monthly_cap_gb,omitempty"`

	// BackhaulSectorID is the parent's sector carrying this relay, set only for
	// BackhaulRelay. The relay's traffic is charged against that sector's
	// airtime, so it competes with real subscribers there.
	BackhaulSectorID string `json:"backhaul_sector_id,omitempty"`

	Enabled bool   `json:"enabled"`
	Notes   string `json:"notes,omitempty"`
}

// Sector is one antenna and radio: a sliver of coverage on a site.
type Sector struct {
	ID     string `json:"id"`
	SiteID string `json:"site_id"`
	Name   string `json:"name"`

	AzimuthDeg    float64 `json:"azimuth_deg"`
	BeamwidthDeg  float64 `json:"beamwidth_deg"`
	VBeamwidthDeg float64 `json:"v_beamwidth_deg"`
	DowntiltDeg   float64 `json:"downtilt_deg"`

	FreqMHz         float64 `json:"freq_mhz"`
	ChannelWidthMHz float64 `json:"channel_width_mhz"`
	TxPowerDBm      float64 `json:"tx_power_dbm"`
	AntennaGainDBi  float64 `json:"antenna_gain_dbi"`
	FeederLossDB    float64 `json:"feeder_loss_db"`

	// ClutterLossDB is the excess path loss this sector sees in practice.
	// Start from a survey, then correct it from the gap between predicted and
	// reported SINR on real installs.
	ClutterLossDB float64 `json:"clutter_loss_db"`
	FadeMarginDB  float64 `json:"fade_margin_db"`

	// InterferenceDBm is the measured co-channel noise floor from a spectrum
	// scan. In unlicensed bands this is usually what caps the sector, so
	// re-scan after any neighbour lights up new gear.
	InterferenceDBm float64 `json:"interference_dbm"`

	// NoiseFigureDB is this radio's own receiver noise, used for the uplink
	// budget where the sector is the receiver.
	NoiseFigureDB float64 `json:"noise_figure_db"`

	// DLULSplit is the share of airtime given to the downlink. Time-division
	// gear shares one pool; set 1.0 for frequency-division gear.
	DLULSplit float64 `json:"dl_ul_split"`

	// AirtimeBudget is the usable fraction of a second, after management
	// traffic and a deliberate headroom reserve. Scheduling to 1.0 means
	// scheduling to the point where latency falls apart; 0.85 is a sane start.
	AirtimeBudget      float64 `json:"airtime_budget"`
	ProtocolEfficiency float64 `json:"protocol_efficiency"`

	MaxTerminals int     `json:"max_terminals"`
	MaxRangeKm   float64 `json:"max_range_km"`

	Enabled bool `json:"enabled"`
}

// Validate checks a sector's RF parameters are plausible.
func (s Sector) Validate() error {
	if s.ID == "" || s.SiteID == "" {
		return fmt.Errorf("sector: id and site_id are required")
	}
	if s.FreqMHz <= 0 {
		return fmt.Errorf("sector %q: freq_mhz must be positive", s.ID)
	}
	if s.ChannelWidthMHz <= 0 {
		return fmt.Errorf("sector %q: channel_width_mhz must be positive", s.ID)
	}
	if s.BeamwidthDeg <= 0 || s.BeamwidthDeg > 360 {
		return fmt.Errorf("sector %q: beamwidth_deg must be in (0, 360]", s.ID)
	}
	if s.AirtimeBudget <= 0 || s.AirtimeBudget > 1 {
		return fmt.Errorf("sector %q: airtime_budget must be in (0, 1]", s.ID)
	}
	return nil
}

// Subscriber is a billable account.
type Subscriber struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Plan string `json:"plan"`

	// Username and PasswordHash are the RADIUS credentials. The hash is
	// PBKDF2; nothing here stores a recoverable password.
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`

	Suspended bool      `json:"suspended"`
	CreatedAt time.Time `json:"created_at"`
	Notes     string    `json:"notes,omitempty"`
}

// Terminal is the CPE radio at a subscriber's premises.
type Terminal struct {
	ID           string `json:"id"` // serial number
	SubscriberID string `json:"subscriber_id"`

	Position       radio.Point `json:"position"`
	AntennaGainDBi float64     `json:"antenna_gain_dbi"`
	NoiseFigureDB  float64     `json:"noise_figure_db"`
	FeederLossDB   float64     `json:"feeder_loss_db"`

	// TxPowerDBm is the CPE's conducted transmit power, for the uplink budget.
	// It is normally well below the tower's, which is why uplink is the
	// direction that fails first on a long link.
	TxPowerDBm float64 `json:"tx_power_dbm"`

	// TokenHash authenticates the terminal to the control-plane API.
	TokenHash string `json:"token_hash"`

	// PinnedSectorID forces an assignment, overriding the scheduler. Useful
	// when a subscriber has a directional antenna aimed at one tower and
	// nothing else will do.
	PinnedSectorID string `json:"pinned_sector_id,omitempty"`

	Enabled   bool      `json:"enabled"`
	InstallAt time.Time `json:"install_at"`
}

// Snapshot is a consistent, read-only view of the whole inventory. The
// scheduler takes one per tick so it is never reasoning about a half-changed
// network.
type Snapshot struct {
	Plans       map[string]PlanTier   `json:"plans"`
	Sites       map[string]Site       `json:"sites"`
	Sectors     map[string]Sector     `json:"sectors"`
	Subscribers map[string]Subscriber `json:"subscribers"`
	Terminals   map[string]Terminal   `json:"terminals"`
}

// SectorsBySite groups enabled sectors by their site.
func (s Snapshot) SectorsBySite() map[string][]Sector {
	out := make(map[string][]Sector)
	for _, sec := range s.Sectors {
		out[sec.SiteID] = append(out[sec.SiteID], sec)
	}
	return out
}

// BackhaulPath returns the chain of sites from the given site up to the PoP,
// starting with the site itself. It returns an error on a cycle, which is worth
// surfacing loudly: a misconfigured parent pointer would otherwise hang
// capacity accounting.
func (s Snapshot) BackhaulPath(siteID string) ([]Site, error) {
	var path []Site
	seen := make(map[string]bool)

	for id := siteID; id != ""; {
		if seen[id] {
			return nil, fmt.Errorf("registry: backhaul cycle at site %q", id)
		}
		seen[id] = true

		site, ok := s.Sites[id]
		if !ok {
			return nil, fmt.Errorf("registry: site %q references missing parent %q", siteID, id)
		}
		path = append(path, site)
		id = site.ParentSiteID

		if len(path) > len(s.Sites)+1 {
			return nil, fmt.Errorf("registry: backhaul path from %q is implausibly long", siteID)
		}
	}
	return path, nil
}

// Plan returns the plan for a subscriber, falling back to a zero plan when the
// account references a plan that no longer exists.
func (s Snapshot) Plan(subscriberID string) (PlanTier, bool) {
	sub, ok := s.Subscribers[subscriberID]
	if !ok {
		return PlanTier{}, false
	}
	p, ok := s.Plans[sub.Plan]
	return p, ok
}
