# What to build and what to adopt

A wireless ISP is four layers. Only one of them is worth writing yourself, and
this repository is that one. Writing the others would be a waste of your time
and worse than what already exists.

## Layer 1 — CPE and access point firmware: adopt

The software on the radio at the subscriber's house and on the tower.

- **OpenWrt** is the answer for anything you flash yourself. Mature, maintained,
  has the routing, firewall, QoS and VLAN support you need.
- **Vendor firmware** (MikroTik RouterOS, Ubiquiti airOS/UISP, Cambium cnWave)
  is the answer for purpose-built fixed-wireless gear, which is most tower
  radios. It already does the hard RF work: MCS adaptation, TDD framing,
  per-client airtime accounting. You are not going to beat it, and you do not
  want to maintain it.

Do not write radio firmware. The RF layer is where the vendors have spent a
decade of engineering, and their gear reports exactly the SINR and airtime
figures this control plane needs.

## Layer 2 — Device fleet management: adopt

Provisioning, config templates, firmware rollout and reachability monitoring for
the access points themselves.

- **OpenWISP** is the strongest open-source option and does genuinely fit: it
  provisions and monitors a fleet of OpenWrt devices, with config templates,
  VPN management and per-device status.
- **UISP** (Ubiquiti) or **MikroTik's own tooling** if your towers are that gear.

What these do *not* do, and why this repository exists: OpenWISP manages
*devices*. It does not know what a subscriber is, does not hold service plans,
does not allocate airtime across competing terminals, does not run AAA, does not
hand out addresses, and does not tell you that tower three cannot honour its
committed rates. Those are subscriber-level concerns and they are a different
problem from device management.

Run OpenWISP *underneath* this control plane, not instead of it.

## Layer 3 — The base station control plane: this repository

The layer that turns a pile of working radios into an ISP:

| Concern | Where |
|---|---|
| Airtime allocation and handoff | `scheduler/` (C++) |
| Subscriber AAA, IPAM, sessions, dataplane | `controlplane/` (Go) |
| RF planning and capacity forecasting | `planning/` (Python) |
| Subscriber lifecycle, billing, provisioning | `oss/` (Java) |
| NOC dashboard | `noc/` (C#) |

Nothing off the shelf does this for a small operator. The commercial products
that do (Amdocs, Netcracker, Sonar, Powercode) are either carrier-priced or
assume a wired network.

## Layer 4 — Internet transit: buy, and keep boring

Your ASN, your BGP sessions, your IP allocations, your peering. Use **BIRD** or
**FRR** for BGP and change nothing clever about it. This is the layer where
Starlink is deliberately conventional too, because it has to interoperate with
everyone.

---

## Two readings of "relay through cell towers"

Both are real architectures and this repository models both, so you do not have
to decide before building.

**Reading A — towers as distribution relays.** Radios on towers serve your
subscribers; towers reach the internet over fibre or microwave, and a tower
without either relays through a neighbouring tower. This is a classic WISP and
the direct analogue of Starlink: the tower sector is the satellite beam, the
tower-to-tower link is the inter-satellite laser, the PoP is the gateway.

Model it with `backhaul_kind: "fiber"`, `"ptp"`, or `"relay"`.

**Reading B — cellular as uplink.** A SIM modem at the tower is the WAN, and you
distribute over Wi-Fi or point-to-point radio. Fastest way to light a site with
no fibre nearby.

Model it with `backhaul_kind: "cellular"`. Read that constant's comment in
`controlplane/internal/registry/types.go` before committing to it: you are
reselling metered capacity from behind someone else's CGNAT, at a latency you do
not control. Good first uplink, good failover, questionable permanent trunk.

A real network ends up with both, which is why backhaul kind is per-site.

## Spectrum, before you transmit anything

The control plane does not care which band you use, but the law does, and this is
the part that ends deployments.

- **Unlicensed** (5 GHz U-NII, 6 GHz, 24/60 GHz, 900 MHz ISM) — no licence, but
  power limits apply to EIRP, not transmitter output, and you have no protection
  from interference. Most small WISPs live here. Expect `interference_dbm` to be
  your real capacity limit, not distance.
- **CBRS** (3.55–3.7 GHz, US) — lightly licensed. Coordinate through a SAS and
  you get real interference protection. Worth the paperwork at any scale.
- **Licensed microwave** (6, 11, 18, 23 GHz) — for backhaul trunks. Coordinated,
  protected, and the right answer for a link you cannot afford to lose.
- **Cellular bands** are licensed to carriers. Transmitting on them needs their
  spectrum, which in practice means an MVNO or roaming agreement, not a radio you
  bought. "Using a cell tower" almost always means *leasing vertical space on the
  structure* and putting your own radios on it — which is normal, and what the
  tower companies sell.

Set `tx_power_dbm`, `antenna_gain_dbi` and `feeder_loss_db` honestly in
inventory and `planning/` will compute the EIRP you are actually radiating, which
is the number a regulator asks about.
