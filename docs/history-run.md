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
case that still reads the previous instant, since both sit in class 3.

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
- Every reading is published synchronously under `Sync` qos, so the number of
  steps is the runtime of the run.

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
- **A run that panicked reports no per-channel breakdown**, only the counters the
  progress reports left behind: the engine returns nothing in that case.
- **The virtual clock has no state history of its own.** The run produces the
  state at the end of the window, not a series of states along it, so a snapshot
  of an intermediate instant is not available afterwards.
