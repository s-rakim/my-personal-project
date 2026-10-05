package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/fsutil"
	"github.com/s-rakim/my-personal-project/controlplane/internal/radio"
)

// Store is the inventory, persisted as one JSON document.
//
// A file rather than a database because a small operator's inventory is a few
// thousand records that change by hand a few times a day, and a file can be
// read, diffed, version-controlled and restored from a backup without a DBA.
// Past roughly ten thousand terminals, or once more than one controller writes
// concurrently, move this to Postgres behind the same interface.
type Store struct {
	mu   sync.RWMutex
	path string
	data document
}

type document struct {
	Version     int                   `json:"version"`
	UpdatedAt   time.Time             `json:"updated_at"`
	Plans       map[string]PlanTier   `json:"plans"`
	Sites       map[string]Site       `json:"sites"`
	Sectors     map[string]Sector     `json:"sectors"`
	Subscribers map[string]Subscriber `json:"subscribers"`
	Terminals   map[string]Terminal   `json:"terminals"`
}

const currentVersion = 1

func emptyDocument() document {
	return document{
		Version:     currentVersion,
		Plans:       make(map[string]PlanTier),
		Sites:       make(map[string]Site),
		Sectors:     make(map[string]Sector),
		Subscribers: make(map[string]Subscriber),
		Terminals:   make(map[string]Terminal),
	}
}

// Open loads the inventory, creating an empty one if the file does not exist.
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: emptyDocument()}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return s, nil
	}

	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("registry: parse %s: %w", path, err)
	}
	if doc.Version > currentVersion {
		return nil, fmt.Errorf("registry: %s was written by a newer version (%d > %d); "+
			"refusing to load it rather than silently dropping fields",
			path, doc.Version, currentVersion)
	}

	// Normalise so callers never have to nil-check a map.
	if doc.Plans == nil {
		doc.Plans = make(map[string]PlanTier)
	}
	if doc.Sites == nil {
		doc.Sites = make(map[string]Site)
	}
	if doc.Sectors == nil {
		doc.Sectors = make(map[string]Sector)
	}
	if doc.Subscribers == nil {
		doc.Subscribers = make(map[string]Subscriber)
	}
	if doc.Terminals == nil {
		doc.Terminals = make(map[string]Terminal)
	}
	s.data = doc
	return s, nil
}

// Snapshot returns a deep copy of the inventory. The scheduler takes one per
// tick so it is never reasoning about a half-edited network.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := Snapshot{
		Plans:       make(map[string]PlanTier, len(s.data.Plans)),
		Sites:       make(map[string]Site, len(s.data.Sites)),
		Sectors:     make(map[string]Sector, len(s.data.Sectors)),
		Subscribers: make(map[string]Subscriber, len(s.data.Subscribers)),
		Terminals:   make(map[string]Terminal, len(s.data.Terminals)),
	}
	for k, v := range s.data.Plans {
		snap.Plans[k] = v
	}
	for k, v := range s.data.Sites {
		snap.Sites[k] = v
	}
	for k, v := range s.data.Sectors {
		snap.Sectors[k] = v
	}
	for k, v := range s.data.Subscribers {
		snap.Subscribers[k] = v
	}
	for k, v := range s.data.Terminals {
		snap.Terminals[k] = v
	}
	return snap
}

// Save persists the inventory atomically. Perm is 0600: the document holds
// password hashes and terminal tokens.
func (s *Store) Save() error {
	s.mu.Lock()
	s.data.UpdatedAt = time.Now().UTC()
	s.data.Version = currentVersion
	raw, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()

	if err != nil {
		return fmt.Errorf("registry: encode: %w", err)
	}
	return fsutil.WriteAtomic(s.path, append(raw, '\n'), 0o600)
}

// Replace swaps the whole inventory, after checking it is internally
// consistent. Used by the provisioning path and by seeding.
func (s *Store) Replace(snap Snapshot) error {
	if err := Validate(snap); err != nil {
		return err
	}
	s.mu.Lock()
	s.data.Plans = snap.Plans
	s.data.Sites = snap.Sites
	s.data.Sectors = snap.Sectors
	s.data.Subscribers = snap.Subscribers
	s.data.Terminals = snap.Terminals
	s.mu.Unlock()
	return s.Save()
}

