# Sharing an environment

## Scope

**Applies when** the devices of one environment are to be usable by another
account — a demo user, a project group — without handing over the environment
itself. The case is a demonstrator whose thirty-odd devices belong to one user
and have to be shown from a second account. **Delimitation:** this shares the
**devices and the graph they appear in**, not the environment: the document
stays with its owner and the platform administrators. Devices the user attached
to an asset are not shared either, because moses does not own them. The rights
are fixed at `read` and `execute` and cannot be chosen per account, and role
entries are not written.

## What it does

`PUT /environments/{id}/shares` replaces the set:

```json
{ "users": ["<keycloak user id>"], "groups": ["/demo"] }
```

Everyone named gets `read` and `execute` on every device moses created for this
environment and on the graph it is mirrored as. Everyone who was in the stored
set and is not named any more loses their entry. `GET` on the same path serves
the stored set together with `devices`, the number of devices it acts on, and
`graph`, whether a graph is shared with them. Both need the owner or an
administrator; anybody else gets `404`, as everywhere else in this api.

At most **100** users and groups together, and at most **256** characters per
entry. The limit is checked against the set that would be stored, not against
the request, so an environment that already carries leftovers of a failed
attempt refuses a call that would grow it further and says so — a call that
shrinks the set is never refused over the limit.

The readings follow the device: an account that may read a device reads its
timeseries, which is what the share exists for.

## The set lives beside the document

The set is **not** a field of the environment. It is stored per environment id in
its own collection, like the runtime state, and `GET /environments/{id}` neither
serves it nor accepts it.

That is not tidiness. The document is written whole on every save, from a copy
the client read earlier: a set inside it would be written back by any save that
started before a share, silently, while the rights on the devices stayed. The
set would then be gone from the record that decides what a withdrawal has to
take back.

It goes with the environment when that is deleted, and an id that is used again
starts unshared — a `POST`, or a `PUT` to an id nothing is stored under, drops
what an earlier environment left behind, and does so before it creates the first
device of the new one.

The set carries a version. Every write is a compare-and-swap against the version
it was read at, so two shares of one environment arriving together cannot each
store their own: the second one is answered `409` **before it touches a single
resource**, and repeating it after a read is all that is needed.

## What is touched

| | |
|---|---|
| a device moses created for an asset | shared |
| a device the user attached to an asset | never touched — moses does not own it and has no `administrate` on it |
| the graph the environment is mirrored as | shared, as one more resource |
| the environment document | not shared |

## The graph

The graph is how another application reads a simulated site, so a share that
stopped at the devices would hand out the readings and hide the structure. It is
a resource of its own in permissions-v2, under the topic `graphs`, addressed by
the `external_graph_ref` of the environment, and it goes through exactly the
same merge as a device: read, `read` and `execute` added or the entry dropped,
written back, `administrate` untouched.

An environment whose mirror never succeeded carries no ref and simply has no
graph to share. A failure on the graph appears in the same `502` list as a
device, with `kind: "graph"`.

## Who may be named

The rights are written with the **caller's own token**, so the platform's rule
decides, not moses: a caller without the `admin` role may share with groups they
are a member of and with users who share a group with them. permissions-v2
refuses anything else, per device, with a `400` that says so — and that refusal
is what the `502` of the share carries. An administrator may name anybody.

There is deliberately no service token here. One would let anybody who owns an
environment hand its devices to accounts they have no relationship with.

## The owner keeps their device

`SetPermission` replaces the whole rights object of a device, so every device is
**read before it is written** and the entries of everybody else are carried over.
On top of that, an entry carrying `administrate` — the owner, an administrator —
is never changed and never removed, not even when it is in the set being
withdrawn. permissions-v2 requires an administrating **user** on every resource
(a group with `administrate` does not satisfy it), so removing one could leave a
device nobody can write, and its owner without their own device.

A `write` an entry already carried stays as it is while the account is shared,
and goes with the entry when the share is withdrawn.

## When it fails

The rights are applied resource by resource, and **the union of the stored and
the requested set is written first**, with the compare-and-swap. Nothing is
granted before that record exists. If a resource fails, the answer carries the
resources and the reasons:

```json
{ "devices": [ { "id": "urn:infai:ses:device:…", "kind": "device", "status": 400, "error": "…" } ] }
```

`status` is what permissions-v2 answered, and it decides the status of the call:
**`400`** when every failure is a refusal of the caller (`400`, `401`, `403`) —
a group they may not share with, a mistyped id — and **`502`** as soon as one
failure is anything else, a `404` for a device permissions-v2 does not know yet
included.

The union is what stands then. That is the point of writing it first: the
resources that did go through carry rights that nothing else remembers, and the
next call — with any set, the empty one included — computes its withdrawals from
the union and takes them back. `GET` shows the union until then, which is the
honest answer: these are the accounts that may have rights on a resource of this
environment. The same holds when the second write fails on its own: the rights
are set, the shrink is not stored, and the union covers them.

Sending the same request again repeats the whole change safely. Applying the same
set twice changes nothing.

## Timing

Each call to permissions-v2 is bounded at five seconds and ends when the caller
goes away; the resources are worked on eight at a time. The application as a
whole is bounded at **eight seconds**, under the api's ten second write timeout:
a share of thirty devices is sixty round trips, and one that does not fit in
that window **stops there and reports the resources it did not reach as
failures**. The union stands, so a second call finishes the work — a very large
environment, or a slow permissions-v2, can therefore need more than one call.

## New devices inherit

An asset added later gets its platform device when the environment is saved, and
that device is given the stored set right after the save. A graph that a save
**creates** — one the environment did not have a ref for — is given it too; a
graph that is only rewritten keeps the rights it already has. A share therefore
does not have to be renewed after an edit.

This is best effort: the failure is a WARN per device and never fails the save.
It is expected rather than exceptional, because the rights of a device reach
permissions-v2 asynchronously and a device created a moment ago may not be known
there yet. The next `PUT` on `/shares` repairs it.

## Known gaps

- **No rollback.** A `502` leaves the devices that already went through changed.
  The stored union is what makes that recoverable, and the repeat the fix.
- **A withdrawal drops the whole entry** unless it carries `administrate`, so a
  `write` somebody granted by hand outside moses goes with it.
- **The set is not reconciled in the background.** A right removed directly in
  permissions-v2 stays removed, and a set that is stored is not proof that every
  device carries it — only a successful `PUT` is.
- **A share of a very large environment may not fit in one call.** The deadline
  turns the rest into failures; repeating finishes it.
- **An instance without a share store answers `500` on both endpoints**, rather
  than claiming an environment is shared with nobody.
