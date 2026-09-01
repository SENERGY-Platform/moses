# Dated changes

## Scope

**Applies when** something about an environment changes on a date and the series
is meant to carry the step — a measure that takes effect, a machine that is
replaced, a tariff that rises. **Delimitation:** the neighbouring case is a plain
edit of the document, which changes the simulation from the moment it is saved
and leaves nothing behind that says when; and the case next to that is a second
environment, which is what to reach for when the *structure* changes rather than
a value. The timeline changes numbers, never shape: no channel appears, no source
kind changes, no asset moves.

## What it is

`Environment.timeline` is a list of dated changes:

```json
"timeline": [
  {"at": "2026-10-01T00:00:00Z", "target": "channel.ch-1.profile.base", "value": 180},
  {"at": "2027-01-01T00:00:00Z", "target": "context.electricity_price", "value": 0.41}
]
```

From `at` on the target carries `value`; before the first change of a target the
document's own inline value stands. The comparison is inclusive — an instant
exactly on `at` already reads the new value — and there is no interpolation: a
measure takes effect on a date, it does not fade in.

`at` must be a whole second, and it is compared through its unix second, so the
zone it was written in decides nothing. Document order is free; the runtime sorts
the changes when it indexes them. A change in the future is explicitly allowed —
a planned measure is the case this exists for.

## One document, one time

The lookup is a pure function of the instant, and all three execution paths read
the same index: the **live** simulation, a **backfill** over a past window, and a
**history run**. A step therefore lands at the same instant in all three, bit for
bit — `lib/runtime/timeline_parity_test.go` compares the three series value by
value against a reference computed without the index at all.

A document that carries no timeline is untouched by any of this: the index is
`nil` and every resolution short-circuits to the inline value.

## What can be addressed

The target list is closed. Ids and keys may contain dots, so a target is parsed
by its **ending** against the fixed field list; a schedule state is split at the
fixed separator `.schedule.states.`, which leaves the dots to the state name.

| Target | Changes |
|---|---|
| `channel.<id>.profile.base` | the base of a profile source |
| `channel.<id>.profile.spread_percent` | its spread |
| `channel.<id>.dataset.scale` | the scale of a replay |
| `channel.<id>.schedule.states.<name>.value` | what one step of a programme publishes |
| `channel.<id>.schedule.states.<name>.spread_percent` | its spread |
| `channel.<id>.schedule.gate.threshold` | the threshold a gate compares against |
| `context_source.<key>.profile.base` | the base of the profile driving a context key |
| `context_source.<key>.profile.spread_percent` | its spread |
| `context_source.<key>.dataset.scale` | the scale of the replay driving a context key |
| `context.<key>` | a static context value |

`value` is a number: every field on the list is one.

### What is deliberately not on it

Each of these is an extension path of one sentence, not a gap:

- **Cadences** (`interval_seconds` on a channel or a source) — the runtime binds
  a ticker per channel at start, so a dated cadence means restarting runners
  mid-window, and the backfill and history grids are laid out per channel before
  the first instant.
- **`formula.expression`, `script.code`, `dataset.ref`** — these are not numbers;
  they would need a typed value and a re-compile or re-load at the instant.
- **`cumulative` on a profile or a dataset** — a flag, and flipping it mid-series
  changes what the numbers *mean* rather than what they are.
- **Schedule durations** (`duration_seconds`, `duration_spread_percent`) — the
  walk from the anchor to now sums whole cycles, so a duration that changed
  part-way would make the position depend on when the walk was made.
- **`seed`** — the whole point of the seed is that it does not move.
- **Factor arrays** (`hour_factors`, `weekday_factors`) — a list, not a number;
  it would need a value type that carries one.
- **Change-trigger thresholds** (`publish_on_change.absolute` / `.relative`) —
  reachable, but the comparison base is persisted state, so a dated threshold
  needs a rule for the reading that is already stored.
- **`dataset.scale` on a cumulative dataset** is refused outright, see below.

## Addressing, and the dots in a name

