// Package ipam allocates subscriber addresses.
//
// Two pools, for two different jobs:
//
//   - A CGNAT IPv4 address from 100.64.0.0/10 (RFC 6598), translated to a public
//     address at the PoP. Shared-address space exists for exactly this, and is
//     what Starlink hands residential subscribers. Using RFC 1918 here instead
//     collides with the subscriber's own LAN behind the CPE, which is a
//     miserable class of support ticket.
//   - A delegated IPv6 prefix, normally a /56. This matters more on a CGNAT
//     network than a wired one: it is the only way a subscriber gets a genuinely
//     reachable address, so anything inbound they care about works over v6 or
//     not at all.
//
// Allocations are sticky. A terminal that comes back gets the address it had,
// because churning addresses breaks the subscriber's own port forwards, DNS and
// firewall rules for no gain.
package ipam

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/s-rakim/my-personal-project/controlplane/internal/fsutil"
)

// Lease is one terminal's addressing.
type Lease struct {
	TerminalID string `json:"terminal_id"`

	V4      netip.Addr `json:"v4"`
	V4Index uint64     `json:"v4_index"`

	// V6 is the prefix delegated to the subscriber, not a single address.
	V6      netip.Prefix `json:"v6"`
	V6Index uint64       `json:"v6_index"`

	AssignedAt time.Time `json:"assigned_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// Config describes the pools.
type Config struct {
	CGNATPool          string
	IPv6Pool           string
	IPv6DelegationBits int
}

// Allocator hands out and remembers leases.
type Allocator struct {
	mu   sync.RWMutex
	path string

	v4Pool netip.Prefix
	v6Pool netip.Prefix
	v6Bits int

	// v4Capacity and v6Capacity are the number of allocatable units. Capped
	// because a /10 holds four million addresses and an unbounded counter type
	// would silently wrap on a /0.
	v4Capacity uint64
	v6Capacity uint64

	leases map[string]Lease  // terminal ID -> lease
	usedV4 map[uint64]string // index -> terminal ID
	usedV6 map[uint64]string

	// cursors advance so fresh allocations do not immediately reuse an address
	// that was just released; a released address only comes back round once the
	// cursor wraps. Reusing an IP within seconds confuses NAT state, connection
	// tracking and anyone reading logs.
	cursorV4 uint64
	cursorV6 uint64
}

type persisted struct {
	Version  int              `json:"version"`
	CursorV4 uint64           `json:"cursor_v4"`
	CursorV6 uint64           `json:"cursor_v6"`
	Leases   map[string]Lease `json:"leases"`
}

// Open loads the lease table, creating an empty one if absent. Leases outside
// the configured pools are discarded with an error rather than silently kept,
// since that means the pool was changed under a running network.
func Open(path string, cfg Config) (*Allocator, error) {
	v4, err := netip.ParsePrefix(cfg.CGNATPool)
	if err != nil {
		return nil, fmt.Errorf("ipam: cgnat pool: %w", err)
	}
	if !v4.Addr().Is4() {
		return nil, fmt.Errorf("ipam: cgnat pool %s is not IPv4", cfg.CGNATPool)
	}
	v6, err := netip.ParsePrefix(cfg.IPv6Pool)
	if err != nil {
		return nil, fmt.Errorf("ipam: ipv6 pool: %w", err)
	}
	if !v6.Addr().Is6() {
		return nil, fmt.Errorf("ipam: ipv6 pool %s is not IPv6", cfg.IPv6Pool)
	}
	if cfg.IPv6DelegationBits <= v6.Bits() || cfg.IPv6DelegationBits > 64 {
		return nil, fmt.Errorf("ipam: ipv6 delegation /%d must be longer than the pool /%d "+
			"and no longer than /64", cfg.IPv6DelegationBits, v6.Bits())
	}

	a := &Allocator{
		path:       path,
		v4Pool:     v4.Masked(),
		v6Pool:     v6.Masked(),
		v6Bits:     cfg.IPv6DelegationBits,
		v4Capacity: capacityFor(32 - v4.Bits()),
		v6Capacity: capacityFor(cfg.IPv6DelegationBits - v6.Bits()),
		leases:     make(map[string]Lease),
		usedV4:     make(map[uint64]string),
		usedV6:     make(map[uint64]string),
	}

	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ipam: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return a, nil
	}

	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ipam: parse %s: %w", path, err)
	}
	for id, lease := range p.Leases {
		if !a.v4Pool.Contains(lease.V4) {
			return nil, fmt.Errorf("ipam: lease for %s holds %s, outside pool %s; "+
				"the pool was changed under a live network and the old leases must be "+
				"migrated or cleared deliberately", id, lease.V4, a.v4Pool)
		}
		if lease.V6.IsValid() && !a.v6Pool.Contains(lease.V6.Addr()) {
			return nil, fmt.Errorf("ipam: lease for %s holds %s, outside pool %s",
				id, lease.V6, a.v6Pool)
		}
		a.leases[id] = lease
		a.usedV4[lease.V4Index] = id
		a.usedV6[lease.V6Index] = id
	}
	a.cursorV4 = p.CursorV4
	a.cursorV6 = p.CursorV6
	return a, nil
}

// capacityFor returns 2^bits, saturating rather than overflowing.
func capacityFor(bits int) uint64 {
	if bits <= 0 {
		return 1
	}
	if bits >= 63 {
		return math.MaxUint64 / 2
	}
	return uint64(1) << uint(bits)
}

// Lease returns the terminal's addressing, allocating it on first call. It is
// idempotent: the same terminal always gets the same lease back.
func (a *Allocator) Lease(terminalID string) (Lease, error) {
	if terminalID == "" {
		return Lease{}, fmt.Errorf("ipam: terminal id is required")
	}

	a.mu.Lock()
	if existing, ok := a.leases[terminalID]; ok {
		existing.LastSeenAt = time.Now().UTC()
		a.leases[terminalID] = existing
		a.mu.Unlock()
		return existing, nil
	}

	v4Index, err := a.nextFree(a.usedV4, &a.cursorV4, a.v4Capacity, "IPv4")
	if err != nil {
		a.mu.Unlock()
		return Lease{}, err
	}
	v6Index, err := a.nextFree(a.usedV6, &a.cursorV6, a.v6Capacity, "IPv6")
	if err != nil {
		a.mu.Unlock()
		return Lease{}, err
	}

	// Offset by one so the pool's own network address is never handed out.
	v4Addr, err := addOffset(a.v4Pool.Addr(), v4Index+1)
	if err != nil {
		a.mu.Unlock()
		return Lease{}, err
	}
	if !a.v4Pool.Contains(v4Addr) {
		// Should be unreachable given the capacity check, but handing out an
		// address from outside the pool would be silent corruption: the dataplane
		// would not NAT it and the subscriber would have no route at all.
		a.mu.Unlock()
		return Lease{}, fmt.Errorf("ipam: computed %s for index %d, outside pool %s",
			v4Addr, v4Index, a.v4Pool)
	}
	v6Prefix, err := nthPrefix(a.v6Pool, a.v6Bits, v6Index)
	if err != nil {
		a.mu.Unlock()
		return Lease{}, err
	}

	now := time.Now().UTC()
	lease := Lease{
		TerminalID: terminalID,
		V4:         v4Addr,
		V4Index:    v4Index,
		V6:         v6Prefix,
		V6Index:    v6Index,
		AssignedAt: now,
		LastSeenAt: now,
	}
	a.leases[terminalID] = lease
	a.usedV4[v4Index] = terminalID
	a.usedV6[v6Index] = terminalID
	a.mu.Unlock()

	if err := a.Save(); err != nil {
		// The lease is live in memory; a failed save means it could be lost on
		// restart. Surface that rather than pretending the allocation is safe.
		return lease, fmt.Errorf("ipam: allocated %s to %s but could not persist it: %w",
			lease.V4, terminalID, err)
	}
	return lease, nil
}

// nextFree finds an unused index, scanning forward from the cursor and wrapping
// once. Caller must hold the lock.
func (a *Allocator) nextFree(used map[uint64]string, cursor *uint64, capacity uint64,
	family string) (uint64, error) {

	if uint64(len(used)) >= capacity {
		return 0, fmt.Errorf("ipam: %s pool is exhausted (%d of %d allocated)",
			family, len(used), capacity)
	}
	start := *cursor % capacity
	for i := uint64(0); i < capacity; i++ {
		idx := (start + i) % capacity
		if _, taken := used[idx]; !taken {
			*cursor = (idx + 1) % capacity
			return idx, nil
		}
	}
	return 0, fmt.Errorf("ipam: %s pool is exhausted", family)
}

// Release returns a terminal's addresses to the pool.
func (a *Allocator) Release(terminalID string) error {
	a.mu.Lock()
	lease, ok := a.leases[terminalID]
	if ok {
		delete(a.leases, terminalID)
		delete(a.usedV4, lease.V4Index)
		delete(a.usedV6, lease.V6Index)
	}
	a.mu.Unlock()
	if !ok {
		return nil
	}
	return a.Save()
}

// Get returns an existing lease without allocating.
func (a *Allocator) Get(terminalID string) (Lease, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	l, ok := a.leases[terminalID]
	return l, ok
}

// Leases returns every allocation, for the admin API and the dataplane.
func (a *Allocator) Leases() []Lease {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]Lease, 0, len(a.leases))
	for _, l := range a.leases {
		out = append(out, l)
	}
	return out
}

// Stats reports pool usage, so running out is something you see coming.
func (a *Allocator) Stats() (v4Used, v4Cap, v6Used, v6Cap uint64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return uint64(len(a.usedV4)), a.v4Capacity, uint64(len(a.usedV6)), a.v6Capacity
}

// Save persists the lease table atomically.
func (a *Allocator) Save() error {
	a.mu.RLock()
	p := persisted{
		Version:  1,
		CursorV4: a.cursorV4,
		CursorV6: a.cursorV6,
		Leases:   make(map[string]Lease, len(a.leases)),
	}
	for k, v := range a.leases {
		p.Leases[k] = v
	}
	a.mu.RUnlock()

	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("ipam: encode: %w", err)
	}
	return fsutil.WriteAtomic(a.path, append(raw, '\n'), 0o600)
}

// nthPrefix returns the nth subnet of length delegatedBits inside pool.
//
// Done with math/big rather than by hand because the index has to be shifted
// into a 128-bit field: for a /32 pool delegating /56s the shift is 72 bits,
// which silently yields zero in a uint64 and hands every subscriber the pool's
// own base prefix. This is not a hot path -- once per terminal, ever -- so
// obvious correctness beats clever bit twiddling.
func nthPrefix(pool netip.Prefix, delegatedBits int, n uint64) (netip.Prefix, error) {
	if delegatedBits <= pool.Bits() || delegatedBits > 128 {
		return netip.Prefix{}, fmt.Errorf(
			"ipam: delegation /%d must be longer than pool /%d and at most /128",
			delegatedBits, pool.Bits())
	}

	indexBits := delegatedBits - pool.Bits()
	if indexBits < 64 && n >= uint64(1)<<uint(indexBits) {
		return netip.Prefix{}, fmt.Errorf(
			"ipam: subnet index %d is beyond the %d /%ds available in %s",
			n, uint64(1)<<uint(indexBits), delegatedBits, pool)
	}

	base := new(big.Int).SetBytes(pool.Masked().Addr().AsSlice())
	offset := new(big.Int).Lsh(new(big.Int).SetUint64(n), uint(128-delegatedBits))
	base.Add(base, offset)

	buf := base.Bytes()
	if len(buf) > 16 {
		return netip.Prefix{}, fmt.Errorf("ipam: subnet index %d overflows the address space", n)
	}
	var arr [16]byte
	copy(arr[16-len(buf):], buf)

	return netip.PrefixFrom(netip.AddrFrom16(arr), delegatedBits), nil
}

// addOffset adds n to an address, working on the full 16-byte form so the same
// code handles both families. Returns an error on overflow past the end of the
// address space rather than wrapping to zero.
func addOffset(base netip.Addr, n uint64) (netip.Addr, error) {
	b := base.As16()

	carry := n
	for i := 15; i >= 0 && carry > 0; i-- {
		sum := uint64(b[i]) + (carry & 0xff)
		b[i] = byte(sum & 0xff)
		carry = (carry >> 8) + (sum >> 8)
	}
	if carry > 0 {
		return netip.Addr{}, fmt.Errorf("ipam: offset %d overflows the address space from %s",
			n, base)
	}

	out := netip.AddrFrom16(b)
	if base.Is4() {
		// Keep the 4-byte form so the address prints as IPv4 rather than as an
		// IPv4-mapped IPv6 address, which confuses every downstream tool.
		return out.Unmap(), nil
	}
	return out, nil
}
