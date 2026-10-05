package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/s-rakim/my-personal-project/mobile/internal/link"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func state(name string, kind link.Kind, mbps, costPerGB, capGB, usedGB float64) link.State {
	return link.State{
		Config: link.Config{
			Name: name, Kind: kind, Interface: "wan-" + name,
			CostPerGB: costPerGB, MonthlyCapGB: capGB, Enabled: true,
		},
		Up: true, ThroughputMbps: mbps, UsedGBThisMonth: usedGB,
	}
}

func engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// The central claim: a background sync must not burn metered data when a free
// link is available, even when the metered one is faster.
func TestBulkPrefersFreeLinkOverFasterMeteredOne(t *testing.T) {
	e := engine(t)
	links := []link.State{
		state("corridor", link.KindCorridor, 25, 0, 0, 0),
		state("cell", link.KindCellular, 150, 8.00, 50, 0),
	}

	d := e.Decide(ClassBulk, links, now)
	if d.Link != "corridor" {
		t.Fatalf("bulk chose %q at %.0f Mbps; it should prefer the free link. Reason: %s",
			d.Link, d.Score, d.Reason)
	}
}

// And the converse: when somebody is waiting, speed wins and cost does not veto.
func TestLiveTakesTheFastLinkEvenWhenMetered(t *testing.T) {
	e := engine(t)
	links := []link.State{
		state("corridor", link.KindCorridor, 25, 0, 0, 0),
		state("cell", link.KindCellular, 150, 8.00, 50, 0),
	}

	d := e.Decide(ClassLive, links, now)
	if d.Link != "cell" {
		t.Fatalf("live chose %q; it should take the faster link regardless of cost", d.Link)
	}
}

func TestLiveWorksWhenOnlyAMeteredLinkExists(t *testing.T) {
	e := engine(t)
	links := []link.State{state("cell", link.KindCellular, 40, 8.00, 50, 0)}

	if d := e.Decide(ClassLive, links, now); d.Link != "cell" {
		t.Fatalf("live got %q with reason %q; it must use what is available", d.Link, d.Reason)
	}
}

// The reserve exists so a sync early in the month cannot spend the allowance
// that calls will need later.
func TestCapReserveProtectsLiveTrafficFromBulk(t *testing.T) {
	e := engine(t)
	// 40 of 50 GB used; the default 25% reserve puts the bulk budget at 37.5.
	links := []link.State{state("cell", link.KindCellular, 100, 8.00, 50, 40)}

	bulk := e.Decide(ClassBulk, links, now)
	if bulk.Link != "" {
		t.Errorf("bulk was allowed onto %q past the reserve", bulk.Link)
	}
	if !strings.Contains(bulk.Reason, "no link is eligible") {
		t.Errorf("reason should say nothing is eligible, got %q", bulk.Reason)
	}

	if live := e.Decide(ClassLive, links, now); live.Link != "cell" {
		t.Errorf("live must still pass once the reserve is reached, got %q", live.Link)
	}
}

func TestExhaustedCapStillCarriesLiveTraffic(t *testing.T) {
	e := engine(t)
	links := []link.State{state("cell", link.KindCellular, 20, 8.00, 50, 60)}

	if d := e.Decide(ClassLive, links, now); d.Link != "cell" {
		t.Errorf("a throttled link beats no link for live traffic, got %q", d.Link)
	}
	if d := e.Decide(ClassBulk, links, now); d.Link != "" {
		t.Errorf("bulk must not run past an exhausted cap, got %q", d.Link)
	}
}

func TestIdleNeverUsesAMeteredLink(t *testing.T) {
	e := engine(t)
	links := []link.State{state("cell", link.KindCellular, 200, 8.00, 500, 0)}

	d := e.Decide(ClassIdle, links, now)
	if d.Link != "" {
		t.Fatalf("idle traffic took metered link %q; it should wait", d.Link)
	}
	var found bool
	for _, c := range d.Candidates {
		if c.Link == "cell" && strings.Contains(c.Why, "metered") {
			found = true
		}
	}
	if !found {
		t.Errorf("the candidate list should explain the refusal: %+v", d.Candidates)
	}
}

func TestSlowFreeLinkIsRefusedForBulk(t *testing.T) {
	e := engine(t)
	// 1 Mbps is free but would hold the queue for days.
	links := []link.State{state("corridor", link.KindCorridor, 1, 0, 0, 0)}

	if d := e.Decide(ClassBulk, links, now); d.Link != "" {
		t.Fatalf("bulk accepted a %0.f Mbps link, below the floor", 1.0)
	}
}

func TestMinDwellHoldsAFreshlySelectedLink(t *testing.T) {
	e := engine(t)
	slow := []link.State{state("corridor", link.KindCorridor, 20, 0, 0, 0)}
	e.Decide(ClassLive, slow, now)

	// A far better link appears five seconds later, inside the dwell window.
	better := []link.State{
		state("corridor", link.KindCorridor, 20, 0, 0, 0),
		state("wifi", link.KindWiFi, 400, 0, 0, 0),
	}
	d := e.Decide(ClassLive, better, now.Add(5*time.Second))
	if d.Link != "corridor" {
		t.Fatalf("switched to %q inside the dwell window", d.Link)
	}
	if !strings.Contains(d.Reason, "dwell") {
		t.Errorf("reason should cite dwell, got %q", d.Reason)
	}

	// Past the dwell window it should move.
	d = e.Decide(ClassLive, better, now.Add(time.Minute))
	if d.Link != "wifi" {
		t.Fatalf("failed to switch after dwell expired, got %q (%s)", d.Link, d.Reason)
	}
	if !d.Changed {
		t.Error("the switch should be reported as a change")
	}
}

