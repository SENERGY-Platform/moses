# Backfill

## Scope

**Applies when** an environment that is running here is to produce readings for
a window that has already passed — the case being operator development, where a
model needs weeks of training data on the day the environment is defined rather
than weeks later. **Delimitation:** the neighbouring case is the live
simulation, the same arithmetic on the same document, and the two differ in
exactly one thing that changes everything downstream: a live reading is stamped
by the platform on arrival, a backfilled one carries its own timestamp in the
message. Everything below follows from that. A second neighbour is the dataset
source with `origin: platform`, which *reads* real history into a simulation;
this writes simulated history out. And this is not an import: it publishes
through the ordinary connector, so a backfilled row is indistinguishable from a
live one once it is in timescale.

## What it does

`POST /environments/{id}/backfill` with `{"from": ..., "to": ...}` computes the
environment over that window and publishes every reading with the timestamp it
would have had. `GET` on the same path follows the job.

The arithmetic is the live arithmetic. `profileValue` and `replayValue` are
functions of the instant alone, so the job is the same loop the live runtime
runs, driven by a different clock. Seed plus window therefore determine the
result: **the same document and the same window produce the same dataset**,
which is what makes a model retrainable on it.

## The hard condition: `senergy/time_path`

The platform's timescale ingestion stamps a row with `time.Now()` unless the
service carries the attribute `senergy/time_path`. That attribute's value is a
dotted path — `root.time` — to the content variable in the service's output that
holds the event time, and the ingestion reads the time from there instead.

**A channel whose service does not carry that attribute cannot be backfilled at
all.** Not slowly, not approximately: its readings would all be stamped with the
moment the job ran, which is a block of identical timestamps and worse than no
data.

**moses stays passive about this.** It does not create device type variants and
does not add the attribute to an existing device type. A device type is shared
inventory of the platform — other devices, real ones, use the same type — and
changing it from a simulator would change how their data is ingested. Making a
channel backfillable is therefore a modelling decision taken in the device
repository, deliberately, by whoever owns the type. The job reports every
channel it had to skip, with the reason, so the gap is visible rather than
silent.

## All four time characteristics work, one of them approximately

The attribute names a content variable, and that variable's characteristic says
how its value is to be read. Four exist, and **all four are backfillable since
`platform-connector-lib` `c8133d0`** (module version
`v0.0.0-20260827082232-c8133d0f997d`, 2026-08-27).

What is left is a **declaration check**: the ingestion reads a unix time out of a
number and an iso timestamp out of a string, so a time variable typed as the
other one is still refused, naming which type it would need.

| Characteristic | Declared type | What moses sends | Precision of the stored row |
|---|---|---|---|
| unix seconds | integer or float | json number | whole seconds |
| unix milliseconds | integer or float | json number | whole milliseconds |
| unix nanoseconds | integer or float | json number | **nearest 256 ns** — see below |
| unix nanoseconds | string | digits as a string | exact |
| iso timestamp | string | RFC3339 with the fractional second, in UTC | exact |

### Why a numeric nanosecond time is only good to 256 ns

The value travels as a json number, and the connector decodes json numbers into
`float64` — its json marshaller calls `json.Unmarshal` into an `interface{}`
without `UseNumber`, so `json.Number` is not reachable from moses. A `float64`
carries 53 significant bits, and a current nanosecond epoch of 1.8e18 lies
between 2^60 and 2^61, where only every 256th integer is representable. The
timestamp is therefore rounded to the nearest multiple of 256 ns: what timescale
stores is exactly `int64(float64(at.UnixNano()))`, **off by at most 128 ns**, and
by at most 256 ns after 2043, when the step doubles again.

That is accepted rather than refused: these rows are training data for operator
models sampled at seconds or minutes, so 128 ns sits far below the resolution of
anything that reads them. A device type that wants
the exact value declares its nanosecond time **as a string**: those digits are
parsed with `strconv.ParseInt` and never touch a float64. An iso timestamp is
exact for the same reason.

