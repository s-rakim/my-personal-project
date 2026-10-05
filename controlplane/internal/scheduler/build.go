package scheduler

import (
	"fmt"
	"sort"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/config"
	"github.com/s-rakim/my-personal-project/controlplane/internal/radio"
	"github.com/s-rakim/my-personal-project/controlplane/internal/registry"
	"github.com/s-rakim/my-personal-project/controlplane/internal/session"
)

// Builder turns inventory plus telemetry into a solver problem.
type Builder struct {
	radioCfg    config.RadioConfig
	policy      Policy
	tickSeconds float64
	staleAfter  time.Duration
}

// NewBuilder returns a builder configured from the daemon's config.
func NewBuilder(cfg config.Config) *Builder {
	return &Builder{
		radioCfg: cfg.Radio,
		policy: Policy{
			HandoffGainThreshold:  cfg.Scheduler.HandoffGainThreshold,
			MinDwellSeconds:       cfg.Scheduler.MinDwellSeconds,
			Stickiness:            cfg.Scheduler.Stickiness,
			ReserveCommittedFirst: cfg.Scheduler.ReserveCommittedFirst,
		},
		tickSeconds: cfg.Scheduler.TickSeconds,
		staleAfter: time.Duration(cfg.Radio.TelemetryStaleSeconds *
			float64(time.Second)),
	}
}

// Build assembles the problem. The returned notes are operator-facing
// explanations of terminals that were left out, which is information you want
// when a subscriber says their service is down and the plan simply does not
// mention them.
func (b *Builder) Build(epoch uint64, snap registry.Snapshot, sessions []session.Session,
	now time.Time) (Problem, []string) {

	var notes []string

	p := Problem{
		Epoch:       epoch,
		GeneratedAt: now.UTC().Format(time.RFC3339),
		TickSeconds: b.tickSeconds,
		Policy:      b.policy,
	}

	// Sites, in sorted order so the problem document is byte-stable.
	siteIDs := sortedKeys(snap.Sites)
	for _, id := range siteIDs {
		site := snap.Sites[id]
		if !site.Enabled {
			continue
		}
		ps := ProblemSite{
			ID:           site.ID,
			ParentSiteID: site.ParentSiteID,
		}
		// A relay site's capacity is its trunk radio; a wired site's is its
		// circuit. Exactly one of the two is ever set.
		if site.BackhaulKind == registry.BackhaulRelay && site.BackhaulSectorID != "" {
			ps.BackhaulSectorID = site.BackhaulSectorID
			ps.RelayAchievableMbps = site.BackhaulMbps
		} else {
			ps.BackhaulMbps = site.BackhaulMbps
		}
		p.Sites = append(p.Sites, ps)
	}

	sectorIDs := sortedKeys(snap.Sectors)
	for _, id := range sectorIDs {
		sec := snap.Sectors[id]
		if !sec.Enabled {
			continue
		}
		site, ok := snap.Sites[sec.SiteID]
		if !ok || !site.Enabled {
			continue
		}
		split := sec.DLULSplit
		if split <= 0 {
			split = 0.75
		}
		p.Sectors = append(p.Sectors, ProblemSector{
			ID:            sec.ID,
			SiteID:        sec.SiteID,
			AirtimeBudget: sec.AirtimeBudget,
			DLULSplit:     split,
			MaxTerminals:  sec.MaxTerminals,
		})
	}

	// Terminals, from live sessions only. A terminal in inventory with no session
	// is not consuming airtime and must not be allocated any.
	for _, sess := range sessions {
		if !sess.Online {
			continue
		}
		term, ok := snap.Terminals[sess.TerminalID]
		if !ok {
			notes = append(notes, fmt.Sprintf(
				"terminal %s has a live session but is not in inventory; skipped",
				sess.TerminalID))
			continue
		}
		if !term.Enabled {
			notes = append(notes, fmt.Sprintf("terminal %s is disabled in inventory; skipped",
				term.ID))
			continue
		}
		sub, ok := snap.Subscribers[term.SubscriberID]
		if !ok {
			notes = append(notes, fmt.Sprintf(
				"terminal %s references unknown subscriber %s; skipped",
				term.ID, term.SubscriberID))
			continue
		}
		if sub.Suspended {
			// Suspended accounts get no allocation. The dataplane still needs to
			// know about them to drop their traffic, which it learns from the
			// session table rather than from the plan.
			notes = append(notes, fmt.Sprintf("subscriber %s is suspended; terminal %s "+
				"gets no allocation", sub.ID, term.ID))
			continue
		}
		plan, ok := snap.Plans[sub.Plan]
		if !ok {
			notes = append(notes, fmt.Sprintf(
				"subscriber %s references unknown plan %q; terminal %s skipped",
				sub.ID, sub.Plan, term.ID))
			continue
		}

		cands := b.Candidates(term, snap, sess, now)
		if len(cands) == 0 {
			notes = append(notes, fmt.Sprintf(
				"terminal %s has no sector it can reach; check obstruction, alignment "+
					"and that its sector is enabled", term.ID))
			// Still submitted, with an empty candidate list, so the solver counts
			// it as unserved rather than the control plane silently forgetting it.
		}

		pt := ProblemTerminal{
			ID:                term.ID,
			Priority:          plan.Priority,
			DemandDownMbps:    sess.DemandDownMbps,
			DemandUpMbps:      sess.DemandUpMbps,
			CeilingDownMbps:   plan.DownMbps,
			CeilingUpMbps:     plan.UpMbps,
			CommittedDownMbps: plan.CommittedDownMbps,
			CommittedUpMbps:   plan.CommittedUpMbps,
			CurrentSectorID:   sess.SectorID,
			DwellSeconds:      sess.DwellSeconds(now),
			PinnedSectorID:    term.PinnedSectorID,
			Candidates:        cands,
		}
		if pt.DemandDownMbps <= 0 {
			pt.DemandDownMbps = b.radioCfg.DefaultDemandMbps
		}
		if pt.DemandUpMbps <= 0 {
			pt.DemandUpMbps = b.radioCfg.DefaultDemandMbps / 8
		}
		p.Terminals = append(p.Terminals, pt)
	}

	sort.Slice(p.Terminals, func(i, j int) bool { return p.Terminals[i].ID < p.Terminals[j].ID })
	return p, notes
}