// PutPlan inserts or updates a service plan.
func (s *Store) PutPlan(p PlanTier) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.data.Plans[p.Name] = p
	s.mu.Unlock()
	return s.Save()
}

// PutSite inserts or updates a tower.
func (s *Store) PutSite(site Site) error {
	if site.ID == "" {
		return fmt.Errorf("registry: site id is required")
	}
	s.mu.Lock()
	s.data.Sites[site.ID] = site
	s.mu.Unlock()
	return s.Save()
}

// PutSector inserts or updates a sector radio.
func (s *Store) PutSector(sec Sector) error {
	if err := sec.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	_, siteOK := s.data.Sites[sec.SiteID]
	if siteOK {
		s.data.Sectors[sec.ID] = sec
	}
	s.mu.Unlock()
	if !siteOK {
		return fmt.Errorf("registry: sector %q references unknown site %q", sec.ID, sec.SiteID)
	}
	return s.Save()
}

// PutSubscriber inserts or updates an account.
func (s *Store) PutSubscriber(sub Subscriber) error {
	if sub.ID == "" {
		return fmt.Errorf("registry: subscriber id is required")
	}
	if sub.Username == "" {
		return fmt.Errorf("registry: subscriber %q needs a username", sub.ID)
	}
	s.mu.Lock()
	_, planOK := s.data.Plans[sub.Plan]
	if planOK {
		if sub.CreatedAt.IsZero() {
			sub.CreatedAt = time.Now().UTC()
		}
		s.data.Subscribers[sub.ID] = sub
	}
	s.mu.Unlock()
	if !planOK {
		return fmt.Errorf("registry: subscriber %q references unknown plan %q", sub.ID, sub.Plan)
	}
	return s.Save()
}

// PutTerminal inserts or updates a CPE.
func (s *Store) PutTerminal(t Terminal) error {
	if t.ID == "" {
		return fmt.Errorf("registry: terminal id is required")
	}
	s.mu.Lock()
	_, subOK := s.data.Subscribers[t.SubscriberID]
	if subOK {
		s.data.Terminals[t.ID] = t
	}
	s.mu.Unlock()
	if !subOK {
		return fmt.Errorf("registry: terminal %q references unknown subscriber %q",
			t.ID, t.SubscriberID)
	}
	return s.Save()
}

// SubscriberByUsername finds an account by its RADIUS username.
func (s *Store) SubscriberByUsername(username string) (Subscriber, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.data.Subscribers {
		if sub.Username == username {
			return sub, true
		}
	}
	return Subscriber{}, false
}

// TerminalsOf returns a subscriber's terminals.
func (s *Store) TerminalsOf(subscriberID string) []Terminal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Terminal
	for _, t := range s.data.Terminals {
		if t.SubscriberID == subscriberID {
			out = append(out, t)
		}
	}
	return out
}

// Terminal looks up one CPE.
func (s *Store) Terminal(id string) (Terminal, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.data.Terminals[id]
	return t, ok
}

