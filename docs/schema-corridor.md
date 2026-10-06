# Schema: your own radio corridor (home to a moving car)

This is the build where your own fibre reaches a moving vehicle over radios you
own, with no carrier and no per-byte cost. It works only inside coverage you
build, so it suits a fixed route — a daily commute, a run between two properties
— not general driving. On unlicensed or lightly-licensed spectrum it is entirely
legal.

## Why 900 MHz, not 5 GHz

A moving vehicle cannot hold a narrow beam. A 5 GHz dish has a ~10° beam, so five
degrees of heading change halves the signal, and a car changes heading by that
constantly. At 900 MHz you can put an **omnidirectional** antenna on the car:
no aiming, and it penetrates foliage and terrain that 5 GHz will not. You trade
peak bandwidth for the ability to move, which is the whole point here.

US: 902–928 MHz is unlicensed ISM, usable at up to 36 dBm EIRP for
point-to-multipoint. Elsewhere the sub-GHz band and limits differ — check your
regulator. The architecture does not change.

## Architecture

```
  home ──fibre──▶ router ──▶ 900 MHz sector on a mast (13 dBi, downtilted)
                                   │  902–928 MHz, 20 MHz channel
                                   ▼
                          902–928 MHz omni on the car roof (6 dBi)
                                   │  PoE
                                   ▼
                          router in the car (NAT, DHCP, wifi for devices)
```

## Range (computed, not quoted)

Sector 13 dBi / 27 dBm to a car omni of 6 dBi, 20 MHz channel, 10 dB fade margin:

| Conditions | ≥10 Mbps | ≥25 Mbps | ≥50 Mbps |
|---|---|---|---|
| Open rural | 20.7 km | 10.4 km | 4.6 km |
| Mostly clear | 10.4 km | 5.2 km | 2.3 km |
| Suburban (buildings, trees) | 4.1 km | 2.1 km | 0.9 km |

Re-run for your own site and gear:

```sh
cd planning
python3 -m bsplan ptp --distance 3 --radio nanobeam-5ac --freq 915 \
  --channel 20 --mast-a 12 --mast-b 2 --clutter 10
```

## The constraint that actually decides it: mast height

Range on paper is useless if the two ends cannot see each other over the earth
and the trees. At 900 MHz, flat ground, both ends:

| Corridor | Clear path | Through 12 m trees |
|---|---|---|
| 2 km | 7.7 m | 19.7 m |
| 5 km | 12.5 m | 24.5 m |
| 8 km | 16.3 m | 28.3 m |
| 12 km | 20.9 m | 32.9 m |

The radio horizon for a 10 m home mast and a 2 m car antenna is ~18.9 km, so
beyond that no power helps — the planet is in the way. In practice trees and
terrain bind well before the horizon does. A 5 km suburban corridor needs a
~25 m mast at home to clear a treeline, which is a tower, not a chimney bracket.

## Bill of materials

| Part | Example | ~Cost |
|---|---|---|
| 900 MHz sector AP | Cambium ePMP Force 300-900 AP / Baicells 900 | $400–700 |
| 900 MHz sector antenna | 13 dBi, 65–90° | included or $150 |
| 900 MHz car radio + omni | ePMP Force 300-900 SM + 6 dBi omni | $250–400 |
| Home mast or tower | the real cost; see above | $500–5,000+ |
| Car router | GL.iNet or MikroTik, PoE in | $60–120 |
| Surge protection, grounding, cable, mounts | — | $400 |
| **Hardware, excluding the mast** | | **~$1,600–2,000** |

The mast is the budget. A rooftop 3 m pole is cheap; a 25 m tower with a concrete
base and guy wires is most of the project, and may need planning permission.

## Honest verdict

This wins only when all three hold: a **fixed route**, within a few km of home,
with enough data that pre-staging cannot absorb it. For a commute to a workshop
two villages over, it is genuinely good and pays for itself against years of
cellular bills. For general driving it does not, because coverage stops at your
mast's reach.

For anything beyond the corridor, the cheaper answer is the rest of this repo: a
small carrier SIM for live traffic, the content cache for everything that can be
pre-staged over fibre, and `mobilelinkd` to keep bulk off the metered link. Use
`usagewatch` first to see which of these you actually need.
