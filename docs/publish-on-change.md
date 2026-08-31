# Publishing on change

## Scope

**Applies when** a channel is to behave like real metering hardware, which does
not send on the clock alone: an Eltako meter sends cyclically every ten minutes
*and* on a step of 0.1 kWh, a Tasmota reading head the same way. Set
`publish_on_change` on the channel and its `interval_seconds` becomes the
heartbeat instead of the publish grid. **Delimitation:** the two neighbours are
`source.interval_seconds`, which says how often the value is *computed* and
changes nothing about when it is sent, and the plain ticker, which is what a
channel without this field is and stays. This is also not a filter over a dense
series: nothing is buffered and nothing is averaged, the value is simply not
sent when it did not move. Not available for context sources and actuators, see
the last section.

## The shape

```json
{
  "interval_seconds": 600,
  "publish_on_change": {
    "absolute": 0.1,
    "relative": 0.02,
    "evaluate_interval_seconds": 10
  }
}
```

That channel computes its value every ten seconds, sends it whenever it moved
by more than 0.1 in its own unit or by more than two percent, and sends it in
any case at most ten minutes after the last time it sent anything.

## What counts as a change

Both thresholds are **ORed**: whichever one is exceeded first sends the value. A
threshold left at zero is an unused one, not a threshold of zero, and a trigger
with neither is refused - it would be a plain ticker wearing the name of an
event driven channel.

The comparison is against **the value last published**, not the value last
computed. That is what makes a slow drift visible: 100 → 105 → 120 at ten
percent publishes 100 and then 120, because the 5 is not lost but kept and the
20 crosses. Against the last computed value neither step would ever cross and
the ramp would never be sent at all.

The relative threshold **multiplies rather than divides**: the deviation is
compared against `relative × |last published|`. At a last published value of 0
that product is 0, so **every deviation from zero is a change** - which is the
reading a meter starting from zero has to produce, and it falls out of the
arithmetic instead of being a special case.

A value that is not a **finite** number never counts as a change - **NaN and
both infinities**, which is what a script sending `1/0` or a formula dividing by
a zero input produces. This is an explicit rule: NaN falls out of the
comparisons on its own, but `|±Inf - last|` exceeds every finite threshold, so
an infinity would otherwise publish on *every* evaluation for as long as the
source stayed broken. It holds even for a channel whose very first value is not
finite - there is no comparison to fail, so the "first value is always
published" bypass below does not extend to one.

Such a value still goes out on the heartbeat, so a channel doing arithmetic on a
missing input is not silent - it just sends at the rate its `interval_seconds`
promises. And it never becomes the comparison base: every later comparison
against it would be false and the channel would fall back to the heartbeat
forever.

### The comparison is strictly greater than

A deviation has to **exceed** the threshold; hitting it exactly is not a
change. An Eltako meter stepping by exactly 0.1 kWh against `absolute: 0.1`
does *not* fire reliably: neither 0.1
nor the accumulated reading is representable in binary floating point, so the
computed deviation lands a hair above the threshold about two thirds of the time
and a hair below it the rest - measured over a hundred increments, roughly 63 of
them published.

Put the threshold **just under** the increment you expect - `0.09` for a 0.1 kWh
step - and every increment fires.

## The heartbeat

`interval_seconds` is the longest silence the channel allows, and the gap starts
again **after every publish, whatever its reason was**. A value that went out
because it moved therefore does not produce a second, nearly identical reading a
moment later.

When the heartbeat comes, the channel sends the value it last computed rather
than recomputing it - the heartbeat means "this is still the reading".

## One evaluation cadence, and it has to be the faster one

A change can only be noticed when the value is computed, so a channel needs
exactly one cadence for that, and it comes from one of two places:

- **`evaluate_interval_seconds` in the trigger**, for a source that computes
  when the channel publishes: profile, dataset, formula, aggregate, and a script
  without an interval of its own.
