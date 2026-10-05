# Hardware for a gigabit-fed fixed wireless network

Costs are indicative USD at the time of writing and move constantly. Treat the
ratios between items as more durable than the absolute numbers.

---

## The number that should drive your purchase order

Run the planner against your own inventory before you buy anything:

```sh
cd planning && python3 -m bsplan --inventory ../var/inventory.json capacity --plan 100 --contention 20
```

On the seeded example it says this, and the shape of the answer will hold for
your build too:

| Limit | Subscribers |
|---|---|
| Airtime across 3 sectors | ~51 |
| 1 Gbps of transit at 5 Mbps busy-hour each | ~200 |

**Spectrum binds, not your fibre.** Three sectors sell about 51 subscribers and
use a quarter of your gigabit. To consume the transit you are about to start
paying for, you need roughly **twelve sectors — four towers of three** — not more
bandwidth.

That single fact should reorder your spending. The temptation with a new gigabit
circuit is to buy a big router. You do not need one. You need radios on towers.

A second number worth internalising, from `bsplan coverage`:

| Deliverable rate | Range from one 40 MHz / 5.8 GHz sector |
|---|---|
| 25 Mbps | 7.3 km |
| 50 Mbps | 3.7 km |
| 100 Mbps | 1.6 km |
| 200 Mbps | 0.5 km |

The link still closes at 10 km. It just cannot carry a 100 Mbps product past
about 1.6 km. **Sell by distance band, not by one flat product**, or you will
promise speeds you cannot deliver to half your coverage area.

---

## Phase 0 — before any hardware

These four things have killed more WISPs than any equipment choice.

**1. Check your fibre contract.** Residential and most small-business gigabit
plans prohibit resale outright. You want a **business circuit with resale
permitted**, ideally a static /29. Ask explicitly; "unlimited" is not the same as
"you may resell this". Budget $300–900/month rather than the $80 consumer price.

**2. You are not transmitting on cellular bands.** "Using a cell tower" means
leasing vertical space on the structure and mounting your own radios. Cellular
spectrum is licensed to carriers. Your options:

| Band | Licence | Use |
|---|---|---|
| 5 GHz U-NII | none | main subscriber access; crowded, scan first |
| 6 GHz | none (AFC for standard power) | newer, quieter, less gear |
| 3.55–3.7 GHz CBRS (US) | light, via a SAS | real interference protection, worth the paperwork |
| 60 GHz | none | short multi-gigabit hops, rain-sensitive |
| 11/18/23 GHz | licensed | protected backhaul trunks |

**3. Tower leases.** $200–1,500/month per site depending on height and owner.
Negotiate before buying radios for a site you have not secured.

**4. Insurance and an entity.** Liability on tower work, and errors-and-omissions
if you are signing service agreements. Non-negotiable before the first customer.

---

## At the PoP (behind the fibre handoff)

This is your base station. It runs `basestationd`.

| Item | Suggested | ~Cost |
|---|---|---|
| Edge router / NAT | MikroTik RB5009UG+S+IN | $220 |
| Control plane server | Mini PC, N100/N305, 16 GB, 512 GB SSD | $300 |
| Managed switch | MikroTik CRS310-1G-5S-4S+IN | $220 |
| UPS | 1500 VA line-interactive | $250 |
| Rack, PDU, patch | 12U wall rack + accessories | $300 |
| **Subtotal** | | **~$1,290** |

Notes that matter:

- **The RB5009 handles gigabit CGNAT and per-subscriber queues comfortably** and
  speaks RADIUS natively, which is what `internal/aaa` targets. You do not need a
  CCR until you pass roughly 2–3 Gbps.
- **Run the control plane on separate hardware from the router.** If the box
  doing packet forwarding also runs your scheduler, a runaway solve becomes an
  outage. The whole design assumes they are separable.
- You do **not** need BGP or your own ASN on one uplink. Add them at two upstreams
  or when you want portable address space. That is a year-two problem.

---

## PoP to tower: the backhaul link

The most important purchase in the build. If this link is weak, everything behind
it is weak, and the planner will show you that as a congested trunk.

| Distance | Gear (need 2, one per end) | ~Cost/pair |
|---|---|---|
| < 3 km, clear LOS | Ubiquiti airFiber 60 LR | $600 |
| 3–15 km | Ubiquiti airFiber 5XHD | $1,400 |
| 3–20 km, needs reliability | Cambium PTP 550 | $1,500 |
| Must not fail | Cambium PTP 820 (licensed) | $6,000+ |

**Size this at twice what the tower's sectors can deliver.** Three sectors at
~88 Mbps aggregate each is ~264 Mbps; a 500 Mbps–1 Gbps link gives you headroom to
add a fourth sector without re-climbing the tower.

If the tower has fibre available, use it and skip this entirely. A relay hop is
the most fragile thing in the network, which is why the scheduler models it as a
synthetic terminal competing for its parent's airtime.

---

## Per tower

