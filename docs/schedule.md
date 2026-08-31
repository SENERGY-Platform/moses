# The schedule source

## Scope

**Applies when** a channel is to follow a machine programme rather than a
pattern: a plant that idles, sets itself up, runs and idles again, with a
different power draw in each step and the name of the step readable from
outside. Set `source.kind: "schedule"` on a sensor channel with an
`interval_seconds`. **Delimitation:** three neighbours are close enough to be
picked by mistake.

- `profile` is a *pattern over the clock* - hour and weekday factors. It has no
  states, nothing it publishes is named, and it cannot be started by anything.
- `script` is what a schedule deliberately is not: a schedule has **no
  transitions and no conditions**. A programme that reacts to a measurement is a
  script and stays one.
- `dataset` replays what actually happened. A schedule declares what is supposed
  to happen.

Not available for context sources, see the last section.

## The shape

A milling machine that runs a 40 minute programme, all day:

```json
{
  "interval_seconds": 10,
  "source": {
    "kind": "schedule",
    "schedule": {
      "state_key": "programme",
      "states": [
        {"name": "idle",    "duration_seconds": 600,  "value": 400},
        {"name": "setup",   "duration_seconds": 300,  "value": 2000,
         "duration_spread_percent": 20},
        {"name": "running", "duration_seconds": 1500, "value": 9000,
         "spread_percent": 5, "state_writes": {"air_demand": 120}}
      ]
    }
  }
}
```

Every ten seconds the channel publishes the value of the state it is in, writes
that state's name into the asset state under `programme`, and writes
`air_demand` - 120 while it runs, **0 while it does not**. After the last state
the first one follows again.

A forklift that charges once per shift, and only while the shift is on:

```json
{
  "interval_seconds": 60,
  "source": {
    "kind": "schedule",
    "schedule": {
      "state_key": "charger",
      "run_once": true,
      "gate": {"context_key": "shift", "threshold": 0},
      "states": [
        {"name": "charging", "duration_seconds": 5400, "value": 7200},
        {"name": "charged",  "duration_seconds": 60,   "value": 0}
      ]
    }
  }
}
```

`run_once` means the last state is **held** rather than the cycle starting over,
which is the shape of a job rather than of a running plant. With a gate, every
opening starts a new single pass.

## The gate

`gate` names a context key and a threshold. The gate is **open while the key
reads strictly greater than the threshold**, so the default threshold of 0 fits
the 0/1 shift calendar a context source writes without anybody having to think
about it.

The key is read with the same leniency a formula input has: a key nothing has
written yet, or one carrying something that is not a number, reads as **0** and
therefore as closed. A gate that defaulted to open would run every declared
machine of a site through a shift nobody scheduled.

**Every rising edge starts the programme over at the first state.** That is the
whole reason the gate exists: a schedule that merely paused would resume in the
middle of whatever it was doing, and the morning after a break would look like
the middle of the previous afternoon. The morning peak of a site is then a
consequence of its programmes rather than a shape somebody drew into a profile.

While the gate is closed the channel publishes **0.0**, writes the name **`off`**
into its state key and writes **0 for every key any of its states declares**. A
gated schedule may therefore not have a state called `off` - validation refuses
it, because "the machine is standing still" and "the machine is in the state its
author called off" would be the same string.

The key a gate names has to be declared in the environment, either in `context`
or as a `context_sources` entry. This is a rule about the document saying what it
does, not about what may write the key: a script writes context keys through
`moses.world.state.set` and `PATCH /environments/{id}/state` writes them too, so
an undeclared key is not necessarily dead - it is unreadable, and whoever opens
the document sees a machine waiting for something nothing in it mentions.
**Declare it in `context` even when a script or the state endpoint writes it - an
initial `0` is enough**, and it is what the validation message asks for.

**A flank is noticed on the next evaluation**, not at the instant it happens: the
channel is what looks at the context, and it looks on its own interval. For a
shift calendar that is irrelevant; for a gate somebody flips by hand it means the
programme starts within one `interval_seconds`.

