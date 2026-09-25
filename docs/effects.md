# The effect graph

## Scope

**Applies when** reading `GET /environments/{id}/effects`: which context keys,
assets and zones drive which others, derived from the stored definition. The
derivation is `lib/effects`, a pure function of `domain.Environment`; nothing is
run and no live state is read. **Delimitation:** the neighbouring case is the
graph mirror (`docs/environment-graphs.md`), which is the *location* tree of a
site in the device-repository. The effect graph is the *data flow* between the
parts of one document and is never stored anywhere.

Access is that of `GET /environments/{id}`: `401` for a token without a subject,
`404` for a missing environment and for one the caller may not see.

## Shape

```json
{
  "nodes": [{"id": "context:shift", "kind": "context_key", "label": "shift", "static": false, "source_kind": "profile", "external_ref": ""}],
  "edges": [{"from": "context:shift", "to": "asset:a-1", "kind": "reads", "via": "script", "channel": "ch-1", "key": "shift", "count": 2}],
  "unresolved": [{"asset": "a-1", "channel": "ch-1", "expression": "E.get(k + '_v')", "reason": "the key is computed at run time"}]
}
```

**Edges point in the direction data flows, producer to consumer.** Two edges that
agree in `from`, `to`, `kind`, `via`, `channel` and `key` are one edge, and
`count` is how many references in the document make it. Nodes are sorted by id,
edges by all six fields, so the same document gives the same bytes.

| Node | Id | Carries |
|---|---|---|
| context key | `context:<key>` | `static`: declared in `context` and driven by no context source. `source_kind`: `profile`, `dataset`, or empty. `external_ref`: the ref of a dataset source |
| asset | `asset:<id>` | `zone` it sits in, `site` (its top level zone), `asset_kind` |
| zone | `zone:<id>` | `zone`, `site` - only when something reads or writes its state |
| timeline | `timeline` | only when the document has dated changes |

Every key of `context` and `context_sources` is a node, edges or not. A key that
is referenced but declared nowhere is a node too, with `static: false` and an
empty `source_kind`. A key both declared in `context` and driven by a source is
not static: the declared value is only its start. A source kind validation
refuses for context sources is reported as an empty `source_kind`.

## What is derived

| Edge | Kind | Via | `channel` | `key` |
|---|---|---|---|---|
| key → asset: a script reads `moses.environment.state` | `reads` | `script` | the script's channel | the context key |
| asset → key: a script writes it | `writes` | `script` | the script's channel | the context key |
| zone → asset, asset → zone: a script reads or writes a zone's state | `reads`, `writes` | `script` | the script's channel | the state key |
| A → B: B's script reads A's state (`getDevice`), or writes it (B → A) | `reads`, `writes` | `script` | the script's channel | the state key |
| key → asset: formula input `context.<key>` | `reads` | `formula` | the formula's channel | the context key |
| own zone → asset: formula input `zone.<key>` | `reads` | `formula` | the formula's channel | the state key |
| A → B: B's formula input `channel.<id>` names a channel of A | `reads` | `formula` | the formula's channel | the channel id read |
| key → asset: a schedule's `gate.context_key` | `gates` | `schedule` | the schedule's channel | the key |
| key → asset: a schedule's `scale_by` | `scales` | `schedule` | the schedule's channel | the key |
| child → meter: `submetered_by` | `submeters` | `submetered_by` | empty | empty |
| child → meter: a matching channel an aggregate of the meter sums | `aggregates` | `aggregate` | the aggregate channel | empty |
| timeline → asset or key: dated changes on one target | `dated_change` | `timeline` | the target channel, else empty | the target |

Formula inputs are read the way the runtime resolves them (`resolveInput` in
`lib/runtime/runtime.go`): `zone.<key>` is the state of the asset's **own** zone
and `asset.<key>` its **own** state, whatever dots the key carries. An asset
reading or writing its own state (`moses.asset.state`, `moses.device.state`,
`asset.<key>`, a `getDevice` naming itself) is no edge.

The aggregate edges repeat the runtime's `indexAggregates`: the channels with the
trimmed `characteristic_id` of the aggregate on every asset whose `submetered_by`
names it, one edge per summed channel. `lib/runtime/effects_parity_test.go` pins
the two to the same inputs.

The document is indexed the way the runtime indexes it: a zone without an id, a
duplicate zone and zones below the depth limit are skipped with everything they
contain, and a duplicate asset id is skipped. The graph shows what runs.

## How a script is read

A script longer than 64 KiB, or with brackets nested deeper than 128 levels,
is not parsed at all and is one unresolved entry: goja's parser recurses per
level and overflows the stack, which ends the process, from about 255,000
unclosed parentheses on. The nesting is counted over every way the script can be
lexed - a `/` after a name or a closing bracket is read both as a division and
as a regular expression - so it can only be over-estimated.

