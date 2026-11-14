# Walkthrough: golem-control's publishing side

This walkthrough is for an engineer new to golem-control. It follows the
code that turns the state document and the config CRDs into signed
snapshots that gateways poll, the leadership that decides who may do so,
and the jobs the leader runs beside it. The design lives elsewhere:
[control-plane.md](../control-plane.md) (§3 the store, §4 distribution,
§6 jobs), [model/leadership.md](../../model/leadership.md) (where the
lease and the fence live, and the publisher's guard) and
[model/publication.md](../../model/publication.md) (the publication model
the code is checked against), and ADRs
[0006](../adr/0006-state-and-distribution-in-object-storage.md) (state and
distribution in object storage),
[0045](../adr/0045-leadership-in-the-state-document.md) (leadership in the
state document) as amended by
[0049](../adr/0049-lease-object-and-fence.md) (the lease in its own object,
the fence in the document), [0047](../adr/0047-identity-from-entra-groups.md)
(identity from Entra groups) and
[0048](../adr/0048-known-spend-by-time-boundary.md) (known spend). Line
numbers are as of the commit that last changed this walkthrough; the
function names are the stable reference.

The packages, bottom up:

| Package | Does |
| --- | --- |
| `control/state` | the state document and its store: reads, `Update` and `FencedUpdate`, the invariants, the restore |
| `control/lead` | leadership: the lease object and the fence in the document (ADR 0049) |
| `control/publish` | the publisher: record, checkpoint, signed pointer; the guard; every pool in one write |
| `control/controller` | the loop: compile the config, publish when due, run the leader's jobs |
| `control/audit` | ships the document's audit outbox |
| `control/entrasync` | the Entra group sync |
| `cmd/golem-control` | the binary: flags, wiring, `keygen`, `pubkey`, `restore` |

The API that writes the document (`control/service`, `control/api`) is
[control-api.md](control-api.md)'s; known spend's job (`control/spend`) is
[accounting.md](accounting.md)'s.

## 1. The state document and its store

Everything golem-control knows is one JSON document, `state/document.json`
in the bucket (`DocumentKey`, `control/state/state.go:35`). `Document`
(`state.go:54`) holds the fence (`Fence`, `state.go:48`: who leads, at
which epoch), the write ID of the update that wrote it (`Write`), each
pool's publication record (`Publications`, a `Record` of epoch, sequence
and source state version, `state.go:40`), the entitlements (`Principals`,
`Scopes`, `Keys`, `Revocations`, `Roles`; `entitlements.go`) and the audit
outbox (`Audit`, `AuditSeq`). Decoding is strict (`Decode`,
`state.go:357`): another schema or an unknown member is refused, so an
older replica never silently drops a field a newer one wrote.

**Reads are from memory.** `Store.Current` (`state.go:198`) hands out a
copy of the document as last read or written. `Load` is a full get;
`Refresh` is a conditional get with the entity tag the store holds, so an
unchanged document costs one 304 (`refresh`, `state.go:139`). `Store.Run`
(`state.go:370`) refreshes every 500 ms. `set` (`state.go:186`) never
moves the document back: a slow get that returns an older `stateVersion`
than one this store wrote is ignored.

**Never adopting a document that went back.** If the bucket holds an older
version than the store has seen, or none (an out-of-band put of an old
version, a deletion), `refresh` returns `ErrRegressed` (`state.go:115`,
message from `regressed`, `state.go:171`) and the store keeps serving what
it last read and writes nothing: adopting the older document would drop
every later revocation. A restore goes through `Restore` (§1.2) instead.
`TestRegressed` covers both cases.

**Writes go through `Update`** (`state.go:211`), a compare-and-swap with
retry:

1. Take the current document and its tag.
2. `apply` (`state.go:298`): run the caller's `change` on a copy, number
   the new audit events, check the invariants against the document it
   started from. A `change` error or a broken invariant ends the update;
   nothing is written.
3. Increment `stateVersion`, stamp a fresh write ID (`Write`,
   `state.go:228`) and `Put` with `If-Match: <tag>` (`state.go:233`).
4. On `ErrPrecondition`, `Load`. If the document there carries this
   attempt's write ID, the put landed and only its reply was lost (a
   retried conditional put fails its own condition): the update succeeded
   (`state.go:241`). Otherwise another writer won: go round again, running
   `change` afresh on the winner's document. After `Attempts` (default 8)
   losses it returns `ErrContended`, which the API reports as `Aborted`
   (retry).

