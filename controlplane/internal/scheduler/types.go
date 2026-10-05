// Package scheduler turns inventory and telemetry into an allocation plan.
//
// The division of labour is deliberate. This package owns everything that needs
// to know about the real network: which sectors a terminal can see, how fast
// each would be, and how much the subscriber is actually using. The solver
// subprocess owns only the allocation decision. That keeps the hard maths pure
// and replayable, and keeps the messy RF and inventory work in a language with
// good library support.
//
// The types here mirror schema/scheduler-problem.schema.json and
// schema/scheduler-plan.schema.json. Those files are authoritative; if these
// structs disagree, these are wrong.
package scheduler

// Policy mirrors the solver's handoff and fairness policy.
type Policy struct {
	HandoffGainThreshold  float64 `json:"handoff_gain_threshold"`
	MinDwellSeconds       float64 `json:"min_dwell_seconds"`
	Stickiness            float64 `json:"stickiness"`
	ReserveCommittedFirst bool    `json:"reserve_committed_first"`
}

// ProblemSite is a tower as the solver sees it.
type ProblemSite struct {
	ID string `json:"id"`

	// BackhaulMbps is a wired circuit's capacity, and is zero for a site that
	// relays over the air. The two are mutually exclusive on purpose: a relay
	// site's ceiling is its trunk radio, and carrying a second meaningless
	// number alongside it invites the dashboard bug where a saturated site
	// reports nine percent utilisation.
	BackhaulMbps float64 `json:"backhaul_mbps"`

	ParentSiteID     string `json:"parent_site_id,omitempty"`
	BackhaulSectorID string `json:"backhaul_sector_id,omitempty"`

	// RelayAchievableMbps is the trunk radio's capacity, set only for relay
	// sites.
	RelayAchievableMbps float64 `json:"relay_achievable_mbps,omitempty"`
}

// ProblemSector is a sector's schedulable capacity.
type ProblemSector struct {
	ID            string  `json:"id"`
	SiteID        string  `json:"site_id"`
	AirtimeBudget float64 `json:"airtime_budget"`
	DLULSplit     float64 `json:"dl_ul_split"`
	MaxTerminals  int     `json:"max_terminals"`
}

// Candidate is one sector a terminal could use, and what it would deliver.
type Candidate struct {
	SectorID           string  `json:"sector_id"`
	AchievableDownMbps float64 `json:"achievable_down_mbps"`
	AchievableUpMbps   float64 `json:"achievable_up_mbps"`
	SINRdB             float64 `json:"sinr_db"`
	MCS                string  `json:"mcs"`

	// Measured distinguishes a terminal's own report from the propagation
	// model's guess. Measured wins, and the difference between them is how the
	// model gets calibrated.
	Measured bool `json:"measured"`
}

// ProblemTerminal is one subscriber's CPE for this tick.
type ProblemTerminal struct {
	ID       string `json:"id"`
	Priority int    `json:"priority"`

	DemandDownMbps float64 `json:"demand_down_mbps"`
	DemandUpMbps   float64 `json:"demand_up_mbps"`

	CeilingDownMbps float64 `json:"ceiling_down_mbps"`
	CeilingUpMbps   float64 `json:"ceiling_up_mbps"`

	CommittedDownMbps float64 `json:"committed_down_mbps"`
	CommittedUpMbps   float64 `json:"committed_up_mbps"`

	CurrentSectorID string  `json:"current_sector_id,omitempty"`
	DwellSeconds    float64 `json:"dwell_seconds"`
	PinnedSectorID  string  `json:"pinned_sector_id,omitempty"`

	Candidates []Candidate `json:"candidates"`
}

// Problem is one tick of work for the solver.
type Problem struct {
	Epoch       uint64            `json:"epoch"`
	GeneratedAt string            `json:"generated_at"`
	TickSeconds float64           `json:"tick_seconds"`
	Policy      Policy            `json:"policy"`
	Sites       []ProblemSite     `json:"sites"`
	Sectors     []ProblemSector   `json:"sectors"`
	Terminals   []ProblemTerminal `json:"terminals"`
}

// Assignment is the solver's decision for one terminal.
type Assignment struct {
	TerminalID string `json:"terminal_id"`
	SectorID   string `json:"sector_id"`

	AirtimeDown float64 `json:"airtime_down"`
	AirtimeUp   float64 `json:"airtime_up"`

	GrantDownMbps float64 `json:"grant_down_mbps"`
	GrantUpMbps   float64 `json:"grant_up_mbps"`

	SINRdB float64 `json:"sinr_db"`
	MCS    string  `json:"mcs"`

	CommittedMet bool `json:"committed_met"`

	// LimitedBy is why the grant is not larger: demand, ceiling, airtime,
	// backhaul or none. The single most useful field when a subscriber calls.
	LimitedBy string `json:"limited_by"`

	// IsRelay marks a synthetic terminal standing in for a child site's trunk.
	// Infrastructure, not a customer; exclude from customer-facing totals.
	IsRelay bool `json:"is_relay"`
}

// Handoff records a terminal moving between sectors.
type Handoff struct {
	TerminalID   string  `json:"terminal_id"`
	FromSectorID string  `json:"from_sector_id"`
	ToSectorID   string  `json:"to_sector_id"`
	Reason       string  `json:"reason"`
	Gain         float64 `json:"gain"`
}

// SectorReport is one sector's load after allocation.
type SectorReport struct {
	SectorID        string  `json:"sector_id"`
	AirtimeUsedDown float64 `json:"airtime_used_down"`
	AirtimeUsedUp   float64 `json:"airtime_used_up"`
	AirtimeBudget   float64 `json:"airtime_budget"`
	Terminals       int     `json:"terminals"`
	OfferedDownMbps float64 `json:"offered_down_mbps"`
	GrantedDownMbps float64 `json:"granted_down_mbps"`

	// Oversubscribed means committed rates alone exceeded the airtime budget.
	// A capacity problem, not a tuning problem.
	Oversubscribed bool `json:"oversubscribed"`
}

// BackhaulReport is one site's trunk utilisation.
type BackhaulReport struct {
	SiteID       string  `json:"site_id"`
	OfferedMbps  float64 `json:"offered_mbps"`
	CapacityMbps float64 `json:"capacity_mbps"`
	Utilization  float64 `json:"utilization"`
	Congested    bool    `json:"congested"`
	RelayDepth   int     `json:"relay_depth"`
}

// Objective summarises how well the tick went.
type Objective struct {
	OfferedDownMbps float64 `json:"offered_down_mbps"`
	GrantedDownMbps float64 `json:"granted_down_mbps"`
	OfferedUpMbps   float64 `json:"offered_up_mbps"`
	GrantedUpMbps   float64 `json:"granted_up_mbps"`

	// CommittedShortfallMbps above zero is a breach of what was sold. It belongs
	// on a dashboard, not in a log file.
	CommittedShortfallMbps float64 `json:"committed_shortfall_mbps"`

	UnservedTerminals int `json:"unserved_terminals"`
}

// Plan is the solver's answer.
type Plan struct {
	Epoch       uint64           `json:"epoch"`
	Solver      string           `json:"solver"`
	SolveMicros int64            `json:"solve_micros"`
	Objective   Objective        `json:"objective"`
	Assignments []Assignment     `json:"assignments"`
	Handoffs    []Handoff        `json:"handoffs"`
	Sectors     []SectorReport   `json:"sectors"`
	Backhaul    []BackhaulReport `json:"backhaul"`
	Warnings    []string         `json:"warnings"`
}
