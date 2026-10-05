// Package session holds live per-terminal state: what the terminal last
// reported, what it is currently allowed, and how long it has held its sector.
//
// This is deliberately in memory and not persisted. It is observational: on
// restart every terminal re-reports within a tick or two, and a stale demand
// estimate recovered from disk would be worse than no estimate at all. The
// durable facts live in the registry and the lease table.
package session

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/ipam"
)

// SectorObservation is one sector as a terminal sees it, from its scan.
//
// A measured value beats the propagation model every time, so these drive the
// scheduler's candidate list whenever they are fresh. The gap between measured
// and predicted is also the only honest way to calibrate a sector's clutter
// loss.
type SectorObservation struct {
	SectorID   string  `json:"sector_id"`
	SINRdB     float64 `json:"sinr_db"`
	RxPowerDBm float64 `json:"rx_power_dbm"`
}

// Telemetry is one report from a terminal.
type Telemetry struct {
	ReportedAt time.Time `json:"reported_at"`

	SectorID   string  `json:"sector_id"`
	SINRdB     float64 `json:"sinr_db"`
	RxPowerDBm float64 `json:"rx_power_dbm"`
	MCS        string  `json:"mcs"`

	// Throughput is what the terminal actually moved since its last report.
	// This, not the plan ceiling, is what the scheduler treats as demand.
	ThroughputDownMbps float64 `json:"throughput_down_mbps"`
	ThroughputUpMbps   float64 `json:"throughput_up_mbps"`

	// Counters are monotonic byte totals since the terminal booted, used for
	// usage accounting and to detect a CPE that silently rebooted.
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`

	// Obstructed reports the fraction of the sky or beam the terminal thinks is
	// blocked, where the hardware can tell. A slowly rising value is a tree
	// growing into the path, which is the most common cause of a link that was
	// fine at install and is not now.
	ObstructedFraction float64 `json:"obstructed_fraction"`

	Visible []SectorObservation `json:"visible"`
}

// Session is one terminal's live state.
type Session struct {
	TerminalID   string `json:"terminal_id"`
	SubscriberID string `json:"subscriber_id"`

	SectorID    string    `json:"sector_id"`
	SectorSince time.Time `json:"sector_since"`

	Lease  ipam.Lease `json:"lease"`
	Online bool       `json:"online"`

	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`

	Telemetry Telemetry `json:"telemetry"`

	// Demand is exponentially smoothed offered load. Raw throughput would make
	// the scheduler chase every burst and re-plan the sector constantly.
	DemandDownMbps float64 `json:"demand_down_mbps"`
	DemandUpMbps   float64 `json:"demand_up_mbps"`

	// Grant is what the last plan allowed, which is what the dataplane is
	// shaping to right now.
	GrantDownMbps float64 `json:"grant_down_mbps"`
	GrantUpMbps   float64 `json:"grant_up_mbps"`
	LimitedBy     string  `json:"limited_by"`
	CommittedMet  bool    `json:"committed_met"`

	// Ceiling is the plan's maximum, carried here so the demand estimator can
	// probe upward without running away past what the subscriber pays for.
	CeilingDownMbps float64 `json:"ceiling_down_mbps"`
	CeilingUpMbps   float64 `json:"ceiling_up_mbps"`

	// Accounting mirrors the RADIUS session, for data caps and billing.
	AcctSessionID string `json:"acct_session_id"`
	InputOctets   uint64 `json:"input_octets"`
	OutputOctets  uint64 `json:"output_octets"`

	// Handoffs counts sector changes this session. A terminal with a climbing
	// count is flapping, and flapping is a tuning problem, not a radio problem.
	Handoffs int `json:"handoffs"`
}

// DwellSeconds is how long the terminal has held its current sector.
func (s Session) DwellSeconds(now time.Time) float64 {
	if s.SectorSince.IsZero() {
		return 0
	}
	return now.Sub(s.SectorSince).Seconds()
}

// TelemetryFresh reports whether the last report is recent enough to trust over
// the propagation model.
func (s Session) TelemetryFresh(now time.Time, staleAfter time.Duration) bool {
	if s.Telemetry.ReportedAt.IsZero() {
		return false
	}
	return now.Sub(s.Telemetry.ReportedAt) <= staleAfter
}

// Manager owns the live session table.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session

	halfLife      time.Duration
	defaultDemand float64
	staleAfter    time.Duration
}

