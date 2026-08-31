# Device lifecycle of assets

**Applies when** an asset names a device type (`external_type_id`) or carries a
platform device (`external_ref`), in documents written since `external_managed`
exists (image v0.11.x). **Delimitation:** documents stored before that decode
every asset as unmanaged — their devices are never auto-deleted, which is
deliberate (`lib/domain/convert.go` preserves refs to keep the timeseries). The
neighbouring case that looks identical is a device the *user* picked and
attached: same fields, opposite rule — it is never deleted.

## Rules

- **Provisioning happens on store, not in the editor.** An asset with an
  `external_type_id` but no `external_ref` gets its platform device created
  after validation and before the write; the ref is written back. A rejected
  document creates nothing; a retry after a failed write is safe because the
  ref is set by then.
- **`external_managed` is decided by the server, never by the client.** The
  whole document is sent on every update, so an echoed or invented flag would
  decide whether somebody's real device gets deleted. A flag is inherited only
  when the same asset id still carries the same ref it was provisioned with;
  everything else is forced to false (`reconcileManagedFlags`, `lib/api/provision.go`).
- **Cleanup runs after the write** (update) or after the delete (environment):
  a failed write must leave every device standing. Cleanup is best effort — a
  failed delete leaves an orphan and a warning log, which is the state every
  removal produced before v0.11.
- **A device still referenced anywhere in the new document is kept**, even by
  an asset that does not own it. The cleanup set is ref-based: exchanging a
  managed device for a picked one releases the old device like a deleted asset
  does.
- **A 404 from the device-manager on delete counts as success** (the device is
  gone, which is what the caller wanted); a 404 on create still fails loudly,
  because there it means the manager URL is wrong (`lib/devices/catalog.go`,
  `ErrNotFound`).
- **A document copied to a new id owns nothing.** `PUT /environments/{new-id}`
  with the export of another environment finds no stored document, so deleting
  the copy never deletes the original's devices.
- **A stored document with duplicate asset ids inherits nothing** for those
  ids (possible in documents predating the uniqueness validation) — guessing
  would decide whether a real device is deleted.

## Concurrent updates, and what the version protects

The document carries a `version` the server counts: every successful write
increments it, and a client sends back the version of the document it read.

**A write that carries a version and loses the race is refused with `409`, and a
refused write deletes nothing.** That is the guarantee, and it is the one that
matters here: without it, of two concurrent updates the loser's cleanup could
delete a device the winning document still publishes through — the winner's
assets would keep referring to a device that no longer exists, and its
timeseries would end there.

Two things carry it, and they are not the same thing:

- **The compare-and-swap in `lib/repo`** is the guard. The expected version is
  part of the filter of the mongodb write, so the comparison and the write are
  one operation on the document. A handler that reads, compares and then writes
  would have the same race, only a shorter one — and the cleanup runs *after*
  the write, so a write refused there has already skipped it.
- **The check in the handler**, against the document it read a moment earlier,
  is what makes the ordinary conflict — a second editor working from a stale
  document — free of side effects at all: it sits before provisioning, before
  the graph mirror and before the cleanup. It cannot be the guard, because two
  callers can pass it in the same instant.

What is left over is bounded and never destructive: when the winning write lands
in the window between the handler's read and its write, provisioning may already
have created a platform device that no stored document then references, and the
graph may have been rebuilt from the losing document. The orphaned device is
logged and outlives the request, which is what a failed write has always left
behind; the graph is rebuilt from the winning document on its next save.

The store counts the version itself, in the database, rather than reading it and
adding one in this process. Two unchecked writers doing the latter would be
handed the same number, and a compare-and-swap against that number would then
accept a document that had already been written over.

## The version is opt-in, and what that costs

**A client that sends `version: 0`, or omits the field, is not protected.** The
write goes through unchecked, and the version is still incremented — so a client
that does take part is protected against it, and against every other unchecked
writer. This is deliberate: every client written before the field existed sends
no version, and refusing those writes would take the api away from them.

The consequence is that the gap above is closed for a client that opts in, not
for the endpoint. A document stored before the field existed reads as version 0
as well, so no caller is ever asked to defend a number it never saw.

Two further cases, both decided the way the copy rules above are:

- **A version carried against an id that is not stored is ignored**, and the
  document is created. Putting an export under a new id is how a document is
  copied, and an export carries the version of the original.
- **`POST` ignores a version in the body**, like it ignores an id: a document
  being created has nothing to be concurrent with.
