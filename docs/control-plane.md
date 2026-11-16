# golem: the control plane, as built

golem-control for M1: one replica that compiles the config checkout and
the state document into signed snapshots for every gateway pool, serves the
control API, ships the audit outbox and syncs Entra groups. The decisions
are ADRs [0006](adr/0006-state-and-distribution-in-object-storage.md),
[0007](adr/0007-bucket-root-of-trust.md),
[0030](adr/0030-authorization-renewal-and-availability.md),
[0031](adr/0031-soft-budgets.md) (known spend),
[0053](adr/0053-one-golem-control-replica-for-m1.md) (one replica, the
fence) and [0054](adr/0054-m1-config-from-a-git-checkout.md) (config from
git). The code's map is the [orientation](walkthroughs/README.md); its
operation is [operations.md](operations.md). What comes in M2 is listed in
[roadmap.md](roadmap.md#phase-3-m2-enterprise-default-1012-weeks), one
line each.

## 1. Two inputs, one output

| Input | Holds | Written by |
|---|---|---|
| The config: the GitOps repository at a commit, checked out by git-sync beside golem-control (`-config`, `-config-link`) | `Backend`, `ModelRoute`, `ScopePolicy` (a scope's models, budget and mode, limits, ceilings), `PriceList` | pull requests, under CODEOWNERS; CI runs `golemctl config validate` |
| The state document, `state/document.json` in the bucket | entitlements: principals, scopes, keys, revocations, roles; the fence, the publication records, the audit outbox | the control API, the group sync, golem-control's loop |

The output is each pool's snapshot: the compiled config joined with the
state's scopes by ID (a scope with no ScopePolicy can call nothing; a
ScopePolicy of no scope is not published; both reported), the entitlements,
each scope's known spend, the period zone, the config's digest and commit,
and an authorization lease. Every pool gets the same snapshot in M1 (pool
selectors come with the edge pool).

## 2. The state document (`control/state`)

JSON, decoded strictly, one object in a versioned bucket. Its fields
(`state.Document`):

| Field | What |
|---|---|
| `schema`, `stateVersion` | the format's version; a counter every write increments |
| `fence` | `{holder, epoch}`: who leads (§4) |
| `writes` | the IDs of the last 32 writes, so a writer whose reply was lost recognises its own |
| `publications` | per pool: `{epoch, seq, source}`, the last publication's record |
| `principals` | by ID: `kind` (user, service), `entra` (object or app ID), `upn`, `scopes` (granted), `synced` (from bound groups), `default` |
| `scopes` | by ID: `groups`, the Entra groups bound to it. A scope's policy is its ScopePolicy, not here |
| `keys` | by SHA-256: `display`, `owner`, `scope`, `models` (narrowing the scope's), `expiresAt`, `revokedAt` |
| `revocations` | `{key, at}`, append-only, so no restore brings a key back |
| `roles` | `{principal, role, scope}`; `manager` only. Admins hold the Entra app role `golem.admin` |
| `audit`, `auditSeq` | the outbox (§6) and its last number |

**Reads** are from memory, refreshed by a conditional GET every 500 ms and
at each step. **Writes** go through `Store.Update`: the change applied to a
copy, the invariants checked (`check`), `stateVersion` incremented, a PUT
with `If-Match`, and on a lost race a re-read and a retry of the whole
change. A write whose reply was lost is recognised by its ID in `writes`.
The invariants, every write: the fence's epoch never goes back; no
publication record is removed or goes back; a key's owner exists and a
live key's owner is a member of its scope; a principal's scopes exist and
its default is one; no two principals share an Entra ID, and no principal
ID begins `entra:`; roles name existing principals and scopes; revocations
are append-only; the outbox follows (§6). A store that finds the document
gone back refuses it (`ErrRegressed`); one that finds it missing where the
bucket shows golem-control has run (a pool's checkpoint or pointer)
refuses to start (`ErrMissing`, §7).

Not in the document: config (git), spend (ClickHouse), slots and rate
buckets (each site's Valkey), shipped audit (the archive and ClickHouse).

## 3. The publication loop (`control/controller`, `control/publish`)

`Controller.Run` claims the fence, then ticks every second:

1. **Refresh** the state; a fence that is no longer this process's ends it
   (§4).
2. **Load the config**: the checkout through git-sync's link, resolved once
   a step so the files and the commit are one checkout; compiled with the
   state's entitlements (`lib/compile`). A config that does not compile is
   reported (`config not applied`) and the last good one stays published;
   if none has compiled since start (a restart over a broken edit), the
   bucket's publication is renewed with the current entitlements, verified
   as a gateway verifies it. An edit that deletes a live scope's
   ScopePolicy is refused the same way.
3. **Freshness**: the checkout's age is git-sync's last fetch
   (`FETCH_HEAD`'s time, `-config-fetched`, `golem.control.config.age`).
   Past `-max-config-age` (30 minutes) no lease is renewed; a change of
   entitlements or epoch is still published, its lease capped at the
   pool's last, so a revocation still reaches the gateways while their
   leases run out.
4. **Which pools are due** (`Controller.due`): a change to the config, the
   entitlements or the epoch, the renewal interval (60 s), or a failed
   pool's backoff (2 s doubling to the interval, jittered), each pool on
   its own.
5. **Known spend** (§5): the spend job's latest result. The job reads
   ClickHouse on its own goroutine every 30 s (and once before the first
   publication), so a ClickHouse that is down or slow delays none.
6. **Publish** (`PublishAll`): every due pool's record won in one fenced
   update (numbered above the record, the verified checkpoint and the
   signed pointer), then for each pool the snapshot
   (`snapshots/<pool>/<hash>.json`), its trusted checkpoint
   (`control/<pool>/checkpoint`, signed) and its signed pointer
   (`snapshots/<pool>/current`, expiring after ten minutes), each written
   only forward. A record below what the bucket proves stops that pool
   (`ErrRegressed`); a checkpoint that does not verify, or has gone, stops
   it too (`ErrTampered`). Other pools go on.

Beside the loop, each on its own goroutine while this replica leads: the
**audit outbox** shipped (§6) every second, and the **group sync** every
5 minutes (`-entra-sync-every`). A revocation is published at the next
step whatever these are waiting on.

The lease is four hours (`-lease`, at most every gateway's `-max-lease`),
counted from the inputs' read. Gateways poll their pointer every second
and apply a newer publication at least their floor, so a revocation
reaches every connected gateway in about two seconds. Each snapshot names
its commit (`Source.ConfigRevision`), logged at each publication.

Every external call has a deadline, so a bucket, ClickHouse or Graph that
accepts a request and never answers fails that step rather than holding
the loop: the S3 client waits at most 15 s for a response's headers, each
state read or write 15 s, a step's publication a minute, known spend 30 s,
an audit batch a minute and a group sync two minutes. A gateway bounds
each poll of its pointer, and its start, at 30 s.

Signing: `-signing-key kms:<arn>` (ECDSA P-256 in KMS, `lib/sign/kmssign`)
or `file:` (Ed25519, development). `golem-control pubkey` prints the public
half for the gateways' `-keys`; `-trusted-keys` keeps a retiring key
trusted until every checkpoint and pointer has been re-signed.

## 4. One replica and the fence

The chart runs one replica (`Recreate`, no PodDisruptionBudget). At start
golem-control claims the document's fence at one above its epoch
(`Store.Claim`) and leads from then on. Every leader-only write (the
publication records, the outbox trim, the group sync) is a
`Store.FencedUpdate`, which checks the fence in the same compare-and-swap.
A predecessor still running (paused, or on a lost node) is refused at its
next fenced write; it then sees another's fence, stops with `ErrDeposed`
and exits non-zero. The control API's writes are not fenced: they are
ordinary compare-and-swap writes. While the pod restarts, nothing is
published and the API refuses connections; gateways serve on their lease.
`golem.control.leader` is 1 while it leads, `golem.control.leader.epoch`
the epoch (it counts golem-control's starts).

## 5. Known spend (`control/spend`)

With `-clickhouse` (its password in `$CLICKHOUSE_PASSWORD`, read-only on
`golem.spend`): per scope and period (`-period-zone`, stated in every
snapshot and checked by every gateway), the spend in `golem.spend_dedup` of
calls that ended before a boundary B, a whole minute `-spend-allowance`
(5 minutes) before now: one query every 30 s, its values bound as parameters.
A failure keeps the last result for its period and reports it stale
(`golem.control.spend.stale`); a new period starts from nothing. Each
site's ingestion lag (now less its latest `ended_at`, from
`golem.ingest_by_site`) is reported, behind once past the allowance
(`golem.control.spend.ingestion_behind`); B is not moved for it. Without
`-clickhouse` snapshots carry no known spend and no budget refuses.

The ledger reaches ClickHouse through the gateway pod's collector sidecar
(deploy/otel/collector.yaml, its persistent queue; ADR 0043) into
deploy/clickhouse/spend.sql's tables and views, checked end to end by
test/ledger. Reconciliation against CUR and the OpenAI usage API is a query
over `golem.spend_dedup` with a written record (m1-plan.md); no job writes
adjustment rows.

## 6. The audit outbox (`control/audit`)

Every change writes its audit events into the document with it, numbered
by the store. With `-audit-archive` (an Object Lock bucket in a separate
account), `Shipper.Step` archives each event create-only as
`audit/<seq>.json` (a retry after a crash finds it there; an archived event
that differs stops shipping, untrimmed), emits it as a `golem.audit` OTel
log record (`-telemetry`; at least once, ClickHouse keeps one row per
seq), and only then trims the outbox, fenced. The outbox is bounded
(`-audit-outbox-max`, 10,000 events): past 80% the store refuses a write
that grants or widens access (each action classed in
control/state/outbox.go, and the change's effective diff checked);
restrictive writes land up to the bound; at the bound every write that adds
an event is refused, and revocation is the site's denylist. Writes that add
no event (publication, the fence, the trim) are never refused. The API
answers `ResourceExhausted`; `golem.control.audit.outbox` over
`golem.control.audit.outbox_max` is the alert.

## 7. Restore and bootstrap

- **History**: bucket versioning, noncurrent versions of `state/` kept 30
  days, a daily copy to the backup account's Object Lock bucket kept a
  year (operations.md, *State history*).
- **Restore** (`golem-control restore -from old.json -actor <who>`, a dry
  run unless `-apply`): every right becomes the narrower of the backup's and
  the current document's (what only the backup has is withheld and
  reported, what was added since is rolled back, keys minted since are
  revoked); the fence, the records, the revocations and the outbox stay the
  current document's; one audited write that lands past the outbox's
  reserve. Scope policy is config: its history is the repository's.
- **A missing document** is never started over silently: `ErrMissing`
  until the newest version is put back, or `golem-control bootstrap
  -pools … -actor <who>` creates one create-only, each pool's record at what
  the bucket proves was published. A document put back to an older version
  leaves the records below the pools' checkpoints, and the publisher halts
  those pools (`ErrRegressed`) until the newest is put back or
  `bootstrap -adopt` accepts the older one.
- **First install**: on a bucket golem-control has never run on, the first
  write creates the document. The first admin is whoever holds
  `golem.admin`; scopes are then created through the API (operations.md §4).

## 8. Identity and the group sync (`control/entrasync`)

Users enter the state through the Entra groups an admin binds to a scope
(ADR 0047): with `-entra-sync-client-id`, the sync reads every bound
group's transitive members from Microsoft Graph by workload identity,
reads all groups before writing, and makes the synced memberships match in
one fenced write. It revokes keys outside the memberships and all of a
disabled or deleted user's. A step that would end more than
`-entra-sync-max-removals` (50) memberships of still-active users writes
only its offboarding, and counts a guard trip. Only the sync and admins set
a principal's Entra ID.

## 9. The control API and golemctl (`control/api`, `control/service`, `cmd/golemctl`)

`-listen` with `-entra-tenant` serves `golem.org.v1.OrgService` and
`golem.keys.v1.KeyService` (proto/golem) over Connect: gRPC, gRPC-Web and
HTTP/JSON. Every call carries an Entra token for the control API's own
registration (`api://golem-control`, scope `golem.manage`; a gateway token
is refused, ADR 0046), verified before the body is read; messages are at
most 1 MiB; an internal error's text is logged, not returned. Until Entra's
keys have been fetched the API answers `Unavailable`, and publication runs
regardless. `-probes` serves `/livez` and `/readyz` (ready while this replica
leads: the state loaded and the fence claimed, until a successor claims
over it); `-drain` gives calls in flight 10 s at shutdown.

| Method | Who | Effect |
|---|---|---|
| `CreateScope` | admins | a scope by ID; it can call nothing until its ScopePolicy merges |
| `AddMember`, `RemoveMember` | the scope's managers, admins | a removal revokes the member's keys in the scope and its role there; adding an existing service needs an admin or a manager of a scope it is already in |
| `BindGroup`, `UnbindGroup`, `BindManager` | admins | Entra groups bound to a scope; a manager role |
| `MintKey` | a member its own; a manager its services' (managing every scope the service is in); admins any | within the scope's models (from the config); expiry at most 90 days; the plaintext returned once, the hash stored |
| `RevokeKey` | the key's owner, the scope's managers, admins | appended to `revocations` |
| `Whoami`, `ListScopes`, `GetScope`, `ListKeys` | members, managers, admins; others get `not found` | reads |

Each mutating method checks authorization (org-model.md §2), validates,
and applies the change with its audit event in one `Store.Update`;
`validate_only` checks everything and writes nothing. `golemctl` is the
client: `login` (Entra device code), `token` (a gateway token for agents'
credential helpers; it cannot call the control API), `whoami`, `scopes`,
`members`, `groups`, `managers`, `keys create|list|revoke`, each with
`--dry-run`, and offline `config validate` and `keys mint --dev`.

## 10. Operational metrics (`control/metrics`)

Observable instruments, read at collection: the last publication's time,
sequence and lease end per pool (`golem.control.publish.*`), publication
failures by reason, the leader and its epoch, the config's age and whether
the last good config is being renewed, unmatched ScopePolicies, known
spend's age, staleness and ingestion lag, the outbox's length and bound,
and the last audit shipment and group sync. operations.md's alerts are
written against them.
