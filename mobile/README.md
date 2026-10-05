# mobilelinkd

Chooses which WAN a vehicle or a site uses, moment to moment, and defers the
traffic that can wait until a link worth using appears.

## Why

Plain failover treats every uplink as interchangeable. They are not. A corridor
radio you built is free and available only where you built it; a cellular SIM
works nearly everywhere and is billed by the gigabyte; the access point at home
is very fast and available for ten minutes a day.

Two observations follow, and the whole design sits on them:

1. **Most bytes are not urgent.** Map tiles, podcasts, photo backup, package
   updates, offline course material and system images are the bulk of the volume
   and nobody is waiting on any of them. Navigation, messaging, calls and the
   page somebody is looking at are a small fraction of the bytes and all of the
   urgency.
2. **You do not need coverage everywhere, only on your routes.** People drive the
   same handful of corridors almost always. That turns an impossible problem
   into a tractable one.

Separating urgent from deferrable means a scarce or expensive link only ever
carries traffic that justifies it. On a school uplink the effect is larger than
on a vehicle: one slow line can serve a classroom if the fifty-megabyte
curriculum download happens overnight instead of during a lesson.

## How

Three traffic classes, each with its own link decision:

| Class | Waits for | Example |
|---|---|---|
| `live` | nothing; takes the best link available, cost ignored | calls, navigation, a page being loaded |
| `bulk` | a link that is cheap enough and fast enough | podcasts, backups, updates |
| `idle` | a free link only | system images, media libraries |

Selection scores each link on measured throughput, loss, latency and cost, then
applies hysteresis so a vehicle at the edge of coverage does not thrash between
relays. That is the same logic the airtime scheduler uses to move a subscriber
between sectors, for the same reason: **a moving vehicle is a terminal that
moves**, and flapping is the same failure in both.

Every decision carries a reason, because "why is this slow" and "why is my data
gone" are the two questions this exists to answer.

## Hardware this assumes

The band matters more than the brand. At 5 GHz a high-gain dish has a beam about
10° wide, so five degrees of heading change halves the signal and a moving
vehicle cannot hold the link. **At 900 MHz you can use an omni**, which has no
aiming problem at all and penetrates foliage and terrain that 5 GHz will not.
You trade bandwidth for the ability to move.

| Role | Gear | Range |
|---|---|---|
| Corridor coverage | Cambium PMP 450i 900 MHz, or similar sub-GHz PMP | 10–20 km NLOS, ~100 Mbps shared |
| Vehicle / site radio | matching subscriber module with an omni | same |
| High-speed stops | ordinary 5 GHz PtP or Wi-Fi at home, the garage, the school | 200+ Mbps for minutes at a time |
| Gap filler | cellular SIM | everywhere, billed by the gigabyte |

Run `mobilelinkd` on something with a real CPU: a Raspberry Pi, an x86 mini PC,
or a mid-range OpenWrt router. **Not on a 16 MB-flash travel router** — a Go
binary will not fit.

## Running it

```sh
go build -o bin/mobilelinkd ./cmd/mobilelinkd
./bin/mobilelinkd -config var/config.json
```

Routes are dry-run by default and only applied when `apply_routes` is explicitly
true. Changing the default route on a device you reach over the network is a good
way to lose it.

Queue something deferrable:

```sh
curl -X POST http://127.0.0.1:8075/v1/queue -d '{
  "id": "curriculum-week-12",
  "url": "https://example.org/material.zip",
  "dest": "/srv/content/material.zip",
  "class": "bulk",
  "size_bytes": 52000000
}'
```

It transfers when a link allowed for bulk traffic appears, resumes from a partial
file if coverage is lost mid-way, and stops retrying a permanently broken URL
rather than paying to attempt it on every link that comes up.

`GET /v1/status` shows every link's measured state, the decision for each class
with its reasoning, and the queue.

## What this is not

This is not novel. Multi-access steering is standardised as **3GPP ATSSS**
(Release 16+), multipath transport as **MPTCP** (RFC 8684), opportunistic
store-and-forward as **delay-tolerant networking** (RFC 4838), and handover as
**IEEE 802.21**. Commercially, Peplink, Cradlepoint, Dejero and LiveU have sold
bonded cost-aware multi-WAN for years.

What is genuinely missing is not the idea but an open, cheap, operable version of
it. The commercial products are priced for enterprises and carriers, and the
standards assume you are one. Community networks — Zenzeleni in South Africa,
Guifi.net in Spain, AlterMundi in Argentina — are bottlenecked on software and
skills, not on physics or patents.

That is the gap this aims at.
