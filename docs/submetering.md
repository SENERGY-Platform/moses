# Submetering

**Applies when** an asset's `submetered_by` is set. **Delimitation:** the
neighbouring case is a `formula` source that sums specific channels by naming
them explicitly (`docs/context-and-context-sources.md` covers `Source` more
generally) - anyone who wants a total over a hand picked list of channels
writes that formula instead. `submetered_by` is for the opposite shape: a
structural meter tree, where the assets that feed into a device are named by
the tree itself rather than by a formula that has to be kept in step with it.

`geltung: allgemein`

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
  modelled per site, so a reference that leaves its top level zone is in
  practice an asset filed under the wrong zone. Refusing it puts that mistake
  in front of its author; accepting it would quietly build a meter tree
  spanning two sites, which nobody asked for and nobody would notice. (The
  mirror is one graph per environment, not one per site - the boundary is a
  statement about the model, not about the graph.)
- **No cycle.** A→B→A would sum forever if anything ever walked it. Nothing
  does yet - the graph mirror is best effort and the repository rejects a
  graph containing a loop outright - but a stored cycle is a modelling mistake
  nothing would otherwise surface, so it is refused at validation time.

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
been walked, not per asset while walking it. It has to be: the graph has one
node per platform device, two assets may publish through one device, and each
of them may say where that device hangs. Five cases fall back to the ordinary
zone edge:

- **No asset carrying the device is sub-metered** (`submetered_by` empty) -
  the existing behavior, unchanged.
- **The target has no platform device of its own.** There is nothing for the
  edge to attach to; the asset behaves as if it were not sub-metered for the
  purpose of the graph, though its `submetered_by` value is untouched and
  still available to whatever sums the channels.
- **The target shares this asset's own device.** Two assets may publish
  through one platform device (`lib/graphs/build.go`); pointing the edge at
  that shared device here would be an edge from a node to itself, and the
  repository rejects the whole graph over one self-loop. Falling back to the
  zone keeps the rest of the graph intact.
- **Two assets on one device name targets on different devices.** The node has
  one parent to give and nothing in the document ranks one of the two
  statements above the other; following the first one and dropping the second
  would be a rule nobody wrote down, and would make the answer depend on the
  order the assets happen to appear in. Neither is followed. Splitting the two
  assets onto devices of their own is what makes both statements expressible.
- **The edge would close a cycle of device edges.** Cycles are refused per
  asset at validation time (above), but a shared device folds several asset
  edges into one node: A on device X metered by B on device Y, and B in turn
  metered by C, which publishes through device X again, is a document in which
  no asset meters itself - and it still asks for X→Y and Y→X. Since the
  repository rejects a graph containing a loop outright, the whole mirror
  would be lost over it. Exactly one edge per cycle is given up, the one
  leaving the device that appears first in the document, so the same document
  always breaks the same edge.

## The unmeasured remainder

An asset that stands for what a meter reads beyond its sub-meters - the
remainder - is not a special case of the model, just an ordinary asset with
`submetered_by` set.

It does get a node. Through the API every asset ends up with a platform
device: `external_type_id` is mandatory, and `provisionDevices` creates the
missing device before `mirrorGraph` builds the graph (`lib/api/environment.go`),
so by the time the graph is built the remainder has a device like every other
asset. The deviceless fallback above is not the way to keep it out of the
graph; it is what happens without a configured device catalog, and for
documents built by hand in a test.

What can be left out is the *publication*, not the node: a channel with a
`source.interval_seconds` and no `interval_seconds` evolves its state on its
own tick and publishes nothing (`lib/runtime/runtime.go`, `runSplitChannel`).
A remainder modelled that way carries its share inside the simulation, is
readable by the formulas of the other channels, and sends nothing to the
platform.

## Limits

**A device shared across sites hangs at the zone of its first carrier.** The
node of a platform device belongs to the first asset in the document that
carries it, which is what keeps an unchanged document mapping to an unchanged
graph. If a second asset in a different top level zone publishes through that
same device, the node stays where the first one put it, and a sub-metering
edge pointing at that device follows it across the site boundary - even though
`submetered_by` itself may not cross one. This is the pre-existing behavior of
node placement, not something sub-metering introduced, and it is left as it
is: moving the node would move it for every reader of the graph, and a device
shared across two sites is a modelling oddity to begin with.