// Validate checks a whole inventory for dangling references and impossible
// values. Run it before accepting a bulk provisioning payload: a broken
// reference found here is a 400 response, while the same reference found at tick
// time is a sector full of subscribers with no service.
func Validate(snap Snapshot) error {
	for name, p := range snap.Plans {
		if name != p.Name {
			return fmt.Errorf("registry: plan keyed %q but named %q", name, p.Name)
		}
		if err := p.Validate(); err != nil {
			return err
		}
	}
	for id, site := range snap.Sites {
		if id != site.ID {
			return fmt.Errorf("registry: site keyed %q but identified %q", id, site.ID)
		}
		if site.ParentSiteID != "" {
			if _, ok := snap.Sites[site.ParentSiteID]; !ok {
				return fmt.Errorf("registry: site %q has unknown parent %q", id, site.ParentSiteID)
			}
		}
		if site.BackhaulSectorID != "" {
			if _, ok := snap.Sectors[site.BackhaulSectorID]; !ok {
				return fmt.Errorf("registry: site %q relays over unknown sector %q",
					id, site.BackhaulSectorID)
			}
		}
		if site.BackhaulKind == BackhaulRelay && site.BackhaulSectorID == "" {
			return fmt.Errorf("registry: site %q is a relay but names no backhaul sector", id)
		}
		// Catch the cycle here rather than letting the solver find it, so a bad
		// edit is rejected at the API instead of degrading the next tick.
		if _, err := snap.BackhaulPath(id); err != nil {
			return err
		}
	}
	for id, sec := range snap.Sectors {
		if id != sec.ID {
			return fmt.Errorf("registry: sector keyed %q but identified %q", id, sec.ID)
		}
		if err := sec.Validate(); err != nil {
			return err
		}
		if _, ok := snap.Sites[sec.SiteID]; !ok {
			return fmt.Errorf("registry: sector %q references unknown site %q", id, sec.SiteID)
		}
	}
	for id, sub := range snap.Subscribers {
		if id != sub.ID {
			return fmt.Errorf("registry: subscriber keyed %q but identified %q", id, sub.ID)
		}
		if _, ok := snap.Plans[sub.Plan]; !ok {
			return fmt.Errorf("registry: subscriber %q references unknown plan %q", id, sub.Plan)
		}
	}
	seenUsernames := make(map[string]string, len(snap.Subscribers))
	for id, sub := range snap.Subscribers {
		if prior, dup := seenUsernames[sub.Username]; dup {
			return fmt.Errorf("registry: subscribers %q and %q share username %q",
				prior, id, sub.Username)
		}
		seenUsernames[sub.Username] = id
	}
	for id, t := range snap.Terminals {
		if id != t.ID {
			return fmt.Errorf("registry: terminal keyed %q but identified %q", id, t.ID)
		}
		if _, ok := snap.Subscribers[t.SubscriberID]; !ok {
			return fmt.Errorf("registry: terminal %q references unknown subscriber %q",
				id, t.SubscriberID)
		}
		if t.PinnedSectorID != "" {
			if _, ok := snap.Sectors[t.PinnedSectorID]; !ok {
				return fmt.Errorf("registry: terminal %q is pinned to unknown sector %q",
					id, t.PinnedSectorID)
			}
		}
	}
	return nil
}