Because `change` may run more than once it must be a function of the
document it is given, not of state outside it. The documents a store holds
are replaced, never edited in place: that is what lets `apply` read the
base without copying it twice.

**Leader-only writes go through `FencedUpdate`** (`state.go:260`): an
`Update` whose change first checks that the document's fence is still the
writer's (holder and epoch), else `ErrFenced` (`state.go:118`) and nothing
is written. The check is inside the same compare-and-swap, so a leader
deposed while it worked (a pause, a slow directory read) writes nothing.
Publication (§3) and the group sync (§5.2) use it.

**Validate-only.** `Store.Validate` (`state.go:275`) runs the same `apply`
and writes nothing: the API's `validate_only` (control-plane.md §2). The
service layer builds every change, with its audit event, once and hands it
to either (`control/service/service.go:93`, `Service.apply`).

**The audit outbox's numbering.** A change appends `AuditEvent`s with
`Seq` 0; `Document.number` (`state.go:80`) gives each the next number from
`AuditSeq` as part of the write, so numbers are assigned under the same
compare-and-swap as the change and a retried change is numbered afresh.

### 1.1 The invariants

`check` (`state.go:317`) runs on every write and validation, in order:

| Invariant | Where | Tested by |
| --- | --- | --- |
| The fence's epoch never goes back, and the fence is never dropped | `checkFence`, `state.go:329` | `TestFenceInvariant` |
| A principal is a user or a service; its ID is not in the reserved `entra:` namespace; its scopes exist; its default is one of them; only a user with an Entra ID has synced memberships | `checkPrincipals`, `entitlements.go:139` | `TestEntitlementInvariants` |
| No two principals share an Entra ID | `checkPrincipals` | `TestEntitlementInvariants` |
| A key's owner exists; a live key's owner is a member of its scope | `checkKeys`, `entitlements.go:171` | `TestEntitlementInvariants` |
| A role is `manager`, of an existing principal on an existing scope | `checkRoles`, `entitlements.go:186` | `TestRolesAndAudit` |
| The outbox loses only its oldest events and gains new ones numbered on without a gap; the counter never goes back | `outboxFollows`, `entitlements.go:306` | `TestRolesAndAudit`, `control/audit`'s tests |
| Revocations are append-only, and a revoked key stays revoked | `checkRevocations`, `entitlements.go:204` | `TestEntitlementInvariants` |
| No pool's publication record is removed or goes back in sequence, epoch or source | `checkPublications`, `state.go:339` | `TestInvariants` |
| A leader-only write lands only under its own fence | `FencedUpdate`, `state.go:260` | `TestFenceInvariant`, `TestStaleLeaderFenced`, `TestSyncFenced` |

Every refusal wraps `ErrInvariant`, which the API maps to Connect's
`FailedPrecondition` (`control/api/api.go`, `connectError`).

### 1.2 Restoring an older document

`state.Restore` (`control/state/restore.go:36`) builds a document that
brings an older one's entitlements back **without widening any right**:
for every right, the narrower of the backup's and the current document's.
A `restorer` walks the scopes, principals, roles and keys in turn
(`scopes`, `principals`, `roles`, `keys`, `restore.go:57`-`119`), then
revokes every live key whose owner is no longer a member of its scope:

- what only the backup has (a scope, principal, membership, role or key
  removed since) is **withheld**, and named in `Report.Withheld`:
  re-granting it is a change of its own;
- what only the current document has (created since) is **rolled back**:
  removed, except that a principal that owns keys is kept with no
  memberships (key records name their owner), and a key minted since is
  revoked rather than removed, so no later restore can revive it;
- what both have takes the narrower of each (`scope`, `principal`, `key`,
  `restore.go:164`-`214`): lists intersected (`meetSet`), limits the
  lower (`lower`, where 0 means the default), storage only if both allow
  it, the worst-case check if either has it, the stricter feature mode, a
  key's earlier expiry; a principal's identity is always the current one's.

The fence, the publication records, the revocations and the outbox are
always the current document's (model/leadership.md's "preserve" restore):
a restore cannot move publication back, change who leads, or revive a
revoked key.

