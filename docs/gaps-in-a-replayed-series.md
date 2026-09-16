# Gaps in a replayed series

## Scope

**Applies when** a dataset source replays a series that real measurement
produced - a platform timeseries, an export, an uploaded file - and that series
has holes. `max_gap` on the source is the widest distance between two
neighbouring points the replay still bridges.

**Delimitation:** the neighbouring case is a series that has *ended* rather than
one with a hole in it. Outside its range an `anchor: original` source is silent
on its own, and a following source holds its newest point for a bounded while;
both are older mechanisms and `docs/publish-on-change.md`, "A dataset that has
run out", describes what a channel then does. `max_gap` is about the middle of a
series, not its edges.

## The problem it solves

Resampling has no opinion about distance. `linear` draws a straight line between
two neighbouring points whether they are ten minutes or two days apart, `hold`
keeps a value for as long as nothing follows it, and `distribute` spreads a
sample across whatever slot it opens.

A weather series with a 27 hour hole made that concrete: the last point before
the hole was 0 W/m² before sunrise, the first one after it 20 W/m² on a bright
morning, and the line between them carried a slowly rising irradiance through
two nights. The photovoltaic plant reading that context key generated at three
in the morning. Nothing was wrong with the resampling - the document had simply
never said how far it was willing to let the source be trusted.

## What it does

```json
"dataset": {
  "origin": "export",
  "ref": "…",
  "column": "temperature_2m",
  "resample": "linear",
  "anchor": "original",
  "max_gap": "2h"
}
```

A duration like `window` and `follow_every`. **Absent means no bound**, which is
what every document written before the field carries and therefore the behaviour
described above. The shortest bound that means anything is one second: the
instants of a series are whole seconds, so anything below that is exceeded by
every distance there is.

Inside a gap wider than the bound the source **produces no value**. That is the
same "nothing to play" a series outside its range produces, and the consumers
already treat it the way they treat that:

- a **channel without a change trigger** stays silent for the width of the gap;
- a **channel with a change trigger** republishes its last reading on the
  heartbeat, exactly as it does for a series that has run out;
- a **context key** keeps what it last held, because nothing overwrites it.

None of the three invents a number. A consumer that wants something better than
the last value - a clear sky estimate for an irradiance hole, say - builds that
itself; the bound is only the point at which the runtime stops guessing.

## It holds for every resample mode

`linear` is the obvious case. `hold` is bounded on purpose too: the mode says a
state persists between two samples, but `max_gap` is the document's statement
that past this distance it no longer vouches for the source at all, and a state
a day and a half old is as much a claim about an unobserved instant as an
interpolated one. Keeping the rule the same across the modes also means that
changing `resample` cannot silently start bridging holes again.

`distribute` is measured differently: it hands a tick its share of the slot a
point opens, so an oversized slot has already thinned the sample out at the
slot's first instant. There the bound is compared against the slot rather than
against the distance across the instant being asked for.

## What is not a gap

- **The instant of a point.** It is a measurement, whatever the source did around
  it, so `hold` and `linear` answer with it even when a hole begins there.
- **The seam of a loop.** With `anchor: loop` the replay runs over the series and
  starts again; the distance from its last point back to its first is not a
  distance between two neighbours.
- **The newest point of a following source.** While the follow hold lasts, the
  clamp to that point answers with the measurement, not with a hole.

## One line per gap

A source that stands in a gap reports it once, keyed on the instant the gap
opens - not once per tick. A source sits in one gap for as many ticks as it is
wide, and a history run walks those in milliseconds.

The line is worth having because the failure is otherwise perfectly quiet: the
27 hour hole above sat in a production series for months, and what eventually
found it was somebody plotting a January night.

The backfill reads through the same bound and produces the same silence, but
does not log - it rebuilds a whole window at once, where a line per instant says
nothing a summary would not say better.

## Where this came up

A DWD weather export replayed by a demonstrator. The hole was in the source
archive rather than in anything the platform did, and it will happen again with
any real series; the fix belongs at the point where the runtime decides what a
missing stretch means, not in the one series that happened to have one.