// Candidates returns every sector the terminal could associate with.
//
// Three filters, in increasing cost order: range, then horizontal beam, then
// vertical beam. The vertical check is the one people forget, and it is why a
// subscriber directly beneath a downtilted tower gets no service while the map
// says they are 200 metres from it.
func (b *Builder) Candidates(term registry.Terminal, snap registry.Snapshot,
	sess session.Session, now time.Time) []Candidate {

	measured := make(map[string]session.SectorObservation)
	if sess.TelemetryFresh(now, b.staleAfter) {
		for _, obs := range sess.Telemetry.Visible {
			measured[obs.SectorID] = obs
		}
	}

	var out []Candidate
	for _, id := range sortedKeys(snap.Sectors) {
		sec := snap.Sectors[id]
		if !sec.Enabled {
			continue
		}
		site, ok := snap.Sites[sec.SiteID]
		if !ok || !site.Enabled {
			continue
		}

		distKm := site.Position.DistanceKm(term.Position)
		if sec.MaxRangeKm > 0 && distKm > sec.MaxRangeKm {
			continue
		}

		bearing := site.Position.BearingDeg(term.Position)
		if sec.BeamwidthDeg > 0 && sec.BeamwidthDeg < 360 {
			if radio.AngleDiffDeg(bearing, sec.AzimuthDeg) > sec.BeamwidthDeg/2 {
				continue
			}
		}

		if sec.VBeamwidthDeg > 0 {
			// The sector's boresight points DowntiltDeg below horizontal, so the
			// expected elevation to a terminal is the negative of the downtilt.
			elev := site.Position.ElevationAngleDeg(term.Position)
			if radio.AngleDiffDeg(elev, -sec.DowntiltDeg) > sec.VBeamwidthDeg/2 {
				continue
			}
		}

		efficiency := sec.ProtocolEfficiency
		if efficiency <= 0 {
			efficiency = b.radioCfg.ProtocolEfficiency
		}
		sectorNF := sec.NoiseFigureDB
		if sectorNF <= 0 {
			sectorNF = b.radioCfg.NoiseFigureDB
		}
		termNF := term.NoiseFigureDB
		if termNF <= 0 {
			termNF = b.radioCfg.NoiseFigureDB
		}

		// Downlink: the sector transmits, the terminal receives.
		down := radio.LinkBudget{
			TxPowerDBm:      sec.TxPowerDBm,
			TxGainDBi:       sec.AntennaGainDBi,
			RxGainDBi:       term.AntennaGainDBi,
			FreqMHz:         sec.FreqMHz,
			ChannelWidthMHz: sec.ChannelWidthMHz,
			DistanceKm:      distKm,
			NoiseFigureDB:   termNF,
			FeederLossDB:    sec.FeederLossDB + term.FeederLossDB,
			ClutterLossDB:   sec.ClutterLossDB,
			FadeMarginDB:    sec.FadeMarginDB,
			InterferenceDBm: sec.InterferenceDBm,
		}
		downSINR := down.SINRdB()

		// Uplink: the terminal transmits, usually at much lower power, which is
		// why uplink is the direction that fails first on a long link.
		txPower := term.TxPowerDBm
		if txPower == 0 {
			txPower = 23
		}
		up := radio.LinkBudget{
			TxPowerDBm:      txPower,
			TxGainDBi:       term.AntennaGainDBi,
			RxGainDBi:       sec.AntennaGainDBi,
			FreqMHz:         sec.FreqMHz,
			ChannelWidthMHz: sec.ChannelWidthMHz,
			DistanceKm:      distKm,
			NoiseFigureDB:   sectorNF,
			FeederLossDB:    sec.FeederLossDB + term.FeederLossDB,
			ClutterLossDB:   sec.ClutterLossDB,
			FadeMarginDB:    sec.FadeMarginDB,
			InterferenceDBm: sec.InterferenceDBm,
		}
		upSINR := up.SINRdB()

		isMeasured := false
		if obs, ok := measured[sec.ID]; ok {
			// A real report beats the model. Shift the uplink estimate by the same
			// error, since whatever the model got wrong about this path (foliage,
			// a mis-set clutter figure, an antenna pointed slightly off) applies
			// in both directions.
			delta := obs.SINRdB - downSINR
			downSINR = obs.SINRdB
			upSINR += delta
			isMeasured = true
		}

		downMbps := radio.AchievableMbps(downSINR, sec.ChannelWidthMHz, efficiency)
		upMbps := radio.AchievableMbps(upSINR, sec.ChannelWidthMHz, efficiency)
		if downMbps <= 0 {
			continue
		}

		mcsName := ""
		if mcs, ok := radio.SelectMCS(downSINR); ok {
			mcsName = mcs.Name
		}

		out = append(out, Candidate{
			SectorID:           sec.ID,
			AchievableDownMbps: round2(downMbps),
			AchievableUpMbps:   round2(upMbps),
			SINRdB:             round2(downSINR),
			MCS:                mcsName,
			Measured:           isMeasured,
		})
	}

	// Best first, ties broken by ID so the document is stable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].AchievableDownMbps != out[j].AchievableDownMbps {
			return out[i].AchievableDownMbps > out[j].AchievableDownMbps
		}
		return out[i].SectorID < out[j].SectorID
	})
	return out
}

// round2 trims float noise so a problem document does not churn between ticks
// purely from the eighteenth decimal place.
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
