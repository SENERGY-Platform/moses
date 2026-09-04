# History run

## Scope

**Applies when** an environment that is running here is to be given a past: it
is simulated from an instant in the past up to now on a virtual clock, publishes
every reading under the instant it was computed for, and **the state it arrives
at becomes the live state**. The case is operator development and measure
simulation, where a model needs an environment that already has history in it
rather than one that starts empty. **Delimitation:** the neighbour is the
backfill, which computes the same window and touches nothing the live simulation
owns. Where the backfill leaves two ramps with a step at the seam, this leaves
one; where the backfill can only reconstruct `profile` and `dataset`, this
simulates every source kind, script and schedule included. The price is that the
live simulation is suspended for the duration and its current state is thrown
away. Also not this: the `dataset` source with `origin: platform`, which *reads*
real history into a simulation.

## What it does

`POST /environments/{id}/history` with `{"from": ...}` stops the live channels,
discards the runtime state, seeds the definition's initial states and simulates
the environment from `from` to now. `GET` on the same path follows it, `DELETE`
aborts it.

There is no `to`: a run always ends at the present, because its result is the
live state. A run of any length has moved on from the present it was asked
about by the time it drains that window, so it then **chases the clock**: it
raises its end to the moment it got there and keeps simulating until the gap is
under a second, which is what keeps the handover from leaving a hole. `to` in
the status is where the run actually ended and therefore moves forward while it
does that.

Each round of that chase has to close **at least half** of what was left, or it
stops. That is what bounds the extra work: the rounds are a halving sequence and
sum to about twice the pass before them, so the chase cannot multiply the work
the step cap allowed.

## The order inside one instant

Several grids fall on the same instant, and the live runtime picks one of the
possible orders per `select`. A run fixes one, so that the same document and
window produce the same series twice:

1. **context sources** — sorted by key
2. **producing channels** — every source except `formula` and `aggregate`, in document order
3. **derived channels** — `formula` and `aggregate`, in document order
4. **the publish half of a split channel**

Everything a channel reads therefore moves before the channel. A derivation over
a derivation — a formula over a formula, an aggregate of aggregates — is the one
case that still reads the previous instant, since both sit in class 3. For an
aggregate that means its first instant publishes nothing when an input of it is
itself derived: the input has no value yet, and an aggregate does not publish a
total that is short by one of its inputs (`docs/submetering.md`). The step is
booked as silent.

## The readings go out in parallel

The loop computes as it always did and hands every reading to a pool of workers
(`PUBLISH_WORKERS`, default 16, clamped to 256) which publish it through the same
synchronous call, so an ack and an error stay per reading. **A channel is pinned
to one worker by a hash of its id**, so the readings of one channel go out in the
order they were computed; two channels may share a worker and then take turns.
What a run costs is the number of readings divided by the workers, times the ack
of one publish. That ack is the kafka produce, and where `publish_to_postgres` is
switched on the longer of it and the timescale write, which the connector starts
next to the produce and waits for afterwards.

**The hand-over never waits**, because it happens with the environment mutex
held and the flusher needs that mutex to write the state of *every* environment
in turn - a run that waited there would keep the state of its neighbours from
reaching the database. The backpressure sits between two instants instead, where
no mutex is held: the loop waits there until the whole pool holds fewer than 32
readings per worker **and** the worker of the channel it last handed one to holds
fewer than 32. The second bound is what a run over few channels needs, since all of
its readings sit on one worker, and it also bounds what an abort has to book as
silent.

The three counters are booked when the ack arrives rather than when the reading
is handed over. `published + silent + failed` is still exactly the number of
steps of the publish grid, and a status polled while the run is going may lag the
computation by the readings in flight. A reading the pool accepted but never sent
- an abort, or the service shutting down - is booked as **silent**: nothing was
attempted, so nothing was refused, and the state of the run is what says why it
is missing. Its message only stands as `last_error` while no platform refusal is
known.

**A channel publishing on change collects the answer of its previous publish
before it decides the next one.** The comparison base is advanced only by a
reading that really went out, and the heartbeat of an instant depends on whether
the change publish of that instant succeeded, so the gate never decides against a
reading whose fate is unknown - a base advanced on a refused reading would
silence the channel for a whole heartbeat and report a run as silent that in
truth failed. Thousands of other events lie between two steps of one channel, so
that answer has normally been there for a long time.

The pool is drained before the chase measures the gap and before the handover, so
the gap is measured on readings that have landed and the live channels never
start next to a history reading still on its way.

**The first reading of every service topic is produced alone.** The platform's
connector library keeps the topics it has created in a map it neither locks for
the write nor for the read every later publish makes, so a first produce next to
any other produce ends the process (SNRGY-4664). A worker that has a first
reading of a topic therefore takes a lock that excludes every other publish of
this process - the lock and the set of produced-to topics are process wide, so
two runs of two environments cannot produce a first reading at once either - and
marks the topic only once the reading really went out, since a publish that
failed for want of a token never created it.

What is left of that gap is what does not go through a pool: the live runners of
every environment, and the responses to device commands. Those still produce on
the same writer, so this reduces the odds and guarantees nothing.

## The hard condition: `senergy/time_path`

The same one the backfill has, and for the same reason: the platform's timescale
ingestion stamps a row with `time.Now()` unless the service carries the
attribute `senergy/time_path`. `docs/backfill.md` holds the details and the four
time characteristics.

The difference is what happens to a channel without it. A backfill skips it. A
run **computes it anyway and only drops its readings**: its state, its meter and
its value cache are what every formula, aggregate and script above it reads, so
leaving it out would give the whole environment a wrong end state.

