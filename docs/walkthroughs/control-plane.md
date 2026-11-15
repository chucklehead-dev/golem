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
| `control/metrics` | golem-control's metrics: observable instruments over the controller's state |
| `cmd/golem-control` | the binary: flags, wiring, `keygen`, `pubkey`, `restore` |

The API that writes the document (`control/service`, `control/api`) is
[control-api.md](control-api.md)'s; known spend's job (`control/spend`) is
[accounting.md](accounting.md)'s.

## 1. The state document and its store

Everything golem-control knows is one JSON document, `state/document.json`
in the bucket (`DocumentKey`, `control/state/state.go:35`). `Document`
(`state.go:54`) holds the fence (`Fence`, `state.go:48`: who leads, at
which epoch), the write IDs of the updates that wrote its last 32
versions (`Writes`, newest last; `RecentWrites`), each
pool's publication record (`Publications`, a `Record` of epoch, sequence
and source state version, `state.go:40`), the entitlements (`Principals`,
`Scopes`, `Keys`, `Revocations`, `Roles`; `entitlements.go`) and the audit
outbox (`Audit`, `AuditSeq`). Decoding is strict (`Decode`,
`state.go:443`): another schema or an unknown member is refused, so an
older replica never silently drops a field a newer one wrote.

**Reads are from memory.** `Store.Current` (`state.go:263`) hands out a
copy of the document as last read or written. `Load` is a full get;
`Refresh` is a conditional get with the entity tag the store holds, so an
unchanged document costs one 304 (`refresh`, `state.go:173`). `Store.Run`
(`state.go:456`) refreshes every 500 ms. `set` (`state.go:251`) never
moves the document back: a slow get that returns an older `stateVersion`
than one this store wrote is ignored.

**Never adopting a document that went back.** If the bucket holds an older
version than the store has seen, or none (an out-of-band put of an old
version, a deletion), `refresh` returns `ErrRegressed` (`state.go:132`,
message from `regressed`, `state.go:217`) and the store keeps serving what
it last read and writes nothing: adopting the older document would drop
every later revocation. A restore goes through `Restore` (§1.2) instead.
`TestRegressed` covers both cases.

A store that has read nothing yet has no memory to compare with, so the
bucket's other objects tell a first start from a deletion: `Installed`
(set by golem-control to the lease object and each configured pool's
checkpoint and pointer; the bucket interface cannot list) are looked for
when there is no document (`installed`, `state.go:228`), and if one
exists, `Load` is `ErrMissing` (message from `missing`, `state.go:223`:
put the newest version back from the bucket's versions, or bootstrap on
purpose), not the empty document. Before this, the next replica to start
after a deletion claimed the fence over an empty document and published
snapshots with no principals or keys: every caller refused. Only
`Store.Bootstrap` (`bootstrap.go`, `golem-control bootstrap`, §6) creates
a document then, create-only (`If-None-Match: *`), each pool's record at
what the bucket proves was published, so publication goes on above what
gateways trust; with `-adopt` it instead accepts a document put back to an
older version, raising its records the same way. A restore over no
document is refused too: restored over nothing, every right would be
withheld and an empty document written. `TestMissingDocument`,
`TestBootstrapCommand`.

A document put back to an older version is still adopted by a store
started after it (nothing in memory to compare). What catches it is the
publisher (§3, step 2): its records are below what the bucket proves was
published, so no pool is published from it.

**Writes go through `Update`** (`state.go:288`), a compare-and-swap with
retry:

1. Take the current document and its tag.
2. `apply` (`state.go:379`): run the caller's `change` on a copy, number
   the new audit events, check the invariants against the document it
   started from. A `change` error or a broken invariant ends the update;
   nothing is written.
3. Increment `stateVersion`, add a fresh write ID to `Writes` (`wrote`,
   which keeps the last `RecentWrites`) and `Put` with `If-Match: <tag>`.
4. On `ErrPrecondition`, `Load`. If the document there has this
   attempt's write ID among its `Writes`, the put landed and only its
   reply was lost (a retried conditional put fails its own condition):
   the update succeeded, and returns the current document without
   applying the change again, even if other writers have written since.
   Before the ring, only the latest write's ID was kept, so a landed
   write that another overtook before the re-read was applied twice (two
   audit events; a removal that landed answered `NotFound`);
   `TestLostReplyOvertaken`. Otherwise another writer won: go round again, running
   `change` afresh on the winner's document. After `Attempts` (default 8)
   losses it returns `ErrContended`, which the API reports as `Aborted`
   (retry).

Because `change` may run more than once it must be a function of the
document it is given, not of state outside it. The documents a store holds
are replaced, never edited in place: that is what lets `apply` read the
base without copying it twice.

**Leader-only writes go through `FencedUpdate`** (`state.go:341`): an
`Update` whose change first checks that the document's fence is still the
writer's (holder and epoch), else `ErrFenced` (`state.go:135`) and nothing
is written. The check is inside the same compare-and-swap, so a leader
deposed while it worked (a pause, a slow directory read) writes nothing.
Publication (§3), the group sync (§5.2) and the audit trim (§5.1) use it.

**Validate-only.** `Store.Validate` (`state.go:356`) runs the same `apply`
and writes nothing: the API's `validate_only` (control-plane.md §2). The
service layer builds every change, with its audit event, once and hands it
to either (`control/service/service.go:99`, `Service.apply`).

