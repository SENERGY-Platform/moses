# Environment graphs

## Scope

**Applies when** an environment is stored or deleted over the environment api,
in a deployment that has a reachable device-repository. **Delimitation:** the
neighbouring case is the graph a user drew by hand in the graph view — the same
resource, the same api, opposite rule: that one is a document its author owns
and nothing overwrites it. A graph carrying the attribute `moses/environment` is
not that; it is a projection and is replaced wholesale on every save, and so
are both graphs an environment is mirrored as. The runtime path is out of scope
as well: `PATCH /environments/{id}/state` changes live values, not the
definition, and touches no graph.

## What is mirrored

Every environment is mirrored as two graphs in the device-repository, so that
applications which consume the platform's graphs — the graph view, the energy
flow evaluations — see a simulated site the way they see a real one: the
**location graph**, where things are, and the **meter graph**, which quantity
contains or supplies which (below). Both mappings live in `lib/graphs`, as pure
functions. The location graph is:

| Environment | Graph |
|---|---|
| the environment | the graph; attribute `moses/environment` = environment id |
| its name | the `name` attribute of the root node |
| a zone, at any depth | a node with the zone id, `name` attribute = zone name |
| a zone's parent | an edge from the zone's node to the parent's node |
| a top level zone | an edge to the node `root` |
| an asset with an `external_ref` | a node with `resource_type: device`, id and `resource_id` = the device id, `name` attribute = asset name |
| an asset's zone, or the device of the asset named by `submetered_by` | an edge from the device node to that node - see `docs/submetering.md` for the five cases that fall back to the zone |

## The four conventions that are contract

None of these is expressed in a schema, all of them are what the frontend
assumes, and `lib/graphs/build_test.go` and `lib/graphs/meter_test.go` pin each
one for both graphs:

- **An edge points from the child to the parent.** `from_node_id` is the
  contained thing, `to_node_id` the container. Reversing it reverses the tree
  for every reader.
- **The root node has the id `root`.** Readers find it by that id, not by
  looking for the node without outgoing edges.
- **The display name of a graph is the `name` attribute of its root node.** A
  graph has no name field of its own.
- **A device node carries the device id in both `id` and `resource_id`.** The
  frontend addresses the node by id, the repository resolves the device by
  resource id.

## The meter graph

`lib/graphs/meter.go` (`BuildMeterGraph`) maps the document onto a graph in
which an edge leads from a node to the node whose quantity contains or supplies
it. A node may have several parents, each with a weight.

| Environment | Meter graph |
|---|---|
| the environment | the graph; attributes `moses/environment` = environment id and `moses/graph` = `meter` |
| its name | the `name` attribute of the root node, with ` (meters)` appended |
| an asset with an `external_ref` whose kind is `meter` or `inverter`, that states meter parents, or that another node names as parent | a device node as in the location graph, belonging to the first asset carrying the device |
| a meter group | a node with the group id, `name` attribute = group name, no resource |
| the effective meter parents of an asset, or the parents of a group | one edge per parent, weighted |
| a node without parents | an edge to `root` with weight 100 |

The **effective meter parents** of an asset are its `meter_parents`, or, where
that is empty, its `submetered_by` with weight 100 (`docs/submetering.md`).
Zones are not part of the meter graph; sensors, actuators and machines that
state no parents and are nobody's parent are left out.

**Weights** are taken as given where the list sets them. Where every weight is
omitted the flow is split equally, and the remainder of 100 divided by the
number of parents goes one point each to the first parents in list order:
three parents carry 34, 33 and 33.

**Conversion.** An edge whose parent has `conversion: true` carries the
attribute `moses/conversion` = `true`: the medium changes there, gas to heat
or electricity, heat to water, so a calorific value or an efficiency lies in
between. Every other edge carries no attributes.

**Fallback to the root.** Dropping one parent of a list would break the weight
sum the repository checks, so a node goes to the root with weight 100 as a
whole when

- one of its parents cannot be resolved: an unknown id, an asset without a
  device, an asset on the node's own device, a group id that a device or an
  earlier group already holds, or weights the repository would refuse,
- two of its parents resolve to the same node,
- two assets carrying its device state different parents (an asset stating
  none does not count),
- it is the member earliest in document order (devices by first carrier, then
  groups) of a cycle that device sharing folded acyclic asset statements into.

