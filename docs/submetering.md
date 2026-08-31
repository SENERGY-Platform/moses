# Submetering

## Scope

**Applies when** an asset's `submetered_by` is set, or a channel's source kind
is `aggregate` - the two halves of the same feature: the tree, and the sum over
it.

**Delimitation:** the neighbouring case is a `formula` source that sums
specific channels by naming them explicitly
(`docs/context-and-context-sources.md` covers `Source` more generally) - anyone
who wants a total over a hand picked list of channels writes that formula
instead. `submetered_by` is for the opposite shape: a structural meter tree,
where the assets that feed into a device are named by the tree itself.

## The field

`Asset.SubmeteredBy` names, by asset id, the asset whose device meters this
one too: what that asset reads already contains what this one draws or
produces on its own. It is authoring, not reconciliation - nothing on the
platform is read back to correct a wrong value, unlike `ExternalManaged` and
`ExternalRef` (`docs/device-lifecycle-of-assets.md`). A wrong value only
misrepresents this simulation's own tree.

Empty is the ordinary case and the default for every document stored before
this field existed: the asset attaches to its zone, both in validation's site
check below and in the graph mirror.

## Validation rules

`lib/domain/validate.go` checks three things about a `submetered_by`
reference, all reported at the asset's `…submetered_by` path:

- **The target has to exist as an asset.** A zone id or a channel id is not a
  valid target even if the string happens to match one - `submetered_by`
  resolves against the document's assets specifically, the same way a
  formula's channel references resolve against its channels specifically.
- **The target has to stay within the same top level zone.** A meter tree is
  modelled per site; accepting a reference that leaves its top level zone would
  quietly build a meter tree spanning two sites. (The mirror is one graph per
  environment, not one per site - the boundary is a statement about the model,
  not about the graph.)
- **No cycle.** Nothing walks the relation: the graph mirror is best effort and
  the repository rejects a graph containing a loop outright, and the aggregate
  source reads last published values instead of recursing (below). A cycle
  would make two totals that each contain the other's previous value, growing
  without bound while still looking like plausible meter readings - worse than
  an endless walk, and why it is refused at validation time.

A forward reference - naming an asset defined later in the document - is
allowed, exactly like a formula's channel references.

There is no separate check that the target has a platform device:
`external_type_id` is already mandatory on every asset
(`lib/domain/validate.go`), and an asset without a device yet is a normal,
already allowed shape (`docs/device-lifecycle-of-assets.md`). What happens to
a submetered_by reference into a deviceless target is a graph mapping
question, answered below, not a validation error.

## The graph mirror

`lib/graphs/build.go` maps a sub-metered asset's edge to the device of the
asset named by `submetered_by`, instead of the zone edge every asset gets
otherwise - so the mirrored graph reads as a meter tree (device under device)
for that edge, on top of the location tree everything else still is.

The decision is made per device node and only after the whole document has
been walked, not per asset while walking it, because the graph has one node
per platform device, two assets may publish through one device, and each of
them may say where that device hangs. Five cases fall back to the ordinary
zone edge:

- **No asset carrying the device is sub-metered** (`submetered_by` empty) -
  the existing behavior, unchanged.
- **The target has no platform device of its own.** There is nothing for the
  edge to attach to; the asset behaves as if it were not sub-metered for the
  purpose of the graph, though its `submetered_by` value is untouched and
  still available to whatever sums the channels.
- **The target shares this asset's own device.** Two assets may publish
  through one platform device (`lib/graphs/build.go`); pointing the edge at
  that shared device would be a self-loop, and the repository rejects the
  whole graph over one. Falling back to the zone keeps the rest intact.
- **Two assets on one device name targets on different devices.** The node has
  one parent to give, and nothing ranks one of the two statements over the
  other, so neither is followed - the order the assets happen to appear in
  would otherwise decide it. Splitting the two assets onto devices of their
  own is what makes both statements expressible.