**The audit outbox's numbering.** A change appends `AuditEvent`s with
`Seq` 0; `Document.number` (`state.go:97`) gives each the next number from
`AuditSeq` as part of the write, so numbers are assigned under the same
compare-and-swap as the change and a retried change is numbered afresh.

**The audit outbox's bound** (`control/state/outbox.go`, ADR 0041 §1).
The outbox is in the document every write and every 500 ms read carries,
and only the leader's shipper trims it, after archival (§5.1): without
`-audit-archive`, or with the archive unreachable, it would grow without
limit. `Store.MaxOutbox` bounds it (`DefaultMaxOutbox`, 10,000;
`golem-control -audit-outbox-max`), and `checkOutbox` (`outbox.go:148`), the last check
`apply` makes, judges every write by the events it adds:

- **Past the reserve** (`Reserve`: 80% of the bound) a write that grants
  or widens access is refused with `*state.OutboxFull`
  (`ErrOutboxFull`), naming the action, how it widens, and the fix (the
  archive is not draining: `-audit-archive` and its bucket; operations.md's
  *Audit outbox not draining*). Restrictive writes land, into the last
  20%, which is theirs.
- **At the bound** every write that adds an event is refused, revocations
  too (`OutboxFull.Hard`): the document must not grow without limit, and
  no change is written without its event. A revocation then goes through
  the site's emergency denylist.
- A write that adds **no** event (publication records, the fence, the
  shipper's trim) is never refused by it.

A write is restrictive only if every event it adds is of a `Restrictive`
action in the class table (`actions`, `outbox.go:69`, beside the action names: revoking a
key, removing a member, unbinding a group, the sync's removals, a restore,
a bootstrap or adoption) **and** the change itself widens nothing, judged
from the documents before and after (`widens`: a scope created or a group
bound, a principal created, given an identity or made a member, a role
bound, a live key minted, moved, revived, or widened in models or expiry).
So a restrictive label cannot carry a grant, and a mixed change is a
widening one (ADR 0041: classification by the effective diff, not the
method). An event of an action not in the table is refused at every write
(`ErrInvariant`), and `TestEveryActionClassified` finds every action
written in `control/` and `cmd/` (an `AuditEvent`'s `Action`, or the
argument of a parameter named `action`) and fails on one unclassified or
a table entry nothing writes. Past the reserve a grant is refused as a
grant even when it would also pass the bound, so a writer can retry its
restrictive part alone (the group sync, §5.2).

### 1.1 The invariants

`check` (`state.go:400`) runs on every write and validation, in order:

| Invariant | Where | Tested by |
| --- | --- | --- |
| The fence's epoch never goes back, and the fence is never dropped | `checkFence`, `state.go:415` | `TestFenceInvariant` |
| A principal is a user or a service; its ID is not in the reserved `entra:` namespace; its scopes exist; its default is one of them; only a user with an Entra ID has synced memberships | `checkPrincipals`, `entitlements.go:139` | `TestEntitlementInvariants` |
| No two principals share an Entra ID | `checkPrincipals` | `TestEntitlementInvariants` |
| A key's owner exists; a live key's owner is a member of its scope | `checkKeys`, `entitlements.go:171` | `TestEntitlementInvariants` |
| A role is `manager`, of an existing principal on an existing scope | `checkRoles`, `entitlements.go:186` | `TestRolesAndAudit` |
| The outbox loses only its oldest events and gains new ones numbered on without a gap; the counter never goes back | `outboxFollows`, `entitlements.go:306` | `TestRolesAndAudit`, `control/audit`'s tests |
| Revocations are append-only, and a revoked key stays revoked | `checkRevocations`, `entitlements.go:204` | `TestEntitlementInvariants` |
| No pool's publication record is removed or goes back in sequence, epoch or source | `checkPublications`, `state.go:425` | `TestInvariants` |
| A store that has read nothing starts from an empty document only where golem-control has not run (no `Installed` object), else `ErrMissing`; only `Bootstrap` creates one then | `refresh`, `installed`, `state.go:228` | `TestMissingDocument`, `TestBootstrapCommand` |
| No pool is published from a record below what the bucket proves (its verified checkpoint or pointer): the document went back, `ErrRegressed` | `prepare` and `below`, `control/publish/publish.go:203` | `TestGuard` |
| A write whose reply was lost is applied once: recognised by its ID among the document's last 32 (`Writes`), even after others wrote over it | `Update`, `wrote`, `state.go` | `TestLostReplyOvertaken`, `TestLostClaim` |
| A leader-only write lands only under its own fence | `FencedUpdate`, `state.go:341` | `TestFenceInvariant`, `TestStaleLeaderFenced`, `TestSyncFenced`, `TestDeposedTrim` |
| A write that adds audit events never leaves the outbox above its bound (`Store.MaxOutbox`); past 80% of it, only a write whose events are all restrictive and whose change widens nothing adds any | `checkOutbox`, `outbox.go:148`; `widens`, `outbox.go:191` | `TestOutboxBound`, `TestOutboxDefault`, `TestWidens`, `TestRestoreLandsPastReserve`; `control/service`'s `TestOutboxBackpressure`, `control/audit`'s `TestShippingReopensGrants` |
| Every audit event is of a classified action | `checkOutbox` (`ErrInvariant`); the source, by `TestEveryActionClassified` | `TestUnclassifiedAction`, `TestEveryActionClassified` |

Every refusal wraps `ErrInvariant`, which the API maps to Connect's
`FailedPrecondition` (`control/api/api.go`, `connectError`), except the
outbox's, `ErrOutboxFull`, which is `ResourceExhausted` and retryable once
the outbox drains.

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
- what both have takes the narrower of each (`principal`, `key`): bound
  groups, memberships and a key's models intersected (`meetSet`), a key's
  earlier expiry; a principal's identity is always the current one's.

A scope's policy is not in the document (its ScopePolicy is config, ADR
0050), so a restore has none to bring back or narrow: a scope's record is
its bound groups, intersected like any other list.

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

