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

## How far will a link actually go

Run it rather than trusting a spec sheet:

```sh
cd planning
python3 -m bsplan ptp --distance 5 --mast-a 6 --mast-b 6 --obstacles 15
```

The ordering of that output is the point. **The radio is almost never what limits
a link; the planet and the trees are.** A LiteBeam pair has enough link budget for
tens of kilometres, but two 6 m masts can only see 20 km of smooth earth, and the
Fresnel zone needs real clearance above everything in between.

Over flat ground, before a single tree:

| Path | Earth bulge at midpoint | Mast height needed, both ends |
|---|---|---|
| 1 km | 0.0 m | 2.2 m |
| 5 km | 0.4 m | 5.2 m |
| 10 km | 1.5 m | 8.3 m |
| 20 km | 5.9 m | 15.6 m |
| 30 km | 13.2 m | 25.1 m |

Add the height of whatever stands in the path. A 15 m treeline halfway along a
5 km hop turns a 5.2 m requirement into 20 m, which is the difference between a
roof mount and a tower.

In exchange, what you get when the path *is* clear (LiteBeam pair, 40 MHz, quiet
band):

| Distance | Modulation | Throughput |
|---|---|---|
| 1 km | 1024QAM 5/6 | 250 Mbps |
| 5 km | 256QAM 5/6 | 200 Mbps |
| 12 km | 64QAM 5/6 | 150 Mbps |
| 20 km | 64QAM 2/3 | 120 Mbps |
| 30 km | 16QAM 3/4 | 90 Mbps |

So: a few kilometres is comfortable from a rooftop, ten is a real project, and
past twenty you are building towers or finding hills.

---

## Vehicles

Two different questions hide in "can I put this on my car", and they have
opposite answers.

**Parked: yes, and it works well.** Mount a radio on a telescoping mast or a
tripod, aim it when you arrive, and you have a real link. This is how people get
usable bandwidth at a cabin or a work site. Aiming takes a few minutes with a
compass and the radio's own signal meter.

**Moving: no, and not because of the software.** These antennas earn their range
by being narrow. A LiteBeam's beam is about 10 degrees wide, so **five degrees of
heading change halves the signal.** A car changes heading by five degrees
constantly, and at 1 km the beam is only 175 m across. The gain that gives you
20 km is exactly what makes the link unusable in motion. A higher-gain dish is
worse, not better: a PowerBeam 620 tolerates 2.5 degrees.

If you want connectivity in a moving vehicle, the options are:

| Approach | Realistic range | Notes |
|---|---|---|
| Omni on the car, sector at home | ~1.5 km at 100 Mbps, ~6.5 km at 25 Mbps | needs line of sight, and buildings end it |
| Cellular modem | wherever there is coverage | the normal answer, and the right one |
| Starlink Roam | anywhere with sky | a phased array that re-aims electronically, which is how it tracks satellites while moving — the same aiming problem, solved in hardware |

The omni figures assume clear line of sight the whole way. In practice a house or
a hill between you and home ends the link, so treat the 1.5 km as an upper bound
for a flat open area rather than a usable service radius.

Two physical cautions if you do mount anything on a vehicle. A mast on a moving
car meets overhead power lines and low bridges, which is a well-known way to be
killed rather than merely inconvenienced; use a mast that folds and make lowering
it part of driving off. And a high-gain dish is a real RF emitter: a LiteBeam pair
radiates about 48 dBm EIRP, which puts the exposure limit around 70 cm from the
dish, so do not aim one into the cabin or at where people sit.

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