`Store.Restore` (`restore.go:325`) runs it inside `Update` with a
`state.restore` audit event naming the actor and the backup's version (or,
as a dry run, inside `Validate`). `golem-control restore` (§6) is its
command line. `TestRestore` covers a backup with rights changed both ways;
`TestRestoreNeverWidens` checks, over 2000 random document pairs, that the
result passes every invariant and grants nothing the current document does
not; `TestRestoreCommand` runs the command's dry run and `-apply`.

## 2. Leadership

Only the leader publishes and runs the jobs. Leadership is two objects
(ADR 0049, model/leadership.md's fence design; `control/lead/lead.go`'s
package comment):

- the **lease**, `lead/lease.json` (`LeaseKey`, `lead.go:41`; the `lease`
  type, `lead.go:44`): holder, epoch, duration, a diagnostic renewal time,
  and `Released`. It holds no authority: losing it or rolling it back only
  delays a takeover. Renewing it rewrites this small object, not the
  document;
- the **fence**, the document's `Fence`: who leads, at which epoch. Every
  leader-only write checks it in its own compare-and-swap
  (`state.FencedUpdate`).

`Elector.Try` (`lead.go:129`) reads the lease (`read`, `lead.go:170`; a
missing one is free), then:

- **mine** (`mine`, `lead.go:155`: held by this replica at the epoch it
  leads at, or at the one it acquired and has yet to claim): `renew`
  (`lead.go:214`) rewrites it conditionally on the object read; if its
  claim is still pending (a claim that failed, or whose reply was lost),
  it claims now; and if the fence names another, this replica has been
  deposed (`errLost`);
- **another's, not lapsed**: this replica does not lead. A lease is lapsed
  (`lapsed`, `lead.go:188`) when this replica has seen the same object
  (its tag) unchanged for the lease's duration *by its own clock*, as
  client-go judges; the stored `RenewedAt` is only diagnostic, so clock
  skew does not matter;
- **free, released or lapsed**: `take` (`lead.go:238`) acquires the lease
  at one above the higher of its epoch and the fence's, then `claim`
  (`lead.go:260`) writes the fence by `Update`, only if the fence's epoch
  is below that. A rolled-back lease object therefore cannot reuse an
  epoch.