The same document always yields the same graph, node and edge order included.

## Why an asset without a device is missing

An asset with no `external_ref` publishes nowhere - a helper inside the
simulation, a computed total, a placeholder for something not yet provisioned -
and there is nothing behind it a consumer of the graph could read. Assets
without a device are therefore left out; the zone they sit in is still there,
so nothing disappears from the structure.

## Why every edge of the location graph has the weight 100

Weights apportion a flow: one meter supplying two areas is 70/30. A location
topology has no such split — every node has exactly one parent and passes on
everything it has. The unweighted case is spelled `100`, not `0`, because the
repository validates the graph before storing it and rejects any edge weight
outside `1..100`, as well as any node whose outgoing weights sum to something
other than 0 or 100. "No edge carries anything" is not a graph it accepts;
"every edge carries all of it" is the same statement in a form it does.

Sub-metering (`docs/submetering.md`) does not change this: it moves *where* an
asset's edge attaches - to a device instead of a zone - not what share of the
flow it carries. Every node still has exactly one parent and still passes on
everything it has to that one parent; the weight stays 100 either way.

## The mirror is not a document

The graph is rebuilt from the environment on every save and written in full. A
node moved, renamed or removed by hand in a graph editor does not survive the
next save - two editable representations of one site would otherwise drift,
with no rule to decide which one is right. That includes the edits of a graph writer
(`docs/sharing.md`); the rights on the graph survive the rewrite.

## Who owns which graph

`external_graph_ref` on the environment names the location graph,
`external_meter_graph_ref` the meter graph. Both follow the same rules below. The
server assigns and enforces them; a value sent by a client is discarded, for the same reason
`external_managed` is (`docs/device-lifecycle-of-assets.md`): the whole document
is sent on every update, so an echoed or invented ref would let one environment
write into the graph of another.

- **An update of a stored document keeps the stored ref.** A document stored
  before the meter graph existed has no meter graph ref and gets a fresh meter
  graph on its next save.
- **A create, and a put to an id that is new here, start without one** and get a
  fresh graph. This is the case a copy of an export falls into: its ref still
  points at the graph of the original, and honouring it would have the copy
  overwrite that graph on save and delete it on delete. **A copy owns no graph.**

The ref cannot be generated in moses. A put to a graph id the repository does
not know is a permission check against a resource that has no permissions yet
and is answered with `403`; only a create without an id yields a usable id. That
is why the mirror is written *before* the environment document — the ref comes
back from the create and is stored in the same write, instead of needing a
second one. `lib/test/graph_test.go` pins this against the real service, because
nothing in the client's signatures says it.

## Failures do not fail the request

Mirroring and deleting the graphs are best effort, and each graph on its own: a
failure is a `WARN` with the environment, the graph and which of the two it is
(`kind`), it does not stop the other graph, and the request succeeds — the same trade the device
cleanup makes, and for the same reason: an orphaned or stale graph is cheaper
than a save that fails, and it is recoverable by hand. A reader being
unreachable is not something the caller of a save can act on.

The graph delete runs after the document delete, so a failed delete leaves the
environment and it keeps its graph. The graph write runs before the document
write, so a failed write leaves a graph ahead of what is stored. For an
environment that already has a ref, the next successful save corrects that graph.
For one that does not — a create whose write failed — the ref was never stored,
so the retry creates a second graph and the first is orphaned.

## Known gaps

- **A stored ref that points at a graph somebody deleted by hand is not
  healed.** The put then fails (the permissions went with the graph) and every
  save of that environment warns. Recovering would mean creating a new graph on
  a `403`, which would also create a second graph whenever a `403` was
  transient — so the case is left visible in the log rather than guessed at.
- **A create whose document write fails orphans the graph it just created.**
  The ref only exists in that request, so the retry mints another one. Bounded
  by how often the store fails, and the alternative — writing the document
  first and the ref in a second write — leaks the same way when that second
  write fails, at the price of an extra write on every create.
- **The first save of an environment stored before the meter graph existed
  orphans that graph if the document write then fails**, a `409` or a lost
  concurrent save included: the meter graph is created before the write, as on
  a create, and its ref is only stored with it.
- **The mirror inherits the concurrency gap of the document.** `PUT` is a
  read-modify-write without a version, so of two concurrent updates the loser's
  graph can be the one that stays.