All of this was read from `platform-connector-lib` `psql/publisher.go` and the
converter it pins. `lib/devices/ingestion_test.go` holds it **against those
dependencies**, not against this paragraph. Two links of the chain — `flatten`
and `toNanoseconds` — are unexported and cannot be called from here, so they are
mirrored in that file and guarded by a hash of the source file they were copied
from: a bump that touches `psql/publisher.go` fails that test and asks for the
ingestion to be read again. The failure is the prompt, not a defect.

## The live path carries the time too

For a service with a usable time path, moses now sends an object — value and
time at their declared paths — on the **live** path as well, not only in a
backfill. This is not a consistency preference; it is the only shape that works
at all. The three candidates were measured against the platform's own code
(`lib/devices/ingestion_test.go` pins the second and third):

| What moses sends | What the platform does |
|---|---|
| the bare value, as before | the message cleaning rejects it — the root of such a service is a record and the value is a number — on **every** event, and each rejection notifies the device's owners |
| the object, time omitted | the cleaning defaults the time member to `null`, and the ingestion cannot read a time out of that: the row is dropped and the device's owners are notified, once per reading. Before `c8133d0` it asserted the `null` to an `int64` and **panicked**, in a goroutine of `sendEventEnvelope` that has no recover |
| the object, time filled in | the row is written under that time |

For every service **without** a time path nothing changed: the bytes are the
bare value, exactly as before, which is what keeps a migrated channel producing
what its script always produced.

## Kafka retention is not the target, timescale is

The kafka record of a backfilled event carries the historical timestamp too, via
`EventTimeProvider` (the reserved `EventMsg` entry `moses/event-time-unix-nano`,
removed again before the message is unmarshalled). That keeps the record and the
row consistent.

It also means **a topic with time-based retention may consider a backfilled
record immediately expired** and delete it in the next cleanup. That is
acceptable and intended: the destination of a backfill is timescale, and the row
there is written synchronously in the same call. A backfill is not a way to
replay history to kafka consumers.

## What is skipped, and why

| Source | Backfilled | Why |
|---|---|---|
| `profile` | yes | a pure function of the clock |
| `dataset` | yes | a pure function of the clock and an anchor |
| `script` | no | stateful: its value depends on the state its earlier runs left behind, and that state does not exist for a past moment |
| `formula` | no | derived: it follows from other channels and the context rather than being a series of its own |
| `aggregate` | no | derived: it follows from the channels of the sub-metered assets rather than being a series of its own |
| `schedule` | no | anchored: it stands where its persisted run anchor and, with a gate, the live context put it, and neither exists for a past moment |

A change trigger changes nothing about *what* is backfillable — it is a property
of the source, and `profile` and `dataset` stay the two that are — only about
*how*, see the section above.

A `schedule` channel is the one whose refusal is not structural: a free running
schedule is a pure function of the seed and its anchor, so a window could in
principle be reconstructed - but the anchor is runtime state that does not
exist before the programme first ran, and a gated schedule also follows a
context key whose past values nothing keeps. See `docs/schedule.md`.

Also skipped, each with its own reason in the status: a channel that does not
publish on a schedule, an asset without a platform device, a channel without a
platform service, a dataset that is not loaded or covers no time span, and every
service whose time path is missing or unusable.

