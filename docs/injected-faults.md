# Injected faults

## Scope

**Applies when** a simulated site is to show that a consumer notices broken
metering: a meter that goes silent, one that freezes on a reading, one that
sends an outlier, one that is exchanged and starts counting from zero. Set
`faults` on a sensor channel. The gain over a random generator is that the
simulation still knows the undisturbed value, so **the ground truth is
available** and the quality of a detection is measurable rather than asserted.

**Delimitation:** this is not `publish_on_change`, which decides when a *correct*
reading is worth sending; a fault disturbs the reading itself and is applied
after that decision has been prepared. It is not the timeline either, which
changes a *declared parameter* of the model from a date on — a measure that took
effect, not a defect. And it is not the profile's `spread_percent`: that is noise
on the world side, which every reader of the value sees, while a fault sits on
the measurement and only what leaves for the platform carries it. Not available
for context sources, actuators and command answers, see the last section.

## A fault sits in the measurement, not in the world

The value is computed, remembered and only then disturbed. Everything inside the
simulation therefore keeps the undisturbed reading:

- the **value cache** a formula's `channel.<id>` reads,
- the **asset state** a cumulative profile counts its meter in,
- every **aggregate** above the channel,
- the neighbouring channels of the same asset.

Nothing that was published ever flows back into the state. A frozen meter does
not freeze the site around it, and an outage on one sub-meter does not put a hole
into the total above it — which is what makes the total usable as the reference.

## The shape

```json
{
  "interval_seconds": 600,
  "faults": [
    {"kind": "outage", "from": "2026-03-02T06:00:00Z", "to": "2026-03-02T09:00:00Z"},
    {"kind": "spike", "per_hour": 0.5, "duration_seconds": 600, "factor": 12},
    {"kind": "meter_exchange", "from": "2026-06-01T00:00:00Z", "reset_to": 0}
  ]
}
```

At most **8** faults per channel. They are applied **in document order**, each one
to what the one before it produced, so overlapping windows compose: a channel that
is frozen and spiked at the same instant publishes twelve times the held value.

## The four kinds

| Kind | What it does |
|---|---|
| `outage` | nothing goes out while it lasts |
| `frozen` | the reading of the instant the occurrence began is repeated |
| `spike` | the reading is multiplied by `factor` |
| `meter_exchange` | the register restarts at `reset_to` and counts on from there |

`factor: 0` is allowed and means it: the sensor that reads nothing is a real,
named defect. `factor: 1` is refused, because it would be invisible in the series.

A `meter_exchange` is **one instant, not a window** — the new register keeps
counting, so there is nothing for a `to` to end. It only applies to a channel
whose reading counts up (`profile.cumulative` or `dataset.cumulative`); there is
no register to restart otherwise.

An outage suppresses the send and nothing else. A meter exchanged or a value
frozen while the channel is silent is exchanged or frozen when it comes back,
which is what the hardware does and what keeps the composition independent of
where the outage sits in the list.

## A window or a rate, never both

**A window** is `from` inclusive and `to` exclusive, both whole seconds, in the
same range a dated change may lie in (`docs/dated-changes.md`). Two windows
meeting at one instant do not both cover it.

**A rate** is `per_hour` occurrences of `duration_seconds` each, drawn from the
environment seed. One draw per evaluation step decides whether an occurrence
begins in it, so `per_hour × step / 3600` may not exceed 1 — more than one
occurrence per step is not something a single draw can express.

A running occurrence carries no state: it is found again by redrawing the
`ceil(duration / step)` steps it can still have begun in. That bound is **64
steps**, and it is what one evaluation of one fault costs. A defect that lasts
longer than 64 evaluation steps is a window, and the refusal says so.

The draw is over the seed, the channel id, the fault's position in the document
and the step — not over anything taken from the run. That is why the same
document produces the same defects on every path.

The position is in the draw, so **editing the list reshuffles the drawn
occurrences of the faults behind the edited one**. That is the same class as
changing a profile's `base`: an edit to the document changes what the document
produces. Dated faults are unaffected, and so is the offset a `meter_exchange`
has already captured — that one is keyed by its instant precisely so an edit
elsewhere in the list cannot reach it.

## The step is the evaluation cadence

The slot a drawn occurrence sits in is counted in `channelBinding.stepSeconds`:
the `evaluate_interval_seconds` of a change trigger, the publish interval
without one. It is the same number a cumulative profile integrates over and a
spread slot is cut by (`docs/publish-on-change.md`). Counting in the publish
interval instead would put the live channel and a reconstruction of the same
window on two different grids.

## What it does to a channel publishing on change

The fault is applied **ahead of the threshold**, which is where the three
documented effects come from:

- **A freeze falls back to the heartbeat cadence.** The held value looks
  unchanged to the trigger, so nothing but the heartbeat publishes it. That is
  what a frozen meter does, and it is deliberately not optimised away.
- **A spike publishes twice**, the outlier and the return to normal: the spiked
  value becomes the comparison base, and the next undisturbed reading is far from
  it. Real hardware does exactly this.
