# Personal use: the short version

Most of this repository is for running an ISP. If you are extending your own
gigabit to your own buildings, or sharing it with a few neighbours, you need a
small fraction of it. This page is that fraction.

**The honest summary: for one to five links, the radios' own firmware does
everything. You do not need a control plane.** A pair of modern fixed-wireless
radios bridges transparently out of the box. Buy them, aim them, done.

---

## What you can ignore

| Component | Why not |
|---|---|
| `oss/` (Java) | billing and subscriber lifecycle — no customers, no invoices |
| `noc/` (C#) | an operations console for a network with operators |
| RADIUS (`internal/aaa`) | authenticating paying subscribers against a plan |
| CGNAT (`internal/ipam`) | you have plenty of RFC 1918 space and can just route |
| Contention ratios, committed rates | these are contract terms |

## What still earns its place

**`planning/` — use this before you buy anything.** It answers the only question
that matters at this scale: *will this link work, and how fast?*

```sh
cd planning
python3 -m bsplan link --site home --to 44.95,-93.09 --height 8
```

It will tell you the distance, bearing, whether the link closes, at what
modulation and rate, and how much Fresnel clearance the midpoint needs. That last
one is the thing that catches people: at 5.8 GHz over 2 km you need about 3 m of
clearance above anything in the path, so a line of trees at the halfway point
matters even when both ends are plainly visible to each other.

**The scheduler starts earning its place** only if you have more than one relay
hop, or more than about ten endpoints sharing one radio, or housemates who
disagree about who is hogging the connection. Below that, it is solving a problem
you do not have.

---

## Hardware

### One link: house to barn, cabin, workshop, or a neighbour

| Item | Qty | Suggested | ~Cost |
|---|---|---|---|
| PtP radio pair | 2 | Ubiquiti NanoBeam 5AC Gen2, or LiteBeam 5AC | $75–90 ea |
| Ethernet surge protector | 2 | one at each building entry | $25 ea |
| Outdoor shielded CAT6 | ~60 m | direct-burial rated | $80 |
| Mounts and weatherproofing | — | pipe mounts, 3M 2228 tape | $50 |
| **Total** | | | **~$330** |

Both radios are PoE-powered over the same cable that carries data, so each end
needs one ethernet run and one power outlet indoors. The pair bridges
transparently: the far building behaves as if it were plugged into your switch.
Expect 100–450 Mbps depending on distance and clutter — run `bsplan link` to find
out which.

For a very short hop with clear line of sight, a 60 GHz pair (Ubiquiti Wave Nano,
~$200 each) carries over a gigabit but needs genuinely unobstructed sight and
degrades in heavy rain.

### Several endpoints from one point

| Item | Qty | Suggested | ~Cost |
|---|---|---|---|
| Sector AP | 1 | Ubiquiti LTU-Rocket + 90° sector antenna | $420 |
| CPE | per site | Ubiquiti LTU-LITE | $90 ea |
| Surge protection, cable, mounts | — | as above, per endpoint | $100 ea |

Around $420 plus roughly $190 per endpoint. This is where you might eventually
want the scheduler, if the endpoints start competing.

### Covering your own property with Wi-Fi

That is not this. Outdoor Wi-Fi access points (UniFi U6 Mesh, ~$180) on your
existing network will serve you better than anything here.

---

## What still applies regardless

Three things do not care whether you are commercial.

**Spectrum.** Unlicensed bands exist precisely for this, so 5 GHz U-NII, 6 GHz and
60 GHz are yours to use. What does not change: you still cannot transmit on
cellular bands, and you still cannot relay through someone else's booster — see
`hardware.md` for why that is a hardware impossibility rather than a rule.

**Power limits.** EIRP caps apply to individuals too. One useful asymmetry: in the
US, point-to-point links in 5.8 GHz UNII-3 are allowed substantially more EIRP
than point-to-multipoint, because a narrow beam aimed at one receiver interferes
with less. A dedicated PtP link is therefore both simpler and legally more
generous than a sector. `bsplan check` reports the EIRP each configured radio
actually radiates.

**Lightning.** This is the one not to skip. A radio on a mast is an attractive
strike path into your house. A $25 surge protector at each building entry and a
properly bonded mast is the difference between replacing a $90 radio and
replacing your router, switch, and whatever else shares that ground.

---

## If it grows

If neighbours start asking to be connected, two things change before anything
technical does: your ISP contract almost certainly prohibits resale, and taking
money makes you a service provider with the obligations that carry. `hardware.md`
covers that transition. The software here is already built for it; the paperwork
is the harder half.
