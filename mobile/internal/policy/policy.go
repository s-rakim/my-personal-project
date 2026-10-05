// Package policy decides which link carries traffic right now.
//
// The decision is the same shape as the one the airtime scheduler makes for a
// subscriber choosing a sector, and it is made the same way: score the
// candidates, favour the incumbent, and refuse to move for a marginal gain. A
// vehicle crossing between corridor relays and a terminal reassigned between
// sectors are the same problem, and flapping is the same failure in both.
//
// What differs is that the candidates here cost different amounts of money. A
// link that is twice as fast and billed by the gigabyte is usually the wrong
// choice for a background sync and the right choice for a video call, so the
// policy is evaluated per traffic class rather than once for the whole device.
package policy

import (
	"fmt"
	"sort"
	"time"

	"github.com/s-rakim/my-personal-project/mobile/internal/link"
)

// Class is a category of traffic, distinguished by what it can tolerate.
type Class string

const (
	// ClassLive must go now over whatever is available: calls, navigation,
	// messaging, a page somebody is waiting on. Cost is secondary to working.
	ClassLive Class = "live"

	// ClassBulk can wait for a cheap link: podcast and music sync, map tiles,
	// photo backup, package updates, offline course content. On a school uplink
	// this is most of the volume, and deferring it is what leaves the expensive
	// link free for the traffic somebody is waiting on.
	ClassBulk Class = "bulk"

	// ClassIdle can wait indefinitely for a free, fast link: full system images,
	// media libraries, archives. Never worth metered bandwidth.
	ClassIdle Class = "idle"
)

// Policy is the tuning for link selection.
type Policy struct {
	// SwitchGainThreshold is how much better a rival link must score before the
	// current one is given up. At 1.0 the device chases every fluctuation and
	// spends its time re-establishing connections instead of using them.
	SwitchGainThreshold float64 `json:"switch_gain_threshold"`

	// MinDwell pins a freshly selected link in place. The other half of flap
	// protection, and the one that matters when two links score nearly equally,
	// which is the normal case at the edge of corridor coverage.
	MinDwell time.Duration `json:"min_dwell"`

	// Stickiness is the bonus the current link receives, expressing that an
	// established connection is worth more than its raw bitrate: switching costs
	// a DHCP round, a NAT rebind, and every live TCP session.
	Stickiness float64 `json:"stickiness"`

	// CostWeight scales how strongly money discourages a link. At 0 the policy
	// ignores cost and behaves like ordinary failover; raising it makes the
	// engine hold out for free links. Applies to ClassBulk and ClassIdle only:
	// live traffic is never refused over cost.
	CostWeight float64 `json:"cost_weight"`

	// BulkMinMbps is the floor below which a link is not worth draining bulk
	// over, even when free. A 1 Mbps link technically works and will take eleven
	// days to move a gigabyte, holding the queue hostage the whole time.
	BulkMinMbps float64 `json:"bulk_min_mbps"`

	// ReserveCapFraction keeps part of a metered link's monthly allowance for
	// live traffic. Without it a background sync can spend the whole cap on the
	// first of the month and leave nothing for the calls that actually needed it.
	ReserveCapFraction float64 `json:"reserve_cap_fraction"`
}

// Default returns settings that behave sensibly on a vehicle or a school uplink.
func Default() Policy {
	return Policy{
		SwitchGainThreshold: 1.3,
		MinDwell:            20 * time.Second,
		Stickiness:          0.25,
		CostWeight:          1.0,
		BulkMinMbps:         5.0,
		ReserveCapFraction:  0.25,
	}
}

// Validate rejects settings that would misbehave.
func (p Policy) Validate() error {
	if p.SwitchGainThreshold < 1 {
		return fmt.Errorf("policy: switch_gain_threshold must be at least 1.0; "+
			"below that the engine would abandon a working link for a worse one, "+
			"got %.2f", p.SwitchGainThreshold)
	}
	// Hysteresis has to come from somewhere. Any one of the three mechanisms is
	// enough, but with all of them off the engine re-selects on every
	// measurement, and at the edge of corridor coverage that means switching
	// several times a minute and losing every live session each time.
	if p.SwitchGainThreshold <= 1 && p.Stickiness <= 0 && p.MinDwell <= 0 {
		return fmt.Errorf("policy: no hysteresis configured. Set at least one of " +
			"switch_gain_threshold above 1.0, a positive stickiness, or a non-zero " +
			"min_dwell, or links will flap continuously")
	}
	if p.ReserveCapFraction < 0 || p.ReserveCapFraction >= 1 {
		return fmt.Errorf("policy: reserve_cap_fraction must be in [0, 1); got %.2f",
			p.ReserveCapFraction)
	}
	if p.Stickiness < 0 {
		return fmt.Errorf("policy: stickiness cannot be negative")
	}
	return nil
}