- **The edge would close a cycle of device edges.** Cycles are refused per
  asset at validation time (above), but a shared device folds several asset
  edges into one node, so a document with no single-asset cycle can still ask
  for X→Y and Y→X once device sharing folds it in. Since the repository
  rejects any graph containing a loop, exactly one edge per cycle is given up
  - the one leaving the device that appears first in the document, so the
  same document always breaks the same edge.

## The aggregate source

A channel with `source.kind: "aggregate"` publishes **the sum of the channels
carrying the same `characteristic_id` on every asset whose `submetered_by`
names this channel's asset**. It has no configuration of its own - no
expression, no input list, no variant in `Source` at all - because the meter
tree already says what the inputs are. Adding a sub-meter to the tree adds it
to every total above it, which is the reason it is not a `formula` with a hand
written input list to keep in step.

Validation therefore refuses (`lib/domain/validate.go`):

- **any source variant next to `kind: aggregate`** - a `profile` block there
  would be stored and never read; the general "only one variant may be set"
  rule does not catch this, since kind aggregate plus exactly one foreign
  variant still passes it.
- **an own `source.interval_seconds`** - an aggregate sums when the channel
  publishes, like a formula computes when the channel publishes.
- **anything but a sensor with an `interval_seconds`** - there has to be a
  publish tick for it to sum on.
- **an empty `characteristic_id`** - it is what picks the summed channels out
  of the sub-metered assets, and the aggregate has no defined set of inputs
  without one.
- **a second sensor channel with the same `characteristic_id` on the asset that
  carries the aggregate** - the subsection below.
- **`aggregate` as a context source** - a context key has no asset below it to
  sum. It is refused as "not supported", not as an unknown kind.

An aggregate over an asset that nothing sub-meters yet is **not** an error: it
publishes 0 until the first sub-meter is added.

### One value per quantity per asset

An asset carrying an aggregate over a characteristic must not carry a second
sensor channel with that same characteristic. Validation refuses that at the
colliding channel (`lib/domain/validate.go`, `checkAggregateOverlap`), trimming
both sides the way the runtime matches - and the shape it refuses is the first
one an author reaches for: a distribution meter asset with both its own kWh
channel and the total over the meters below it.

That shape is wrong in two ways at once: two channels of one quantity on one
asset are indistinguishable to whoever reads that asset's readings, and an
aggregate one level up sums this asset's channels by characteristic, so it adds
the meter and the total of the same sub-tree together.

The way to model a distribution meter's own share is **an asset of its own,
sub-metered by it** - the remainder shape below, which needs no device to be a
legal document. The share is then a leaf like any other, the meter asset carries
nothing but its total, and every level is summed exactly once. An actuator is not
affected by the rule: it publishes no reading of its own and no aggregate sums
it, so a setpoint carrying the same characteristic stays storable.

### What it reads, and what that costs

`lib/runtime/environment.go` resolves each aggregate channel to a list of
channel ids once, while the definition is indexed (`aggregateInputs`), and
`executeAggregate` sums **the value each of those channels last published**
(`env.lastValues`), one level deep, never walking the tree at execution time.
Four consequences follow, and all four are deliberate:

- **A channel that has not produced a value yet counts as 0.** That is the same
  convention a formula's channel reference follows.
- **Each level of the tree costs one publish tick.** An intermediate aggregate
  is summed like any other channel, so its own total reaches the level above on
  the next tick of the channel above - a three-level tree is therefore up to
  two ticks behind its leaves.
- **After a restart the total is short by its non-cumulative children until
  each of them has ticked once**, so for at most one of their intervals.
  `lastValues` is in memory only and starts empty - except for the cumulative
  channels: a cumulative meter's reading is persisted state, and the last value
  such a channel published is that same reading, so it is restored into the
  cache at start (`lib/runtime/environment.go`, `carryLastValues`). Without
  that, the total of a cumulative chain would read 0 for a tick and then jump
  to the real sum, in a series whose whole point is that it only ever rises.