// Options configures the manager.
type Options struct {
	// DemandHalfLife is how fast the smoothed demand follows a change. Short
	// chases bursts; long misses the evening peak.
	DemandHalfLife time.Duration

	// DefaultDemandMbps is assumed for a terminal that has not reported yet.
	// Seeding this at the plan ceiling instead would reserve the network for
	// subscribers who are asleep.
	DefaultDemandMbps float64

	TelemetryStaleAfter time.Duration
}

// New returns an empty manager.
func New(opts Options) *Manager {
	if opts.DemandHalfLife <= 0 {
		opts.DemandHalfLife = 2 * time.Minute
	}
	if opts.DefaultDemandMbps <= 0 {
		opts.DefaultDemandMbps = 8
	}
	if opts.TelemetryStaleAfter <= 0 {
		opts.TelemetryStaleAfter = 3 * time.Minute
	}
	return &Manager{
		sessions:      make(map[string]*Session),
		halfLife:      opts.DemandHalfLife,
		defaultDemand: opts.DefaultDemandMbps,
		staleAfter:    opts.TelemetryStaleAfter,
	}
}

// StaleAfter is the telemetry freshness window.
func (m *Manager) StaleAfter() time.Duration { return m.staleAfter }

// Start brings a terminal online, reusing its existing session if it already
// had one. Reconnects are common and should not reset dwell or demand history:
// a CPE that bounces its link should not also lose its place in the queue.
func (m *Manager) Start(terminalID, subscriberID string, lease ipam.Lease) Session {
	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[terminalID]
	if !ok {
		s = &Session{
			TerminalID:     terminalID,
			SubscriberID:   subscriberID,
			StartedAt:      now,
			DemandDownMbps: m.defaultDemand,
			DemandUpMbps:   m.defaultDemand / 8,
			CommittedMet:   true,
		}
		m.sessions[terminalID] = s
	}
	s.SubscriberID = subscriberID
	s.Lease = lease
	s.Online = true
	s.LastSeen = now
	return *s
}

// Stop marks a terminal offline but keeps its session, so a brief outage does
// not erase its history. Use Prune to actually remove it.
func (m *Manager) Stop(terminalID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[terminalID]; ok {
		s.Online = false
		s.GrantDownMbps = 0
		s.GrantUpMbps = 0
	}
}

// Get returns a copy of one session.
func (m *Manager) Get(terminalID string) (Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[terminalID]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

// All returns copies of every session, sorted by terminal ID so output is
// stable.
func (m *Manager) All() []Session {
	m.mu.RLock()
	out := make([]Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, *s)
	}
	m.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].TerminalID < out[j].TerminalID })
	return out
}

// Online returns copies of the sessions currently up.
func (m *Manager) Online() []Session {
	all := m.All()
	out := make([]Session, 0, len(all))
	for _, s := range all {
		if s.Online {
			out = append(out, s)
		}
	}
	return out
}