`PublishAll` (`control/publish/publish.go:123`) publishes every pool given
the fence the elector holds (`Lease`, an alias of `state.Fence`,
`publish.go:85`) and `source`, the state version the snapshots are built
from. `Publisher.Publish` (`publish.go:107`) is the same for one pool. In
order:

1. **Fencing**: refresh the state; if the fence is not this one (`holds`,
   `publish.go:319`, so a deposed publisher writes no snapshot either), or
   a pool's record has a later epoch or source (`prepare`), that is
   `ErrFenced`.
2. **Numbering** (`floor`, `publish.go:334`; the model's GUARD and
   VERIFY): the floor is the highest of the pool's verified checkpoint and
   its signed pointer (each counted only if it verifies as this pool's),
   and the next publication is one above the record. A record **below**
   the floor (`publish.go:203`) can only mean the document went back: a
   record is written before its checkpoint and pointer, and a restore keeps
   the current records. `below` (`publish.go:232`) re-reads the document
   first, so a record another publisher won meanwhile is `ErrConflict` (or
   `ErrFenced`), not a rollback; otherwise it is `ErrRegressed`: that pool
   is not published, and the error says to put the newest document back,
   or, if it cannot be recovered, redo the revocations since and run
   `golem-control bootstrap -adopt`. The model's guard numbers past such a
   record instead, which keeps the floor but would publish the older
   document's entitlements, revoked keys revived (model/leadership.md §6).
   A checkpoint that does not verify, or is missing once a publication was
   made (the record above 1, or a signed pointer present), is
   `ErrTampered`: that pool is not published until a person looks. Record
   1 without a checkpoint is a publisher that died between the first two
   writes, not tampering. The checkpoint and pointer are verified with the
   signer's own public half or any of `Keys` (`keys`, `publish.go:421`):
   golem-control's `-trusted-keys`, the retiring keys during a rotation.
   What a retiring key signed counts, and step 5 re-signs it with the
   current signer, so the publication after needs only the new key
   (operations.md §2.1). Before `-trusted-keys`, a rotated signer met the
   old key's checkpoint as `ErrTampered`, and every pool stopped.
3. **Build** each snapshot and write it, content-addressed (`prepare`,
   `publish.go:194`). A pool that fails here is left out; the others go on.
4. **Win every pool's record in one `FencedUpdate`** (`publish.go:154`):
   one document version a cycle, whatever the number of pools (ADR 0049).
   If a record changed since step 1, `ErrConflict`. The store's
   `checkPublications` keeps each record monotonic.
5. Each pool's signed **checkpoint** `control/<pool>/checkpoint`, then its
   signed **pointer** (`finish`, `publish.go:260`), each only forward
   (`advance`, `publish.go:381`): written conditionally on the object
   read, refused (`ErrFenced`) over a later publication's (a successor's,
   while this publisher was paused). A stored checkpoint that does not
   verify is `ErrTampered`; a stored pointer that does not verify is
   replaced (gateways refuse it too). The pointer carries its issue time
   and `Expires` (`PointerTTL`, ten minutes), after which gateways report
   it overdue.

**Why the record, not the pointer.** Anyone who can write the pool's
objects but cannot sign can copy an old signed pointer back (the model's
`replayPointer`). Numbering from the pointer alone would let that replay
set the sequence back; a record checked against the other two cannot be
set back by any one of them (a rolled-back record halts publication; an
old checkpoint or pointer copied back is below the record and changes
nothing).

A publisher that dies after step 4 leaves the checkpoint and pointer behind
the record; the next publication (the renewal, within a minute) moves them
on. Until then a gateway restarted with an empty disk takes the older
checkpoint as its floor.

