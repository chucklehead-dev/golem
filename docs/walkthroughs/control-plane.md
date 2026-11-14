# Walkthrough: golem-control's publishing side

This walkthrough is for an engineer new to golem-control. It follows the
code that turns the state document and the config CRDs into signed
snapshots that gateways poll. The design lives elsewhere:
[control-plane.md](../control-plane.md) (§3 the store, §4 distribution),
[model/publication.md](../../model/publication.md) (the publication model
the code is checked against), and ADRs
[0006](../adr/0006-state-and-distribution-in-object-storage.md) (state and
distribution in object storage),
[0044](../adr/0044-budgets-are-controls-not-ceilings.md) (budgets are
controls, which is what a scope's `declaredWorstCaseCheck` carries into a
snapshot) and [0045](../adr/0045-leadership-in-the-state-document.md)
(leadership in the state document). Line numbers are as of this writing;
the function names are the stable reference.

The packages, bottom up:

| Package | Does |
| --- | --- |
| `control/state` | the state document and its store: reads, `Update`, the invariants |
| `control/lead` | the leader's lease, kept in the state document |
| `control/publish` | one pool's publisher: record, checkpoint, signed pointer |
| `control/controller` | the loop: compile the config, publish when due |
| `control/audit` | ships the document's audit outbox |
| `cmd/golem-control` | the binary: flags, wiring |

## 1. The state document and its store

Everything golem-control knows is one JSON document, `state/document.json`
in the bucket (`control/state/state.go:33`). `Document` holds the lease
(`Leader`), each pool's publication record (`Publications`), the
entitlements (`Principals`, `Scopes`, `Keys`, `Revocations`, `Roles`) and
the audit outbox (`Audit`, `AuditSeq`). Decoding is strict
(`Decode`, `state.go:306`): another schema or an unknown member is refused,
so an older replica never silently drops a field a newer one wrote.

**Reads are from memory.** `Store.Current` (`state.go:168`) hands out a
copy of the document as last read or written. `Load` is a full get;
`Refresh` is a conditional get with the entity tag the store holds, so an
unchanged document costs one 304 (`refresh`, `state.go:131`). `Store.Run`
refreshes every 500 ms. `set` (`state.go:156`) never moves the document
back: a slow get that returns an older `stateVersion` than one this store
wrote is ignored.

**Writes go through `Update`** (`state.go:181`), a compare-and-swap with
retry:

1. Take the current document and its tag.
2. `apply` (`state.go:247`): run the caller's `change` on a copy, number
   the new audit events, check the invariants against the document it
   started from. A `change` error or a broken invariant ends the update;
   nothing is written.
3. Increment `stateVersion` and `Put` with `If-Match: <tag>`
   (`state.go:202`).
4. On `ErrPrecondition` (another writer won), `Load` and go round again,
   running `change` afresh on the winner's document. After `Attempts`
   (default 8) losses it returns `ErrContended`, which the API reports as
   `Aborted` (retry).

Because `change` may run more than once it must be a function of the
document it is given, not of state outside it. The documents a store holds
are replaced, never edited in place: that is what lets `apply` read the
base without copying it twice.

**Validate-only.** `Store.Validate` (`state.go:221`) runs the same `apply`
and writes nothing: the API's `validate_only` (control-plane.md §2). The
service layer builds every change, with its audit event, once and hands it
to either (`control/service/service.go`, `Service.apply`).

**The audit outbox's numbering.** A change appends `AuditEvent`s with
`Seq` 0; `Document.number` (`state.go:80`) gives each the next number from
`AuditSeq` as part of the write, so numbers are assigned under the same
compare-and-swap as the change and a retried change is numbered afresh.

### The invariants

`check` (`state.go:266`) runs on every write and validation, in order:

| Invariant | Where | Tested by |
| --- | --- | --- |
| The lease's epoch never goes back, and the lease is never dropped | `checkLease`, `state.go:278` | no direct test (a gap); `control/lead` only ever moves the epoch forward |
| A principal is a user or a service; its scopes exist; its default is one of them | `checkPrincipals`, `entitlements.go:110` | `TestEntitlementInvariants` |
| No two principals share an Entra ID | `checkPrincipals` | `TestEntitlementInvariants` |
| A key's owner exists; a live key's owner is a member of its scope | `checkKeys`, `entitlements.go:136` | `TestEntitlementInvariants` |
| A role is `manager`, of an existing principal on an existing scope | `checkRoles`, `entitlements.go:151` | `TestRolesAndAudit` |
| The outbox loses only its oldest events and gains new ones numbered on without a gap; the counter never goes back | `outboxFollows`, `entitlements.go:260` | `TestRolesAndAudit` |
| Revocations are append-only, and a revoked key stays revoked | `checkRevocations`, `entitlements.go:169` | `TestEntitlementInvariants` |
| No pool's publication record is removed or goes back in sequence, epoch or source | `checkPublications`, `state.go:288` | `TestInvariants` |