A `formula` or `aggregate` channel is derived rather than backfilled directly:
reconstructing it would mean reconstructing its inputs at every instant, which
is exactly what the backfill of those inputs already writes, so a consumer
takes the total from the backfilled inputs (`docs/submetering.md` for the
aggregate's tree) at any moment it likes.

## A channel publishing on change is reconstructed with its trigger

A channel carrying `publish_on_change` is not backfilled on its publish interval
but on its **evaluation grid**, and the trigger decides which of those instants
produce a reading — through the same `exceedsChange` the live gate uses, which is
what makes the reconstructed series and the live one agree instead of merely
resembling each other. See `docs/publish-on-change.md`.

Three consequences are worth knowing before reading such a dataset:

- **The volume check counts evaluation steps**, not heartbeats. That is the
  honest upper bound: in the worst case such a channel publishes on every
  evaluation. A window that is fine for an hourly heartbeat can therefore be
  refused for the same channel with a ten second evaluation.
- **A suppressed instant counts as silent.** Published plus silent plus failed
  is still the number of steps of the grid, only the grid is now the evaluation
  one.
- **A heartbeat lands up to one evaluation late** when the heartbeat interval is
  not a whole multiple of the evaluation interval: the job publishes at the first
  grid instant at which the gap has run, and there is no grid instant in between.
  With a multiple — the usual shape — it lands exactly.

Like the replay anchor and the cumulative counter, the job keeps its own
comparison base and its own heartbeat, starting from nothing. The live channel's
persisted `last_published` is neither read nor written, so a job over a window
that is weeks old cannot silence or wake the live channel.

## The clock a profile is read by

A profile's hour and weekday factors are read off the instant *in the location
it carries*. The live path hands `profileValue` a local `time.Now()`; a window
arrives over the api as RFC3339 and is usually UTC. The job therefore converts
each instant to `time.Local` before computing, so both paths mean the same
clock. Without that conversion every backfilled day profile would sit at the
server's zone offset away from the live one - a silent error in the training
data. `lib/runtime/backfill_test.go` pins it.

## What the job does not touch

The live simulation of the same environment keeps running while a job does, and
the job holds no part of its state:

- **A looping dataset replay uses an anchor local to the job**, the window's own
  start. The persisted anchor is the moment the live simulation started, which
  lies after the window, and writing the job's anchor back would make the live
  channel jump weeks in its data.
- **A cumulative profile counts in a counter local to the job**, from zero. The
  live meter reading in the asset state is left where it is.

The consequence of the second one is visible in the data and is worth knowing:
**the backfilled meter reading is a ramp of its own** and does not join up with
the live one at the end of the window. A consumer that needs one continuous
meter has to offset the backfilled segment itself.

## Limits

- **366 days** per window, so that "the last twelve months" needs no reasoning
  about leap years.
- **Two million readings** per job, counted across all scheduled channels of the
  environment before the job starts. Every reading is published under `Sync`
  qos, which is what makes the postgres write synchronous, so the number of
  readings *is* the runtime of the job. The loop itself costs about 400 ns per
  reading with the platform faked away (`TestBackfillThroughput`), so the rate
  in a deployment is set entirely by the kafka and postgres writes.
- **One job per environment**; a second is answered with `409`. Two jobs over
  overlapping windows would write two rows per instant and timescale keeps both.
- A window may not end in the future. One ending within a minute of now is cut
  off at now rather than refused, because a caller computes "now" on its own
  clock.
- A window may not start before the year 2000, which also keeps it inside the
  range int64 nanoseconds can express.

## The job registry is volatile

Jobs live in memory. A restart forgets them, and `GET` then answers `404` rather
than claiming a state it cannot know.

That is deliberate rather than unfinished. A job is not resumable: resuming
would mean knowing which readings already reached timescale, and timescale
appends a second row rather than replacing one, so a wrong guess duplicates data
instead of losing it. After a restart the correct move is to look at the data
that is there and start a job for whatever is missing.

Deleting the environment cancels its job before anything else happens, so it
stops publishing to devices that are being deleted with it. A `Reload` during a
job does not disturb it: the job holds the definition generation it started with,
which is immutable, so an edit takes effect on the live simulation and the job
finishes the window it was given.

## Known gaps

- **A backfill is not idempotent.** Running the same window twice writes every
  row twice. The `409` prevents it concurrently, not sequentially.
- **A failed reading is counted and reported, not retried.** The job continues;
  the channel's status carries the count and the last message.
- **Whether a channel is backfillable is decided when the job runs**, not when
  the window is validated, because it needs a device type read per asset. A
  request is therefore accepted before it is known that every channel of the
  environment will be skipped; the status says so a moment later.