The tests (`control/publish/publish_test.go`): `TestPublish`, `TestFenced`
(the model's FENCE), `TestConcurrentPublishers`,
`TestLeaseLostBeforeRecord`, `TestPausedAfterRecord`, `TestGuard`
(model/leadership.md's counterexamples, each refused: a rolled-back record,
alone or met by an older checkpoint, halting until adopted; a race for the
record and another pool's signed pointer not mistaken for one; a forged or
deleted checkpoint; and the first publication's crash window not mistaken
for tampering),
`TestPublishAll` (one state write for several pools; a failing pool left
out without stopping the others) and `TestKeyRotation` (a signer rotated
with the old key trusted goes on and re-signs; without it, `ErrTampered`;
`cmd/golem-control`'s `TestTrustedKeys` runs the same through the flag).

## 4. The controller loop

`Controller.Run` (`control/controller/controller.go:643`) starts the
elector's loop and, every second, runs `tick` (`controller.go:668`): a
publication if due (`Step`), then, while this replica leads, the audit
outbox shipped (§5.1) and the group sync if due (§5.2). Each error is
logged once until it changes. `Run` returns only once the elector's loop
has, so a `Release` after it cannot be undone by a renewal in flight.

`Controller.Step` (`controller.go:294`):

1. **Not leading** (`Elector.Fence` reports none): do nothing.
2. **Entitlements**: refresh the state (`controller.go:302`) and hash its
   principals, scopes, keys and revocations (`entitlementsSum`,
   `controller.go:710`). The state version is not hashed: the publisher's
   own record write moves it at every publication.
3. **Config compile** (`load`): hash the config directory; if it
   changed, decode it and trial-compile it with the state's entitlements,
   as it will be published. A config that compiles becomes the **last good
   config**; one that does not is reported (`cfgErr`) and the last good one
   stays in use, so a broken edit cannot let every gateway's authorization
   lapse. That includes a ScopePolicy whose budget reaches a model no
   PriceList prices (a price removed, a route moved, a budget added): both
   are config, so the edit is the config's mistake, and `golemctl config
   validate` finds it before it is merged.
   **Until some config has compiled** since this replica started (a
   restart, or a takeover, while the directory is broken), the last good
   config is the published one: `loadPublished` reads each pool's current
   pointer and snapshot once, verified as a gateway verifies them
   (`Publisher.Published`: the pointer's signature and pool, the
   snapshot's hash and sequence), and each publication renews it
   (`compile.Renew`). A published snapshot decomposes: its backends,
   routes, currency and prices are the config's alone, and each scope's
   policy is its ScopePolicy's (a `LocalIdentity` being refused outside
   standalone mode), so `Renew` keeps them as published and gives the
   publication a new sequence, lease and the current entitlements, each of
   the state's scopes joined with the policy it was published with (a
   scope created since has none: it is published as `DefaultScope` and
   reported, as `Compile` reports it). The step's error
   says so (`renewing the published config: the config directory does not
   compile`, with the directory's problems) every step until the
   directory compiles; then that config is published. With nothing
   published to renew either, nothing is published. Before this, the last
   good config lived only in memory, and a restarted leader over a broken
   directory published nothing until every gateway's lease lapsed.
4. **Due?** (`due`, `controller.go:463`) Each pool is due on its own
   (`poolState`): when the good config's hash, the entitlements' hash or
   the epoch differs from its last publication's, or the renewal interval
   (`Renew`, default a minute) has passed. Renewal extends the snapshots'
   authorization lease (`Lease`, default four hours; ADRs 0030, 0032). A
   pool whose last attempt failed is due once its backoff has passed
   (`backoff`: `Retry`, two seconds, doubling at each failure in a row up
   to the renewal interval, less up to a quarter of jitter, so it is still
   tried at least once a renewal interval), or at once if what is to be
   published changed since that attempt (a revocation is not held back by
   a retry's wait). Nothing due: nothing is published, and the step's
   error still names each failing pool's last failure.
5. **Known spend**: if a known-spend job is set, run it (§4.1).
6. **Publish** every pool (`publish`, `controller.go:473`), in sorted order,
   through one `PublishAll` with the same snapshot built by
   `compile.Compile` from the good config (or `compile.Renew`, step 3) and
   `entitlements`: the state's `Document.Entitlements` (scopes by ID, keys'
   models limited to routes that exist) with known spend laid over it,
   which the compile joins with the ScopePolicies (below).
   Only the due pools are published. Each that succeeded records its
   publication and clears its failures; each that failed counts one more
   failure and waits its backoff. Before this, a publication was
   remembered only if every pool succeeded, so one pool failing for good
   (a tampered checkpoint) made every step due: the healthy pools were
   published, and the document written, every second rather than every
   minute. `TestFailingPoolBacksOff`.

**A mismatch of state and config degrades one scope, not the
publication.** A scope exists in the state (an admin created it through
the control API) and its policy is its ScopePolicy, in the config (ADR
0050); the compile joins them by ID (`join`, `lib/compile/policy.go`).
The two change apart (an admin creates a scope, a pull request adds its
policy, in either order), so neither order may stop a publication. A
scope of the state's with no ScopePolicy is published as
`compile.DefaultScope`: **no models**, so its members can call nothing
(it fails closed; a policy is what grants anything). A ScopePolicy naming
a scope the state does not have is left out. Both go to
`Options.Unmatched`: the operator's status (`Controller.Unmatched`),
reported with every step's error (`ErrUnmatched`, naming each and the
fix: add the ScopePolicy, or create the scope) until they match, and
counted as `scope_policy_unmatched` against the pools attempted; every
other scope and pool is published as usual. `TestUnmatchedScopes`,
`TestUnpricedPolicy`, `lib/compile`'s `TestJoin`.

The control API checks a key's models against its scope's ScopePolicy
where the key is minted: `Service.MintKey` asks `Service.ScopeModels`,
which golem-control sets to `Controller.ScopeModels` (the last good
config, or on a replica where none has compiled, the directory's; a
scope with no policy allows none, and a directory that does not compile
checks nothing). `TestScopeModels`, `TestKeyModelsFromConfig`.

### 4.1 Known spend

With `-clickhouse`, `Controller.Spend` is a `spend.Job`: when a
publication is due, `Step` runs `spend.Job.Step`, and `entitlements` puts
its current result into every scope of the snapshot (`Period`, `Since`,
`KnownSpend`, `controller.go:536`): each scope's spend this period from
the ledger, up to a boundary a few minutes back. Gateways add their site's
own minutes from that boundary on (ADR 0048). A failed query keeps the
last result for its period and is reported with the step; with no result
at all a snapshot carries none and gateways count their site alone. How
the job queries and why the boundary is safe:
[accounting.md](accounting.md) §2 and §5.

### 4.2 What the loop reports

A stalled control plane must be seen before the gateways' lease runs out,
so the loop keeps a record of its own work (`status`, under the
controller's mutex, written by the loop and copied out by accessors that
hold the lock for a map copy, never across I/O), and `control/metrics`
(`Register`, `control/metrics/metrics.go:49`) exports it as observable
instruments, read when the meter collects:

- **Each pool's last successful publication** by this replica
  (`Publications`, `controller.go:215`): its time by the controller's
  clock, its sequence and the lease it issued (the built snapshot's
  `ValidUntil`, kept from the build callback), recorded in `publish` per
  pool, so a pool that publishes while another fails still moves. A
  replica that stops leading keeps its last values; the alerts take the
  newest across replicas.
- **Failures** (`Failures`, `controller.go:225`): a count by pool and
  reason. Each pool's own failure in `publish` is classified by
  `FailureReason` (`controller.go:162`), by `errors.Is` against the
  sentinels: `tampered` (`publish.ErrTampered`), `regressed`
  (`publish.ErrRegressed`, `state.ErrRegressed`, `state.ErrMissing`),
  `fenced` (`publish.ErrFenced`, `state.ErrFenced`), `conflict`
  (`publish.ErrConflict`, `state.ErrContended`), else `other`. A step that
  cannot read the state counts its reason against every pool, every second
  it fails; an attempted publication (a due step) also counts
  `config_renewing` while the directory is not what is published and
  `scope_policy_unmatched` while scopes and ScopePolicies do not match
  (`degraded`), against the pools it attempted. Steps with nothing
  due count nothing, so a pool that keeps failing counts once per retry
  of its backoff: at least once a renewal interval.
- **The config's state**: `ConfigRenewing` (the last step found the
  directory not compiling, or an edit refused) and `Unmatched`.
- **The jobs**: the last audit step and group sync that succeeded
  (`ShippedAt`, `SyncedAt`, set in `tick` and `syncStep`), and the sync's
  removal guard tripping (`GuardTrips`); the outbox's length is the
  store's (`Store.OutboxLen`, `state.go:275`: a `len` under its lock, no
  copy of the document), and its bound (`Store.OutboxMax`), so the alert
  is a ratio; known spend's age and staleness are the job's
  own (`spend.Job.Age`, `Stale`).

| Instrument | Read from |
| --- | --- |
| `golem.control.publish.time`, `.seq`, `.valid_until` {pool} | `Controller.Publications` |
| `golem.control.publish.failures` {pool, reason} | `Controller.Failures` (every pool and reason, from zero) |
| `golem.control.leader`, `golem.control.leader.epoch` | `Elector.Epoch` |
| `golem.control.scope_policy.unmatched`, `golem.control.config.renewing` | `Controller.Unmatched`, `ConfigRenewing` |
| `golem.control.spend.age`, `.stale` {period} | `spend.Job.Current`, `Age`, `Stale` |
| `golem.control.entra_sync.time`, `.guard_trips` | `Controller.SyncedAt`, `GuardTrips` |
| `golem.control.audit.outbox`, `golem.control.audit.outbox_max`, `golem.control.audit.shipped.time` | `Store.OutboxLen`, `Store.OutboxMax`, `Controller.ShippedAt` |

Units, the alerts over them and their runbooks: operations.md §2.2.

## 5. The leader's jobs

### 5.1 Audit shipping

`Shipper.Step` (`control/audit/audit.go:51`) takes up to `Batch` (100) of
the outbox's oldest events and, in order:

1. **Archives** each as `audit/<seq>.json` (`Key`, `audit.go:32`),
   create-only (`If-None-Match: *`), in an Object Lock bucket in a
   separate account (`archive`, `audit.go:83`). If the object exists (a
   step that died before its trim), it must be byte-identical, or shipping
   stops with `ErrMismatch` and nothing is trimmed: a person looks.
2. **Emits** each as a `golem.audit` OTel log record for ClickHouse
   (`emit`, `audit.go:104`), at least once; ClickHouse keeps one row per
   seq.
3. **Trims** the outbox up to the last event archived, by `FencedUpdate`
   under the fence `tick` passes (`Elector.Fence`), as every leader-only
   write: a shipper deposed while it archived trims nothing
   (`state.ErrFenced`); the new leader finds the objects identical and
   goes on (`TestDeposedTrim`). Events written meanwhile are numbered
   higher and stay.

The archive, not the collector, is the durability boundary: an event leaves
the outbox only once it is durably archived. While it is not (no
`-audit-archive`, or the archive refusing), the outbox fills towards its
bound (§1): past 80% grants are refused, at 100% every audited change,
and the first step that archives and trims makes room again
(`TestShippingReopensGrants`).

### 5.2 Entra group sync

`Sync.Step` (`control/entrasync/sync.go:109`, ADR 0047) runs from the
controller loop (`syncStep`, `controller.go:687`) every `SyncEvery` (five
minutes) while this replica leads, under the elector's fence:

1. **Read every bound group first** (`boundGroups`: each scope's
   `Groups`), through `Directory.Members`: `Graph` (`graph.go`) lists a
   group's transitive user members (`id`, `userPrincipalName`,
   `accountEnabled`), following `@odata.nextLink` only on Graph's own
   host, with a token from `FederatedToken` (the pod's projected service
   account token as the client assertion). An account Graph does not
   report as enabled is a member of nothing; only an explicit
   `accountEnabled: false` makes it `Disabled`. Then **the users who
   left** (`left`: principals with synced memberships in no group read)
   are looked up one by one (`Directory.User`; `Graph.User`: `/users/<id>`,
   failing that `/directory/deletedItems/<id>`), because Entra drops a
   deleted user from its groups and a departure alone cannot tell a
   deletion from a membership removed: an account in the deleted items is
   `Deleted`; one in neither (`ErrNoUser`, purged) is a plain removal. A
   404 alone is never taken for a deletion. Any failure but a deleted group
   (`ErrNoGroup`, no members) or `ErrNoUser` ends the step with nothing
   written.
2. **Reconcile** in one `FencedUpdate` (`reconcile`, `sync.go:255`), so a
   replica deposed while it read the directory writes nothing: `wanted`
   gives each enabled member of a group bound to a scope that scope in
   `Synced`; a user not yet a principal is created (`reconciler.create`,
   `principalID`: the sign-in name, lowercased; with part of the object ID
   if taken; `user-<oid>` for a name in the reserved `entra:` namespace);
   a user in no bound group, or not enabled, has `Synced` emptied. Each
   changed principal's keys outside its memberships are revoked
   (`reconciler.setSynced`, `Document.RevokeOutside`), all of them for a
   user not enabled or whose account is disabled or deleted, and the
   change is audited by `system:entra-sync`: `principal.create`,
   `membership.sync.add` for memberships added and
   `membership.sync.remove` for those ended, keys revoked or a default
   lost, as separate events. Past the audit outbox's reserve (§1) the
   store refuses the write for its creations and additions; the step then
   writes again with `restrictOnly` (no user created, no membership
   added), so removals and offboarding land in the reserve, and returns
   the `*state.OutboxFull` with what was held back
   (`TestSyncOutboxBackpressure`).
3. **Guard**: `reconcile` plans every user's change before making any.
   **Offboarding** (a user a group reports `Disabled`, or who left and
   whose account is disabled or deleted: `Result.Offboarded`) is never
   held back. Over `MaxRemovals` (50) memberships of everyone else
   ending (a reorg, a group re-bound, Graph answering empty), the step
   writes only the offboarding and returns `ErrTooManyRemovals`, which
   the controller counts as a guard trip; with no offboarding, nothing is
   written. A step changing nothing writes nothing either. Before this,
   the guard counted every removal and refused the step whole, so a mass
   offboarding (60 users disabled at once) revoked nothing until an
   operator raised the limit. `TestSyncOffboarding`.

Memberships granted in golem (`Scopes`) are never the sync's: a member by
both ways keeps one when the other ends, and `members remove` refuses a
membership that is only a group's.

## 6. The binary

`main` (`cmd/golem-control/main.go:58`) dispatches the subcommands
(`dispatch`, `main.go:66`); anything else is golem-control itself, `run`
(`main.go:281`): `parseFlags` (`main.go:239`), the bucket and signer
opened, the state loaded, `newController` (`main.go:356`) wiring an
elector and a publisher per pool and the jobs the flags ask for, then the
state's refresh loop, the API (if `-listen`) and the controller until
SIGTERM or a component fails. With `-telemetry`, `openMetrics` registers
the controller's instruments (§4.2) on a meter provider over `lib/otlp`
(`otlp.MeterProvider`), its resource naming the replica
(`service.instance.id`). Shutdown is in order: the publishing loop stops
while the API drains, then the lease is released, the audit's log records
are flushed, and the metrics' last collection is exported. The API is
served by `serveAPI`, which returns only once `Shutdown` has: the calls in
flight have finished, or `-drain` (10 s) has passed and they are cut,
logged with the fix. `ListenAndServe` returns `ErrServerClosed` the
moment `Shutdown` begins, so before this the lease was released and `run`
returned while calls were still in flight: a `MintKey` whose write had
landed was cut before the key reached its caller. `TestAPIDrains`.

| Flag | Meaning |
| --- | --- |
| `-config` | the config CRDs' directory (required) |
| `-bucket` | the state and snapshot bucket: a directory, or `s3://<bucket>/<prefix>` with the default AWS credential chain (required) |
| `-signing-key` | `file:<Ed25519 PKCS#8 PEM>` (key ID: the file's name) or `kms:<ARN>` (ECC_NIST_P256, checked at start) (required) |
| `-trusted-keys` | a directory of retiring publishing keys' public halves (`*.pub`, read by `sign.LoadKeys` as a gateway's `-keys`): their checkpoints and pointers are trusted, and re-signed with `-signing-key` at the next publication (a key rotation, operations.md §2.1) |
| `-pools` | the pools to publish, comma-separated (default `internal`) |
| `-id` | this replica's name (default the host name): the lease holder |
| `-lease` | the snapshots' authorization lease (default 4h) |
| `-listen` | serve the control API here; needs `-entra-tenant` |
| `-drain` | at shutdown, how long the API's calls in flight may take before they are cut and the lease given up (10s) |
| `-entra-tenant`, `-entra-audience` | the Entra tenant, and the control API registration's audiences (default `api://golem-control`, ADR 0046) |
| `-audit-archive` | the audit archive bucket; empty: the outbox is not shipped and fills to its bound |
| `-audit-outbox-max` | the audit outbox's bound, in events (default 10,000, at least 10): past 80% grants are refused, at it every audited change (§1); also a flag of `restore` and `bootstrap` |
| `-telemetry` | `off`, or an OTLP/HTTP collector: golem-control's metrics (§4.2) and, with `-audit-archive`, the audit's log records |
| `-entra-sync-client-id` | the jobs registration (entra.md #4): run the group sync by workload identity; needs `-entra-tenant` |
| `-entra-sync-every`, `-entra-sync-max-removals` | the sync's interval (5m) and its removal guard (50) |
| `-clickhouse`, `-clickhouse-user` | the ledger's ClickHouse for known spend (password in `$CLICKHOUSE_PASSWORD`); empty: snapshots carry none |
| `-spend-allowance` | how far back known spend's boundary is (5m) |
| `-period-zone` | the budget periods' time zone (default UTC; the gateways' `-period-zone`) |

The subcommands:

- `golem-control keygen <dir>` (`keygen`, `main.go:602`) writes a
  development key pair (`publishing.key`, never overwritten, and
  `publishing.pub` for gateways). `TestKeygen`.
- `golem-control pubkey <signing key>` (`pubkey`, `main.go:531`) prints the
  key's public half for gateways' `-keys` directories: a PEM with a
  `Key-ID` header (a KMS key's ARN, a file key's name), which
  `sign.LoadKeys` trusts under that ID. `TestPubkey`.
- `golem-control restore -bucket <...> -from <document.json> -actor <who>
  [-apply]` (`restore`, `main.go:170`) runs `Store.Restore` (§1.2) with a
  backup document taken from the bucket's history or its Object Lock
  replica. Without `-apply` it is a dry run: it prints what would be
  withheld and rolled back and writes nothing. `TestRestoreCommand`.
- `golem-control bootstrap -bucket <...> -keys <dir> -pools <a,b> -actor
  <who> [-adopt]` (`bootstrap`, `main.go:111`) runs `Store.Bootstrap`
  (§1) with each pool's floor (`Publisher.Floor`: the checkpoint and
  pointer verified with `-keys`, the publishing keys' public halves as
  gateways hold them): it creates a deleted document, empty of
  entitlements, or with `-adopt` accepts an older one put back. Stop every
  replica first: one still running refuses the new document as having
  gone back. `TestBootstrapCommand`.

## 7. Worked example: a key revoked through the API reaches a gateway

1. **The API call.** `keys.RevokeKey` (`control/api/keys.go:30`) calls
   `Service.RevokeKey` (`control/service/service.go:435`), which checks the
   caller is the key's owner, an admin, or, for a service's key, a manager
   of its scope, and calls `Service.apply` (`service.go:99`) with a change
   that calls `Document.Revoke` (`control/state/entitlements.go:217`): the
   key's `RevokedAt` is set and a `Revocation` appended. `apply` adds a
   `key.revoke` audit event.
2. **The state update.** `Store.Update` (`state.go:288`) copies the
   document, applies the change, numbers the audit event (`state.go:97`),
   checks the invariants (`checkRevocations` among them), and writes
   `stateVersion + 1` with `If-Match` (`state.go:311`). On a lost race it
   re-reads and reapplies; on a lost reply it finds its own write ID.
3. **The controller's next step**, within a second on the leader (it may
   be another replica: its `Refresh` at `controller.go:302` reads the new
   document). `entitlementsSum` (`controller.go:710`) now differs, so
   `due` (`controller.go:463`) says a publication is due.
