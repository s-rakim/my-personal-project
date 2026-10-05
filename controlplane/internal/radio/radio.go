// Package radio models the RF link between a tower sector and a subscriber
// terminal.
//
// Three questions matter to the control plane, and this package answers all
// three:
//
//  1. Can this terminal reach this sector at all? (link budget -> SINR)
//  2. If so, how fast? (SINR -> modulation -> bits/sec)
//  3. What does that cost the sector? (bits/sec -> airtime fraction)
//
// Question 3 is the one that gets networks into trouble. A sector does not have
// "500 Mbps to share". It has one second of airtime per second. A distant
// terminal running at 1.0 bits/Hz burns eight times the airtime of a close one
// at 8.0 bits/Hz to move the same bytes. Plan capacity in bandwidth and a
// handful of far subscribers will quietly consume the whole sector.
package radio

import "math"

// NoInterference is the InterferenceDBm value meaning "nothing measured".
// It is far enough below any real thermal noise floor to vanish in the sum.
const NoInterference = -200.0

// thermalNoiseDBmPerHz is kTB expressed as dBm/Hz at 290 K.
const thermalNoiseDBmPerHz = -174.0

// earthRadiusKm is the mean Earth radius used by the haversine distance.
const earthRadiusKm = 6371.0088

// Point is a position on the Earth plus height above ground level.
type Point struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	HeightM float64 `json:"height_m"`
}

// DistanceKm is the great-circle distance between two points, ignoring height.
func (p Point) DistanceKm(q Point) float64 {
	lat1 := rad(p.Lat)
	lat2 := rad(q.Lat)
	dLat := lat2 - lat1
	dLon := rad(q.Lon - p.Lon)

	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusKm * math.Asin(math.Sqrt(math.Min(1, h)))
}

// BearingDeg is the initial compass bearing from p to q, in [0, 360).
func (p Point) BearingDeg(q Point) float64 {
	lat1 := rad(p.Lat)
	lat2 := rad(q.Lat)
	dLon := rad(q.Lon - p.Lon)

	y := math.Sin(dLon) * math.Cos(lat2)
	x := math.Cos(lat1)*math.Sin(lat2) - math.Sin(lat1)*math.Cos(lat2)*math.Cos(dLon)
	return math.Mod(deg(math.Atan2(y, x))+360, 360)
}

// ElevationAngleDeg is the angle from p up to q, positive when q is higher.
// Used to check a terminal sits inside the sector's vertical beamwidth, which
// is the usual reason a link that looks fine on a map does not work: the
// subscriber is directly under the tower, below the downtilt.
func (p Point) ElevationAngleDeg(q Point) float64 {
	d := p.DistanceKm(q) * 1000
	if d < 0.1 {
		if q.HeightM > p.HeightM {
			return 90
		}
		return -90
	}
	return deg(math.Atan2(q.HeightM-p.HeightM, d))
}

// Destination returns the point reached by travelling distanceKm from p along
// bearingDeg. Height is carried over unchanged.
//
// Needed wherever coverage is reasoned about forwards rather than backwards:
// placing a test subscriber inside a sector's beam, walking a coverage arc, or
// working out where a sector's edge actually falls on the ground.
func (p Point) Destination(bearingDeg, distanceKm float64) Point {
	lat1 := rad(p.Lat)
	lon1 := rad(p.Lon)
	theta := rad(bearingDeg)
	delta := distanceKm / earthRadiusKm

	sinLat2 := math.Sin(lat1)*math.Cos(delta) + math.Cos(lat1)*math.Sin(delta)*math.Cos(theta)
	lat2 := math.Asin(math.Max(-1, math.Min(1, sinLat2)))
	lon2 := lon1 + math.Atan2(
		math.Sin(theta)*math.Sin(delta)*math.Cos(lat1),
		math.Cos(delta)-math.Sin(lat1)*sinLat2,
	)

	return Point{
		Lat: deg(lat2),
		// Normalise into [-180, 180] so a point near the antimeridian stays valid.
		Lon:     math.Mod(deg(lon2)+540, 360) - 180,
		HeightM: p.HeightM,
	}
}

// AngleDiffDeg is the absolute difference between two bearings, in [0, 180].
func AngleDiffDeg(a, b float64) float64 {
	d := math.Mod(math.Abs(a-b), 360)
	if d > 180 {
		d = 360 - d
	}
	return d
}

