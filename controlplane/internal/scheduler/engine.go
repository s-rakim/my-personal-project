package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/config"
	"github.com/s-rakim/my-personal-project/controlplane/internal/dataplane"
	"github.com/s-rakim/my-personal-project/controlplane/internal/ipam"
	"github.com/s-rakim/my-personal-project/controlplane/internal/registry"
	"github.com/s-rakim/my-personal-project/controlplane/internal/session"
	"github.com/s-rakim/my-personal-project/controlplane/internal/telemetry"
)

// Metric names exported by the engine.
const (
	MetricTicks           = "bswisp_scheduler_ticks_total"
	MetricTickErrors      = "bswisp_scheduler_tick_errors_total"
	MetricSolveMicros     = "bswisp_scheduler_solve_microseconds"
	MetricTickMicros      = "bswisp_scheduler_tick_microseconds"
	MetricSectorAirtime   = "bswisp_sector_airtime_used"
	MetricSectorOversub   = "bswisp_sector_oversubscribed"
	MetricSectorTerminals = "bswisp_sector_terminals"
	MetricSectorGranted   = "bswisp_sector_granted_mbps"
	MetricBackhaulUtil    = "bswisp_backhaul_utilization"
	MetricBackhaulOffered = "bswisp_backhaul_offered_mbps"
	MetricGrantDown       = "bswisp_terminal_grant_down_mbps"
	MetricTerminalSINR    = "bswisp_terminal_sinr_db"
	MetricShortfall       = "bswisp_committed_shortfall_mbps"
	MetricUnserved        = "bswisp_unserved_terminals"
	MetricHandoffs        = "bswisp_handoffs_total"
	MetricOfferedDown     = "bswisp_offered_down_mbps"
	MetricGrantedDown     = "bswisp_granted_down_mbps"
	MetricIPAMv4Used      = "bswisp_ipam_v4_used"
	MetricIPAMv4Capacity  = "bswisp_ipam_v4_capacity"
	MetricSessionsOnline  = "bswisp_sessions_online"
)

// offlineRetention is how long an offline session is kept before its address is
// released. Long enough that a subscriber's power cut or a CPE reboot does not
// change their IP, short enough that a cancelled account frees its address the
// same day.
const offlineRetention = 6 * time.Hour

// Engine runs the allocation loop.
type Engine struct {
	cfg      config.Config
	reg      *registry.Store
	leases   *ipam.Allocator
	sessions *session.Manager
	builder  *Builder
	solver   *Solver
	dp       dataplane.Dataplane
	metrics  *telemetry.Registry
	log      *slog.Logger

	mu          sync.RWMutex
	epoch       uint64
	lastPlan    Plan
	lastProblem Problem
	lastNotes   []string
	lastError   string
	lastTickAt  time.Time
	ticked      bool
}