A script that passes is parsed with goja's parser and evaluated abstractly: each
expression is reduced to what it denotes - `moses`, the environment, a zone, an
asset, one of their states, a `get` or `set` bound to a state, a literal table,
a string - or to unknown. All names of the api are known (`moses.world` is
`moses.environment`, `room`/`zone`, `device`/`asset`, `service`/`channel`).

- **Aliases** are followed through `var`, `let`, `const` and plain assignments,
  several declarators in one statement included:
  `var E = moses.environment.state, K = moses.environment.getRoom('z').getDevice('a').state;`.
  A name is an alias only while **every** binding it has anywhere in the script
  denotes the same thing. A second binding to something else ends it: a function
  parameter, a loop variable, a destructuring, a declaration without initializer
  in a function or block (it holds `undefined`), and a write through the global
  object (`globalThis.k = ..`, `this.k = ..`). A redeclaration in the global scope
  (`var E;`) changes nothing.
- **Arguments** are read when they are a string literal, a template without
  substitutions, an alias of one, a concatenation of those with `+`, or a cell of
  a literal table.
- **Tables:** `L[i][n]`, where `L` is an array literal of array literals, stands
  for every row, and each row is one access; `ids[i]` over an array literal of
  strings likewise. An integer index stands for that row only. All table
  arguments of one access must index the same table with the same variable, so a
  row's zone, asset and key stay together. Accesses that read the same table the
  same way are resolved once.
- `a || b`, `a ?? b` and `a && b` forward a handle where the handle, or a `null`
  or `undefined` on the left, decides the result; a conditional whose branches
  denote the same handle forwards it.
- `set` without a value, or with `null`, `undefined` or `void ..` as the value, and
  `get` or `set` without a key do nothing in the runtime and are no edge.
- A `get` of a foreign key nobody has written seeds it with 0 in that state, as
  every `get` does (`docs/context-and-context-sources.md`); that is still only a
  read edge.

## What is reported unresolved

A reference is listed under `unresolved` with its source (at most 80
characters) and a reason instead of being guessed; everything else of the
document is derived all the same. The reasons:

- **A key, zone id or asset id that is not readable:** computed at run time
  (`k + '_v'` with `k` a parameter, a template with substitutions, a call), held
  in a variable that is not bound to one string literal, a number, or read from
  something that is not a literal table of strings.
- **The api is changed or handed on:** a write into any part of it
  (`moses.environment.state.get = ..`, `moses.environment = ..`, `delete moses.zone`,
  the asset's own handles included), or a handle that flows where it is not
  followed - passed to a function, returned, stored in an object or array, bound to
  a name that is not an alias. Whatever received the handle may change it, so
  **every access of that script** is then unresolved too, not only the one
  involved. The asset's own handles may go anywhere, and the channel api
  (`moses.channel`, `moses.service`) may be written; neither is read through.
- **The global object is written through a computed name or handed on**
  (`globalThis[name] = ..`, `var g = this`), which may rebind any name: the script
  then has no aliases and no `moses` at all.
- **A computed member of a handle** (`moses.environment[name]`), **a `get` taken
  off its state and called through `.call`**, **a `with` statement**, **a script
  that binds `moses` itself**, **`moses` reached through the global object**.
- **Code that can run a string:** any reference to `eval` or `Function`, and any
  member named `eval`, `Function` or `constructor`, spelled out or concatenated
  from literals. A name built any other way at run time is not recognised.
- **A write to a timeline-governed key is dropped at runtime:** a script's `set`
  on a context key a `context.<key>` dated change targets never lands
  (`docs/dated-changes.md`), so it is no edge.
- **A table** the script modifies or hands on, **a row** without a string at the
  indexed position, **arguments from different tables or rows**.
- **The document does not carry what is named:** no such zone, no such asset
  directly in that zone (the runtime's `getDevice` finds nothing there), a formula
  or timeline naming an unknown channel, a timeline target of none of the listed
  forms, a `submetered_by` naming no asset.
- **A script that does not parse, is too large or nests too deep** is one entry;
  one whose syntax tree is deeper than the analysis follows (2000 levels) is cut
  off with one entry.
- **Truncation:** one entry when a script or the document exceeds what the
  analysis reports - per script 50,000 table rows, 10,000 edges and 500
  unresolved entries, per document 500,000 rows, 100,000 edges and 10,000
  entries. A truncated script contributes nothing past the cap.

A key that is an empty string is refused by the runtime and is neither an edge
nor unresolved.

## Limits

- The analysis is flow-insensitive: a read in a branch that never runs, or after
  a `return`, is still an edge.
- A table index is trusted to range over the rows. `L[i]` inside a loop that only
  visits some rows still yields an edge per row.
- `moses` reached through something else than its name or the global object is
  not followed and not reported.
- Source map comments are ignored; the parser would otherwise read the file a
  `sourceMappingURL` names.