Every refusal wraps `ErrInvariant`, which the API maps to Connect's
`FailedPrecondition` (`control/api/api.go`, `connectError`).

## 2. Leadership

Only the leader publishes and ships audit. The lease is the document's
`Leader` field (ADR 0045), not a Kubernetes Lease, so its epoch is written
by the same compare-and-swap as the publication record it fences, and
golem-control needs no API server to lead.

`Elector.Try` (`control/lead/lead.go:71`) is one `Update`:

- no lease: take it at epoch 1;
- held by this replica: renew it, same epoch;
- held by another and not lapsed by this replica's clock: refuse
  (`errHeld`), and this replica now knows it does not lead;
- lapsed: take it at the next epoch.

Each renewal stamps `RenewedAt` and the duration (default 15 s; `Run`
renews every third of it).

**Giving up by one's own clock.** `Elector.Epoch` (`lead.go:62`) is what
the rest of golem-control asks. It returns 0 once the replica has seen
another holder, or once its own clock passes its last renewal plus the
duration (`epochLocked`, `lead.go:105`). Belief runs from the time stamped
*before* the write (`lead.go:101`), so a slow write only shortens it, and a
store error within the lease keeps it. A paused leader therefore stops
believing before any other replica, by that replica's clock, may take the
lease. Clock skew changes only when a lapsed lease may be taken; it cannot
let an old epoch publish over a newer one, because the record refuses it.

## 3. Publication

`Publisher.Publish` (`control/publish/publish.go:63`) publishes one pool,
given the epoch its elector believes and `source`, the state version the
snapshot is built from. In order:

1. Read the pool's **publication record** (`Record`: a `Refresh`, then
   the record). **Fencing**: if `epoch` or `source` is below the record's,
   return `ErrFenced` (`publish.go:69`): this publisher lost its lease or
   reads stale state. This is the model's FENCE.
2. Build the snapshot as publication `record.Seq + 1` and write it,
   content-addressed, as `snapshots/<pool>/<hash>.json` (`publish.go:84`).
   An object nobody points at is harmless.
3. Win the record by `Update`: if the record changed since step 1, return
   `ErrConflict` (`publish.go:89`); otherwise write `{epoch, seq, source}`.
   The store's `checkPublications` makes the record monotonic whatever any
   writer does.
4. Write the **trusted checkpoint** `control/<pool>/checkpoint` with the
   new sequence (`publish.go:100`). A gateway restarted with an empty disk
   takes it as its floor.
5. Sign and write the **pointer** `snapshots/<pool>/current`
   (`publish.go:104`): pool, sequence, epoch, source, issue time and the
   snapshot's hash. Only the pointer is signed; the snapshot is checked
   against the pointer's hash.

**Why the record, not the pointer.** Anyone who can write the pool's
objects but cannot sign can copy an old signed pointer back (the model's
`replayPointer`). Numbering from the pointer would let that replay set the
sequence back, and the next publication would reuse a number a gateway has
already passed. The record lives in the state document, which only
golem-control's role can write, and only moves forward.

A publisher that dies after step 3 leaves the checkpoint and pointer behind
the record; the next publication (the renewal, within a minute) moves them
on.

## 4. The controller loop

`Controller.Step` (`control/controller/controller.go:82`) runs every second
from `Controller.Run` (`controller.go:157`), which also starts the
elector's loop:

1. **Not leading** (`Elector.Epoch() == 0`): do nothing.
2. **Config compile** (`load`, `controller.go:130`): hash the config
   directory; if it changed, decode it and trial-compile it. A config that
   compiles becomes the **last good config**; one that does not is
   reported (`cfgErr`) and the last good one stays in use, so a broken edit
   cannot let every gateway's authorization lapse. Until some config has
   compiled, nothing is published.
3. **Entitlements**: refresh the state and hash its principals, scopes,
   keys and revocations (`entitlementsSum`, `controller.go:186`). The
   state version is not hashed: the publisher's own record write moves it
   at every publication.
4. **Due?** A publication is due when the good config's hash, the
   entitlements' hash or the epoch differs from the last publication's, or
   the renewal interval (`Renew`, default a minute) has passed. Renewal
   extends the snapshots' authorization lease (`Lease`, default four
   hours; ADRs 0030, 0032).
5. **Publish** every pool, in sorted order, with the same snapshot built by
   `compile.Compile` with the state's `Document.Entitlements` (scopes'
   models limited to routes that exist). Only if every pool succeeded is
   the publication remembered; otherwise the next step tries them all
   again.

After each step, if this replica leads and an audit shipper is set, `Run`
ships the outbox.

## 5. Audit shipping

`Shipper.Step` (`control/audit/audit.go:49`) takes up to `Batch` (100) of
the outbox's oldest events and, in order:

1. **Archives** each as `audit/<seq>.json`, create-only (`If-None-Match:
   *`), in an Object Lock bucket in a separate account (`archive`,
   `audit.go:77`). If the object exists (a step that died before its
   trim), it must be byte-identical, or shipping stops with `ErrMismatch`
   and nothing is trimmed: a person looks.