// NewEngine assembles the loop.
func NewEngine(cfg config.Config, reg *registry.Store, leases *ipam.Allocator,
	sessions *session.Manager, dp dataplane.Dataplane, metrics *telemetry.Registry,
	log *slog.Logger) *Engine {

	e := &Engine{
		cfg:      cfg,
		reg:      reg,
		leases:   leases,
		sessions: sessions,
		builder:  NewBuilder(cfg),
		solver:   NewSolver(cfg.Scheduler.SolverPath, cfg.SolverTimeout()),
		dp:       dp,
		metrics:  metrics,
		log:      log,
	}

	for _, m := range []struct {
		name string
		kind telemetry.Kind
		help string
	}{
		{MetricTicks, telemetry.Counter, "Allocation ticks completed"},
		{MetricTickErrors, telemetry.Counter, "Allocation ticks that failed"},
		{MetricSolveMicros, telemetry.Gauge, "Microseconds the solver took on the last tick"},
		{MetricTickMicros, telemetry.Gauge, "Microseconds the whole last tick took"},
		{MetricSectorAirtime, telemetry.Gauge, "Downlink airtime used, as a fraction of a second"},
		{MetricSectorOversub, telemetry.Gauge, "1 when committed rates exceed the sector's airtime budget"},
		{MetricSectorTerminals, telemetry.Gauge, "Terminals assigned to the sector, relays included"},
		{MetricSectorGranted, telemetry.Gauge, "Megabits per second granted on the sector"},
		{MetricBackhaulUtil, telemetry.Gauge, "Site trunk utilisation, 0 to 1"},
		{MetricBackhaulOffered, telemetry.Gauge, "Megabits per second offered into the site's trunk"},
		{MetricGrantDown, telemetry.Gauge, "Downlink grant per terminal, megabits per second"},
		{MetricTerminalSINR, telemetry.Gauge, "Signal to interference plus noise ratio, dB"},
		{MetricShortfall, telemetry.Gauge, "Committed rate the network could not deliver. Above zero is a breach of contract"},
		{MetricUnserved, telemetry.Gauge, "Terminals the scheduler could not place on any sector"},
		{MetricHandoffs, telemetry.Counter, "Sector reassignments"},
		{MetricOfferedDown, telemetry.Gauge, "Total offered downlink demand, megabits per second"},
		{MetricGrantedDown, telemetry.Gauge, "Total granted downlink, megabits per second"},
		{MetricIPAMv4Used, telemetry.Gauge, "CGNAT addresses allocated"},
		{MetricIPAMv4Capacity, telemetry.Gauge, "CGNAT addresses available"},
		{MetricSessionsOnline, telemetry.Gauge, "Terminals currently online"},
	} {
		metrics.Define(m.name, m.kind, m.help)
	}
	return e
}

// Run ticks until the context is cancelled. The first tick fires immediately so
// a restart does not leave subscribers unshaped for a whole interval.
func (e *Engine) Run(ctx context.Context) {
	tick := e.cfg.Tick()
	e.log.Info("scheduler running", "interval", tick.String(),
		"solver", e.cfg.Scheduler.SolverPath)

	if err := e.Tick(ctx); err != nil {
		e.log.Error("first allocation tick failed", "error", err)
	}

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.log.Info("scheduler stopping")
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil {
				e.log.Error("allocation tick failed", "error", err)
			}
		}
	}
}