Every such channel is a WARN line when the run starts and appears in
`channels` of the status with its reason. That matters more than it sounds: a
device repository that is briefly unreachable makes **every** channel of the
environment unpublishable, and the run then discards the state, publishes
nothing for hours and reports `done` — the reason is the only thing that tells
that apart from an environment that has nothing to say.

## What is replaced

Everything the runtime keeps for the environment:

| | |
|---|---|
| context, zone and asset states | discarded, then seeded from the definition's `initial_states` |
| cumulative meter readings | recomputed from zero over the window |
| replay anchors | recreated at the virtual start, so a looping dataset plays the window and the live channel carries on mid-loop |
| `last_published` of a change trigger | rebuilt by the run's own publishes, so the live channel compares against what the run last sent |
| schedule anchors | created at the virtual start; a gated programme starts at the rising edge of its context key **inside** the window |
| the value cache behind formulas and aggregates | filled by the run's own ticks |
| the offsets of an injected `meter_exchange` | rebuilt over the window and handed over, so the live channel continues on the new register (`docs/injected-faults.md`) |
| a context key the timeline governs | seeded with the value it stands at at `from`, and read against the virtual instant from there (`docs/dated-changes.md`) |

A value that survived from the live simulation would be a value from the future,
which is why none of it is carried.

## Invariants

- **State `done` means the live simulation is running again**, on the state the
  run arrived at. The handover flushes that state, reads the definition again,
  starts the channels and only then turns the status.
- **One run per environment**, and a run and a backfill exclude each other in
  both directions. Both are `409`.
- **The state the run arrived at is on disk before a live tick can move it.** The
  handover forces that write rather than relying on the dirty flag, and the
  flushes of one environment are serialised, so the final state is what lands
  last.
- **The same document and the same window produce the same series and the same
  end state**, with the order above. The same limits as the backfill apply:
  `httpGet` in a script and `time.Local` are outside it, and so is how far the
  run chases the clock at the end.

## While a run is going

| | |
|---|---|
| `PATCH /environments/{id}/state` | `409` — the change would be thrown away with the state |
| `GET /environments/{id}/state` | `200` with `running: false` and `history_running: true` |
| a second run, or a backfill | `409` |
| a device command | dropped, with a WARN. A command already in flight when the run starts is waited for, since it does not run on the environment context |
| an edit to the definition | accepted and stored; it takes effect when the run ends, since the handover reads the definition again |
| deleting the environment | aborts the run, and it is not restarted |
| shutting the service down | aborts the run; the partial state is flushed |

## Limits

- **`from` must lie in the past**, at least **one minute** and not more than
  **366 days** ago. The lower bound is there because the operation is
  destructive: nobody discards a live state to reconstruct a second of it.
- **Twenty million simulation steps** per run, counted across every channel and
  context source of the environment before anything is stopped, and asked again
  against the definition that is actually going to be run. A channel publishing
  on change counts on its evaluation grid, a split channel on both of its grids.
  Fifty channels on a minute grid over a year is 26 million and is refused:
  start later, or widen the intervals. The steps the run adds while it chases
  the clock are not counted, and are bounded instead by the halving above: at
  most about twice the pass they follow.
- Every reading is published under `Sync` qos, so one publish costs a kafka
  produce ack - and where `publish_to_postgres` is on, the longer of that and
  the timescale write beside it rather than the sum of the two. The publish pool
  divides that by its workers.
- **Since the pool, the loop's computing bounds a run, not the acks.** Measured
  on the 69-channel demonstrator with 16 workers: 0.65 ms per simulation step,
  about 1,500 steps and 1,150 readings a second, a year in roughly three hours —
  where the synchronous path took 1.4 ms per step and six and a half hours. The
  loop is one goroutine and most of a step is script execution in otto, so more
  workers change nothing further; the next gain has to come from the compute
  side.

## Operating it

`DELETE /environments/{id}/history` aborts. The run stops at its next step and
hands the environment back, which leaves the live simulation on the partial
state — a consistent state of an earlier instant, not a rollback. `failed` and
`cancelled` mean the same thing for the state: it is what the run had reached.

The run registry lives in memory. A restart forgets every run and `GET` then
answers `404`, exactly as it does for a backfill.

Kafka retention applies as it does to a backfill: a record carrying a historical
timestamp may be considered expired at once. The destination is timescale, and
that row is written synchronously in the same call.

## Known gaps

- **A run that publishes slower than half of real time does not close the seam.**
  The chase gives up when a round no longer halves the gap, because such a run
  never would; `position` in the status then says how much of it is still open,
  and the live simulation starts there. Widening the intervals or shortening the
  window is the way to a seam that closes.
- **No resume after a crash.** The flusher keeps writing during the run, so a
  crash leaves the live simulation on the virtual state of the last flush — a
  consistent state of an earlier instant. Starting the run again is the way back.
- **A run is not idempotent.** Running the same window twice writes every row
  twice; the `409` prevents it concurrently, not sequentially.
- **A failed reading is counted and named, not retried.**
- **A reading an injected fault suppresses is counted as silent, never as
  failed**: nothing was attempted, so nothing could be refused
  (`docs/injected-faults.md`).
- **A run that panicked reports no per-channel breakdown**, only the counters the
  progress reports left behind: the engine returns nothing in that case.
- **The virtual clock has no state history of its own.** The run produces the
  state at the end of the window, not a series of states along it, so a snapshot
  of an intermediate instant is not available afterwards.