| Item | Qty | Suggested | ~Cost |
|---|---|---|---|
| Sector radio | 3 | Cambium ePMP 4600L, or Ubiquiti LTU-Rocket | $700 / $220 ea |
| Sector antenna | 3 | 90° or 120°, 5 GHz, 17–19 dBi | $250 ea |
| Backhaul radio | 1 | far end of the link above | (above) |
| Outdoor PoE switch | 1 | Ubiquiti USW-Industrial, or Tycon | $400 |
| **Ethernet surge protector** | 5+ | Ubiquiti ETH-SP-G2 or Tycon | $25 ea |
| Grounding kit | 1 | rods, bonding, lightning arrestor | $300 |
| NEMA 4X enclosure | 1 | with passive vents | $300 |
| Outdoor shielded CAT6 | 150 m | direct-burial rated | $200 |
| Mounts, hardware, weatherproofing | — | pipe mounts, 3M 2228, ties | $250 |
| **Subtotal (Cambium)** | | | **~$4,400** |
| **Subtotal (Ubiquiti)** | | | **~$2,900** |

The two things people skimp on and should not:

- **Grounding and surge protection are not optional.** A tower is a lightning rod
  with your equipment attached. One strike with inadequate bonding destroys every
  radio on the site and possibly the switch at the far end of the fibre. Surge
  protector on *every* ethernet run, both ends. This is $150 of parts against
  $5,000 of radios.
- **Do not climb the tower yourself.** Hire a certified climber with insurance.
  $500–1,500 per visit, and worth every cent. Plan your installs so one visit does
  everything.

**Cambium vs Ubiquiti:** Cambium's ePMP 4600 is 4×4 MU-MIMO and will carry
meaningfully more per sector in a congested band; Ubiquiti LTU is roughly a third
of the price and easier to find. Start with Ubiquiti if cash is tight and you have
a quiet band. Buy Cambium if `bsplan check` shows a high interference floor — that
is the case where the better radio actually pays back.

---

## Per subscriber

| Item | Suggested | ~Cost |
|---|---|---|
| CPE radio | Ubiquiti LTU-LITE / Cambium Force 400C | $90 / $250 |
| Mount | wall or roof, non-penetrating options exist | $40 |
| Surge protector | one at the building entry | $25 |
| Cable and weatherproofing | 30 m outdoor CAT6 + sealing | $40 |
| **Hardware per install** | | **~$200–360** |
| Labour | 2–3 hours | $150–300 |

At roughly $400–650 all-in per subscriber, a $99/month plan pays back the install
in five to seven months. **That payback period is why install fees and contracts
exist in this industry** — plan for churn before month six to be unprofitable.

---

## Tools (buy once)

| Item | ~Cost | Why |
|---|---|---|
| Spectrum analyser | $300–2,000 | In unlicensed bands, interference sets your capacity more than distance does. Scan before choosing every channel. Most radios have a built-in scanner (airView, Cambium's spectrum tool) which is adequate to start. |
| Cable tester + crimper | $150 | A bad field termination is the most common install fault. |
| Inclinometer and compass | $80 | Downtilt and azimuth set what your sector covers. `bsplan link` tells you the numbers; these set them on the pole. |
| Laptop with the planner | — | `bsplan link --site X --to lat,lon` before every truck roll tells you whether the address is serviceable and at what rate. |

---

## What to buy first

**Phase 1 — prove it works (~$6,000–8,500 plus the circuit).**
PoP kit, one tower, three sectors, one backhaul link, and ten CPEs. Target your
densest cluster inside the 100 Mbps design radius. Ten subscribers at $99 is
~$990/month against perhaps $600/month of circuit and tower lease. Thin, but it
proves the business and the software against real RF.

**Phase 2 — reach breakeven (~$3,000).**
Fill the first tower to 40–50 subscribers. This is pure CPE and labour spend
against existing fixed costs, so it is where the margin appears. Do not add a
second tower until the first one is near its airtime limit — the planner tells you
when, and the NOC console will start warning before it bites.

**Phase 3 — consume the gigabit (~$15,000–25,000).**
Three more towers, nine more sectors, ~150 more subscribers. Only now does the
gigabit become the binding constraint, and only now does a bigger router matter.

---

## Run the numbers against your own terrain

Everything above assumes the seeded example's clutter and interference figures.
Yours will differ, and the difference is large. Before ordering:

```sh
# Edit var/inventory.json with your real tower positions, heights and bands,
# then:
cd planning
python3 -m bsplan --inventory ../var/inventory.json check
python3 -m bsplan --inventory ../var/inventory.json coverage --sector <your-sector>
python3 -m bsplan --inventory ../var/inventory.json capacity --plan 100 --contention 20
```

`check` will tell you if a sector exceeds the EIRP its band allows, if a
subscriber sits outside every beam, or if a relay is pointed outside the sector it
relays through. It flagged all three sectors of the example network for EIRP on
its first run, which is exactly the kind of thing that is cheap to fix on paper
and expensive to fix on a tower.
