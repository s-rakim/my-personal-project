package radio

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestLinkBudgetConformance checks this package against the shared fixture that
// the Python planner is also tested against.
//
// The point is not that the arithmetic is hard. It is that a planning tool which
// disagrees with the live scheduler is worse than no planning tool: it produces
// confident capacity figures the network will never honour, and the discrepancy
// only shows up as unhappy subscribers months later.
func TestLinkBudgetConformance(t *testing.T) {
	path := filepath.Join("..", "..", "..", "schema", "fixtures", "linkbudget-cases.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var doc struct {
		Tolerance float64 `json:"tolerance"`
		Cases     []struct {
			Name  string `json:"name"`
			Input struct {
				TxPowerDBm      float64 `json:"tx_power_dbm"`
				TxGainDBi       float64 `json:"tx_gain_dbi"`
				RxGainDBi       float64 `json:"rx_gain_dbi"`
				FreqMHz         float64 `json:"freq_mhz"`
				ChannelWidthMHz float64 `json:"channel_width_mhz"`
				DistanceKm      float64 `json:"distance_km"`
				NoiseFigureDB   float64 `json:"noise_figure_db"`
				FeederLossDB    float64 `json:"feeder_loss_db"`
				ClutterLossDB   float64 `json:"clutter_loss_db"`
				FadeMarginDB    float64 `json:"fade_margin_db"`
				InterferenceDBm float64 `json:"interference_dbm"`
			} `json:"input"`
			Expect struct {
				EIRPdBm         float64 `json:"eirp_dbm"`
				RxPowerDBm      float64 `json:"rx_power_dbm"`
				SINRdB          float64 `json:"sinr_db"`
				MCS             string  `json:"mcs"`
				AchievableMbps  float64 `json:"achievable_mbps"`
			} `json:"expect"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("fixture holds no cases")
	}

	tol := doc.Tolerance
	if tol <= 0 {
		tol = 1e-6
	}

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			b := LinkBudget{
				TxPowerDBm:      c.Input.TxPowerDBm,
				TxGainDBi:       c.Input.TxGainDBi,
				RxGainDBi:       c.Input.RxGainDBi,
				FreqMHz:         c.Input.FreqMHz,
				ChannelWidthMHz: c.Input.ChannelWidthMHz,
				DistanceKm:      c.Input.DistanceKm,
				NoiseFigureDB:   c.Input.NoiseFigureDB,
				FeederLossDB:    c.Input.FeederLossDB,
				ClutterLossDB:   c.Input.ClutterLossDB,
				FadeMarginDB:    c.Input.FadeMarginDB,
				InterferenceDBm: c.Input.InterferenceDBm,
			}

			check := func(label string, got, want float64) {
				if math.Abs(got-want) > tol {
					t.Errorf("%s = %.9f, fixture says %.9f (difference %.2e)",
						label, got, want, math.Abs(got-want))
				}
			}
			check("EIRPdBm", b.EIRPdBm(), c.Expect.EIRPdBm)
			check("PredictedRxPowerDBm", b.PredictedRxPowerDBm(), c.Expect.RxPowerDBm)
			check("SINRdB", b.SINRdB(), c.Expect.SINRdB)
			check("AchievableMbps",
				AchievableMbps(b.SINRdB(), b.ChannelWidthMHz, ProtocolEfficiency),
				c.Expect.AchievableMbps)

			name := ""
			if mcs, ok := SelectMCS(b.SINRdB()); ok {
				name = mcs.Name
			}
			if name != c.Expect.MCS {
				t.Errorf("MCS = %q, fixture says %q", name, c.Expect.MCS)
			}
		})
	}
}

// TestAirtimeIsNotBandwidth is the one property the whole design rests on: a
// slower link costs proportionally more airtime for the same payload.
func TestAirtimeIsNotBandwidth(t *testing.T) {
	fast := AirtimeFraction(10, 200)
	slow := AirtimeFraction(10, 25)

	if slow <= fast {
		t.Fatalf("a slower link must cost more airtime: fast=%v slow=%v", fast, slow)
	}
	if math.Abs(slow/fast-8) > 1e-9 {
		t.Errorf("a link 8x slower should cost 8x the airtime, got %.6fx", slow/fast)
	}
	if got := AirtimeFraction(10, 0); !math.IsInf(got, 1) {
		t.Errorf("a dead link must cost infinite airtime so it loses every comparison, got %v", got)
	}
}

// TestGeodesicRoundTrip checks Destination against Distance and Bearing, which is
// what the seed fixtures rely on to place subscribers inside a real beam.
func TestGeodesicRoundTrip(t *testing.T) {
	start := Point{Lat: 44.9412, Lon: -93.0998, HeightM: 42}

	for _, bearing := range []float64{0, 45, 90, 180, 270, 359} {
		for _, dist := range []float64{0.5, 5, 50} {
			end := start.Destination(bearing, dist)

			if got := start.DistanceKm(end); math.Abs(got-dist) > 1e-6 {
				t.Errorf("bearing %.0f, %.1f km: round-trip distance %.9f", bearing, dist, got)
			}
			gotBearing := start.BearingDeg(end)
			if d := AngleDiffDeg(gotBearing, bearing); d > 1e-6 {
				t.Errorf("bearing %.0f, %.1f km: round-trip bearing %.9f (off by %.2e)",
					bearing, dist, gotBearing, d)
			}
		}
	}
}
