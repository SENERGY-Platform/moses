# Context and context sources

## Scope

**Applies when** working with an environment's `context` and
`context_sources`. **Delimitation:** the neighbouring case is a channel's
source on an asset — the same `Source` type with opposite interval rules (a
channel's dataset source must *not* carry its own interval, it follows the
channel's publish tick; a context source *must* carry one).

## Static context

`context` is a map of site-wide values every zone, script
(`moses.environment.state`) and formula (`context.<key>`) can read. A static
entry keeps its value until changed — by an editor, by
`PATCH /environments/{id}/state`, or by a script — **unless the environment's
`timeline` governs it**, which is the one exception to that sentence.

A key the timeline carries a `context.<key>` change for is a declared function
of time and therefore read-only from outside: the state endpoint answers `400`
for a value that differs from the declared one (an unchanged value is accepted,
so reading the state and sending it back still works), and a script's `set` is
dropped. Such a key must be declared here and must *not* also be driven by a
context source, which would overwrite the dated value on its next tick
(`docs/dated-changes.md`).

## Context sources

`context_sources` map a context key to a `Source` that drives it over time,
each on its own ticker (`lib/runtime/contextsource.go`):

- **Allowed: `profile` and `dataset`.** A day-cycle temperature, a replayed
  weather series. A profile's hour and weekday factors are local hours of the
  process (`TZ`), see `docs/backfill.md`.
- **Refused: `script`, `formula`, `aggregate` and `schedule`** — validation
  answers `not supported for context sources`. A formula reading the context it
  writes would be a cycle; scripts already can write the context directly; an
  aggregate needs an asset to sum below; a schedule writes the name of its state
  into an *asset* state, and a context key has none.
- **`interval_seconds` is mandatory and > 0** — validation answers
  `a context source has no publish tick to piggyback on, it needs its own interval`.

Replay anchors of dataset context sources persist under the series id
`"context:" + key`, so they cannot collide with channel anchors.

Nothing is published by a context source itself: it only moves the value that
channels and formulas then read on their own ticks.

## A context key as a switch

A context key is also what a `schedule` channel's `gate` reads: the schedule
restarts its programme at the first state whenever the key rises above the
gate's threshold, and stands the machine still while it does not (`docs/schedule.md`).
The key has to be declared here — as a static `context` entry or as a
`context_sources` entry — or validation refuses the gate. A script and the state
endpoint write context keys too, so an undeclared key is not necessarily dead;
what it is, is unreadable, a machine waiting for something nothing in the
document mentions. Declaring an initial `0` is the whole cost of it. The schedule
only *reads* the key; nothing about it makes the schedule a context source.
