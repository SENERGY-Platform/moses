# Plan: dataset sources over exports, and a follow mode (SNRGY-4656, SNRGY-4657)

Status: in flight, started 2026-09-09. Removed when both parts are released;
what survives goes to `docs/context-and-context-sources.md` and the vault.

## Goal

An environment can read a weather series that a platform **import** produced,
and keep reading it while it runs: the history of the import comes from its
analytics-serving export, the present from periodic re-reads. Together with a
new DWD import this lets the demonstrator run on real weather instead of a CSV
pinned into its repository.

## Part 1: origin `export`

- `domain.DatasetOrigin` gains `OriginExport = "export"`. `Ref` is the export
  id, `Column` the column name of the export (chosen by whoever created the
  export, not derivable from the import type), `ServiceRef` must be empty,
  `Window` as for `platform`.
- `timeseries.Client` sends `exportId` instead of `deviceId`/`serviceId`. The
  wrapper addresses a series by exactly one of those, so `queryElement` gets an
  `ExportId *string` with `omitempty` and `DeviceId`/`ServiceId` become
  `omitempty` too; `Fetch` and `HasReadings` take a small `Series` value
  (device+service or export) instead of the two ids. Every caller adapts:
  `fetchPlatformSeries`, the history-run occupancy check (which stays a
  device check: an export is nobody's managed device).
- The runtime loads an `export` source exactly like a `platform` one, with the
  owner's token. The wrapper verifies export access against analytics-serving
  as the calling user, so the environment's owner has to own the export; the
  docs say so.
- Validation, docs (`docs/context-and-context-sources.md`, `docs/backfill.md`
  where origins are listed) and the swagger regenerate.

## Part 2: follow mode

- `DatasetSource.Follow` (`follow`, bool) for origins `platform` and `export`,
  only with `anchor: original`; `FollowEvery` (`follow_every`, duration string,
  default `30m`, minimum `1m`). Validation rejects `follow` on `file`, on
  `anchor: loop`, and a `follow_every` outside the bounds.
- The runtime keeps the fetched series per channel and, on a timer per
  environment, fetches `[last point, now]` for every following source and
  appends what is new, deduplicated on the instant. Live replay with
  `anchor: original` then reads the newest value through the existing resample
  path (`hold` gives the last value). A failed re-read is a WARN with the next
  attempt at the next tick, never a stop.
- History run and backfill use the frozen series as today; a run over a year
  needs `window: 1y` on the source, which the docs state.
- Tests: validation cases red first; a runtime test with a fake fetcher that
  answers a longer series on the second call and asserts the appended points
  and the deduplication; a test that a fetch error keeps the old series.

## Assumptions

- The wrapper's `exportId` query returns rows shaped like a device query
  (`[time, value]` per column), as `docs`/vault notes on the wrapper say.
- The web-ui environments module needs the new origin and the follow fields
  in its editor; that is a separate ticket, not this plan.
- The DWD import itself (DAS-96) is a separate repository and plan.

## Not doing

- Switching the demonstrator to the import (DAS-97): start instant, PV factor
  calibration against the export, weather-coupled loads.
- A follow mode for `file` sources or for context sources of kind `profile`.

## Risk

Part 1 touches the request every platform-origin source sends; the request
shape test in `lib/timeseries/client_test.go` pins it. Part 2 adds a timer per
environment; it must stop with the environment and never run during a history
run. Roll back by pinning the previous tag in rancher-2-defs.

## Verification

`go test ./lib/domain/ ./lib/timeseries/ ./lib/runtime/`, the full gate run,
then an environment on the throwaway copy with one `export` source against a
real export.
