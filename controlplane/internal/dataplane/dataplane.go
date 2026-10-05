// Package dataplane programs the kernel to enforce the scheduler's grants.
//
// The plan is advice until something drops packets. This package is the part
// that makes a grant real: per-subscriber rate limits, CGNAT translation, and a
// hard stop for suspended accounts.
//
// Two backends. The "dryrun" backend renders exactly what it would run and
// writes it to disk, changing nothing. The "linux" backend applies it with nft
// and tc, and only when explicitly enabled. Two separate switches, because an
// accidental ruleset flush on a PoP router takes every subscriber offline
// simultaneously and is noticed by all of them at once.
package dataplane

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"sync"
)

// Subscriber is one subscriber's enforcement state for this tick.
type Subscriber struct {
	TerminalID   string
	SubscriberID string

	V4 netip.Addr
	V6 netip.Prefix

	// GrantDownMbps and GrantUpMbps are this tick's allowance from the plan.
	GrantDownMbps float64
	GrantUpMbps   float64

	// BurstDownMbps is the ceiling a subscriber may briefly exceed their grant
	// to, which is what makes a connection feel fast on page loads without
	// selling sustained capacity you do not have. Zero disables bursting.
	BurstDownMbps float64

	// Suspended accounts are dropped rather than shaped to zero. Shaping to zero
	// leaves connections hanging and generates support calls; an outright reject
	// is unambiguous.
	Suspended bool

	// Unserved means the scheduler could not place this terminal. Its traffic is
	// policed to a trickle rather than dropped, so the CPE stays manageable and
	// can keep reporting telemetry.
	Unserved bool
}

// State is the complete intended dataplane configuration for one tick.
type State struct {
	Epoch       uint64
	Subscribers []Subscriber
}

// Dataplane applies state to the kernel.
type Dataplane interface {
	// Apply makes the kernel match the given state. Implementations must be
	// idempotent and must replace the previous configuration wholesale rather
	// than accumulating rules: a diff-based approach drifts, and drift in a
	// firewall is how subscribers end up with each other's bandwidth.
	Apply(ctx context.Context, s State) error

	// Describe returns what the last Apply rendered, for the admin API.
	Describe() map[string]string

	Close() error
}

// classAllocator hands out stable tc class IDs.
//
// Stability matters: tc identifies a class by a 16-bit minor number, and if a
// subscriber's number changed between ticks their queue would be torn down and
// rebuilt every 15 seconds, dropping their in-flight packets each time. So a
// terminal keeps its number for as long as it is present, and released numbers
// are only reused once the range wraps.
type classAllocator struct {
	mu     sync.Mutex
	byTerm map[string]uint16
	used   map[uint16]bool
	next   uint16
}

// classIDBase keeps away from 1:0 (the qdisc) and 1:1 (the root class).
const classIDBase = 0x10

func newClassAllocator() *classAllocator {
	return &classAllocator{
		byTerm: make(map[string]uint16),
		used:   make(map[uint16]bool),
		next:   classIDBase,
	}
}

func (a *classAllocator) get(terminalID string) (uint16, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if id, ok := a.byTerm[terminalID]; ok {
		return id, nil
	}
	// 0xFFFF is reserved by convention for the default class.
	const max = 0xFFFE
	span := max - classIDBase + 1
	if len(a.used) >= span {
		return 0, fmt.Errorf("dataplane: out of tc class IDs (%d in use); "+
			"a single HTB tree cannot hold this many subscribers, so split them "+
			"across interfaces or move to a hardware-offloaded shaper", len(a.used))
	}
	for i := 0; i < span; i++ {
		cand := uint16(classIDBase + (int(a.next-classIDBase)+i)%span)
		if !a.used[cand] {
			a.used[cand] = true
			a.byTerm[terminalID] = cand
			a.next = uint16(classIDBase + (int(cand-classIDBase)+1)%span)
			return cand, nil
		}
	}
	return 0, fmt.Errorf("dataplane: out of tc class IDs")
}

// retain drops class IDs for terminals no longer present, so the map does not
// grow without bound as subscribers churn.
func (a *classAllocator) retain(present map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for term, id := range a.byTerm {
		if !present[term] {
			delete(a.byTerm, term)
			delete(a.used, id)
		}
	}
}

// sortSubscribers orders by terminal ID so rendered output is byte-stable and
// therefore diffable between ticks.
func sortSubscribers(subs []Subscriber) []Subscriber {
	out := make([]Subscriber, len(subs))
	copy(out, subs)
	sort.Slice(out, func(i, j int) bool { return out[i].TerminalID < out[j].TerminalID })
	return out
}

// kbit converts Mbps to the kilobits tc expects, with a floor.
//
// The floor is not cosmetic: tc rejects a rate of zero, and an HTB class with a
// near-zero rate stalls TCP so badly that the subscriber cannot even load a
// status page to see why. 64 kbit is slow but alive.
func kbit(mbps float64) int {
	const minimum = 64
	v := int(mbps * 1000)
	if v < minimum {
		return minimum
	}
	return v
}
