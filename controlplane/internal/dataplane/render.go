package dataplane

import (
	"fmt"
	"strings"
)

// Rendered holds the scripts for one tick.
type Rendered struct {
	NFT        string
	TCAccess   string
	TCUplink   string
	SetupIFB   string
	Subscriber int
}

// Config describes the interfaces and addresses to render against.
type Config struct {
	Table           string
	WANInterface    string
	AccessInterface string
	IFBInterface    string
	PublicIPv4      string
	CGNATPool       string
}

// render produces the nftables and tc scripts for a state.
//
// Downlink and uplink are shaped on different devices, which is the part worth
// understanding. Downlink is egress on the access interface, so it is filtered on
// destination address. Uplink is ingress on that same interface, and Linux cannot
// queue on ingress, so traffic is redirected to an IFB device and shaped there as
// egress, filtered on source address. Shaping uplink as a simple ingress policer
// instead would drop rather than queue, which TCP handles much worse.
func render(cfg Config, s State, classes *classAllocator) (Rendered, error) {
	subs := sortSubscribers(s.Subscribers)

	present := make(map[string]bool, len(subs))
	for _, sub := range subs {
		present[sub.TerminalID] = true
	}
	classes.retain(present)

	var nft, down, up strings.Builder

	// ---- nftables ----
	//
	// The table is deleted and rebuilt in one atomic transaction. nft applies a
	// whole file as a single commit, so there is no window where subscribers are
	// unprotected or unNATted.
	fmt.Fprintf(&nft, "#!/usr/sbin/nft -f\n")
	fmt.Fprintf(&nft, "# bswisp dataplane, epoch %d. Generated; do not edit.\n", s.Epoch)
	fmt.Fprintf(&nft, "# Applied as one transaction: nft -f this-file\n\n")
	fmt.Fprintf(&nft, "destroy table inet %s\n", cfg.Table)
	fmt.Fprintf(&nft, "table inet %s {\n", cfg.Table)

	// Suspended subscribers go in a named set so the forward chain stays short
	// regardless of how many there are.
	fmt.Fprintf(&nft, "  set suspended_v4 {\n    type ipv4_addr\n    flags interval\n")
	var suspendedV4 []string
	var suspendedV6 []string
	for _, sub := range subs {
		if !sub.Suspended {
			continue
		}
		if sub.V4.IsValid() {
			suspendedV4 = append(suspendedV4, sub.V4.String())
		}
		if sub.V6.IsValid() {
			suspendedV6 = append(suspendedV6, sub.V6.String())
		}
	}
	if len(suspendedV4) > 0 {
		fmt.Fprintf(&nft, "    elements = { %s }\n", strings.Join(suspendedV4, ", "))
	}
	fmt.Fprintf(&nft, "  }\n\n")

	fmt.Fprintf(&nft, "  set suspended_v6 {\n    type ipv6_addr\n    flags interval\n")
	if len(suspendedV6) > 0 {
		fmt.Fprintf(&nft, "    elements = { %s }\n", strings.Join(suspendedV6, ", "))
	}
	fmt.Fprintf(&nft, "  }\n\n")

	fmt.Fprintf(&nft, "  chain forward {\n")
	fmt.Fprintf(&nft, "    type filter hook forward priority filter; policy drop;\n")
	fmt.Fprintf(&nft, "    ct state established,related accept\n")
	// Suspension is checked before anything else accepts, and in both directions,
	// so a suspended account cannot be reached either.
	fmt.Fprintf(&nft, "    ip saddr @suspended_v4 reject with icmp type admin-prohibited\n")
	fmt.Fprintf(&nft, "    ip daddr @suspended_v4 reject with icmp type admin-prohibited\n")
	fmt.Fprintf(&nft, "    ip6 saddr @suspended_v6 reject with icmpv6 type admin-prohibited\n")
	fmt.Fprintf(&nft, "    ip6 daddr @suspended_v6 reject with icmpv6 type admin-prohibited\n")
	// Subscribers must not reach each other directly through the PoP: it is not
	// a LAN, and letting it behave like one exposes every CPE management
	// interface to every other subscriber.
	fmt.Fprintf(&nft, "    iifname \"%s\" oifname \"%s\" drop comment \"no subscriber-to-subscriber\"\n",
		cfg.AccessInterface, cfg.AccessInterface)
	fmt.Fprintf(&nft, "    iifname \"%s\" oifname \"%s\" accept\n",
		cfg.AccessInterface, cfg.WANInterface)
	fmt.Fprintf(&nft, "    iifname \"%s\" oifname \"%s\" accept\n",
		cfg.WANInterface, cfg.AccessInterface)
	fmt.Fprintf(&nft, "  }\n\n")

	fmt.Fprintf(&nft, "  chain postrouting {\n")
	fmt.Fprintf(&nft, "    type nat hook postrouting priority srcnat; policy accept;\n")
	// Only the v4 pool is translated. IPv6 is routed natively, which is the whole
	// point of delegating a prefix: it is the one address a CGNAT'd subscriber has
	// that is actually reachable from outside.
	fmt.Fprintf(&nft, "    ip saddr %s oifname \"%s\" snat to %s\n",
		cfg.CGNATPool, cfg.WANInterface, cfg.PublicIPv4)
	fmt.Fprintf(&nft, "  }\n")
	fmt.Fprintf(&nft, "}\n")

	// ---- tc, downlink: egress on the access interface ----
	fmt.Fprintf(&down, "# bswisp downlink shaping, epoch %d. Apply with: tc -force -batch\n", s.Epoch)
	fmt.Fprintf(&down, "qdisc del dev %s root\n", cfg.AccessInterface)
	fmt.Fprintf(&down, "qdisc add dev %s root handle 1: htb default 0xffff\n", cfg.AccessInterface)
	// The root class is the aggregate ceiling. Sized generously because the real
	// constraints are per-sector airtime and the site's backhaul, both already
	// accounted for by the solver.
	fmt.Fprintf(&down, "class add dev %s parent 1: classid 1:1 htb rate 10000mbit\n",
		cfg.AccessInterface)

	fmt.Fprintf(&up, "# bswisp uplink shaping, epoch %d. Apply with: tc -force -batch\n", s.Epoch)
	fmt.Fprintf(&up, "qdisc del dev %s root\n", cfg.IFBInterface)
	fmt.Fprintf(&up, "qdisc add dev %s root handle 1: htb default 0xffff\n", cfg.IFBInterface)
	fmt.Fprintf(&up, "class add dev %s parent 1: classid 1:1 htb rate 10000mbit\n",
		cfg.IFBInterface)

	for _, sub := range subs {
		if sub.Suspended {
			// Dropped in nftables; no queue needed.
			continue
		}
		cid, err := classes.get(sub.TerminalID)
		if err != nil {
			return Rendered{}, err
		}

		downRate := sub.GrantDownMbps
		upRate := sub.GrantUpMbps
		if sub.Unserved {
			// Keep the CPE reachable so it can still report telemetry and be
			// diagnosed, but give it nothing useful.
			downRate, upRate = 0, 0
		}

		downCeil := downRate
		if sub.BurstDownMbps > downCeil {
			downCeil = sub.BurstDownMbps
		}

		fmt.Fprintf(&down, "class add dev %s parent 1:1 classid 1:%x htb rate %dkbit ceil %dkbit "+
			"burst 15k quantum 1514\n",
			cfg.AccessInterface, cid, kbit(downRate), kbit(downCeil))
		// fq_codel per subscriber, not a single shared queue: it keeps one
		// subscriber's bulk download from adding latency to their own video call,
		// and costs nothing.
		fmt.Fprintf(&down, "qdisc add dev %s parent 1:%x fq_codel\n", cfg.AccessInterface, cid)

		fmt.Fprintf(&up, "class add dev %s parent 1:1 classid 1:%x htb rate %dkbit ceil %dkbit "+
			"burst 15k quantum 1514\n",
			cfg.IFBInterface, cid, kbit(upRate), kbit(upRate))
		fmt.Fprintf(&up, "qdisc add dev %s parent 1:%x fq_codel\n", cfg.IFBInterface, cid)

		// Downlink filters on destination, uplink on source.
		if sub.V4.IsValid() {
			fmt.Fprintf(&down, "filter add dev %s parent 1: protocol ip prio 1 u32 "+
				"match ip dst %s/32 flowid 1:%x\n", cfg.AccessInterface, sub.V4, cid)
			fmt.Fprintf(&up, "filter add dev %s parent 1: protocol ip prio 1 u32 "+
				"match ip src %s/32 flowid 1:%x\n", cfg.IFBInterface, sub.V4, cid)
		}
		if sub.V6.IsValid() {
			fmt.Fprintf(&down, "filter add dev %s parent 1: protocol ipv6 prio 2 u32 "+
				"match ip6 dst %s flowid 1:%x\n", cfg.AccessInterface, sub.V6, cid)
			fmt.Fprintf(&up, "filter add dev %s parent 1: protocol ipv6 prio 2 u32 "+
				"match ip6 src %s flowid 1:%x\n", cfg.IFBInterface, sub.V6, cid)
		}
	}

	// A default class catches anything unrecognised and throttles it hard rather
	// than letting it run unshaped. Unclassified traffic on a subscriber network
	// is either a bug or a spoofed source, and neither deserves full rate.
	fmt.Fprintf(&down, "class add dev %s parent 1:1 classid 1:ffff htb rate 1mbit ceil 1mbit\n",
		cfg.AccessInterface)
	fmt.Fprintf(&up, "class add dev %s parent 1:1 classid 1:ffff htb rate 1mbit ceil 1mbit\n",
		cfg.IFBInterface)

	// IFB setup is separate because it is one-time: creating the device and
	// redirecting ingress to it does not need redoing every tick.
	var setup strings.Builder
	fmt.Fprintf(&setup, "#!/bin/sh\n")
	fmt.Fprintf(&setup, "# One-time uplink shaping setup. Run once at boot, before the daemon.\n")
	fmt.Fprintf(&setup, "set -eu\n")
	fmt.Fprintf(&setup, "modprobe ifb numifbs=1\n")
	fmt.Fprintf(&setup, "ip link add %s type ifb 2>/dev/null || true\n", cfg.IFBInterface)
	fmt.Fprintf(&setup, "ip link set dev %s up\n", cfg.IFBInterface)
	fmt.Fprintf(&setup, "tc qdisc del dev %s ingress 2>/dev/null || true\n", cfg.AccessInterface)
	fmt.Fprintf(&setup, "tc qdisc add dev %s handle ffff: ingress\n", cfg.AccessInterface)
	fmt.Fprintf(&setup, "tc filter add dev %s parent ffff: protocol all prio 1 u32 "+
		"match u32 0 0 action mirred egress redirect dev %s\n",
		cfg.AccessInterface, cfg.IFBInterface)

	return Rendered{
		NFT:        nft.String(),
		TCAccess:   down.String(),
		TCUplink:   up.String(),
		SetupIFB:   setup.String(),
		Subscriber: len(subs),
	}, nil
}