- **An outage on a heartbeat firing costs a full gap of silence.** The heartbeat
  timer is reset by the attempt, not by the publish — a rule that predates the
  faults — so the next reading is a whole `interval_seconds` later, not one
  evaluation.

An outage never moves the comparison base: it suppressed a reading, it did not
publish one, and a base moved by a reading nobody received would silence the
channel afterwards.

## Bookkeeping

A suppressed reading is **silent, never failed**, in the backfill status and in
the history run status alike: nothing was attempted, so nothing could be refused.
The invariant holds in both: published plus silent plus failed is the number of
steps of the channel's grid.

## The meter exchange and what a restart does

The offset the new register counts from — the difference between `reset_to` and
the undisturbed reading at the moment of the exchange — is captured at the first
reading at or after `from` and **persisted** in `RuntimeState.meter_exchanges`.
Without that, an exchanged meter would jump back to its old reading on the next
deployment. The entry is dropped again once the fault is gone from the document,
redated, or the channel with it.

It is keyed by **the instant and the channel**, not by the fault's position in
the list — which is why two `meter_exchange` faults of one channel may not carry
the same `from`. A position is not a stable identity: deleting an unrelated fault
ahead of the exchange would shift it, the stored offset would be pruned as
belonging to nothing, and the register would restart at `reset_to` on the next
reading. A backwards step in a cumulative counter *is* the signal "the meter was
exchanged", so that edit would have fabricated a second exchange nobody wrote.

A **freeze** keeps no persisted memory. A restart in the middle of a freeze
therefore takes the held value again, from the reading of the first evaluation
after the restart. The known gap is deliberate: it is one value inside one
occurrence, against a fourth stored map and a migration.

The **backfill keeps its own offsets**, like its own replay anchor and its own
meter counter: a reconstructed window never moves the register the live
simulation counts on. A **history run** builds the offsets up over its window and
hands them over with the rest of the state, so the live channel that follows
continues on the new register.

## The three paths agree

The live simulation, a backfill and a history run of one window produce the same
disturbed series. **Bit for bit**: the same `float64` values in the same order,
not equal within an epsilon — three paths that agree to six decimals are three
paths that will disagree somewhere else.

What that promise does *not* cover is the same as everywhere else in this
service: the live path's grid starts when the runner starts, while both
reconstructing paths start at the window's own `from`. Two consequences follow,
and both are about the grid rather than about the fault:

- a `meter_exchange` is captured at the first evaluation at or after its instant,
  which is the same instant on both reconstructing paths and the next live
  evaluation on the live one;
- a cumulative meter counts from zero in a reconstruction, as it does without any
  fault (`docs/backfill.md`).

`lib/runtime/faults_parity_test.go` pins the promise against a reference that
resolves nothing, over a fixture carrying an outage across a heartbeat firing and
a channel whose evaluation cadence is not its publish interval.

## Reading the ground truth

Four forms, none of which needs a line of code:

- **A formula channel** `{"expression": "x", "inputs": {"x": "channel.<id>"}}`
  publishes the undisturbed value of the faulted channel next to it.
- **An aggregate** above the asset sums the undisturbed children.
- **The asset state** carries the undisturbed reading of a cumulative profile, and
  is readable through `GET /environments/{id}/state`.
- **Bit-exactly for a past window**: backfill the same document with `faults`
  removed. Both series are functions of the seed and the window, so they line up
  reading for reading.

**A twin channel is not ground truth.** A second channel with the same profile
draws its spread under *its own* id, and the draws of two ids are independent —
including two that differ in one character. Measured on a profile with
`base: 100` and `spread_percent: 20`, a twin sits **13 % of the base away from
the original on average and up to 40 % away**, at every instant, without any
fault being injected at all. It looks like the same series in a chart and is a
different one in every number.

## The other form, and why it is not here

A meter exchange as a *new platform device* — the old one archived, a new one
created — is the other half of what happens on a real site, and it is not
available. Creating the device runs on the store path, where it would collide
with the version compare-and-swap and with the reconciliation of the managed
flag, and it cannot be reproduced retroactively for a past window at all: the
device either exists now or it does not. The register restart above is the part
that shows up in the data.

Two more limits worth naming: the timeline cannot address a fault parameter — a
target would have to name the fault by its position in the list, which reorders
under an edit — and there are no noise or drift kinds, because `spread_percent`
already covers noise, on the world side where a reader of the value should see it.

## Not available for

- **Context sources**: they write a context key and publish nothing, so there is
  no send to disturb — and every zone and formula reads the key, so a fault there
  would be a defect of the world rather than of a measurement.
- **Actuators**: driven from outside, they publish no reading of their own.
- **Command answers**: a command is answered on the spot and is not a series.
- **A channel without an interval**: there are no readings to disturb.

## A document that bypassed the api

Every rule above is enforced by validation. A document that carries an unusable
fault anyway — hand written, or written for a later version of the format — keeps
its channel and every fault that does work: the unusable one is dropped with a
WARN naming the reason, exactly as an unusable change trigger is. The backfill
resolves the faults with the same function, so both paths always agree on which
defects count.