// Decision is the engine's answer for one traffic class.
type Decision struct {
	Class Class `json:"class"`

	// Link is the chosen link's name, empty when nothing is suitable.
	Link string `json:"link,omitempty"`

	// Score is what the winner scored, for comparison against the runners-up.
	Score float64 `json:"score"`

	// Changed reports whether this differs from the previous decision.
	Changed bool `json:"changed"`

	// Reason explains the decision in a line an operator can act on. The point
	// of carrying it is that "why is this slow" and "why is my data gone" are
	// the two questions this system exists to answer.
	Reason string `json:"reason"`

	// Candidates is every link considered and what it scored, worst to best.
	Candidates []Candidate `json:"candidates"`
}

// Candidate is one link's evaluation.
type Candidate struct {
	Link     string  `json:"link"`
	Score    float64 `json:"score"`
	Eligible bool    `json:"eligible"`
	Why      string  `json:"why,omitempty"`
}

// Engine makes and remembers decisions.
type Engine struct {
	policy  Policy
	current map[Class]string
	since   map[Class]time.Time
}

// New returns an engine with no history.
func New(p Policy) (*Engine, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Engine{
		policy:  p,
		current: make(map[Class]string),
		since:   make(map[Class]time.Time),
	}, nil
}

// Current returns the link presently selected for a class.
func (e *Engine) Current(c Class) string { return e.current[c] }

// Decide picks the link for one traffic class.
func (e *Engine) Decide(c Class, states []link.State, now time.Time) Decision {
	d := Decision{Class: c}

	var best *link.State
	bestScore := 0.0

	for i := range states {
		s := states[i]
		eligible, why := e.eligible(c, s)
		score := 0.0
		if eligible {
			score = e.score(c, s, s.Config.Name == e.current[c])
		}
		d.Candidates = append(d.Candidates, Candidate{
			Link: s.Config.Name, Score: score, Eligible: eligible, Why: why,
		})
		if eligible && score > bestScore {
			bestScore = score
			best = &states[i]
		}
	}

	sort.Slice(d.Candidates, func(i, j int) bool {
		if d.Candidates[i].Score != d.Candidates[j].Score {
			return d.Candidates[i].Score > d.Candidates[j].Score
		}
		return d.Candidates[i].Link < d.Candidates[j].Link
	})

	currentName := e.current[c]

	if best == nil {
		if currentName != "" {
			e.current[c] = ""
			d.Changed = true
		}
		d.Reason = "no link is eligible for " + string(c) + " traffic"
		return d
	}

	// Hold the incumbent unless the challenger clears both the dwell time and
	// the gain threshold.
	if currentName != "" && currentName != best.Config.Name {
		var currentScore float64
		var currentUsable bool
		for _, s := range states {
			if s.Config.Name != currentName {
				continue
			}
			if ok, _ := e.eligible(c, s); ok {
				currentUsable = true
				currentScore = e.score(c, s, true)
			}
		}

		if currentUsable {
			held := now.Sub(e.since[c])
			if held < e.policy.MinDwell {
				d.Link = currentName
				d.Score = currentScore
				d.Reason = fmt.Sprintf("holding %s: selected %.0fs ago, minimum dwell is %.0fs",
					currentName, held.Seconds(), e.policy.MinDwell.Seconds())
				return d
			}
			if bestScore <= currentScore*e.policy.SwitchGainThreshold {
				d.Link = currentName
				d.Score = currentScore
				d.Reason = fmt.Sprintf(
					"holding %s: %s scores %.2f against %.2f, short of the %.2fx needed to switch",
					currentName, best.Config.Name, bestScore, currentScore,
					e.policy.SwitchGainThreshold)
				return d
			}
		}
	}

	d.Link = best.Config.Name
	d.Score = bestScore
	d.Changed = currentName != best.Config.Name

	if d.Changed {
		e.current[c] = best.Config.Name
		e.since[c] = now
		if currentName == "" {
			d.Reason = fmt.Sprintf("selected %s at %.0f Mbps",
				best.Config.Name, best.ThroughputMbps)
		} else {
			d.Reason = fmt.Sprintf("switched from %s to %s (%.0f Mbps, %s)",
				currentName, best.Config.Name, best.ThroughputMbps,
				costLabel(best.Config))
		}
	} else {
		// Name the runner-up when it is faster on paper. Otherwise an operator
		// reading "staying on corridor" cannot tell whether the faster link was
		// rejected deliberately or never seen.
		d.Reason = fmt.Sprintf("staying on %s at %.0f Mbps",
			best.Config.Name, best.ThroughputMbps)
		if rival, ok := fasterRival(states, best); ok {
			d.Reason += fmt.Sprintf("; %s is faster at %.0f Mbps but does not clear "+
				"the %.2fx switch threshold once stickiness is applied",
				rival.Config.Name, rival.ThroughputMbps, e.policy.SwitchGainThreshold)
		}
	}
	return d
}