## The state writes, and why they are a union

`state_writes` declares further asset state values a step stands for: the air a
running machine consumes, the setpoint it asks of its hall. They are ordinary
asset state, so a formula reads them through `asset.<key>`, the live state
endpoint returns them, and a dashboard tile shows them.

Every key **any** state of the schedule declares is written on every evaluation.
A key the running state does not declare is written as **0**. Without that union
the air demand of a machine that was running would keep standing while it idles,
and nothing in the document would ever say that it stopped.

The name of the running state is written under `state_key`, which is mandatory
and explicit. A key nobody chose is a key nobody finds, and the whole difference
between this source and a script is that the running state is not an anonymous
number.

## It is a pure function of seed, anchor and clock

Durations and values vary, and both draws are hashes rather than a random
stream, for the reason `profile` uses one: a stream's position would depend on
how many ticks had happened before, so a restart would replay different values.

- **`duration_spread_percent`** varies the length of a step **per cycle**. It is
  drawn once for the whole step - a step whose own end kept moving while it ran
  would be a step nobody could measure - and it is floored at one second.
- **`spread_percent`** varies the published value **per evaluation step**, the
  way a profile's spread does. A perfectly flat value would give a channel
  publishing on change nothing to see between two state transitions, and a real
  machine's power draw is not flat while it runs.

## It survives a restart

Where a programme stands is persisted per channel (`RuntimeState.schedule_runs`):
the anchor the walk starts at (`start_unix`), how many whole cycles lie behind it
(`cycle_offset`), the instant the current pass began (`pass_unix`) and whether the
gate was open. Without it a deployment would look like every declared machine of a
site setting itself up at the same moment, and a gated one would sit closed until
the next rising edge - for a shift calendar, the next morning.

The anchor of **every cycling** schedule, gated or not, is **rolled forward** by
the whole cycles it has consumed, so the walk from the anchor to now stays short
however long the environment runs. The cycles that roll-forward drops keep being
counted in `cycle_offset`, and the per-cycle duration draw is taken on that
absolute count - which is what makes the roll-forward invisible in the values
rather than a slow drift. It is pinned by a test over a dense time series, for
both shapes (`TestRollingTheAnchorForwardMovesNeitherThePositionNorTheDraws`).

A **gated** schedule needs one number more, because its durations are salted per
pass: two mornings do not set up in exactly the same number of seconds. That salt
is `pass_unix` and **not** the anchor - written when the run is created and again
on every rising edge, never moved afterwards. Salting on the anchor instead would
mean redrawing the whole running shift on every roll; not rolling a gated run at
all would mean walking from the rising edge again on every evaluation, and a gate
that never closes is not exotic - a 24/7 line is a context key held at 1. A run
persisted without `pass_unix` adopts its anchor as the salt once, on the next
evaluation, before that anchor starts moving.

A **run_once** schedule does not roll: it has no second cycle to advance into.

**A clock that ran backwards** - or an anchor written by a machine whose clock was
ahead - clamps the programme to its first state. Any other answer would be a
position nobody can derive.

## Publishing on change composes without a special case

A schedule computes when the channel publishes, so it carries no
`source.interval_seconds` and a `publish_on_change` trigger supplies its
`evaluate_interval_seconds` - exactly the shape `profile`, `dataset`, `formula`
and `aggregate` have. A state transition is then published the moment the
evaluation notices it, instead of on the next heartbeat.

## It is not backfilled

A schedule stands where its persisted anchor and, with a gate, the live context
put it. Neither exists for a past moment, so a reconstructed window would be a
different programme than the one that ran - see `docs/backfill.md`.

## Validation rules

Every rule refuses a document that would look like a declared machine cycle in
the editor and be something else in the data.

- The channel must be a **sensor with an `interval_seconds`**, and
  `source.interval_seconds` must be **0**: a schedule computes when the channel
  publishes.
- **At least one state**, at most **256**. The runtime walks all of them on
  every evaluation.