// Tick computes and applies one allocation.
//
// A failure anywhere leaves the previous allocation in force. That is
// deliberate: a slightly stale set of rate limits is a far better failure mode
// than an empty one, which would either unshape everybody or cut everybody off.
func (e *Engine) Tick(ctx context.Context) error {
	started := time.Now()

	e.mu.Lock()
	e.epoch++
	epoch := e.epoch
	e.mu.Unlock()

	snap := e.reg.Snapshot()
	sessions := e.sessions.Online()
	problem, notes := e.builder.Build(epoch, snap, sessions, started.UTC())

	for _, n := range notes {
		e.log.Warn("scheduler problem note", "epoch", epoch, "note", n)
	}

	var plan Plan
	if len(problem.Terminals) == 0 {
		// Nothing to allocate. Still push an empty dataplane state so a
		// subscriber who just went away stops being shaped as though present.
		plan = Plan{Epoch: epoch, Solver: "none (no terminals online)"}
	} else {
		var err error
		plan, err = e.solver.Solve(ctx, problem)
		if err != nil {
			e.metrics.Inc(MetricTickErrors, nil)
			e.recordFailure(problem, notes, err.Error(), started)
			return err
		}
	}

	for _, w := range plan.Warnings {
		e.log.Warn("scheduler plan warning", "epoch", epoch, "warning", w)
	}

	// Fold the plan into session state, then into the dataplane.
	assigned := make(map[string]bool, len(plan.Assignments))
	dpSubs := make([]dataplane.Subscriber, 0, len(plan.Assignments))

	for _, a := range plan.Assignments {
		if a.IsRelay {
			// Relays are infrastructure. They have no session, no lease and no
			// rate limit of their own; the trunk radio enforces itself.
			continue
		}
		assigned[a.TerminalID] = true

		term, ok := snap.Terminals[a.TerminalID]
		if !ok {
			continue
		}
		sub, ok := snap.Subscribers[term.SubscriberID]
		if !ok {
			continue
		}
		plan := snap.Plans[sub.Plan]

		if moved := e.sessions.ApplyAssignment(a.TerminalID, a.SectorID,
			a.GrantDownMbps, a.GrantUpMbps, plan.DownMbps, plan.UpMbps,
			a.LimitedBy, a.CommittedMet); moved {
			e.metrics.Inc(MetricHandoffs, telemetry.Labels{"sector": a.SectorID})
		}

		sess, ok := e.sessions.Get(a.TerminalID)
		if !ok {
			continue
		}
		burst := plan.BurstMbps

		dpSubs = append(dpSubs, dataplane.Subscriber{
			TerminalID:    a.TerminalID,
			SubscriberID:  sub.ID,
			V4:            sess.Lease.V4,
			V6:            sess.Lease.V6,
			GrantDownMbps: a.GrantDownMbps,
			GrantUpMbps:   a.GrantUpMbps,
			BurstDownMbps: burst,
			Suspended:     sub.Suspended,
		})

		e.metrics.Set(MetricGrantDown, telemetry.Labels{"terminal": a.TerminalID}, a.GrantDownMbps)
		e.metrics.Set(MetricTerminalSINR, telemetry.Labels{"terminal": a.TerminalID}, a.SINRdB)
	}

	// Terminals the solver could not place, and suspended accounts, still need a
	// dataplane entry. Omitting them would leave them running at whatever the
	// previous tick allowed.
	for _, sess := range sessions {
		if assigned[sess.TerminalID] {
			continue
		}
		e.sessions.MarkUnserved(sess.TerminalID)

		term, ok := snap.Terminals[sess.TerminalID]
		if !ok {
			continue
		}
		sub, ok := snap.Subscribers[term.SubscriberID]
		if !ok {
			continue
		}
		dpSubs = append(dpSubs, dataplane.Subscriber{
			TerminalID:   sess.TerminalID,
			SubscriberID: sub.ID,
			V4:           sess.Lease.V4,
			V6:           sess.Lease.V6,
			Suspended:    sub.Suspended,
			Unserved:     !sub.Suspended,
		})
		e.metrics.Set(MetricGrantDown, telemetry.Labels{"terminal": sess.TerminalID}, 0)
	}

	if err := e.dp.Apply(ctx, dataplane.State{Epoch: epoch, Subscribers: dpSubs}); err != nil {
		e.metrics.Inc(MetricTickErrors, nil)
		e.recordFailure(problem, notes, err.Error(), started)
		return err
	}

	e.publishMetrics(plan, len(sessions))
	e.reapSessions()

	e.mu.Lock()
	e.lastPlan = plan
	e.lastProblem = problem
	e.lastNotes = notes
	e.lastError = ""
	e.lastTickAt = time.Now().UTC()
	e.ticked = true
	e.mu.Unlock()

	e.metrics.Inc(MetricTicks, nil)
	e.metrics.Set(MetricSolveMicros, nil, float64(plan.SolveMicros))
	e.metrics.Set(MetricTickMicros, nil, float64(time.Since(started).Microseconds()))

	e.log.Debug("allocation applied",
		"epoch", epoch, "terminals", len(problem.Terminals),
		"handoffs", len(plan.Handoffs),
		"granted_mbps", plan.Objective.GrantedDownMbps,
		"shortfall_mbps", plan.Objective.CommittedShortfallMbps,
		"took", time.Since(started).String())
	return nil
}