// eligible reports whether a link may carry a class at all, and why not.
//
// Eligibility is separate from scoring because the reasons are categorical: a
// link is not merely a poor choice for bulk traffic when it is metered and
// nearly out of allowance, it is the wrong choice at any speed.
func (e *Engine) eligible(c Class, s link.State) (bool, string) {
	if !s.Usable() {
		if !s.Up {
			return false, "down"
		}
		if s.LossPct >= 30 {
			return false, fmt.Sprintf("%.0f%% packet loss", s.LossPct)
		}
		return false, "no measured throughput"
	}

	metered := s.Config.Kind.Metered() || s.Config.CostPerGB > 0

	if s.CapExhausted() {
		// Live traffic still goes: a throttled or billed link beats no link when
		// somebody is waiting. Background transfers do not get that excuse.
		if c == ClassLive {
			return true, "over monthly cap, allowed for live traffic only"
		}
		return false, fmt.Sprintf("monthly cap spent (%.1f of %.1f GB)",
			s.UsedGBThisMonth, s.Config.MonthlyCapGB)
	}

	switch c {
	case ClassLive:
		return true, ""

	case ClassBulk:
		if metered {
			if s.Config.MonthlyCapGB <= 0 {
				return false, "metered with no cap set; refusing bulk traffic over it"
			}
			// Keep a reserve so a sync cannot spend the allowance that live
			// traffic will need later in the month.
			budget := s.Config.MonthlyCapGB * (1 - e.policy.ReserveCapFraction)
			if s.UsedGBThisMonth >= budget {
				return false, fmt.Sprintf(
					"metered, and the %.0f%% reserve for live traffic is reached (%.1f of %.1f GB)",
					e.policy.ReserveCapFraction*100, s.UsedGBThisMonth, budget)
			}
		}
		if s.ThroughputMbps < e.policy.BulkMinMbps {
			return false, fmt.Sprintf("%.1f Mbps is below the %.1f Mbps bulk floor",
				s.ThroughputMbps, e.policy.BulkMinMbps)
		}
		return true, ""

	case ClassIdle:
		if metered {
			return false, "metered; idle traffic waits for a free link"
		}
		if s.ThroughputMbps < e.policy.BulkMinMbps {
			return false, fmt.Sprintf("%.1f Mbps is below the %.1f Mbps bulk floor",
				s.ThroughputMbps, e.policy.BulkMinMbps)
		}
		return true, ""
	}
	return false, "unknown traffic class"
}

// score ranks an eligible link. Higher is better.
func (e *Engine) score(c Class, s link.State, incumbent bool) float64 {
	score := s.ThroughputMbps

	// Loss hurts more than its percentage suggests, because TCP responds to it
	// by backing off: a link losing 10% delivers far less than 90% of its rate.
	score *= (1 - s.LossPct/100) * (1 - s.LossPct/100)

	// Latency matters for live traffic and barely at all for a background sync.
	if c == ClassLive && s.LatencyMs > 0 {
		score *= 100 / (100 + s.LatencyMs)
	}

	// Cost discourages metered links for anything deferrable. Live traffic is
	// never refused over money.
	if c != ClassLive && e.policy.CostWeight > 0 && s.Config.CostPerGB > 0 {
		score /= 1 + e.policy.CostWeight*s.Config.CostPerGB
	}

	// A link that keeps dropping and returning is worth less than its current
	// numbers suggest, since each flap costs every live session on it.
	if s.FlapCount > 0 {
		score /= 1 + float64(s.FlapCount)/10
	}

	if incumbent {
		score *= 1 + e.policy.Stickiness
	}
	score *= 1 + float64(s.Config.Priority)/100
	return score
}

// fasterRival finds an eligible link with higher raw throughput than the
// chosen one, for explaining why it was not taken.
func fasterRival(states []link.State, chosen *link.State) (link.State, bool) {
	var best link.State
	found := false
	for _, s := range states {
		if s.Config.Name == chosen.Config.Name || !s.Usable() {
			continue
		}
		if s.ThroughputMbps > chosen.ThroughputMbps &&
			(!found || s.ThroughputMbps > best.ThroughputMbps) {
			best = s
			found = true
		}
	}
	return best, found
}

func costLabel(c link.Config) string {
	if c.CostPerGB <= 0 {
		return "free"
	}
	return fmt.Sprintf("%.2f per GB", c.CostPerGB)
}