- **A reload drops the remembered value of every channel that can no longer
  produce one.** The environment object and its value cache survive a reload,
  the generation does not, so the cache is pruned against the new generation
  (`carryLastValues`): an entry is kept for a channel that has a runner and for
  a channel a command can reach, and dropped otherwise. Without the prune, an
  edit that took a channel's runner away would leave its last reading in every
  total above it for as long as the process ran - a missing summand shows up as
  a total that is too low, a frozen one looks exactly like a meter still
  reading.
- **An input whose last value is not a finite number is left out of the sum.**
  `checkStates` refuses NaN and infinity for stored states, but nothing stops a
  script from sending `1/0` on a channel. One such child would otherwise turn
  the total of every level above it into a non-number, which is a larger loss
  than one summand missing from one total; the tick logs a WARN naming how many
  inputs it left out.
- **The order of the sum is document order**, fixed at index time. Float
  addition is not associative, so a set iteration here would make the same
  document produce marginally different totals from one start to the next.

Two WARNs at index time flag when a total will be short - a plausible number is
the one kind of wrong value nobody notices:

- **A summed channel with no runner** - no publish interval and no source
  interval, so nothing ticks it. If a command can reach it, it contributes the
  value it last produced until the next command moves it; if it cannot, it
  contributes 0 for as long as the generation runs. The two are logged as
  different lines, because only the second one is silence.
- **An aggregate whose sub-metered assets match nothing** - the tree is there
  and the input list is still empty, either because the characteristics differ
  or because the sub-metered channels carry none at all. That is what a
  document migrated from the legacy format looks like: `lib/repo/convert.go`
  leaves `characteristic_id` empty, so a migrated document carries **no usable
  aggregate until the characteristics have been filled in** and publishes a
  well formed 0 in the meantime. Characteristics are compared trimmed on both
  sides, so a trailing space is not one of the ways this can happen.

An aggregate channel is **not backfilled** (`docs/backfill.md`): it follows
from the channels of the sub-metered assets, and those are backfilled.

### A remainder that is summed but not published

The remainder shape below - a channel with a `source.interval_seconds` and no
`interval_seconds` - contributes to every total above it without sending
anything to the platform: it has a runner, so it gets no WARN, and its value
is remembered before any publish is attempted, which is what makes it
countable.

A channel with no `external_ref` counts too, since the value is remembered
before the publish is refused with a warning (`lib/runtime/aggregate_test.go`)
- but the interval-only channel is what to model a remainder with, since a
missing `external_ref` is a gap the api fills in on the next write
(`provisionDevices`).

## The unmeasured remainder

An asset that stands for what a meter reads beyond its sub-meters - the
remainder - is not a special case of the model, just an ordinary asset with
`submetered_by` set.

It does get a node: through the API every asset ends up with a platform
device (`external_type_id` is mandatory), and `provisionDevices` creates the
missing device before `mirrorGraph` builds the graph
(`lib/api/environment.go`), so the remainder has a device like every other
asset by the time the graph is built. The deviceless fallback above only
happens without a configured device catalog, or for documents built by hand in
a test.

What can be left out is the *publication*, not the node: a channel with a
`source.interval_seconds` and no `interval_seconds` evolves its state on its
own tick and publishes nothing (`lib/runtime/runtime.go`, `runSplitChannel`).
A remainder modelled that way carries its share inside the simulation, is
readable by the formulas of the other channels and counted by the aggregate
above it, and sends nothing to the platform.

## Limits

**A device shared across sites hangs at the zone of its first carrier.** The
node of a platform device belongs to the first asset in the document that
carries it, which is what keeps an unchanged document mapping to an unchanged
graph. If a second asset in a different top level zone publishes through that
same device, the node stays where the first one put it, and a sub-metering
edge pointing at that device follows it across the site boundary - even though
`submetered_by` itself may not cross one. This is pre-existing behavior of
node placement, not something sub-metering introduced: moving the node would
move it for every reader of the graph, and a device shared across two sites is
a modelling oddity to begin with.