func (e *Engine) recordFailure(p Problem, notes []string, msg string, started time.Time) {
	e.mu.Lock()
	e.lastProblem = p
	e.lastNotes = notes
	e.lastError = msg
	e.lastTickAt = time.Now().UTC()
	e.mu.Unlock()
	e.metrics.Set(MetricTickMicros, nil, float64(time.Since(started).Microseconds()))
}

func (e *Engine) publishMetrics(plan Plan, online int) {
	for _, s := range plan.Sectors {
		l := telemetry.Labels{"sector": s.SectorID}
		e.metrics.Set(MetricSectorAirtime, l, s.AirtimeUsedDown)
		e.metrics.Set(MetricSectorTerminals, l, float64(s.Terminals))
		e.metrics.Set(MetricSectorGranted, l, s.GrantedDownMbps)
		oversub := 0.0
		if s.Oversubscribed {
			oversub = 1
		}
		e.metrics.Set(MetricSectorOversub, l, oversub)
	}
	for _, b := range plan.Backhaul {
		l := telemetry.Labels{"site": b.SiteID}
		e.metrics.Set(MetricBackhaulUtil, l, b.Utilization)
		e.metrics.Set(MetricBackhaulOffered, l, b.OfferedMbps)
	}
	e.metrics.Set(MetricShortfall, nil, plan.Objective.CommittedShortfallMbps)
	e.metrics.Set(MetricUnserved, nil, float64(plan.Objective.UnservedTerminals))
	e.metrics.Set(MetricOfferedDown, nil, plan.Objective.OfferedDownMbps)
	e.metrics.Set(MetricGrantedDown, nil, plan.Objective.GrantedDownMbps)
	e.metrics.Set(MetricSessionsOnline, nil, float64(online))

	v4Used, v4Cap, _, _ := e.leases.Stats()
	e.metrics.Set(MetricIPAMv4Used, nil, float64(v4Used))
	e.metrics.Set(MetricIPAMv4Capacity, nil, float64(v4Cap))
}

// reapSessions drops long-offline sessions, releases their addresses and removes
// their metric series. Without this, both the session table and the metrics
// endpoint grow for every subscriber who has ever connected.
func (e *Engine) reapSessions() {
	for _, id := range e.sessions.Prune(offlineRetention) {
		if err := e.leases.Release(id); err != nil {
			e.log.Warn("could not release lease", "terminal", id, "error", err)
		}
		e.metrics.DropMatching(MetricGrantDown, telemetry.Labels{"terminal": id})
		e.metrics.DropMatching(MetricTerminalSINR, telemetry.Labels{"terminal": id})
		e.log.Info("session reaped after being offline", "terminal", id,
			"retention", offlineRetention.String())
	}
}

// Status is a snapshot of the engine for the admin API.
type Status struct {
	Epoch       uint64    `json:"epoch"`
	LastTickAt  time.Time `json:"last_tick_at"`
	LastError   string    `json:"last_error,omitempty"`
	Notes       []string  `json:"notes,omitempty"`
	Healthy     bool      `json:"healthy"`
	TickSeconds float64   `json:"tick_seconds"`
}

// Status returns the engine's current state.
func (e *Engine) Status() Status {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Status{
		Epoch:       e.epoch,
		LastTickAt:  e.lastTickAt,
		LastError:   e.lastError,
		Notes:       e.lastNotes,
		Healthy:     e.ticked && e.lastError == "",
		TickSeconds: e.cfg.Scheduler.TickSeconds,
	}
}

// LastPlan returns the most recent plan.
func (e *Engine) LastPlan() Plan {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastPlan
}

// LastProblem returns the most recent problem, which is what to capture and
// replay through the solver by hand when an allocation looks wrong.
func (e *Engine) LastProblem() Problem {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastProblem
}

// SolverVersion asks the solver to identify itself.
func (e *Engine) SolverVersion(ctx context.Context) (string, error) {
	return e.solver.Version(ctx)
}
