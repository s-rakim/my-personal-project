# Wire contracts

Five languages touch the same data. Without one authoritative description of
that data, they drift, and the drift shows up as a subtly wrong bandwidth grant
at 2am rather than as a build failure.

Everything here is the source of truth. If a struct in Go, a class in C++, a
dataclass in Python, a record in Java or a DTO in C# disagrees with a file in
this directory, the file is right and the code is wrong.

| File | Produced by | Consumed by |
|---|---|---|
| `scheduler-problem.schema.json` | Go control plane | C++ solver, Python planner |
| `scheduler-plan.schema.json` | C++ solver | Go control plane, C# NOC console |
| `inventory.schema.json` | Java OSS (provisioning) | Go, Python, C# |
| `provisioning-event.schema.json` | Java OSS | Go control plane |

## Conformance

`make conformance` round-trips the fixtures in `fixtures/` through every
language's serialiser and diffs the result. A language that cannot reproduce a
fixture byte-for-byte after a parse-and-emit cycle has drifted.

## Rules for changing a schema

1. Additive changes only, on a running network. Readers must ignore unknown
   fields; every implementation here does.
2. Never repurpose a field name. Add a new one and retire the old one a release
   later.
3. All rates are megabits per second, `*_mbps`, decimal. All airtime is a
   dimensionless fraction of one second in `[0, 1]`. All times are RFC 3339 with
   an explicit offset. Getting bits/bytes or Mbps/MBps wrong is the single most
   common unit bug in networking code, so the unit is always in the field name.