A lease write that loses its race (`errHeld`) or a claim beaten by a later
epoch (`errLost`) is not a failure: the replica steps down (`stepDown`,
`lead.go:163`) and `Try` reports epoch 0. The replica leads once it has
claimed. A claim whose reply was lost is adopted at the next try (the
fence already names it, `claim`'s first case), and the store recognises
its own write when a retried put fails its condition (the write ID, §1).

`Elector.Fence` (`lead.go:112`) is the leadership this replica acts under,
for its leader-only writes; the controller passes it to the publisher and
the group sync.

**Giving up by one's own clock.** `Elector.Epoch` (`lead.go:104`) returns
0 once the replica has seen another holder, or once its own clock passes
its last renewal plus the duration (`epochLocked`). Belief runs from the
time stamped *before* the write, so a slow write only shortens it, and a
store error within the lease keeps it. A paused leader stops believing
before any taker can have watched its lease for a whole duration. Whatever
it believes, its writes are refused once another has claimed the fence.

**Timing.** `Run` (`lead.go:300`) tries every `RenewEvery` (default a
third of the 30 s `Duration`, so 10 s), and after a failed try again after
`RetryEvery` (2 s), jittered by a quarter either way (`jitter`), so a store
blip does not wait a whole renewal and replicas do not retry in step.

**Giving up.** On SIGTERM, golem-control stops its publishing loop and the
elector's, then calls `Elector.Release` (`lead.go:285`): belief ends
first, then the lease is marked released, and another replica takes it on
its next try, so a rolling restart loses no publication time. The fence
stays; the next taker claims a later epoch.

The tests (`control/lead/lead_test.go`): `TestLease` (taking, renewing,
lapse judged by the taker's own clock, the old holder learning it lost),
`TestRenewalKeepsIt`, `TestGivesUpByItsClock`, `TestStaleLeaderFenced` (a
paused leader's publication refused by the fence), `TestLeaseRolledBack`
(no epoch reused, the old holder not reinstated), `TestRelease`, and
`TestLostClaim` (a landed claim whose reply was lost adopted, and a landed
write recognised as the writer's own).

## 3. Publication

`PublishAll` (`control/publish/publish.go:110`) publishes every pool given
the fence the elector holds (`Lease`, an alias of `state.Fence`,
`publish.go:72`) and `source`, the state version the snapshots are built
from. `Publisher.Publish` (`publish.go:94`) is the same for one pool. In
order:

1. **Fencing**: refresh the state; if the fence is not this one (`holds`,
   `publish.go:126`, so a deposed publisher writes no snapshot either), or
   a pool's record has a later epoch or source (`prepare`), that is
   `ErrFenced`.
2. **Numbering** (`floor`, `publish.go:250`; the model's GUARD and
   VERIFY): each pool's next publication is one above the highest of its
   record, its verified checkpoint and its signed pointer
   (`publish.go:193`), so a rolled-back record, alone or beside an older
   checkpoint copied back, still numbers above what gateways trust. A
   checkpoint that does not verify, or is missing once a publication was
   made (the record above 1, or a signed pointer present), is
   `ErrTampered`: that pool is not published until a person looks. Record
   1 without a checkpoint is a publisher that died between the first two
   writes, not tampering. The checkpoint and pointer are verified with
   `Keys`, or the signer's own public half (`keys`, `publish.go:333`).
3. **Build** each snapshot and write it, content-addressed (`prepare`,
   `publish.go:181`). A pool that fails here is left out; the others go on.
4. **Win every pool's record in one `FencedUpdate`** (`publish.go:141`):
   one document version a cycle, whatever the number of pools (ADR 0049).
   If a record changed since step 1, `ErrConflict`. The store's
   `checkPublications` keeps each record monotonic.
5. Each pool's signed **checkpoint** `control/<pool>/checkpoint`, then its
   signed **pointer** (`finish`, `publish.go:213`), each only forward
   (`advance`, `publish.go:293`): written conditionally on the object
   read, refused (`ErrFenced`) over a later publication's (a successor's,
   while this publisher was paused). A stored checkpoint that does not
   verify is `ErrTampered`; a stored pointer that does not verify is
   replaced (gateways refuse it too). The pointer carries its issue time
   and `Expires` (`PointerTTL`, ten minutes), after which gateways report
   it overdue.

**Why the record, not the pointer.** Anyone who can write the pool's
objects but cannot sign can copy an old signed pointer back (the model's
`replayPointer`). Numbering from the pointer alone would let that replay
set the sequence back; numbering from the highest of all three cannot be
set back by any one of them.

A publisher that dies after step 4 leaves the checkpoint and pointer behind
the record; the next publication (the renewal, within a minute) moves them
on. Until then a gateway restarted with an empty disk takes the older
checkpoint as its floor.

The tests (`control/publish/publish_test.go`): `TestPublish`, `TestFenced`
(the model's FENCE), `TestConcurrentPublishers`,
`TestLeaseLostBeforeRecord`, `TestPausedAfterRecord`, `TestGuard`
(model/leadership.md's counterexamples, each refused: a rolled-back record
met by an older checkpoint, a forged or deleted checkpoint, and the first
publication's crash window not mistaken for tampering) and
`TestPublishAll` (one state write for several pools; a failing pool left
out without stopping the others).

## 4. The controller loop

`Controller.Run` (`control/controller/controller.go:211`) starts the
elector's loop and, every second, runs `tick` (`controller.go:236`): a
publication if due (`Step`), then, while this replica leads, the audit
outbox shipped (§5.1) and the group sync if due (§5.2). Each error is
logged once until it changes. `Run` returns only once the elector's loop
has, so a `Release` after it cannot be undone by a renewal in flight.

`Controller.Step` (`controller.go:94`):

1. **Not leading** (`Elector.Fence` reports none): do nothing.
2. **Entitlements**: refresh the state (`controller.go:110`) and hash its
   principals, scopes, keys and revocations (`entitlementsSum`,
   `controller.go:389`). The state version is not hashed: the publisher's
   own record write moves it at every publication.
3. **Config compile** (`load`, `controller.go:271`): hash the config
   directory; if it changed, decode it and trial-compile it (`trial`) with
   the state's entitlements, as it will be published. A config that
   compiles becomes the **last good config**; one that does not is
   reported (`cfgErr`) and the last good one stays in use, so a broken edit
   cannot let every gateway's authorization lapse. That includes an edit
   that, against the state's scopes, leaves a budgeted scope reaching a
   model no PriceList prices where the last good config priced it (a price
   removed, a route moved): the edit is the config's mistake. Until some
   config has compiled, nothing is published.
4. **Due?** (`due`, `controller.go:130`) A publication is due when the good
   config's hash, the entitlements' hash or the epoch differs from the last
   publication's, or the renewal interval (`Renew`, default a minute) has
   passed. Renewal extends the snapshots' authorization lease (`Lease`,
   default four hours; ADRs 0030, 0032).
5. **Known spend**: if a known-spend job is set, run it (§4.1).
6. **Publish** every pool (`publish`, `controller.go:139`), in sorted order,
   through one `PublishAll` with the same snapshot built by
   `compile.Compile` from the good config and `entitlements`
   (`controller.go:165`): the state's `Document.Entitlements` (scopes'
   models limited to routes that exist) with known spend laid over it.
   Only if every pool succeeded is the publication remembered; otherwise
   the next step tries them all again.

**A problem of the state's degrades one scope, not the publication.** A
scope with a budget is charged by price, so a scope that reaches a model
no PriceList prices could not be charged (`checkPriced`,
`lib/compile/compile.go`). A config that causes it is refused at step 3.
When the state causes it (a budgeted scope created, or given a model,
over an unpriced alias), the compile is told by `Options.Unpriced`: the
scope is published **without that alias** (`withoutUnpriced`), so it
fails closed (its members cannot call what it could not be charged for;
a key narrowed to that alias calls nothing, since the gateway intersects
a key's models with its scope's), and every other scope and pool is
published as usual. Each such alias is the operator's status
(`Controller.Unpriced`) and is reported with every step's error
(`ErrUnpriced`, naming the scope, alias and model, and the fix: price the
model, or take the alias out of the scope) until it is fixed. Before
this, one such scope failed every pool's compile on every step, and every
gateway's authorization lapsed within its lease. The control API refuses
the mistake where it is made: `Service.CreateScope` checks a budgeted
scope with `Service.Priced`, which golem-control sets to
`Controller.CheckScope` (the last good config, or on a replica where none
has compiled, the directory's). `TestUnpricedScope`, `TestCreateScopePriced`.

### 4.1 Known spend

With `-clickhouse`, `Controller.Spend` is a `spend.Job`: when a
publication is due, `Step` runs `spend.Job.Step`, and `entitlements` puts
its current result into every scope of the snapshot (`Period`, `Since`,
`KnownSpend`, `controller.go:172`): each scope's spend this period from
the ledger, up to a boundary a few minutes back. Gateways add their site's
own minutes from that boundary on (ADR 0048). A failed query keeps the
last result for its period and is reported with the step; with no result
at all a snapshot carries none and gateways count their site alone. How
the job queries and why the boundary is safe:
[accounting.md](accounting.md) §2 and §5.

## 5. The leader's jobs

### 5.1 Audit shipping

`Shipper.Step` (`control/audit/audit.go:48`) takes up to `Batch` (100) of
the outbox's oldest events and, in order:

1. **Archives** each as `audit/<seq>.json` (`Key`, `audit.go:32`),
   create-only (`If-None-Match: *`), in an Object Lock bucket in a
   separate account (`archive`, `audit.go:80`). If the object exists (a
   step that died before its trim), it must be byte-identical, or shipping
   stops with `ErrMismatch` and nothing is trimmed: a person looks.
2. **Emits** each as a `golem.audit` OTel log record for ClickHouse
   (`emit`, `audit.go:101`), at least once; ClickHouse keeps one row per
   seq.
3. **Trims** the outbox up to the last event archived, by `Update`
   (`audit.go:70`). Events written meanwhile are numbered higher and stay.

The archive, not the collector, is the durability boundary: an event leaves
the outbox only once it is durably archived.

### 5.2 Entra group sync

`Sync.Step` (`control/entrasync/sync.go:79`, ADR 0047) runs from the
controller loop (`syncStep`, `controller.go:251`) every `SyncEvery` (five
minutes) while this replica leads, under the elector's fence:

1. **Read every bound group first** (`boundGroups`: each scope's
   `Groups`), through `Directory.Members`: `Graph` (`graph.go`) lists a
   group's transitive user members (`id`, `userPrincipalName`,
   `accountEnabled`), following `@odata.nextLink` only on Graph's own
   host, with a token from `FederatedToken` (the pod's projected service
   account token as the client assertion). Any failure but a deleted group
   (`ErrNoGroup`, no members) ends the step with nothing written.
2. **Reconcile** in one `FencedUpdate` (`reconcile`, `sync.go:162`), so a
   replica deposed while it read the directory writes nothing: `wanted`
   gives each enabled member of a group bound to a scope that scope in
   `Synced`; a user not yet a principal is created (`reconciler.create`,
   `principalID`: the sign-in name, lowercased; with part of the object ID
   if taken; `user-<oid>` for a name in the reserved `entra:` namespace);
   a user in no bound group, or disabled, has `Synced` emptied. Each
   changed principal's keys outside its memberships are revoked
   (`reconciler.setSynced`, `Document.RevokeOutside`), a disabled user's
   all of them, and the change is audited by `system:entra-sync`.
3. **Guard**: a step ending more than `MaxRemovals` (50) memberships
   returns `ErrTooManyRemovals` and writes nothing; a step changing nothing
   writes nothing either.

Memberships granted in golem (`Scopes`) are never the sync's: a member by
both ways keeps one when the other ends, and `members remove` refuses a
membership that is only a group's.

## 6. The binary

`main` (`cmd/golem-control/main.go:53`) dispatches the subcommands
(`dispatch`, `main.go:61`); anything else is golem-control itself, `run`
(`main.go:188`): `parseFlags` (`main.go:152`), the bucket and signer
opened, the state loaded, `newController` (`main.go:262`) wiring an
elector and a publisher per pool and the jobs the flags ask for, then the
state's refresh loop, the API (if `-listen`) and the controller until
SIGTERM or a component fails. Shutdown is in order: the publishing loop
stops, the lease is released, the API drains, and the audit's log records
are flushed.

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
| `-clickhouse`, `-clickhouse-user` | the ledger's ClickHouse for known spend (password in `$CLICKHOUSE_PASSWORD`); empty: snapshots carry none |
| `-spend-allowance` | how far back known spend's boundary is (5m) |
| `-period-zone` | the budget periods' time zone (default UTC; the gateways' `-period-zone`) |

The subcommands:

- `golem-control keygen <dir>` (`keygen`, `main.go:438`) writes a
  development key pair (`publishing.key`, never overwritten, and
  `publishing.pub` for gateways). `TestKeygen`.
- `golem-control pubkey <signing key>` (`pubkey`, `main.go:367`) prints the
  key's public half for gateways' `-keys` directories: a PEM with a
  `Key-ID` header (a KMS key's ARN, a file key's name), which
  `sign.LoadKeys` trusts under that ID. `TestPubkey`.
- `golem-control restore -bucket <...> -from <document.json> -actor <who>
  [-apply]` (`restore`, `main.go:88`) runs `Store.Restore` (§1.2) with a
  backup document taken from the bucket's history or its Object Lock
  replica. Without `-apply` it is a dry run: it prints what would be
  withheld and rolled back and writes nothing. `TestRestoreCommand`.

## 7. Worked example: a key revoked through the API reaches a gateway

1. **The API call.** `keys.RevokeKey` (`control/api/keys.go:30`) calls
   `Service.RevokeKey` (`control/service/service.go:367`), which checks the
   caller is the key's owner, an admin, or, for a service's key, a manager
   of its scope, and calls `Service.apply` (`service.go:93`) with a change
   that calls `Document.Revoke` (`control/state/entitlements.go:217`): the
   key's `RevokedAt` is set and a `Revocation` appended. `apply` adds a
   `key.revoke` audit event.
2. **The state update.** `Store.Update` (`state.go:211`) copies the
   document, applies the change, numbers the audit event (`state.go:80`),
   checks the invariants (`checkRevocations` among them), and writes
   `stateVersion + 1` with `If-Match` (`state.go:233`). On a lost race it
   re-reads and reapplies; on a lost reply it finds its own write ID.
3. **The controller's next step**, within a second on the leader (it may
   be another replica: its `Refresh` at `controller.go:105` reads the new
   document). `entitlementsSum` (`controller.go:109`) now differs, so
   `due` (`controller.go:113`) says a publication is due.
4. **The publication.** `Controller.publish` calls `PublishAll`
   (`controller.go:145`) under the elector's fence. Each pool is numbered
   above its record, checkpoint and pointer (`publish.go:189`-`193`), its
   snapshot built by `compile.Compile` with `Controller.entitlements`, in
   which `Document.Entitlements` (`entitlements.go:247`) lists the key with
   `Revoked: true` (`entitlements.go:289`). Every pool's record is won in
   one `FencedUpdate` (`publish.go:141`), then each checkpoint and signed
   pointer is written (`publish.go:218`, `:228`).
5. **The gateway's poll** (`gateway/snapshot/bucket.go:172`, `Bucket.Poll`),
   every second: a conditional get of the pointer, now changed. `read`
   (`bucket.go:272`) verifies the signature, the pool and the floor
   (`openPointer`), fetches the snapshot, checks its hash and sequence
   (`openSnapshot`), and `Holder.Apply` (`gateway/snapshot/holder.go:109`)
   swaps it in. The gateway refuses the key by name from then on.

End to end: the write plus up to a second for the controller and a second
for the poll. `control/controller`'s `TestEntitlements` runs exactly this
path with a real gateway source over one bucket.

## 8. How it is tested

- `control/state`: the store's update, retry under 8 concurrent writers,
  a document gone back refused (`TestRegressed`), every invariant above
  (`TestInvariants`, `TestFenceInvariant`, `TestEntitlementInvariants`,
  `TestRolesAndAudit`), and the restore (`TestRestore`,
  `TestRestoreNeverWidens`).
- `control/lead`: the tests in §2.
- `control/publish`: the tests in §3.
- `control/entrasync`: the sync (`TestSync`, `TestSyncNames`), its safety
  (`TestSyncSafety`), Graph and the token (`TestGraph`,
  `TestFederatedToken`), and a sync deposed mid-read writing nothing
  (`TestSyncFenced`).
- `control/controller`: two replicas and a gateway over one bucket
  (`TestPublishes`: a change arrives, a renewal extends the lease, a
  broken edit renews the last good config, a paused leader is fenced), the
  revocation path (`TestEntitlements`), the group sync reaching the
  gateway (`TestGroupSync`), known spend reaching it (`TestKnownSpend`),
  and an unpriced budgeted scope failing closed alone while every pool is
  published, an edit leaving one unpriced refused (`TestUnpricedScope`).
- `control/audit`: archive, emit, trim; a retry after a crash; a mismatch;
  batches.
- `cmd/golem-control`: `TestKeygen`, `TestOpenSigner`, `TestAPINeedsTenant`,
  `TestPubkey`, `TestRestoreCommand`.
- `model/check-leadership.sh`: the lease and fence placements compared by
  exhaustive search, each mechanism shown necessary.
- `gateway/snapshot/publication_mbt_test.go`: model/publication.qnt's
  traces drive the real `Elector`, `Publisher` and gateway source,
  including replayed pointers and empty-disk restarts
  (`QUINT_BACKEND=typescript go test ./gateway/snapshot/`). The Hegel
  properties in `test/props` (a module of its own) exercise publication
  from the gateway's side.

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
- **Restore over the API**: `golem-control restore` runs against the
  bucket directly; `golemctl state restore` over the admin API is next
  (control-plane.md §3). `ErrRegressed`'s message already names that
  command rather than `golem-control restore`.
- **One failing pool republishes the rest every second**: `Step` records a
  publication only when every pool succeeded, so while one pool fails (a
  tampered checkpoint, say) the others are published, and the document
  written, at every step rather than every renewal interval.
- **An unpriced scope's status is a log line**: a budgeted scope published
  without its unpriced aliases is reported in the step's error (logged at
  error level once until it changes) and by `Controller.Unpriced`; no CRD
  status or API field shows it yet. `CreateScope` refuses the mistake, but
  nothing else writes a scope's models or budget yet; when something does
  (a scope update), it needs the same check.
- **The audit trim is not fenced**: `Shipper.Step` trims with `Update`, not
  `FencedUpdate`. It only removes events already archived, which
  `outboxFollows` checks, so a deposed leader's trim loses nothing.
