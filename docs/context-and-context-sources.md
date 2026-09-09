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
- **A dataset's origin can be `file`, `platform` or `export`**, the same
  choice a channel's dataset has. For `export` `ref` is the export id and
  `column` the export's own column name, chosen by whoever created it; the
  environment's owner must own the export, since the wrapper checks export
  access as the calling user. `window` works as it does for `platform`.
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

## Follow

`follow` (bool) and `follow_every` (a duration like `window`, default `30m`,
minimum `1m`) keep a `platform` or `export` dataset current after the initial
fetch: on `follow_every` a refresh fetches `[last fetched point, now]` and
appends what is new (rows the upstream writes late, behind the newest stored instant, are not picked up), so the replay keeps reading real time instead of stopping
at the instant the environment started or last reloaded. It applies to a
channel's dataset and to a context source's dataset alike.

Two constraints: `follow` is refused on a `file` origin, which has nothing to
poll again, and it needs `anchor: original` - a `loop` replays the frozen
window it already fetched rather than reading further. A run over a year of
following data still needs `window: 1y` on the source; `follow` only keeps the
present moving forward from wherever the window left off.

Between two refreshes the newest measurement is held: an `original` anchor is
otherwise silent for every instant after its last point, which for a following
source is nearly every tick. A source that does not follow keeps falling silent
outside its range.

**The hold is bounded at three `follow_every`.** Past that the upstream has
missed enough refreshes to count as gone, and the channel is silent again, the
way a non-following original-anchored replay is outside its range — a held
value carries no age with it, so a dead import would otherwise publish last
week's temperature forever. The loop writes one WARN when a source crosses that
bound (`a following dataset source has gone stale`, with the newest instant and
the number of empty refreshes) and one INFO when it delivers again; not one per
tick. That ties `follow_every` to how often the upstream really publishes:
following an hourly import every ten minutes leaves the channel silent for half
of every hour and reports it stale, because three cadences pass between two
measurements. Set `follow_every` no shorter than the upstream's own interval.

**`resample: distribute` does not hold at all.** It hands out the share of a
slot that one tick stands for, so holding the last sample past the end of the
series would keep integrating that sample's quantity into every slot after it —
an energy nobody measured, growing for as long as the upstream stays away. Past
its last point a distributing source is silent whether it follows or not.
`hold` and `linear` both yield the newest value and are held.

One timer per environment ticks at the shortest `follow_every` of its sources,
and every source is refreshed on its own cadence, so an hourly source next to a
minutely one is still read once an hour. A cadence that is not a multiple of
that shared tick lands on whichever tick is nearest its due instant: 90 s next
to a 60 s tick alternates between 60 s and 120 s and averages out at the 90 s it
declares. A refresh that brings no new measurement is the ordinary answer of a
source that publishes less often than it is followed and is not reported on its
own. A failed refresh is logged (WARN) and the series stays as it was; the next
tick of that source tries again. That also covers a source whose initial load
failed — the wrapper was down while the environment started: the loop fetches
the whole window again, and once that succeeds the environment is reloaded, both
to build the channel binding and because the reload is what actually stores the
series. A reload that fails changes nothing, and the next tick fetches again.

The stored series is trimmed to `window` after every refresh, so a source
followed for weeks does not grow without bound; the last two points are kept in
any case, since a replay needs a span. A backfill job reads a snapshot taken at
its start and a history run loads the datasets fresh into a generation of its
own, so a refresh cannot change the series under a reconstruction that is
already running.

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
