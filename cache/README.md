# cached

Serves content from local disk so it does not have to cross a slow, metered or
intermittent link more than once.

## What it is for

Not for speeding up a fast connection. Behind a gigabit line this does nothing
worth having. It earns its place at the far end of a *bad* link: a vehicle on a
cellular tunnel, a cabin behind a relay hop, anywhere bytes are slow or billed.

Two things it gives you there:

- **Offline availability.** Content pulled in advance is served at local speed
  with the uplink saturated, degraded, or entirely absent.
- **Fetch once, serve many.** Several devices pulling the same OS update, map
  region or media file cost one transfer rather than several.

The second is a large multiplier across thirty devices and a modest one across
three. Be honest about which you have: for a single person with a single laptop,
pre-staging is the whole value and deduplication is a rounding error.

## What it deliberately does not do

**It is not a transparent HTTPS proxy.** Caching encrypted traffic generically
means terminating TLS with your own certificate authority installed on every
client, which means decrypting everything those devices do. That is a large
thing to build in order to save some bandwidth. Content here is cached because
something asked for it to be cached.

**It does not fetch on miss by default.** Behind a slow link, a miss that
silently fetches upstream is a slow request with extra steps. The default is to
report the miss and let you decide when the bytes cross.

## How it works

Content is addressed by the SHA-256 of the bytes themselves, not by URL. The same
file arriving under three different names is stored once, and a blob is only
deleted when the last name referring to it goes. Writes land under their own hash
via an atomic rename, so a partial file can never be served as a complete one.

Eviction keeps the store under a disk budget. Pinned entries are never evicted;
after that the lowest priority goes first, and only within a priority does
least-recently-used decide. A plain LRU would discard the thing you deliberately
kept because nobody opened it this week.

Concurrent requests for the same uncached name are coalesced into one upstream
fetch. Without that, a cold cache behind a slow link is worse than no cache:
every device opens its own transfer and they compete for the bandwidth the cache
exists to conserve.

## Using it

```sh
go build -o bin/cached ./cmd/cached
./bin/cached -config var/config.json
```

Pre-stage something while you are on a good link:

```sh
curl -X POST http://127.0.0.1:8078/v1/prestage -d '{
  "name": "maps/region.mbtiles",
  "url": "https://example.org/region.mbtiles",
  "priority": 10,
  "pin": true
}'
```

Then read it back from anywhere, with or without an uplink:

```sh
curl http://127.0.0.1:8078/c/maps/region.mbtiles
```

`GET /v1/stats` reports the figure that justifies the disk:

```json
{ "hit_rate": 0.82, "bytes_served": 41e9, "bytes_fetched": 7e9,
  "bytes_saved": 34e9, "multiplier": 5.8 }
```

A multiplier of 5.8 means a 10 Mbps line delivered content as though it were
58 Mbps, for the cost of a disk.

## With mobilelinkd

These are two halves of one idea. `mobilelinkd` decides *when* bytes cross the
link and defers what can wait; `cached` makes sure they only cross *once*. One
defers in time, the other in space.

Point a deferred transfer at the cache's pre-stage endpoint and content arrives
overnight over whichever link was cheapest, then serves locally for as long as
you keep it.