- **Names** are non-empty, carry **no leading or trailing whitespace** and are
  **unique**: the name is the only thing a reader has to tell two steps apart,
  and it is written into the asset state verbatim - `" idle "` is what a formula
  would have to compare against while every human reading the document sees
  `idle`. `off` is refused for a **gated** schedule.
- **`duration_seconds` > 0** and at most a year. A step of no length is never
  reached, and an unbounded one would overflow the sum the walk takes.
- **`duration_spread_percent` in [0, 100)** - at 100 a cycle could draw a step of
  no length at all - and **`spread_percent` ≥ 0**.
- Every number is **finite**: value, both spreads, the gate threshold and every
  state write. NaN and infinity are what a generated document produces from a
  division, and neither survives the round trip through bson.
- **`state_key`** and every **state write key** are non-empty, carry no leading
  or trailing whitespace and no `.` or `$`, the rule every other state key
  follows. The key is written into the asset state exactly as it stands, so a
  stray space is a value nothing reading the document can address.
- A **gate** names a non-empty context key without surrounding whitespace - the
  runtime looks it up verbatim - and that key is **declared in `context` or in
  `context_sources`**.
- **No key has two writers on one asset.** The asset state map is flat and
  shared, so a `state_key` or a write key may not be the **channel id** of a
  channel of the same asset - that is exactly where a cumulative profile stores
  its meter reading - may not collide with the keys of **another schedule** of
  the same asset, and may not be both the state key and a write key of the same
  schedule. A collision produces no error at runtime: the last writer of the tick
  wins, and the loser is a reading that silently stops moving.

## Rejected alternatives

- **Transitions and conditions per state.** It would make this a state machine
  language: every condition needs a value to read, an editor to express it and a
  cycle check nobody can do statically. Anything conditional stays `script`.
- **A configurable name for the closed state.** A dashboard reading two
  environments has to be able to compare them, and one document calling it
  `aus` and the next `standby` would make that impossible.
- **A `schedule` context source.** A schedule writes the name of its state into
  an *asset* state, and a context key has no asset. A gate reading the context
  that the same schedule writes would be a cycle on top of it.
- **Keeping the anchor in `RuntimeState.anchors`.** That map holds one int64 per
  channel, and a run is three values. Widening it would have touched the replay.
- **A backfill of the gate-less case.** Possible in principle - a free running
  schedule is a pure function of seed and anchor - but the anchor is runtime
  state, so a window before the anchor exists has no defined position. It is
  left out rather than half done.

## Limits

- **A script writing the same asset state key is not caught.** Validation can see
  the keys a schedule declares, but not the ones a script writes at runtime.
  Whoever writes both owns the collision.
- **A removed schedule leaves the value under its `state_key` behind.** The prune
  drops the channel's entry from `schedule_runs`, but the asset state is not
  touched, so the name of the state that ran last stays there as a string nobody
  writes any more. It is harmless until the key is reused: give a later
  **cumulative profile** the channel id that string sits under, and the runtime
  reads the non-numeric value as no reading at all and starts the meter from 0.
  Validation cannot see it - it is live state and not a document. There is no way
  to remove a state key: `PATCH /environments/{id}/state` merges and never
  deletes, so the remedy is to not reuse the key, or to set it to the number the
  new writer should start from.
- **The gap after a very long downtime is closed over the next few
  evaluations.** The walk is bounded (`maxScheduleCycleWalk`, about a million
  cycles); beyond that the rest of the gap is folded into the cycle the walk
  stopped at, and the roll-forward closes the remainder on the following
  evaluations. It is unreachable for anything but stored state that is years old
  or a clock that jumped by years.
- **The value is a bare number.** The name of the state travels in the asset
  state, not in the published event: a platform service carries the reading its
  device type declares.
- **Re-adding a gate does not fire a rising edge.** A run that carried on
  gate-less keeps `open` standing, so when a document edit puts a gate back on
  the channel while that gate reads open, the programme continues on its old
  pass instead of starting over - the edge fires the next time the gate
  actually closes and opens. Deterministic, and only reachable by editing the
  gate off and on again.
