# bswisp

A base station for a fixed wireless ISP: the software that turns a fibre handoff
and some radios on towers into an internet service.

It is the Starlink architecture at a scale you can build. Starlink's satellites
are relays between its gateway and its subscribers; here, towers are. Everything
below the satellites is the same problem, and the hard part is identical:
**deciding who gets how much airtime, every few seconds, across every sector.**

```
                                        ┌──────────── subscribers
   fibre ──▶ PoP ──▶ tower ──▶ sector ──┤
             │        │                 └──────────── subscribers
             │        └── relay ──▶ tower ──▶ sector ─ subscribers
             │
             └── basestationd: AAA, addressing, allocation, enforcement
```

## The idea the whole thing rests on

A sector does not have bandwidth to divide. It has **one second of airtime per
second**.

A subscriber at the cell edge running QPSK 1/2 spends eight times the airtime of
a close one running 1024QAM 5/6 to move the same bytes. Divide *bandwidth*
equally and a handful of distant subscribers silently consume the sector. Divide
*airtime* and it behaves.

Every capacity number in this repository derives from that, and the two most
useful consequences fall straight out of it:

- Your sector's link closes at 10 km but only carries a 100 Mbps product to
  1.6 km. Sell by distance band.
- Three sectors on a gigabit sell ~51 subscribers and use a quarter of the
  transit. **Spectrum binds, not fibre.** You need more radios, not more bandwidth.

## Layout

| Directory | Language | Job |
|---|---|---|
| `scheduler/` | C++17 | the airtime solver: who gets what, every tick |
| `controlplane/` | Go | AAA, addressing, sessions, dataplane, the tick loop |
| `planning/` | Python | link budgets, coverage, capacity forecasting |
| `oss/` | Java | subscribers, billing, provisioning |
| `noc/` | C# | the operations console |
| `schema/` | — | the wire contracts all of them share |

Five languages because the jobs genuinely differ, and real carrier stacks divide
along these same lines. The cost of that is contract drift, so `schema/` is
authoritative and `make conformance` fails if the Go and Python link budgets
disagree by more than 1e-6.

Every component builds with nothing but its own toolchain. No package manager
runs, nothing is fetched. A tower site is a bad place to discover a broken
dependency.

## Try it

```sh
make build
make test          # every suite, in every language
make demo          # the whole stack, driven by simulated terminals
```

`make demo` binds everything to loopback and leaves the dataplane in dry-run, so
it changes nothing on the host. Then:

- NOC console — <http://127.0.0.1:8070>
- control plane — <http://127.0.0.1:8080> (admin token `dev-admin-token`)

The simulated terminals compute what they would really see from the inventory's
RF parameters, report it, receive a grant, and then offer load bounded by that
grant. It is a closed loop, so oversubscribing a sector here shows you the real
fairness behaviour before any hardware exists.

## Where to look first

- **`scheduler/src/airtime.cpp`** — the allocator. Two phases: reserve every
  committed rate, then share the surplus by weighted max-min fairness. Sectors are
  solved deepest-site-first so a relay's load on its parent is known before the
  parent is divided up, with the relay injected into the parent sector as a
  synthetic terminal carrying the summed weight of everything behind it. That is
  what the radios actually do, and it is what naive relay models get wrong.
- **`controlplane/internal/radio/radio.go`** — the link budget and the airtime
  arithmetic, with the reasoning attached.
- **`controlplane/internal/session/session.go`** — `estimateDemand`, and the
  feedback loop it exists to avoid.
- **`noc/Alerts.cs`** — the rules that decide what needs a human, and what to tell
  them to do about it.

## Documentation

| Doc | What it covers |
|---|---|
| [`docs/personal-use.md`](docs/personal-use.md) | **Start here if this is for your own buildings** — most of this repo is overkill for that |
| [`docs/hardware.md`](docs/hardware.md) | What to buy, in what order, and why spectrum binds before fibre |
| [`docs/off-the-shelf.md`](docs/off-the-shelf.md) | What to adopt instead of writing — OpenWrt, OpenWISP, BIRD — and where this fits above them |
| [`schema/README.md`](schema/README.md) | The wire contracts and the rules for changing them |

## Before you transmit

Spectrum is licensed. You are almost certainly leasing space on a tower and
mounting your own radios in unlicensed or lightly-licensed bands, not
transmitting on cellular spectrum. `bsplan check` will tell you when a sector
radiates above the EIRP its band allows — it flagged all three sectors of the
example network the first time it ran.

And check your fibre contract: most consumer and small-business gigabit plans
prohibit resale. `docs/hardware.md` covers both.

## Status

Every component builds and its tests pass. The scheduler, the control plane, the
planner and the OSS have been run end-to-end against each other on a seeded
two-site relay network. Nothing here has met a real radio yet.