4. **The publication.** `Controller.publish` calls `PublishAll`
   (`controller.go:473`) under the elector's fence. Each pool is numbered
   from its record, checked against its checkpoint and pointer (`publish.go:199`-`206`), its
   snapshot built by `compile.Compile` with `Controller.entitlements`, in
   which `Document.Entitlements` (`entitlements.go:247`) lists the key with
   `Revoked: true` (`entitlements.go:289`). Every pool's record is won in
   one `FencedUpdate` (`publish.go:154`), then each checkpoint and signed
   pointer is written (`publish.go:265`, `:275`).
5. **The gateway's poll** (`gateway/snapshot/bucket.go:242`, `Bucket.Poll`),
   every second: a conditional get of the pointer, now changed. `read`
   (`bucket.go:284`) verifies the signature, the pool and the floor
   (`openPointer`), fetches the snapshot, checks its hash and sequence
   (`openSnapshot`), and `Holder.Apply` (`gateway/snapshot/holder.go:113`)
   swaps it in. The gateway refuses the key by name from then on.

End to end: the write plus up to a second for the controller and a second
for the poll. `control/controller`'s `TestEntitlements` runs exactly this
path with a real gateway source over one bucket.

## 8. How it is tested

- `control/state`: the store's update, retry under 8 concurrent writers,
  a document gone back refused (`TestRegressed`), a deleted one not
  started over unless bootstrapped (`TestMissingDocument`), every
  invariant above (`TestInvariants`, `TestFenceInvariant`,
  `TestEntitlementInvariants`, `TestRolesAndAudit`), the restore
  (`TestRestore`, `TestRestoreNeverWidens`), and the outbox's bound
  (`TestOutboxBound`, `TestOutboxDefault`, `TestWidens`,
  `TestUnclassifiedAction`, `TestRestoreLandsPastReserve`,
  `TestEveryActionClassified`).
