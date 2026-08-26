# Device lifecycle of assets

**Applies when** an asset names a device type (`external_type_id`) or carries a
platform device (`external_ref`), in documents written since `external_managed`
exists (image v0.11.x). **Delimitation:** documents stored before that decode
every asset as unmanaged — their devices are never auto-deleted, which is
deliberate (`lib/domain/convert.go` preserves refs to keep the timeseries). The
neighbouring case that looks identical is a device the *user* picked and
attached: same fields, opposite rule — it is never deleted.

`geltung: allgemein`

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

## Known gap

`PUT` is a read-modify-write without optimistic concurrency. Of two concurrent
updates to one environment, the loser's cleanup can delete a device the winning
document still references. Closing it needs a version field on the document; a
re-read after the write would only narrow the window.
