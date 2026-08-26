# Context and context sources

**Applies when** working with an environment's `context` and
`context_sources`. **Delimitation:** the neighbouring case is a channel's
source on an asset — the same `Source` type with opposite interval rules (a
channel's dataset source must *not* carry its own interval, it follows the
channel's publish tick; a context source *must* carry one).

`geltung: allgemein`

## Static context

`context` is a map of site-wide values every zone, script
(`moses.environment.state`) and formula (`context.<key>`) can read. A static
entry keeps its value until changed — by an editor, by
`PATCH /environments/{id}/state`, or by a script.

## Context sources

`context_sources` map a context key to a `Source` that drives it over time,
each on its own ticker (`lib/runtime/contextsource.go`):

- **Allowed: `profile` and `dataset`.** A day-cycle temperature, a replayed
  weather series.
- **Refused: `script` and `formula`** — validation answers
  `not supported for context sources`. A formula reading the context it writes
  would be a cycle; scripts already can write the context directly.
- **`interval_seconds` is mandatory and > 0** — validation answers
  `a context source has no publish tick to piggyback on, it needs its own interval`.

Replay anchors of dataset context sources persist under the series id
`"context:" + key`, so they cannot collide with channel anchors.

Nothing is published by a context source itself: it only moves the value that
channels and formulas then read on their own ticks.