- `control/lead`: the tests in §2.
- `control/publish`: the tests in §3.
- `control/entrasync`: the sync (`TestSync`, `TestSyncNames`), its safety
  (`TestSyncSafety`), offboarding past the guard and the guard over
  active users (`TestSyncOffboarding`), Graph and the token (`TestGraph`,
  `TestFederatedToken`), a sync deposed mid-read writing nothing
  (`TestSyncFenced`), and a sync past the outbox's reserve writing its
  removals alone (`TestSyncOutboxBackpressure`).
- `control/controller`: two replicas and a gateway over one bucket
  (`TestPublishes`: a change arrives, a renewal extends the lease, a
  broken edit renews the last good config, a paused leader is fenced), the
  revocation path (`TestEntitlements`), the group sync reaching the
  gateway (`TestGroupSync`), known spend reaching it (`TestKnownSpend`),
  a scope with no ScopePolicy failing closed alone while every pool is
  published (`TestUnmatchedScopes`), an unpriced budget refused as a
  config (`TestUnpricedPolicy`), a key's models checked against the
  config (`TestScopeModels`),
  and a leader started over a broken directory renewing the published
  config (`TestRestartOverBrokenConfig`; `lib/compile`'s `TestRenew`:
  a renewal is what compiling the config again would publish).