func TestMarginalGainDoesNotCauseASwitch(t *testing.T) {
	e := engine(t)
	links := []link.State{state("corridor", link.KindCorridor, 50, 0, 0, 0)}
	e.Decide(ClassLive, links, now)

	// A link 20% faster, well past the dwell window. Stickiness plus the 1.3x
	// threshold should hold the incumbent.
	rival := []link.State{
		state("corridor", link.KindCorridor, 50, 0, 0, 0),
		state("other", link.KindCorridor, 60, 0, 0, 0),
	}
	d := e.Decide(ClassLive, rival, now.Add(time.Hour))
	if d.Link != "corridor" {
		t.Fatalf("flapped to %q for a 20%% gain", d.Link)
	}
	// The reason must name the faster link it passed over and why, otherwise an
	// operator cannot tell a deliberate hold from a link that was never seen.
	if !strings.Contains(d.Reason, "threshold") || !strings.Contains(d.Reason, "other") {
		t.Errorf("reason should name the rejected rival and the threshold, got %q", d.Reason)
	}
}

func TestLossyLinkLosesToASlowerCleanOne(t *testing.T) {
	e := engine(t)
	lossy := state("corridor", link.KindCorridor, 100, 0, 0, 0)
	lossy.LossPct = 25 // survives the usability floor, but barely delivers

	clean := state("wifi", link.KindWiFi, 60, 0, 0, 0)

	d := e.Decide(ClassBulk, []link.State{lossy, clean}, now)
	if d.Link != "wifi" {
		t.Fatalf("chose the lossy link %q; loss should outweigh raw rate", d.Link)
	}
}

func TestUnusableLinksAreExcludedWithAReason(t *testing.T) {
	e := engine(t)
	down := state("corridor", link.KindCorridor, 0, 0, 0, 0)
	down.Up = false

	shredded := state("cell", link.KindCellular, 50, 8.00, 100, 0)
	shredded.LossPct = 60

	d := e.Decide(ClassLive, []link.State{down, shredded}, now)
	if d.Link != "" {
		t.Fatalf("selected unusable link %q", d.Link)
	}
	for _, c := range d.Candidates {
		if c.Eligible {
			t.Errorf("%s should not be eligible", c.Link)
		}
		if c.Why == "" {
			t.Errorf("%s was excluded without a reason", c.Link)
		}
	}
}

// Every decision has to be explainable. "Why is this slow" and "why is my data
// gone" are the two questions this exists to answer.
func TestEveryDecisionCarriesAReason(t *testing.T) {
	e := engine(t)
	links := []link.State{
		state("corridor", link.KindCorridor, 25, 0, 0, 0),
		state("cell", link.KindCellular, 150, 8.00, 50, 48),
	}
	for _, c := range []Class{ClassLive, ClassBulk, ClassIdle} {
		d := e.Decide(c, links, now)
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s decision has no reason", c)
		}
		if len(d.Candidates) != len(links) {
			t.Errorf("%s evaluated %d candidates, expected %d",
				c, len(d.Candidates), len(links))
		}
	}
}

func TestPolicyRejectsFlapProneSettings(t *testing.T) {
	// Below 1.0 is nonsensical: it abandons a working link for a worse one.
	bad := Default()
	bad.SwitchGainThreshold = 0.8
	if _, err := New(bad); err == nil {
		t.Error("a threshold below 1.0 should be rejected")
	}

	// Exactly 1.0 is allowed as long as hysteresis comes from somewhere else.
	ok := Default()
	ok.SwitchGainThreshold = 1.0
	if _, err := New(ok); err != nil {
		t.Errorf("1.0 with stickiness and dwell set should be accepted: %v", err)
	}

	// All three mechanisms off means it flaps on every measurement.
	none := Default()
	none.SwitchGainThreshold = 1.0
	none.Stickiness = 0
	none.MinDwell = 0
	if _, err := New(none); err == nil {
		t.Error("a policy with no hysteresis at all should be rejected")
	}

	bad = Default()
	bad.ReserveCapFraction = 1.0
	if _, err := New(bad); err == nil {
		t.Error("a reserve of the entire cap should be rejected")
	}
}

func TestMeteredLinkWithNoCapRefusesBulk(t *testing.T) {
	e := engine(t)
	// No cap means no way to bound the spend, so bulk must not run over it.
	links := []link.State{state("cell", link.KindCellular, 200, 8.00, 0, 0)}

	if d := e.Decide(ClassBulk, links, now); d.Link != "" {
		t.Fatalf("bulk ran over an uncapped metered link %q", d.Link)
	}
}
