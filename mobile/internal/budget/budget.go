// Package budget turns observed traffic into a data-plan recommendation.
//
// The question it answers is the one that actually costs money on a vehicle or a
// remote site: how large a metered bundle do you need, and how much of that need
// would disappear if the deferrable traffic were pre-staged over a free link
// instead of pulled live over the metered one.
//
// Everything here is pure arithmetic over samples. The sampling itself, which
// reads kernel counters, lives in cmd/usagewatch so this half stays testable
// without a network.
package budget

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Sample is one reading of cumulative bytes on one link.
//
// Counters are cumulative since boot, so a single sample means nothing; the
// consumption between two samples is what matters. Taking the difference also
// means a link that was never used simply shows zero, with no special case.
type Sample struct {
	At      time.Time
	Link    string
	Metered bool
	RxBytes uint64
	TxBytes uint64

	// Deferrable marks bytes that did not have to travel when they did: a
	// background sync, an update, a media prefetch. On a metered link these are
	// exactly the bytes pre-staging would have carried for free. The sampler
	// fills this from the traffic classes mobilelinkd already assigns, so the
	// estimate reflects real policy rather than a guess.
	DeferrableRxBytes uint64
}

// Window is the consumption between two samples on one link.
type Window struct {
	Link       string
	Metered    bool
	Duration   time.Duration
	Bytes      uint64
	Deferrable uint64
}

// diff computes a window between two cumulative samples, guarding against a
// counter that went backwards (a reboot, or 32-bit wrap on older hardware).
// A reset is counted as zero rather than a vast bogus delta, because one wrong
// sample must not poison a month's projection.
func diff(prev, cur Sample) Window {
	w := Window{
		Link:     cur.Link,
		Metered:  cur.Metered,
		Duration: cur.At.Sub(prev.At),
	}
	w.Bytes = delta(prev.RxBytes, cur.RxBytes) + delta(prev.TxBytes, cur.TxBytes)
	w.Deferrable = delta(prev.DeferrableRxBytes, cur.DeferrableRxBytes)
	if w.Deferrable > w.Bytes {
		// Deferrable is a subset of total; if a counter reset desynchronised the
		// two, clamp rather than report more deferrable than total.
		w.Deferrable = w.Bytes
	}
	return w
}

