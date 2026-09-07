# Plan: history runs in minutes, restart-safe, remembered (SNRGY-4655)

Status: agreed 2026-09-04, in progress. Remove this file when all three steps
have shipped; the surviving decisions go to `docs/history-run.md` first.

## Goal

A year-long history run of a site the size of the Musterwerke demonstrator
(69 channels, 16.8 million steps) takes minutes instead of six and a half hours,
survives a pod restart, and `GET /environments/{id}/history` still knows the run
afterwards. A run over a window that already holds readings is refused unless
the caller forces it.

## Today

`runHistory` (`lib/runtime/history.go`) drains a heap of instants and calls
`historySend` -> `publishAt` -> `PublishEventAt` for every value, which blocks
until the Kafka ack (`platform_connector_lib.Sync`), about 530 values per second.
The run registry is `Runtime.histories`, a map in memory; a restart forgets every
run and the endpoint answers 404. State reaches Mongo only in `finishHistory`.
The backfill (`lib/runtime/backfill.go`) shares the publish path, the change gate
and the bookkeeping shape, but carries no state.

## Steps, each shippable on its own

### 0. Run the scripts on goja — shipped 2026-09-07

Measured 2026-09-07 on the Musterwerke document with a publisher that costs
nothing: 0.55 ms per publish step, 2 h 21 min per year, and over 95 % of it is
otto. Three quarters of that is not the script but the timeout: with
`vm.Interrupt` set, otto calls `runtime.Gosched()` on every expression and
statement node, which shows up as 40 % of the process in scheduler churn. A
fresh `otto.New()` per run and its allocations are the next 30 %. Compiling once
and reusing the vm does not help while the interrupt stays (4.2 ms against 4.5 ms
on the heaviest script), and dropping the interrupt leaves a `while(true)`
holding the environment mutex forever.

goja runs the same script in 0.39 ms against 4.48 ms, the light ones in 15 µs
against 267 µs, with the timeout kept: its `Interrupt` is a flag the vm checks
itself, armed by `time.AfterFunc`, no goroutine per run. All 27 scripts of the
demonstrator compile on it; two were checked value for value against otto.

The change: `run` takes a `*goja.Program` compiled once per channel at
generation build (`channelBinding.script`, next to `code`), starts a fresh
`goja.New()` per run so every run has fresh globals exactly as today, arms the
timeout after the mutex is taken, and maps `*goja.InterruptedError` to
`ErrScriptTimeout`. goja hands integral numbers to Go as `int64` where otto gave
`float64`; `state.set` and `service.send` normalise every number to `float64`,
so state maps, `sameStateValue`, the persisted state and the payload stay as they
are. `lib/state/jsvm.go` keeps otto until the legacy package dies, which is also
what allows a differential test running the same scripts on both engines.

New dependency: `github.com/dop251/goja` (MIT), transitively
`dlclark/regexp2/v2` (MIT), `go-sourcemap/sourcemap` (BSD-2-Clause),
`google/pprof` (Apache-2.0). Expected: a year in about 15 minutes of compute,
after which the publish pool bounds the run again.

### 1. Publish in parallel, keep the order per channel

The loop computes as today but hands every value to a bounded queue. A pool of
workers (config, default 16) sends through the existing synchronous path, so ack
and error stay per value. A channel is pinned to a worker by a hash of its id, so
the order inside one channel is preserved; the change gate and its comparison
base depend on that. The published/silent/failed counters are booked when the
ack arrives; the invariant published + silent + failed = steps stays exact and
is asserted by a test. The backfill gets the same pool. No change to the
connector lib. Expected: a year in 20 to 40 minutes, bounded by ack latency times
parallelism.

### 2. Compute in chunks, checkpoint after each — shipped 2026-09-07

The window is worked in chunks of one virtual hour: compute the chunk, wait for
every ack (`pool.Drain`, `historySettleAll`, exactly as at the end of a pass),
then write a checkpoint. Everything is quiescent at that boundary, which is what
makes the checkpoint complete.

**One document per environment in a new collection `history_jobs`**, behind a
repository interface next to `States`: the status as `GET` serves it, the
definition the run was started against (written once), and per checkpoint the
position, the tick of every grid keyed by identity (`context:<key>`,
`<channel id>`, `<channel id>:publish`) rather than by index, the per-channel
memory the state does not carry (the `pending` value of a split channel, the
heartbeat gap of a change-trigger channel, the frozen holds of its faults, the
three counters), the value cache behind formulas and aggregates, and the state
snapshot. The flusher keeps writing `states` as it does today; for a resume only
the checkpoint counts.

**On service start**, after the environments are up, every job in state
`running` is resumed: the environment is put under history exactly as
`StartHistory` does, the generation is built from the stored definition (the
datasets are loaded fresh), state, cache and channel memory come from the
checkpoint, the heap is rebuilt from the ticks, and the run continues. The
handover at the end reads the current definition, so an edit made during the
run still takes effect there. A job whose environment is gone is closed as
cancelled. `GET` and `DELETE` fall back to the document when the registry knows
nothing; the document is deleted with the environment.

A parity test shows that a run interrupted at an arbitrary chunk boundary and
resumed produces the same series, reading for reading, and the same end state
as an uninterrupted one. The engine takes an optional resume point and a
checkpoint function so a test can interrupt after the n-th checkpoint.

Known gap, documented: timescale has no uniqueness on time, so a crash inside a
chunk republishes that chunk on resume - at most one virtual hour of duplicates.
Deleting rows is not available (SNRGY-4663). The drain per chunk costs the run
some minutes over a year; measured with the profile test, the chunk size stays a
constant until that measurement says otherwise.

### 3. Refuse an occupied window

Before starting, the service asks the timescale wrapper whether any managed
device of the environment already has readings in the first day from `from`.
If so, POST answers 409 naming the devices, unless the body carries
`force: true`. The first day is what tells the two relevant cases apart - an
earlier run, or a smoke run whose window reaches into live data - while the end
of any window of an environment that ever ran live always holds readings.
`force` is allowed for both cases; the demo setup service will use it on purpose.

## Assumptions

- kafka-go's writer tolerates concurrent synchronous calls; the pool relies on it.
- Worker count is configuration with default 16.
- The backfill takes step 1 only: it has no state to checkpoint.

## Not doing

- No deletion of timescale rows (no endpoint). Rewriting a demo is delete the
  environment and its devices, then recreate - the demo setup service's job.
- No leader lock; rollout overlap is covered by `strategy: Recreate`.
- No change to the live loop, no web-ui (SNRGY-4658).

## Risk

Step 1 changes concurrency on the publish path; step 2 changes state handling,
where a defect corrupts the handover to live. Both are covered by the existing
parity tests plus the new resume parity test. Rollback is the image tag in
rancher-2-defs.

## Verification

`go test -race ./lib/runtime/ ./lib/api/`, the docker integration suite before
the merge, the OpenAPI drift check, the full gate run. Then on production: a
two-hour smoke run on a throwaway copy of the demonstrator with throughput
measured, and a pod kill mid-run to watch the resume. `docs/history-run.md` gains
pipeline, checkpoints, persistence and the 409; the Confluence page MOSES follows.