// LinkBudget is everything needed to predict the SINR of one sector-to-terminal
// path. Fill it from inventory and a spectrum scan; the result is a prediction,
// not a measurement. Always prefer a real reported SINR when the terminal is
// associated and reporting one.
type LinkBudget struct {
	// TxPowerDBm is conducted power at the radio port, before antenna gain.
	// Note that regulatory limits are on EIRP (TxPowerDBm + TxGainDBi minus
	// feeder loss), not on this number.
	TxPowerDBm float64

	TxGainDBi float64
	RxGainDBi float64

	FreqMHz         float64
	ChannelWidthMHz float64
	DistanceKm      float64

	// NoiseFigureDB is the receiver's own added noise. 5-7 dB is typical for
	// commodity outdoor gear.
	NoiseFigureDB float64

	// FeederLossDB is combined cable, connector and radome loss across both
	// ends of the link.
	FeederLossDB float64

	// ClutterLossDB is excess loss over free space: foliage, buildings,
	// Fresnel zone intrusion, rain. Zero means a clean, fully cleared line of
	// sight, which is rarer than planning spreadsheets assume. Measure it by
	// comparing a real install against PredictedRxPowerDBm and keep the
	// residual per sector.
	ClutterLossDB float64

	// FadeMarginDB is held back so the link survives weather and multipath
	// rather than running at the edge of its modulation. 10 dB is a normal
	// starting point; raise it above 11 GHz where rain fade dominates.
	FadeMarginDB float64

	// InterferenceDBm is aggregate co-channel power from everything that is
	// not this link. In unlicensed spectrum this, not distance, is what
	// usually sets your capacity. Use NoInterference when unmeasured.
	InterferenceDBm float64
}

// FreeSpacePathLossDB is the ITU free-space loss for a distance and frequency.
func FreeSpacePathLossDB(distKm, freqMHz float64) float64 {
	if distKm <= 0 || freqMHz <= 0 {
		return 0
	}
	return 32.44 + 20*math.Log10(distKm) + 20*math.Log10(freqMHz)
}

// NoiseFloorDBm is the thermal noise in the channel plus the receiver's noise
// figure.
func NoiseFloorDBm(channelWidthMHz, noiseFigureDB float64) float64 {
	if channelWidthMHz <= 0 {
		return thermalNoiseDBmPerHz + noiseFigureDB
	}
	return thermalNoiseDBmPerHz + 10*math.Log10(channelWidthMHz*1e6) + noiseFigureDB
}

// EIRPdBm is the effective radiated power, which is the figure regulators cap.
func (b LinkBudget) EIRPdBm() float64 {
	return b.TxPowerDBm + b.TxGainDBi - b.FeederLossDB/2
}

// PredictedRxPowerDBm is received signal power at the terminal.
func (b LinkBudget) PredictedRxPowerDBm() float64 {
	return b.TxPowerDBm + b.TxGainDBi + b.RxGainDBi -
		FreeSpacePathLossDB(b.DistanceKm, b.FreqMHz) -
		b.FeederLossDB - b.ClutterLossDB
}

// SINRdB is the predicted signal-to-interference-plus-noise ratio, with the
// fade margin already deducted so callers plan against the bad-weather case
// rather than the brochure case.
func (b LinkBudget) SINRdB() float64 {
	noise := NoiseFloorDBm(b.ChannelWidthMHz, b.NoiseFigureDB)
	interference := b.InterferenceDBm
	if interference == 0 {
		interference = NoInterference
	}
	// Noise and interference add as powers, not as decibels.
	total := 10 * math.Log10(dbmToMilliwatt(noise)+dbmToMilliwatt(interference))
	return b.PredictedRxPowerDBm() - total - b.FadeMarginDB
}

// MCS is one modulation and coding scheme: a required SINR and the spectral
// efficiency it buys.
type MCS struct {
	Name      string  `json:"name"`
	Index     int     `json:"index"`
	MinSINRdB float64 `json:"min_sinr_db"`
	BitsPerHz float64 `json:"bits_per_hz"`
}

