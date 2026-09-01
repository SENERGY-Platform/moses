# My Own Smart Environment Simulator (MOSES)

Simulates complete sites for the SENERGY platform: a metalworking shop with its
halls, machines and meters, publishing sensor values the platform cannot tell
from real ones. Definitions are documents, the runtime executes them, and the
platform sees ordinary devices.

## The model

An **environment** is one site. It holds **zones** (nestable: site → hall →
corner), zones hold **assets** (a machine, a meter), assets hold **channels**
(one value each, published to a platform service). Everything is stored as one
document; the runtime is restarted from it on every change.

An asset can name another asset that meters it too via `submetered_by`,
forming a meter tree the graph mirror reflects as device-under-device instead
of the usual device-under-zone — see `docs/submetering.md`.

Each channel has a **source** that produces its value:

| Kind | Produces |
|---|---|
| `script` | ES5 JavaScript per tick (see below) |
| `profile` | a day/week pattern: base × hour factor × weekday factor, with spread and optional cumulation |
| `dataset` | replay of an uploaded CSV timeseries (stored in GridFS, German CSV dialect detected, timezone parameter) |
| `formula` | an [expr-lang/expr](https://github.com/expr-lang/expr) expression over other channels and the context |
| `aggregate` | the sum of the same-`characteristic_id` channels of every asset whose `submetered_by` points here — no configuration, the meter tree is the configuration |
| `schedule` | a declared machine programme: named states with a duration and a value each, cycled deterministically, optionally started by a context key |

A sensor channel can carry `publish_on_change`: it then sends when its value
moves by more than an absolute and/or a relative threshold, and in any case
once per `interval_seconds` (the heartbeat). That is what real metering
hardware does, and the last published value is persisted so a restart produces
no burst of transients — see `docs/publish-on-change.md`.

A `schedule` channel writes the name of the state it is in into the asset
state, along with the values that state declares (the air a running machine
consumes), so a formula, the live state and a dashboard read what the plant is
doing instead of guessing it from the load. A `gate` on a context key — a shift
calendar — restarts the programme at its first state on every rise, which is
where a morning peak comes from. Where the programme stands is persisted, so a
restart continues it — see `docs/schedule.md`.

The **context** is the site-wide state every zone and formula can read
(`context.outdoor_temperature`). Static entries keep their value; *context
sources* drive entries over time — see
`docs/context-and-context-sources.md`.

Zones can carry **time constants**: a changed value approaches its target as
`target + (from-target)*exp(-elapsed/tau)`, resolved lazily when read.

All stochastic sources derive from the environment's **seed** — same seed, same
clock and same timeline, same values.

## Scripts

A script source runs with `moses` bound in the VM:

- `moses.environment.state` — the context (`get(key)` / `set(key, value)`)
- `moses.zone.state`, `moses.asset.state`, `moses.channel.input` / `moses.channel.send(value)`
- navigation: `moses.environment.getRoom(zoneId)`, `moses.zone.getDevice(assetId)`
- `httpGet(url)` as a global

`world`, `room`, `device` and `service` are aliases of `environment`, `zone`,
`asset` and `channel` — migrated legacy scripts run verbatim.

## Platform integration

- Device types come from the device-repository; an asset built from one gets
  its platform device created on store and deleted again when the asset goes —
  rules in `docs/device-lifecycle-of-assets.md`.
- Every environment is mirrored as a graph in the device-repository, so other
  applications consume a simulated site like a real one — mapping and ownership
  rules in `docs/environment-graphs.md`.
- Channels publish through `platform-connector-lib` (Kafka).
- The live state of a running environment is read with
  `GET /environments/{id}/state` and turned with `PATCH` on the same path — the
  same shape both ways, so a boundary condition can be read, changed and sent
  back. An environment that is not simulated here answers `running: false`.
- An environment can be reconstructed over a past window and published with
  historical timestamps, so a model has training data at once — conditions and
  limits in `docs/backfill.md`.
- An environment can also be *given* a past: simulated from an instant in the
  past up to now, with state, so the reconstructed meter and the live one are one
  ramp and the end state becomes the live state — `docs/history-run.md`.
- Users see their own environments; the platform `admin` role sees all.
  Ownership never transfers.

## Legacy model

The world/room/device endpoints and their Otto-based change routines
(`lib/state`) are still served for documents migrated by
`tools/migratelegacy`, but deprecated: the environments model above replaces
them, and no current client calls them.

## Dependencies

* Persists data in MongoDB (official go driver `go.mongodb.org/mongo-driver`)
* Uses the SENERGY device-repository/device-manager to manage device types and devices
* Uses permissions-v2 for resource permissions
* Publishes and consumes messages via Kafka (through `platform-connector-lib`)
* Golang library dependencies are managed by the go.mod file

## Tests

* `go test -short ./...` runs the fast unit tests only
* `go test ./...` additionally runs the integration tests, which start docker
  containers via testcontainers (kafka, mongodb, memcached,
  ghcr.io/senergy-platform/device-repository, ghcr.io/senergy-platform/permissions-v2)

## Releases

Pushing to master **is** the release — the workflow assigns the version
itself. Never tag manually: `docs/pushing-to-master-is-the-release.md`.

## Documentation

`docs/` holds the knowledge documents named above, next to the generated
`swagger.json`/`swagger.yaml` (regenerate with `go generate ./...`).