func delta(prev, cur uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// Report is what the watcher concludes.
type Report struct {
	Observed            time.Duration `json:"observed"`
	MeteredBytes        uint64        `json:"metered_bytes"`
	FreeBytes           uint64        `json:"free_bytes"`
	DeferrableOnMetered uint64        `json:"deferrable_on_metered_bytes"`

	// ProjectedMeteredGBPerMonth is the metered consumption scaled to 30 days at
	// the observed rate. The headline number for choosing a bundle.
	ProjectedMeteredGBPerMonth float64 `json:"projected_metered_gb_per_month"`

	// ProjectedWithPrestagingGBPerMonth is the same projection once the
	// deferrable share is assumed to move over a free link instead. The gap
	// between the two is what the cache and the deferred-transfer queue buy you.
	ProjectedWithPrestagingGBPerMonth float64 `json:"projected_with_prestaging_gb_per_month"`

	Recommendation Recommendation `json:"recommendation"`
	Confidence     string         `json:"confidence"`
}

// SavingsGBPerMonth is the monthly metered data pre-staging would remove.
func (r Report) SavingsGBPerMonth() float64 {
	s := r.ProjectedMeteredGBPerMonth - r.ProjectedWithPrestagingGBPerMonth
	if s < 0 {
		return 0
	}
	return s
}

// Plan is a bundle an operator can buy.
type Plan struct {
	Name    string  `json:"name"`
	CapGB   float64 `json:"cap_gb"`
	PriceMo float64 `json:"price_mo"`
}

// Recommendation names the smallest plan that covers projected need, with and
// without pre-staging, so the saving is expressed as a cheaper plan rather than
// an abstract number of gigabytes.
type Recommendation struct {
	WithoutPrestaging Plan    `json:"without_prestaging"`
	WithPrestaging    Plan    `json:"with_prestaging"`
	MonthlySaving     float64 `json:"monthly_saving"`
	Note              string  `json:"note"`
}

// headroom is applied to a projection before choosing a plan. A month that lands
// exactly on the cap throttles for the last day, so size for a bit more than the
// mean. 1.3 covers normal week-to-week variation without overbuying.
const headroom = 1.3

// Analyse turns a stream of samples into a report.
//
// Samples may arrive interleaved across links and out of order; they are grouped
// by link and sorted by time here, so the caller can just append readings as
// they come.
func Analyse(samples []Sample, plans []Plan) (Report, error) {
	if len(samples) < 2 {
		return Report{}, fmt.Errorf("budget: need at least two samples to measure consumption, got %d",
			len(samples))
	}

	byLink := make(map[string][]Sample)
	for _, s := range samples {
		byLink[s.Link] = append(byLink[s.Link], s)
	}

	var report Report
	var earliest, latest time.Time

	for _, group := range byLink {
		sort.Slice(group, func(i, j int) bool { return group[i].At.Before(group[j].At) })
		for i := 1; i < len(group); i++ {
			w := diff(group[i-1], group[i])
			if w.Duration <= 0 {
				continue
			}
			if w.Metered {
				report.MeteredBytes += w.Bytes
				report.DeferrableOnMetered += w.Deferrable
			} else {
				report.FreeBytes += w.Bytes
			}
		}
		if earliest.IsZero() || group[0].At.Before(earliest) {
			earliest = group[0].At
		}
		if last := group[len(group)-1].At; last.After(latest) {
			latest = last
		}
	}

	report.Observed = latest.Sub(earliest)
	if report.Observed <= 0 {
		return Report{}, fmt.Errorf("budget: samples span no time")
	}

	const gb = 1e9
	const month = 30 * 24 * time.Hour
	scale := float64(month) / float64(report.Observed)

	report.ProjectedMeteredGBPerMonth = float64(report.MeteredBytes) / gb * scale
	// Pre-staging removes the deferrable share from the metered total. What is
	// left is the genuinely live traffic that must travel when it is generated.
	liveMetered := report.MeteredBytes - report.DeferrableOnMetered
	report.ProjectedWithPrestagingGBPerMonth = float64(liveMetered) / gb * scale

	report.Recommendation = recommend(report, plans)
	report.Confidence = confidence(report.Observed)
	return report, nil
}

// recommend picks the cheapest adequate plan for each scenario.
func recommend(r Report, plans []Plan) Recommendation {
	sorted := append([]Plan(nil), plans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CapGB < sorted[j].CapGB })

	withoutP := smallestCovering(sorted, r.ProjectedMeteredGBPerMonth*headroom)
	withP := smallestCovering(sorted, r.ProjectedWithPrestagingGBPerMonth*headroom)

	rec := Recommendation{
		WithoutPrestaging: withoutP,
		WithPrestaging:    withP,
		MonthlySaving:     withoutP.PriceMo - withP.PriceMo,
	}
	switch {
	case rec.MonthlySaving > 0:
		rec.Note = fmt.Sprintf(
			"pre-staging drops you from the %s plan to the %s plan, saving %.2f per month",
			withoutP.Name, withP.Name, rec.MonthlySaving)
	case r.SavingsGBPerMonth() > 1:
		rec.Note = fmt.Sprintf(
			"pre-staging saves about %.1f GB per month but does not cross a plan boundary; "+
				"the next tier down would need %.1f GB of headroom",
			r.SavingsGBPerMonth(), r.ProjectedWithPrestagingGBPerMonth*headroom)
	default:
		rec.Note = "little deferrable traffic was seen; pre-staging would not change the plan you need"
	}
	return rec
}

// smallestCovering returns the cheapest plan whose cap meets need, or a
// synthetic "larger plan needed" entry when nothing is big enough. Returning a
// named sentinel rather than an error keeps the report renderable: "you need
// more than we listed" is itself a useful recommendation.
func smallestCovering(sortedByCap []Plan, needGB float64) Plan {
	for _, p := range sortedByCap {
		if p.CapGB >= needGB {
			return p
		}
	}
	return Plan{
		Name:  fmt.Sprintf("larger than any listed (need ~%.0f GB)", math.Ceil(needGB)),
		CapGB: math.Ceil(needGB),
	}
}

// confidence hedges the projection by how long it watched. A projection from an
// hour of data is a guess; a fortnight catches weekday and weekend both.
func confidence(observed time.Duration) string {
	switch {
	case observed >= 14*24*time.Hour:
		return "high: covers more than two weeks, including weekends"
	case observed >= 3*24*time.Hour:
		return "moderate: covers several days; a full fortnight would catch weekly peaks"
	case observed >= 12*time.Hour:
		return "low: less than a day, so it misses the daily cycle; keep sampling"
	default:
		return "very low: too short to project from; treat as a sanity check only"
	}
}