`channel.urn:infai:ses:x.1.profile.base` addresses the channel
`urn:infai:ses:x.1`, and `channel.ch-1.schedule.states.run.fast.value` addresses
the state `run.fast`. Both work because the parse cuts the *suffix* first. The
one shape it cannot read is a channel id that itself contains the literal
`.schedule.states.`, which is where the split is made.

A context source is addressed by the key it writes, not by a channel id: the two
namespaces are separate, and a change under one never reaches the other.

## The edges

- **A base change shifts the value, it does not redraw the spread.** The draw
  hangs on the seed, the channel id and the time slot alone, so doubling the base
  at one instant doubles the value at that instant exactly. A spread change
  scales the same draw. The series is continuous through both.
- **A cumulative profile bends, it does not jump.** All three paths integrate the
  *effective* base per tick, so a base change is a change of slope with no step
  in the reading — which is what a meter has to look like.
- **`scale` on a cumulative dataset is refused.** Such a reading already contains
  every loop it has counted, so scaling it from an instant on would restate the
  whole meter instead of bending it from there. Model the change as a second
  channel.
- **`scale: 0` means unscaled**, exactly as an omitted inline scale does, so a
  change back to `0` is a change and not an unset field.
- **A gate threshold takes effect at the next evaluation** of that gate, which is
  the gate semantics that was already there.
- **A channel publishing on change publishes immediately** when a dated change
  moves its value past the trigger threshold. That is intended: the value did
  move, and the trigger reports movement.

## A governed context key is read-only

A key with a `context.<key>` change on it is a declared function of time, and it
is read-only for everything outside the timeline — from the start, not from its
first change, or a value set in between would jump back when the change arrives.
Four places read it, and all four go through the same layer:

| Reader | What it sees |
|---|---|
| a formula input `context.<key>` | the declared value of the instant it computes for |
| a schedule's gate | the same |
| a script (`moses.environment.state.get`) | the same, and the key is *not* seeded |
| `GET /environments/{id}/state` | the same, laid over the answer |

`PATCH` on the same path compares against exactly that answer, which is what
keeps the two endpoints a round trip.

Moving it is refused rather than silently overwritten:

- `PATCH /environments/{id}/state` answers **400**, naming the key — but only for
  a value that actually differs from the declared one. Submitting the value the
  read handed out changes nothing and is accepted, so the documented round trip
  holds: read the state, edit one key, send the whole thing back. The comparison
  is numeric, so a whole number that arrives as an integer counts as the float it
  stands for. (A change that takes effect between the read and the write is
  therefore refused — the value really did move, and the message names the key.)
- A script's `moses.environment.state.set` is dropped, with one warning per key.
  The warning is issued again after an edit of the document, since what the
  timeline governs is a property of the definition.

A change that names both an unknown zone or asset id and a governed key is
answered once, with both halves in the body.

The key must be declared in `context` — that is where the value before the first
change comes from — and it must not be driven by a `context_sources` entry, which
would overwrite the dated value on its next tick.

**One known corner:** the flush persists the raw state map, and the snapshot lays
the declared value *over* the answer rather than writing it into that map. So the
stored value for a governed key is whatever was last written there — the seeded
one, in practice. If the timeline is later deleted, that stored value reappears as
the live value. Nothing reads it while the key is governed.

## Limits

- **10000 changes** per environment. An imported document is untrusted input, and
  the index sorts and searches every entry it carries.
- `at` must lie between the years 2000 and 2262, which keeps every instant inside
  the range int64 nanoseconds can express and catches the mistyped year.
- One value per target per instant: two would leave which of them applies to
  nothing the document says.

## Where it lives

- `lib/domain/timeline.go` — the model and `ParseTimelineTarget`.
- `lib/domain/validate.go`, `checkTimeline` — the third second pass, next to the
  gate and sub-meter passes, since a target names something from the top of the
  document that is only known once the tree has been walked.
- `lib/runtime/timeline.go` — the index, the `effective*` resolutions and the
  context layer.