// RecordTelemetry stores a report and folds its throughput into the smoothed
// demand estimate.
func (m *Manager) RecordTelemetry(terminalID string, t Telemetry) bool {
	now := time.Now().UTC()
	if t.ReportedAt.IsZero() {
		t.ReportedAt = now
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[terminalID]
	if !ok {
		return false
	}

	// Exponential smoothing with a half-life, so the weight of a sample depends
	// on elapsed time rather than on report count. Terminals report on their own
	// irregular schedules and a count-based filter would weight a chatty CPE
	// more heavily than a quiet one.
	dt := now.Sub(s.LastSeen).Seconds()
	if s.LastSeen.IsZero() || dt <= 0 {
		dt = 1
	}
	alpha := 1 - math.Pow(2, -dt/m.halfLife.Seconds())
	if alpha < 0 {
		alpha = 0
	}
	if alpha > 1 {
		alpha = 1
	}

	s.DemandDownMbps = estimateDemand(s.DemandDownMbps, t.ThroughputDownMbps,
		s.GrantDownMbps, s.CeilingDownMbps, alpha)
	s.DemandUpMbps = estimateDemand(s.DemandUpMbps, t.ThroughputUpMbps,
		s.GrantUpMbps, s.CeilingUpMbps, alpha)

	// A counter that went backwards means the CPE rebooted. Reset the accounting
	// baseline instead of recording a vast negative delta.
	if t.RxBytes < s.Telemetry.RxBytes || t.TxBytes < s.Telemetry.TxBytes {
		s.InputOctets = 0
		s.OutputOctets = 0
	} else {
		s.InputOctets += t.RxBytes - s.Telemetry.RxBytes
		s.OutputOctets += t.TxBytes - s.Telemetry.TxBytes
	}

	s.Telemetry = t
	s.LastSeen = now
	s.Online = true
	return true
}

// Tuning for the demand estimator.
const (
	// saturationRatio is how close to its grant a terminal must run before it is
	// treated as grant-limited rather than demand-limited.
	saturationRatio = 0.95

	// probeGrowth is how much the estimate is raised per report while saturated.
	// Geometric, like TCP slow start, so a subscriber who genuinely wants much
	// more reaches it in a few ticks instead of a few hundred.
	probeGrowth = 1.5

	// probeFloorMbps gets a terminal off zero. A subscriber granted nothing
	// reports nothing, and a purely multiplicative probe can never leave zero.
	probeFloorMbps = 2.0
)

// estimateDemand updates a smoothed demand estimate from one throughput sample.
//
// The subtlety here is a feedback loop that is easy to build by accident. If the
// estimate simply follows observed throughput, then throttling a subscriber makes
// them report less, which lowers the estimate, which justifies throttling them
// further. They converge to whatever they were first given and can never climb
// out, however much they actually want. The symptom is a network where every
// subscriber sits at a few megabits and nothing looks broken anywhere.
//
// The way out is to notice that throughput pinned at the grant is not a
// measurement of demand at all; it is a measurement of the limit. So when a
// terminal runs at its allowance, probe upward instead of converging. When it is
// genuinely below its allowance, the sample is real demand and smoothing is right.
//
// The probe stops at the plan ceiling, so a saturated subscriber climbs to what
// they pay for and no further. Over-estimating demand is cheap: the scheduler
// shares surplus by weighted fairness and an idle subscriber's share is simply
// reallocated. Under-estimating it is what strands people.
func estimateDemand(current, sample, grant, ceiling, alpha float64) float64 {
	if sample < 0 {
		sample = 0
	}

	if grant > 0 && sample >= grant*saturationRatio {
		probe := math.Max(sample, grant) * probeGrowth
		if probe < probeFloorMbps {
			probe = probeFloorMbps
		}
		if ceiling > 0 && probe > ceiling {
			probe = ceiling
		}
		// Ratchet: a probe must never lower the estimate.
		if probe > current {
			return probe
		}
		return current
	}

	next := current + alpha*(sample-current)
	if next < 0 {
		return 0
	}
	if ceiling > 0 && next > ceiling {
		return ceiling
	}
	return next
}

// ApplyAssignment records what the latest plan granted a terminal. Returns true
// when the sector changed, which the caller counts as a handoff.
func (m *Manager) ApplyAssignment(terminalID, sectorID string, grantDown, grantUp float64,
	ceilingDown, ceilingUp float64, limitedBy string, committedMet bool) bool {

	now := time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[terminalID]
	if !ok {
		return false
	}

	moved := s.SectorID != sectorID
	if moved {
		s.SectorID = sectorID
		s.SectorSince = now
		if s.Handoffs < math.MaxInt32 {
			s.Handoffs++
		}
	}
	s.GrantDownMbps = grantDown
	s.GrantUpMbps = grantUp
	s.CeilingDownMbps = ceilingDown
	s.CeilingUpMbps = ceilingUp
	s.LimitedBy = limitedBy
	s.CommittedMet = committedMet
	return moved
}

// MarkUnserved clears a terminal's grant when the scheduler could not place it,
// so the dataplane stops shaping to a stale allowance.
func (m *Manager) MarkUnserved(terminalID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[terminalID]; ok {
		s.GrantDownMbps = 0
		s.GrantUpMbps = 0
		s.LimitedBy = "airtime"
		s.CommittedMet = false
	}
}

// Prune removes sessions that have been offline longer than the given age, and
// returns their terminal IDs so the caller can release addresses and drop
// per-terminal metrics. Without this, metric series and session entries
// accumulate for every subscriber who ever connected.
func (m *Manager) Prune(olderThan time.Duration) []string {
	cutoff := time.Now().UTC().Add(-olderThan)

	m.mu.Lock()
	defer m.mu.Unlock()

	var removed []string
	for id, s := range m.sessions {
		if !s.Online && s.LastSeen.Before(cutoff) {
			delete(m.sessions, id)
			removed = append(removed, id)
		}
	}
	sort.Strings(removed)
	return removed
}

// SetAccounting records RADIUS accounting counters for a session.
func (m *Manager) SetAccounting(terminalID, acctSessionID string, in, out uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[terminalID]; ok {
		s.AcctSessionID = acctSessionID
		s.InputOctets = in
		s.OutputOctets = out
		s.LastSeen = time.Now().UTC()
	}
}