2. **Emits** each as a `golem.audit` OTel log record for ClickHouse
   (`emit`, `audit.go:98`), at least once; ClickHouse keeps one row per
   seq.
3. **Trims** the outbox up to the last event archived, by `Update`
   (`audit.go:67`). Events written meanwhile are numbered higher and stay.

The archive, not the collector, is the durability boundary: an event leaves
the outbox only once it is durably archived.

## 6. The binary's flags

`cmd/golem-control/main.go:58` onwards:

| Flag | Meaning |
| --- | --- |
| `-config` | the config CRDs' directory (required) |
| `-bucket` | the state and snapshot bucket: a directory, or `s3://<bucket>/<prefix>` with the default AWS credential chain (required) |
| `-signing-key` | `file:<Ed25519 PKCS#8 PEM>` (key ID: the file's name) or `kms:<ARN>` (ECC_NIST_P256, checked at start) (required) |
| `-pools` | the pools to publish, comma-separated (default `internal`) |
| `-id` | this replica's name (default the host name): the lease holder |
| `-lease` | the snapshots' authorization lease (default 4h) |
| `-listen` | serve the control API here; needs `-entra-tenant` |
| `-entra-tenant`, `-entra-audience` | the Entra tenant and audiences the API accepts |
| `-audit-archive` | the audit archive bucket; empty: the outbox is not shipped and grows |
| `-telemetry` | `off`, or an OTLP/HTTP collector for the audit's log records (only with `-audit-archive`) |

`golem-control keygen <dir>` writes a development key pair
(`publishing.key`, never overwritten, and `publishing.pub` for gateways).

## 7. Worked example: a key revoked through the API reaches a gateway

1. **The API call.** `Service.RevokeKey` (`control/service/service.go`)
   checks the caller is the key's owner, a manager of its scope or an
   admin, and calls `Service.apply` with a change that calls
   `Document.Revoke` (`control/state/entitlements.go:182`): the key's
   `RevokedAt` is set and a `Revocation` appended. `apply` adds a
   `key.revoke` audit event.
2. **The state update.** `Store.Update` (`state.go:181`) copies the
   document, applies the change, numbers the audit event
   (`state.go:80`), checks the invariants (`checkRevocations` among them),
   and writes `stateVersion + 1` with `If-Match`. On a lost race it
   re-reads and reapplies.
3. **The controller's next step**, within a second on the leader (it may
   be another replica: its `Refresh` at `controller.go:93` reads the new
   document). `entitlementsSum` now differs, so a publication is due
   (`controller.go:102`).
4. **The publication.** `Publish` (`controller.go:110` calling
   `publish.go:63`) builds the snapshot; `Document.Entitlements`
   (`entitlements.go:201`) lists the key with `Revoked: true`. The record
   is won, the checkpoint and the signed pointer written.
5. **The gateway's poll** (`gateway/snapshot/bucket.go`, `Bucket.Poll`),
   every second: a conditional get of the pointer, now changed. `read`
   verifies the signature, the pool and the floor, fetches the snapshot,
   checks its hash and sequence, and `Holder.Apply`
   (`gateway/snapshot/holder.go`) swaps it in. The gateway refuses the key
   by name from then on.

End to end: the write plus up to a second for the controller and a second
for the poll. `control/controller`'s `TestEntitlements` runs exactly this
path with a real gateway source over one bucket.

## 8. How it is tested

- `control/state`: the store's update, retry under 8 concurrent writers,
  and every invariant above.
- `control/lead`: taking, renewing and losing the lease; giving up by the
  replica's own clock; a stale leader fenced by the record.
- `control/publish`: numbering from the record, fencing, racing
  publishers.
- `control/controller`: two replicas and a gateway over one bucket (a
  change arrives, a renewal extends the lease, a broken edit renews the
  last good config, a paused leader is fenced), and the revocation path.
- `control/audit`: archive, emit, trim; a retry after a crash; a mismatch.
- `gateway/snapshot/publication_mbt_test.go`: model/publication.qnt's
  traces drive the real `Elector`, `Publisher` and gateway source,
  including replayed pointers and empty-disk restarts
  (`QUINT_BACKEND=typescript go test ./gateway/snapshot/`).

## 9. Known gaps

- **Encoding**: the document and snapshots are JSON. The design's protobuf
  with zstd (ADR 0006) replaces it behind `Encode` and `Decode`.
- **The real AWS run**: S3 and KMS are implemented (`lib/bucket/s3bucket`,
  `lib/sign/kmssign`) and tested against a stand-in, but nothing has run in
  the target account with the bucket policy that denies other writers,
  which is what makes the checkpoint trustworthy.
- **Pool selectors**: every pool gets the same snapshot. Selection by
  site and audience (control-plane.md §4), and the CRD watch replacing
  the directory, come later.
- **Restore** (`golemctl state restore`) is not built; the store's refusal
  to move back (`set`) has not yet met a restored document.
