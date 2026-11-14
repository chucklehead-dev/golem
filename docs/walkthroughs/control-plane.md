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
| `control/lead` | leadership: the lease object and the fence in the document (ADR 0049) |
| `control/publish` | one pool's publisher: record, checkpoint, signed pointer |
| `control/controller` | the loop: compile the config, publish when due |
| `control/audit` | ships the document's audit outbox |
| `cmd/golem-control` | the binary: flags, wiring |

## 1. The state document and its store

Everything golem-control knows is one JSON document, `state/document.json`
in the bucket (`control/state/state.go:33`). `Document` holds the fence
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
| The fence's epoch never goes back, and the fence is never dropped | `checkLease` | no direct test (a gap); `control/lead` only ever moves the epoch forward |
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

Only the leader publishes and runs the jobs. Leadership is two objects
(ADR 0049, model/leadership.md's "fence" design):

- the **lease**, `lead/lease.json` (`LeaseKey`): holder, epoch, duration.
  It holds no authority; renewing it rewrites a few hundred bytes, not the
  document;
- the **fence**, the document's `Fence`: who leads, at which epoch. Every
  leader-only write checks it in its own compare-and-swap
  (`state.FencedUpdate`).

`Elector.Try` (control/lead/lead.go) reads the lease, then:

- **mine** (held by this replica at the epoch it leads at, or has
  acquired): renew it; if its claim is still pending (a lost reply), claim
  now; if the fence names another, this replica has been deposed;
- **another's, not lapsed**: this replica does not lead. A lease is lapsed
  when this replica has seen the same object (its tag) unchanged for the
  lease's duration *by its own clock*, as client-go judges; the stored
  `RenewedAt` is only diagnostic, so clock skew does not matter;
- **free, released or lapsed** (`take`): acquire the lease at one above
  the higher of its epoch and the fence's, then `claim` the fence, only if
  the fence's epoch is below that. A rolled-back lease object therefore
  cannot reuse an epoch, and only delays a takeover.

The replica leads once it has claimed. A claim whose reply was lost is
adopted at the next try (the fence already names it); the store also
recognises its own write when a retried put fails its condition (each
version carries a write ID, `Document.Write`).

Defaults: a 30 s lease renewed every 10 s; a failed try retried after 2 s,
jittered (`Run`).

**Giving up by one's own clock.** `Elector.Epoch` returns 0 once the
replica has seen another holder, or once its own clock passes its last
renewal plus the duration. Belief runs from the time stamped *before* the
write, so a slow write only shortens it, and a store error within the
lease keeps it. A paused leader stops believing before any taker can have
watched its lease for a whole duration. Whatever it believes, its writes
are refused once another has claimed the fence.

**Giving up.** On SIGTERM, golem-control stops its publishing loop and the
elector's, then calls `Elector.Release`, which marks the lease released:
another replica takes it on its next try, and a rolling restart loses no
publication time.

## 3. Publication

`PublishAll` (control/publish/publish.go) publishes every pool given the
fence its elector holds (`Lease`, a `state.Fence`) and `source`, the
state version the snapshots are built from. `Publisher.Publish` is the
same for one pool. In order:

1. **Fencing**: refresh the state; if the fence is not this one (`holds`)
   or a pool's record has a later epoch or source, that is `ErrFenced`.
2. **Numbering** (`floor`, the model's GUARD and VERIFY): each pool's next
   publication is one above the highest of its record, its verified
   checkpoint and its signed pointer, so a rolled-back record, alone or
   beside an older checkpoint copied back, still numbers above what
   gateways trust. A checkpoint that does not verify, or is missing once
   one was written, is `ErrTampered`: that pool is not published until a
   person looks. (Record 1 without a checkpoint is a publisher that died
   between the first two writes, not tampering.)
3. Build each snapshot and write it, content-addressed.
4. **Win every pool's record in one `FencedUpdate`**: one document
   version a cycle, whatever the number of pools. If a record changed
   since step 1, `ErrConflict`. The store's `checkPublications` keeps each
   record monotonic.
5. Each pool's signed **checkpoint** `control/<pool>/checkpoint`, then its
   signed **pointer**, each only forward (`advance`): written conditionally
   on the object read, refused (`ErrFenced`) over a later publication's
   (a successor's, while this publisher was paused). A stored checkpoint
   that does not verify is `ErrTampered`; a stored pointer that does not
   verify is replaced (gateways refuse it too).

**Why the record, not the pointer.** Anyone who can write the pool's
objects but cannot sign can copy an old signed pointer back (the model's
`replayPointer`). Numbering from the pointer alone would let that replay
set the sequence back; numbering from the highest of all three cannot be
set back by any one of them.

A publisher that dies after step 4 leaves the checkpoint and pointer behind
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

### 5.1 Entra group sync

`Sync.Step` (`control/entrasync/sync.go`, ADR 0047) runs from the
controller loop (`syncStep`) every `SyncEvery` (five minutes) while this
replica leads:

1. **Read every bound group first** (`boundGroups`: each scope's
   `Groups`), through `Directory.Members`: `Graph` lists a group's
   transitive user members (`id`, `userPrincipalName`, `accountEnabled`),
   following `@odata.nextLink` only on Graph's own host, with a token from
   `FederatedToken` (the pod's projected service account token as the
   client assertion). Any failure but a deleted group (`ErrNoGroup`, no
   members) ends the step with nothing written.
2. **Reconcile** in one `Update` (`reconcile`): each enabled member of a
   group bound to a scope has the scope in `Synced`; a user not yet a
   principal is created (`principalID`: the sign-in name, lowercased; with
   part of the object ID if taken; `user-<oid>` for a name in the reserved
   `entra:` namespace); a user in no bound group, or disabled, has
   `Synced` emptied. Each changed principal's keys outside its
   memberships are revoked (`Document.RevokeOutside`), a disabled user's
   all of them, and the change is audited by `system:entra-sync`.
3. **Guard**: a step ending more than `MaxRemovals` (50) memberships
   returns `ErrTooManyRemovals` and writes nothing; a step changing nothing
   writes nothing either.

Memberships granted in golem (`Scopes`) are never the sync's: a member by
both ways keeps one when the other ends, and `members remove` refuses a
membership that is only a group's.

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
| `-entra-tenant`, `-entra-audience` | the Entra tenant, and the control API registration's audiences (default `api://golem-control`, ADR 0046) |
| `-audit-archive` | the audit archive bucket; empty: the outbox is not shipped and grows |
| `-telemetry` | `off`, or an OTLP/HTTP collector for the audit's log records; refused without `-audit-archive` |
| `-entra-sync-client-id` | the jobs registration (entra.md #4): run the group sync by workload identity; needs `-entra-tenant` |
| `-entra-sync-every`, `-entra-sync-max-removals` | the sync's interval (5m) and its removal guard (50) |

`golem-control pubkey <signing key>` prints the key's public half for
gateways' `-keys` directories: a PEM with a `Key-ID` header (a KMS key's
ARN, a file key's name), which `sign.LoadKeys` trusts under that ID.

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
- `control/lead`: taking, renewing and losing the lease; lapse judged by
  the taker's own clock; giving up by the holder's; a stale leader fenced;
  a rolled-back lease object (no epoch reused); a lost claim adopted and a
  landed write recognised as one's own.
- `control/publish`: numbering, fencing, racing publishers, one state
  write for every pool, and the guard (`TestGuard`: model/leadership.md's
  counterexamples, each refused).
- `control/entrasync`: a sync deposed mid-read writes nothing
  (`TestSyncFenced`).
- `model/check-leadership.sh`: the placements compared, each mechanism
  shown necessary.
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
