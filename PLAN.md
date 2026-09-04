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

### 2. Compute in chunks, checkpoint after each

The window is worked in chunks of one virtual hour: compute the chunk, publish
it, wait for every ack, then write a checkpoint. The checkpoint is a document in
a new collection `history_jobs`, one per environment: window, started at, state,
position, counters per channel, plus the simulation state at that position as a
snapshot separate from the live state document. On service start, jobs in state
`running` are resumed: environment put under history, state loaded from the
snapshot, heap rebuilt from definition and position, run continues. `GET` serves
the document after a restart, `DELETE` marks it cancelled. A parity test shows
that a run interrupted at an arbitrary chunk boundary and resumed produces the
same series bit for bit as an uninterrupted one.

Known gap, documented: timescale has no uniqueness on time, so a crash inside a
chunk republishes that chunk on resume - at most one virtual hour of duplicates.
Deleting rows is not available (SNRGY-4663).

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
