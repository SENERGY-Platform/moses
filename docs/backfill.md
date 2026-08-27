# Backfill

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

`geltung: allgemein`

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

## Only two of the four time units work

The attribute names a content variable, and that variable's characteristic says
what unit its number is in. Four exist. **Only unix seconds and unix
milliseconds are accepted**, and the two rejections are not caution:

- **Unix nanoseconds crashes the connector process.** The ingestion casts the
  value to `UnixNanoSeconds` and then writes `timeVal.(int64)`. For a value that
  is already in nanoseconds the converter short-circuits on `from == to` and
  returns what the json marshaller produced, which is a `float64` — so the
  assertion panics, in a goroutine of `sendEventEnvelope` that has no recover.
  Separately, a nanosecond epoch (1.8e18) is far past the point where a float64
  represents integers exactly (9.0e15), so even a corrected assertion would
  round the timestamp.
- **An iso timestamp is never read.** The ingestion flattens the message before
  it looks the time up, and `flatten` wraps every string in the single quotes it
  needs for the SQL literal. What reaches `time.Parse` is
  `'2026-01-01T00:00:00Z'`, which it rejects. The row is not written and the
  device's owner is notified — once per reading.

Both were read from `platform-connector-lib` `psql/publisher.go` at
`v0.0.0-20260826082643-802ca9df203c` and the converter it pins.
`lib/devices/ingestion_test.go` pins them **against those dependencies**, not
against this paragraph: a version that repairs either case makes that test fail,
which is the signal to lift the refusal here.

## The live path carries the time too

For a service with a usable time path, moses now sends an object — value and
time at their declared paths — on the **live** path as well, not only in a
backfill. This is not a consistency preference; it is the only shape that works
at all. The three candidates were measured against the platform's own code
(`lib/devices/ingestion_test.go` pins the second and third):

| What moses sends | What the platform does |
|---|---|
| the bare value, as before | the message cleaning rejects it — the root of such a service is a record and the value is a number — on **every** event, and each rejection notifies the device's owners |
| the object, time omitted | the cleaning defaults the time member to `null`, and the ingestion asserts that `null` to an `int64`: **panic**, in a goroutine of `sendEventEnvelope` that has no recover |
| the object, time filled in | the row is written under that time |

So a channel on a time-path service never worked on the live path before this,
and the fix for it is the same shaping the backfill needs.

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

Also skipped, each with its own reason in the status: a channel that does not
publish on a schedule, an asset without a platform device, a channel without a
platform service, a dataset that is not loaded or covers no time span, and every
service whose time path is missing or unusable.

A formula channel is the one worth a second look. Reconstructing it would mean
reconstructing its inputs at every instant, which is exactly what the backfill
of those inputs already writes — so the honest place to derive it is downstream,
from the backfilled inputs, not here.

## The clock a profile is read by

A profile's hour and weekday factors are read off the instant *in the location
it carries*. The live path hands `profileValue` a local `time.Now()`; a window
arrives over the api as RFC3339 and is usually UTC. The job therefore converts
each instant to `time.Local` before computing, so both paths mean the same
clock. Without that conversion every backfilled day profile would sit at the
server's zone offset away from the live one — a silent, entirely plausible-looking
error in the data a model is trained on. `lib/runtime/backfill_test.go` pins it.

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
meter has to offset the backfilled segment itself. Starting the job's counter
from the live value instead would make the result depend on when the job ran,
which would cost the reproducibility the whole feature is for.

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