// mcsTable is a generic OFDM ladder. Real radios differ in both thresholds and
// efficiency; replace this with your vendor's published table and the whole
// scheduler sharpens up, because every capacity number downstream derives from
// it.
var mcsTable = []MCS{
	{Name: "BPSK 1/2", Index: 0, MinSINRdB: 2, BitsPerHz: 0.50},
	{Name: "QPSK 1/2", Index: 1, MinSINRdB: 5, BitsPerHz: 1.00},
	{Name: "QPSK 3/4", Index: 2, MinSINRdB: 8, BitsPerHz: 1.50},
	{Name: "16QAM 1/2", Index: 3, MinSINRdB: 11, BitsPerHz: 2.00},
	{Name: "16QAM 3/4", Index: 4, MinSINRdB: 15, BitsPerHz: 3.00},
	{Name: "64QAM 2/3", Index: 5, MinSINRdB: 18, BitsPerHz: 4.00},
	{Name: "64QAM 3/4", Index: 6, MinSINRdB: 20, BitsPerHz: 4.50},
	{Name: "64QAM 5/6", Index: 7, MinSINRdB: 23, BitsPerHz: 5.00},
	{Name: "256QAM 3/4", Index: 8, MinSINRdB: 26, BitsPerHz: 6.00},
	{Name: "256QAM 5/6", Index: 9, MinSINRdB: 28, BitsPerHz: 6.67},
	{Name: "1024QAM 3/4", Index: 10, MinSINRdB: 31, BitsPerHz: 7.50},
	{Name: "1024QAM 5/6", Index: 11, MinSINRdB: 34, BitsPerHz: 8.33},
}

// MinimumViableSINRdB is the SINR below which no modulation closes the link.
func MinimumViableSINRdB() float64 { return mcsTable[0].MinSINRdB }

// MCSTable returns the ladder, for operators who want to see it.
func MCSTable() []MCS {
	out := make([]MCS, len(mcsTable))
	copy(out, mcsTable)
	return out
}

// SelectMCS returns the fastest modulation the given SINR supports. ok is false
// when the link does not close at all, which is a real and common answer.
func SelectMCS(sinrDB float64) (mcs MCS, ok bool) {
	for i := len(mcsTable) - 1; i >= 0; i-- {
		if sinrDB >= mcsTable[i].MinSINRdB {
			return mcsTable[i], true
		}
	}
	return MCS{}, false
}

// ProtocolEfficiency is the fraction of raw airtime that carries payload, after
// preambles, guard intervals, block acknowledgements and TDD turnaround. 0.70
// to 0.80 matches most fixed-wireless TDD gear; measure yours and set it from
// config rather than trusting this.
const ProtocolEfficiency = 0.75

// AchievableMbps is the usable throughput of a link at a given SINR. Returns 0
// when the link does not close.
func AchievableMbps(sinrDB, channelWidthMHz, protocolEfficiency float64) float64 {
	mcs, ok := SelectMCS(sinrDB)
	if !ok || channelWidthMHz <= 0 {
		return 0
	}
	if protocolEfficiency <= 0 {
		protocolEfficiency = ProtocolEfficiency
	}
	return mcs.BitsPerHz * channelWidthMHz * protocolEfficiency
}

// AirtimeFraction is the share of a sector's airtime consumed by carrying
// demandMbps over a link whose capacity is achievableMbps.
//
// This is the function the whole scheduler is built around. Sum it across a
// sector's terminals and the total must stay under 1.0, or the sector is
// oversubscribed no matter how much backhaul sits behind it.
//
// Returns +Inf when the link cannot close, so an unreachable sector loses every
// comparison naturally instead of needing a special case.
func AirtimeFraction(demandMbps, achievableMbps float64) float64 {
	if demandMbps <= 0 {
		return 0
	}
	if achievableMbps <= 0 {
		return math.Inf(1)
	}
	return demandMbps / achievableMbps
}

// MbpsForAirtime inverts AirtimeFraction: the throughput a terminal gets when
// handed a share of airtime on a link of a given capacity.
func MbpsForAirtime(airtime, achievableMbps float64) float64 {
	if airtime <= 0 || achievableMbps <= 0 {
		return 0
	}
	return airtime * achievableMbps
}

func dbmToMilliwatt(dbm float64) float64 { return math.Pow(10, dbm/10) }

func rad(d float64) float64 { return d * math.Pi / 180 }
func deg(r float64) float64 { return r * 180 / math.Pi }