- `control/audit`: archive, emit, trim; a retry after a crash; a mismatch;
  batches; a deposed shipper's trim refused (`TestDeposedTrim`); grants
  refused while the archive is unreachable and reopened by a step that
  ships (`TestShippingReopensGrants`).
- `control/metrics`: through the OTel SDK's manual reader, a leader's
  publication (time, sequence, lease, leadership, known spend's age),
  then a tampered checkpoint counted as `tampered` with the last
  publication standing, a refused edit as `config_renewing`, and the
  leadership's end (`TestMetrics`); the outbox's length and bound
  (`TestOutbox`).
  `control/controller`'s `TestStatus` (the jobs' last successes, the
  guard tripping) and `TestFailureReason`.
- `cmd/golem-control`: `TestKeygen`, `TestOpenSigner`, `TestAPINeedsTenant`, `TestAPIDrains`,
  `TestPubkey`, `TestRestoreCommand`, `TestTrustedKeys`,
  `TestBootstrapCommand`.
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
  bucket directly, and `ErrRegressed`'s message names it; `golemctl state
  restore` over the admin API is next (control-plane.md §3).
- **A rolled-back document is caught by the publisher, not the store**: a
  replica started after an older version was put back adopts it (it has
  nothing to compare with) and serves it to the API until a person acts;
  the publisher refuses to publish from it (`ErrRegressed`). A rollback
  that also puts back every pool's checkpoint and pointer leaves nothing
  newer to compare with: the bucket policy is what prevents that
  (model/leadership.md §6).
- **A missing document is recognised by name**: the bucket interface
  cannot list, so `Installed` is the lease object and the configured
  pools' checkpoints and pointers. A deletion of the document together
  with all of those (or a deployment whose `-pools` names none that were
  published) is taken for a first start.
- **A mismatch's status is a log line and a count**: a scope with no
  ScopePolicy, or a policy of no scope, is reported in the step's error
  (logged at error level once until it changes), by `Controller.Unmatched`
  and as `golem.control.scope_policy.unmatched`; no CRD status names the
  ScopePolicy yet (the CRD watch, with its status, comes later), and
  `golemctl config validate` cannot see it (it has no state).
- **A renewal knows only the policies it published**: while no config has
  compiled since a replica started, a scope created meanwhile is renewed
  with no models even if the directory has its ScopePolicy.
- **Metrics need `-telemetry`**: without a collector the loop's record is
  kept but not exported. The failure counter counts attempts, not
  incidents: a pool that keeps failing counts at each retry of its
  backoff (every two seconds at first, then once a renewal interval), so
  alerts read its increase over a window longer than a renewal interval,
  not its value.
- **A departure costs a Graph call**: the sync looks up every user who
  left every bound group (two calls for a deleted one), so a directory
  answering empty costs a call per synced user each step until it
  recovers; Graph's throttling then fails the step, which writes nothing.
  A user with only memberships granted in golem is not looked up: their
  keys are not revoked when Entra deletes them.
- **The audit outbox's bound is a count, at 100% even revocations wait**:
  `-audit-outbox-max` counts events, not bytes (an event is a few hundred
  bytes; a large detail map is not weighed). At the bound a revocation is
  refused like any other audited change, rather than written without its
  event: the emergency denylist (operations.md §2) is the path then, and
  the page at 80% is meant to come first. The class table is by action
  with the effective diff as a check: a new action is refused until
  classed (`TestEveryActionClassified`), and the diff's notion of access
  must grow with the document (a new kind of right in `Document` needs a
  case in `widens`). A policy widening or capture enablement is config
  (ADR 0050), not a state write, and is not held back by it.
- **A replica that will not start reports nothing**: `ErrMissing` at
  `Load`, or a bad flag, stops golem-control before its metrics exist;
  the alert is their absence.