- **`source.interval_seconds`**, for a source that already carries one. A script
  that evolves state every five seconds is evaluated every five seconds, and the
  trigger must leave its own field at zero.

Both set is refused, and so is neither. Evaluating more rarely than the
heartbeat fires is refused as well: the heartbeat would always be first and the
trigger could never be the reason for a publish. Equal is the densest legal
shape and is allowed.

## What one evaluation stands for

The evaluation cadence is also the span one computed value covers, and three
things are cut by it (`channelBinding.stepSeconds`):

- a **cumulative profile** adds the share of its hourly rate that one
  evaluation is worth, not a whole heartbeat's worth each time.
- the **spread slot** of a profile: the draw is stable within one evaluation.
- a **distributing replay** hands out the share of a sample one evaluation is
  worth.

Without a trigger this is exactly `interval_seconds`, so every stored document
computes what it always computed.

## It survives a restart

The last published value and the second it went out are persisted per channel
(`RuntimeState.last_published`). Two things follow, and both are the reason it is
stored rather than kept in memory:

- **No burst after a deployment.** Without the stored value, every channel of
  every site would publish once on its first evaluation - a transient nothing
  in the simulation produced.
- **The heartbeat gap is not restarted either.** What was published before the
  restart still stands, so only the rest of its gap is owed. The remainder is
  clamped into `[evaluation, heartbeat]`: never before the value has been
  computed once, and never more than a full gap even when the clock jumped or
  the stored timestamp lies in the future.

A publish the platform refused is **never** remembered. The next evaluation
therefore compares against the same base and sends again, which is a retry
falling out of the arithmetic rather than a queue somebody has to maintain.

An entry of a channel that no longer has a trigger is dropped on the next start
or reload, the same prune the value cache gets - otherwise it would be written
out on every flush forever and would come back as a comparison base if the
trigger were ever added again.

## Values that are not numbers

A script may send a string or a boolean. Such a value has no distance to another
one, so the gate **fails open**: it is published on every evaluation, and the
bookkeeping is left alone so that a later numeric value still compares against
the last number that actually went out. A string-sending script with a trigger
is therefore a channel publishing on its evaluation cadence, which is loud but
never silent.

## A dataset that has run out

A dataset channel with `anchor: original` plays its samples at the wall clock
instants they carry and has nothing to say outside that range. What the two
shapes then do differs, and the difference is worth knowing before a
demonstration runs past the end of its data:

- **Without a trigger** the channel goes **silent**. The source produces nothing,
  so the ticker publishes nothing, and it stays that way.
- **With a trigger** the heartbeat **republishes the last sample of the file, for
  as long as the environment runs**. The evaluation produces nothing, so the
  value the heartbeat repeats is still the last one the file had - the heartbeat
  means "this is still the reading", and from the runtime's side that is exactly
  what it is.

Neither is wrong, and neither is a fallback for the other: a channel that stops
sending reports a data source that ended, one that keeps repeating its last
reading reports a meter whose value stopped moving. Which one a document wants
is a modelling decision. `anchor: loop` sidesteps it entirely and is what a
long running demonstration should use.

## A document that bypassed the api

Every rule above is enforced by validation. A document that carries an unusable
trigger anyway - hand written, or written for a later version of the format -
does not lose its channel: the trigger is dropped with a WARN naming the reason,
and the channel runs as the plain ticker its `interval_seconds` describes. The
same resolution is used by the backfill, so both paths always agree on whether a
trigger counts.

## Not available for context sources and actuators

- A **context source** writes a context key. It has no platform service and
  publishes nothing at all, so there is no send to make conditional; what it
  drives is read by zones and formulas whenever they run.
- An **actuator** is driven from outside and publishes no reading of its own.

## The backfill reproduces it

A reconstructed window applies the same rule to the same values, through the
same `exceedsChange`, with bookkeeping local to the job - see the section in
`docs/backfill.md`.
