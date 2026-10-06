package budget

import (
	"testing"
	"time"
)

var plans = []Plan{
	{Name: "1GB", CapGB: 1, PriceMo: 3},
	{Name: "5GB", CapGB: 5, PriceMo: 8},
	{Name: "20GB", CapGB: 20, PriceMo: 20},
	{Name: "50GB", CapGB: 50, PriceMo: 40},
}

var t0 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

// A cumulative counter difference, not an absolute, is what consumption is. Two
// samples a known time apart project to a month.
func TestProjectsMonthlyFromCumulativeCounters(t *testing.T) {
	// 1 GB of metered traffic over one day projects to ~30 GB/month.
	samples := []Sample{
		{At: t0, Link: "cell", Metered: true, RxBytes: 0},
		{At: t0.Add(24 * time.Hour), Link: "cell", Metered: true, RxBytes: 1_000_000_000},
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if r.ProjectedMeteredGBPerMonth < 29 || r.ProjectedMeteredGBPerMonth > 31 {
		t.Errorf("projected %.1f GB/month, want ~30", r.ProjectedMeteredGBPerMonth)
	}
}

// The headline value: deferrable traffic moved to a free link drops the bundle.
func TestPrestagingDropsThePlan(t *testing.T) {
	// 18 GB/month metered, of which 15 is deferrable. Live-only is ~3 GB.
	// Without pre-staging: 18 * 1.3 = 23.4 -> needs 50 GB plan.
	// With: 3 * 1.3 = 3.9 -> fits the 5 GB plan.
	day := 24 * time.Hour
	perDay := uint64(18_000_000_000 / 30)
	deferPerDay := uint64(15_000_000_000 / 30)
	samples := []Sample{
		{At: t0, Link: "cell", Metered: true},
		{At: t0.Add(day), Link: "cell", Metered: true,
			RxBytes: perDay, DeferrableRxBytes: deferPerDay},
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if r.Recommendation.WithoutPrestaging.Name != "50GB" {
		t.Errorf("without pre-staging should need 50GB, got %q",
			r.Recommendation.WithoutPrestaging.Name)
	}
	if r.Recommendation.WithPrestaging.Name != "5GB" {
		t.Errorf("with pre-staging should fit 5GB, got %q",
			r.Recommendation.WithPrestaging.Name)
	}
	if r.Recommendation.MonthlySaving != 32 {
		t.Errorf("saving should be 40-8=32, got %.2f", r.Recommendation.MonthlySaving)
	}
}

// Free-link traffic must never count toward the metered bundle, however large.
func TestFreeTrafficDoesNotCountTowardBundle(t *testing.T) {
	day := 24 * time.Hour
	samples := []Sample{
		{At: t0, Link: "wifi", Metered: false},
		{At: t0.Add(day), Link: "wifi", Metered: false, RxBytes: 500_000_000_000},
		{At: t0, Link: "cell", Metered: true},
		{At: t0.Add(day), Link: "cell", Metered: true, RxBytes: 500_000_000},
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if r.ProjectedMeteredGBPerMonth > 16 {
		t.Errorf("500 GB of wifi leaked into the metered projection: %.1f GB",
			r.ProjectedMeteredGBPerMonth)
	}
	if r.FreeBytes != 500_000_000_000 {
		t.Errorf("free bytes wrong: %d", r.FreeBytes)
	}
}

// A counter that resets (reboot, 32-bit wrap) must not become a huge bogus delta.
func TestCounterResetIsNotCountedAsTraffic(t *testing.T) {
	samples := []Sample{
		{At: t0, Link: "cell", Metered: true, RxBytes: 4_000_000_000},
		{At: t0.Add(time.Hour), Link: "cell", Metered: true, RxBytes: 1_000}, // rebooted
		{At: t0.Add(2 * time.Hour), Link: "cell", Metered: true, RxBytes: 50_000_000},
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	// Only the post-reset 50 MB - 1 KB should count, not the 4 GB apparent drop.
	if r.MeteredBytes > 60_000_000 {
		t.Errorf("counter reset was counted as traffic: %d bytes", r.MeteredBytes)
	}
}

func TestNeedBeyondLargestPlanIsNamedNotHidden(t *testing.T) {
	day := 24 * time.Hour
	samples := []Sample{
		{At: t0, Link: "cell", Metered: true},
		{At: t0.Add(day), Link: "cell", Metered: true, RxBytes: 10_000_000_000}, // ~300 GB/mo
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	// Nothing listed covers 300 GB; the recommendation must say so rather than
	// silently returning the largest plan as if it were enough.
	if got := r.Recommendation.WithoutPrestaging.Name; got == "50GB" {
		t.Errorf("returned the 50GB plan for a 300GB need; should flag a larger plan")
	}
	if r.Recommendation.WithoutPrestaging.CapGB < 300 {
		t.Errorf("sentinel cap should reflect the real need, got %.0f",
			r.Recommendation.WithoutPrestaging.CapGB)
	}
}

func TestDeferrableIsClampedToTotal(t *testing.T) {
	// A desynchronised counter must not report more deferrable than total.
	samples := []Sample{
		{At: t0, Link: "cell", Metered: true, RxBytes: 1000, DeferrableRxBytes: 0},
		{At: t0.Add(time.Hour), Link: "cell", Metered: true,
			RxBytes: 2000, DeferrableRxBytes: 999_999}, // absurd
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if r.DeferrableOnMetered > r.MeteredBytes {
		t.Errorf("deferrable %d exceeds total %d", r.DeferrableOnMetered, r.MeteredBytes)
	}
}

func TestConfidenceGrowsWithObservation(t *testing.T) {
	cases := []struct {
		dur  time.Duration
		want string
	}{
		{2 * time.Hour, "very low"},
		{18 * time.Hour, "low"},
		{4 * 24 * time.Hour, "moderate"},
		{15 * 24 * time.Hour, "high"},
	}
	for _, c := range cases {
		got := confidence(c.dur)
		if len(got) < len(c.want) || got[:len(c.want)] != c.want {
			t.Errorf("confidence(%s) = %q, want prefix %q", c.dur, got, c.want)
		}
	}
}

func TestTooFewSamplesIsAnError(t *testing.T) {
	if _, err := Analyse([]Sample{{At: t0, Link: "cell"}}, plans); err == nil {
		t.Error("one sample should be an error: you cannot measure a difference")
	}
}

func TestInterleavedOutOfOrderSamples(t *testing.T) {
	// Samples arrive mixed across links and not in time order; grouping and
	// sorting must still produce the right windows.
	day := 24 * time.Hour
	samples := []Sample{
		{At: t0.Add(day), Link: "cell", Metered: true, RxBytes: 1_000_000_000},
		{At: t0, Link: "cell", Metered: true, RxBytes: 0},
		{At: t0.Add(day), Link: "wifi", Metered: false, RxBytes: 5_000_000_000},
		{At: t0, Link: "wifi", Metered: false, RxBytes: 0},
	}
	r, err := Analyse(samples, plans)
	if err != nil {
		t.Fatalf("Analyse: %v", err)
	}
	if r.MeteredBytes != 1_000_000_000 {
		t.Errorf("metered bytes wrong after reordering: %d", r.MeteredBytes)
	}
	if r.FreeBytes != 5_000_000_000 {
		t.Errorf("free bytes wrong after reordering: %d", r.FreeBytes)
	}
}
