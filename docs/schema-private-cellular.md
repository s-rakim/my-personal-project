# Schema: your own private cellular network (CBRS)

This is the legal, real version of "my own cellular that my phone connects to."
You run a small base station and your own core network on spectrum you are
permitted to use, with SIMs you provision yourself. It is what universities,
farms, ports and factories deploy, and nothing in it touches a carrier's
spectrum or a carrier's keys.

## The one rule that makes it legal

Transmission is **gated by a Spectrum Access System (SAS)**. In the US, CBRS
(band n48, 3.55–3.70 GHz) is licensed-by-rule: your base station must register
with a SAS, and the SAS grants it a channel and a power limit, dynamically,
around incumbents. The radio physically will not transmit until it holds a
grant. That gate is the difference between this and an illegal transmitter, and
it is enforced in FCC-certified hardware, not by your honesty.

- **US:** CBRS via a SAS (Google, Federated Wireless, Sony). ~$2–10/month.
- **EU / UK / others:** equivalents exist — shared-access or local licences in
  3.8–4.2 GHz (UK Ofcom), 3.7–3.8 GHz local licences (Germany), n78 arrangements
  elsewhere. Check your regulator before buying; the band and the paperwork
  differ, the architecture below does not.

You provision your own SIMs with keys you generate. You are not bypassing an
authentication system — you are operating both ends of your own one.

## Architecture

```
  ┌──────────────────────────── your premises ────────────────────────────┐
  │                                                                        │
  │   your fibre ──▶ edge router ──▶ mini PC: 5G core (Open5GS)            │
  │                                      │         │                       │
  │                                      │         └── subscriber DB (your  │
  │                                      │              own SIM keys)       │
  │                                      ▼                                  │
  │                               CBRS base station (CBSD) ◀── SAS grant   │
  │                                      │         (internet: SAS coord.)  │
  └──────────────────────────────────────┼─────────────────────────────────┘
                                          │  3.55–3.70 GHz, SAS-granted
                                          ▼
                                  your device with a
                                  band-n48 modem + your SIM
                                  (phone, car modem, CPE)
```

The core (Open5GS or free5GC, both open source, free) does the authentication
and routing. The CBSD is the radio. The SAS is an internet service the CBSD
talks to for permission. Your fibre is the backhaul.

## Bill of materials

| Part | Example | ~Cost |
|---|---|---|
| CBRS base station (CBSD, Category A indoor or B outdoor) | Baicells Nova 436Q / 430i | $1,500–2,500 |
| Core-network host | mini PC, 4-core, 8 GB, 128 GB SSD | $250 |
| Programmable SIMs | sysmoISIM-SJA2 (you set Ki/OPc) | ~$12 each |
| SIM programmer | PC/SC reader | $30 |
| SAS subscription | Google / Federated | $2–10/mo |
| CPIsigned install (Cat B) | a certified installer enters the location | $100–300 once |
| Band-n48 modem for the vehicle | Quectel RM500Q-GL module + enclosure | $150–300 |

Roughly **$2,500–4,000** all in for a Category A indoor cell, more for an
outdoor Category B with real range.

## Range

A Category A indoor CBSD covers a building and its immediate surroundings —
hundreds of metres. A Category B outdoor one on a mast reaches **1–10 km** with
height and line of sight, at up to ~100 Mbps shared. Run the numbers for your
own site:

```sh
cd planning
python3 -m bsplan ptp --distance 3 --radio powerbeam-620 --freq 3600 \
  --mast-a 10 --mast-b 2
```

(That uses the point-to-point calculator as a stand-in; a real CBRS link budget
is similar at these distances, dominated by the same geometry.)

## What it does and does not get you

**Does:** your own cellular bubble around home. Your phone or car modem, with
your SIM, connects to it and gets internet over your fibre, free per byte, with
no carrier involved. Inside its coverage this is exactly "my home internet,
wirelessly, to a moving device."

**Does not:** follow you down the motorway. Coverage is your mast's coverage, a
few km at most. Past that you are back to a carrier SIM — which is why the
usage-watcher and the content cache matter: they shrink what that SIM has to
carry to almost nothing.

## Build order

1. Confirm your regulator's rule for the band, and register for a SAS account.
2. Stand up Open5GS on the mini PC, on a lab bench, with no radio. Provision one
   test SIM. Verify the core authenticates it against your own keys.
3. Add the CBSD indoors, register it with the SAS, confirm it receives a grant.
4. Put the SIM in a band-n48 device and confirm it attaches and routes through
   your fibre.
5. Only then consider an outdoor Category B install for range, which needs a
   certified professional installer (CPI) to sign off the location.

Steps 1–4 are a weekend on a bench. Step 5 is a real project.
