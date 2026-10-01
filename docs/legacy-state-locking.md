# Locking in the legacy state runtime

## Scope

**Applies when** changing `lib/state`: the legacy worlds, rooms, devices,
services and change routines, their CRUD in `crud.go`, the `Dev*` functions
and the routine runner in `staterepo.go`, `start.go` and `jsvm.go`.
**Delimitation:** the environment runtime in `lib/runtime` has its own
per-environment mutex and history jobs and follows none of this.

## Two locks, one order

- `StateRepo.mux` (RWMutex) guards `this.Worlds` and the indexes
  (`deviceRoomIndex`, `deviceWorldIndex`, `changeRoutineIndex`, ...).
- `World.mux` guards one world while a script of it runs; every run holds it
  for its whole duration.

The order is always `this.mux` first, then `world.mux`. Nothing that holds a
world's lock may take `this.mux`: script runs and the JS api never do, and
`Start`/`Stop` take neither.

## An edit is atomic

Every mutator in `crud.go` and every `Dev*` function holds the write lock of
`this.mux` across the whole read-modify-write:

1. owner and existence are checked from the index, without converting the
   world, so a denied or unknown request stops nothing;
2. `Stop()` ends every routine and waits for running ones - the only wait;
3. one snapshot of the world is taken under its lock (`snapshotLocked`);
4. the change is applied to that snapshot and stored while the routines are
   still stopped (`storeWorldLocked`), then the worlds are swapped and the
   deferred `Start()` restarts every routine from them.

A routine run holds the world it started with and its `state.set` stores that
world, so storing before `Stop` would let a run that ends afterwards write the
old world over the update. A failed store or a validation error after `Stop`
also restarts the routines, which resets their tickers, as a successful edit
always does. Reads that only need one world (`ReadWorld`, `ReadRoom`, ...)
and `ReadWorlds` convert under the world's lock as well, because `ToMsg`
writes into the state maps.

## What bounds a wait

- A script run ends at `JS_TIMEOUT`; the interrupt fires between statements.
- `httpGet` in a legacy script shares that budget, counted from the same
  instant as the interrupt timer. A request that misses it ends the run the way
  the interrupt does (`panic(halt)`), so nothing of that statement is stored;
  other request errors and refused requests (`docs/script-limits.md`) return
  `""`. Inside `try`, otto v0.4.0 reports the halt as a `TypeError` instead of
  the timeout text.
- An edit therefore waits for running scripts once. A command behind an edit
  can still wait about two script timeouts, when a routine re-ticks just before
  `Stop` and a run is queued on the world.

## Shutdown

`Shutdown()` takes the write lock, marks the repository as shut down and stops
the routines. An edit after that returns an error instead of storing into the
closed database and restarting routines.