// Example is a small but realistic network: a PoP site with two sectors, a
// relay site behind it, and a handful of subscribers across three plans. Used by
// `basestationd seed` so there is something to run against before any real
// inventory exists.
func Example() Snapshot {
	now := time.Now().UTC()
	snap := Snapshot{
		Plans: map[string]PlanTier{
			"residential-50": {
				Name: "residential-50", DownMbps: 50, UpMbps: 10,
				CommittedDownMbps: 10, CommittedUpMbps: 2, Priority: 1,
				BurstMbps: 75, BurstSeconds: 20,
			},
			"residential-150": {
				Name: "residential-150", DownMbps: 150, UpMbps: 25,
				CommittedDownMbps: 25, CommittedUpMbps: 5, Priority: 2,
				BurstMbps: 200, BurstSeconds: 20,
			},
			"business-200": {
				Name: "business-200", DownMbps: 200, UpMbps: 50,
				CommittedDownMbps: 50, CommittedUpMbps: 15, Priority: 5,
			},
		},
		Sites: map[string]Site{
			"pop-ridge": {
				ID: "pop-ridge", Name: "Ridge Road water tower",
				Position:     radio.Point{Lat: 44.9412, Lon: -93.0998, HeightM: 42},
				BackhaulKind: BackhaulFiber, BackhaulMbps: 1000,
				Enabled: true,
				Notes:   "Primary PoP. Fibre handoff in the ground-level cabinet.",
			},
			"relay-mill": {
				ID: "relay-mill", Name: "Mill Street rooftop",
				Position:     radio.Point{Lat: 44.9731, Lon: -93.0401, HeightM: 18},
				ParentSiteID: "pop-ridge", BackhaulKind: BackhaulRelay,
				BackhaulSectorID: "ridge-n", BackhaulMbps: 180,
				Enabled: true,
				Notes:   "No fibre on this block. Relays off ridge-n.",
			},
		},
		Sectors: map[string]Sector{
			"ridge-n": {
				ID: "ridge-n", SiteID: "pop-ridge", Name: "Ridge north",
				AzimuthDeg: 0, BeamwidthDeg: 90, VBeamwidthDeg: 9, DowntiltDeg: 3,
				FreqMHz: 5775, ChannelWidthMHz: 40, TxPowerDBm: 25,
				AntennaGainDBi: 17, FeederLossDB: 1.5, ClutterLossDB: 8,
				FadeMarginDB: 10, InterferenceDBm: -92,
				AirtimeBudget: 0.85, ProtocolEfficiency: 0.75,
				MaxTerminals: 60, MaxRangeKm: 12, Enabled: true,
			},
			"ridge-s": {
				ID: "ridge-s", SiteID: "pop-ridge", Name: "Ridge south",
				AzimuthDeg: 180, BeamwidthDeg: 90, VBeamwidthDeg: 9, DowntiltDeg: 4,
				FreqMHz: 5815, ChannelWidthMHz: 40, TxPowerDBm: 25,
				AntennaGainDBi: 17, FeederLossDB: 1.5, ClutterLossDB: 10,
				FadeMarginDB: 10, InterferenceDBm: -95,
				AirtimeBudget: 0.85, ProtocolEfficiency: 0.75,
				MaxTerminals: 60, MaxRangeKm: 12, Enabled: true,
			},
			"mill-e": {
				ID: "mill-e", SiteID: "relay-mill", Name: "Mill east",
				AzimuthDeg: 90, BeamwidthDeg: 120, VBeamwidthDeg: 12, DowntiltDeg: 3,
				FreqMHz: 5500, ChannelWidthMHz: 40, TxPowerDBm: 23,
				AntennaGainDBi: 15, FeederLossDB: 1.0, ClutterLossDB: 12,
				FadeMarginDB: 10, InterferenceDBm: -90,
				AirtimeBudget: 0.85, ProtocolEfficiency: 0.75,
				MaxTerminals: 40, MaxRangeKm: 6, Enabled: true,
			},
		},
		Subscribers: make(map[string]Subscriber),
		Terminals:   make(map[string]Terminal),
	}

	// relay-mill backhauls over ridge-n, so it has to sit inside ridge-n's beam.
	// Placing it by bearing from the PoP guarantees that; a hand-picked
	// coordinate does not, and an inventory where the relay is outside the beam
	// it supposedly uses is quietly inconsistent.
	ridge := snap.Sites["pop-ridge"]
	mill := snap.Sites["relay-mill"]
	mill.Position = ridge.Position.Destination(20, 5.0)
	mill.Position.HeightM = 18
	snap.Sites["relay-mill"] = mill

	// Subscribers are likewise placed by bearing and distance from their serving
	// site. Coordinates chosen by hand land a degree or two outside a sector's
	// azimuth, and the resulting "no service" takes an afternoon to trace back to
	// a rounding error in a seed fixture.
	type seed struct {
		id, name, plan string
		siteID         string
		bearingDeg     float64
		distanceKm     float64
		heightM        float64
	}
	seeds := []seed{
		{"sub-0001", "A. Okonkwo", "residential-150", "pop-ridge", 10, 1.2, 7},
		{"sub-0002", "B. Lindqvist", "residential-50", "pop-ridge", 340, 2.5, 6},
		{"sub-0003", "C. Herrera", "business-200", "pop-ridge", 185, 1.8, 9},
		{"sub-0004", "D. Nakamura", "residential-150", "relay-mill", 95, 1.0, 6},
		{"sub-0005", "E. Balogun", "residential-50", "relay-mill", 120, 2.0, 6},
	}
	for i, s := range seeds {
		servingSite := snap.Sites[s.siteID]
		position := servingSite.Position.Destination(s.bearingDeg, s.distanceKm)
		position.HeightM = s.heightM

		snap.Subscribers[s.id] = Subscriber{
			ID: s.id, Name: s.name, Plan: s.plan,
			Username: s.id + "@bswisp.net",
			// Deliberately empty: `basestationd passwd` sets a real hash. An
			// empty hash can never match, so a seeded account cannot
			// authenticate by accident.
			PasswordHash: "",
			CreatedAt:    now,
		}
		termID := fmt.Sprintf("cpe-%04d", i+1)
		snap.Terminals[termID] = Terminal{
			ID: termID, SubscriberID: s.id,
			Position:       position,
			AntennaGainDBi: 19, NoiseFigureDB: 6, FeederLossDB: 0.5,
			Enabled: true, InstallAt: now,
		}
	}
	return snap
}
