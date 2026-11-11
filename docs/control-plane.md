# golem: the control plane

Status: draft, fifth pass. golem-control, one binary in two roles: `api`
(the APIs and the CRD watch) and `worker` (jobs), over one store (ADR
0025). Components and APIs at a glance: architecture.md §1–3.

## 1. Responsibilities

- Hold the source of truth for **entitlements** (org-model.md) and
  **operational overrides** (ADR 0023); read **declarative config** from
  its cluster's CRDs (`Backend`, `ModelRoute`, `ScopePolicy`, `Hook`,
  `PriceList`), validate it, and write its status.
- Enforce every rule of the org model on writes: who may do what, budget
  arithmetic, approvals, key constraints within the owner's rights.
- Compile both into **snapshots** and distribute them to gateways.
- Grant and rebalance **regional budget leases** (ADR 0016).
- Answer spend queries from ClickHouse.
- Record every change as an audit event in the same transaction.

## 2. Service layer

`control/service` holds the rules; `control/api` is transport only. Each
service method:

1. authenticates the caller (Entra token validated by `lib/entra`; the
   operator and worker as service principals),
2. loads what it needs and checks authorization against org-model §2,
3. validates the change (cross-resource: a key's models are within its
   scope's policy; a cap's member is in the scope; an override names a
   deployment that exists),
4. runs one store transaction that writes the change, its audit event and
   its change-log row, retried on optimistic-concurrency conflicts,
5. returns the new resource with its etag.

Services: `ConfigService` (read, validate a candidate CRD, explain,
overrides), `OrgService` (scopes, memberships, services, role bindings),
`KeyService`, `BudgetService` (grants, caps, adjustment requests),
`SpendService`, `AuditService`, `AdminService`, and the internal
`SnapshotBuilder` (CRDs from the watch, store state from the change log;
`LeaseService` joins when there are two regions). The worker role calls
the same services as a service principal; it never writes the store
directly.

Validation is shared with clients: `validate_only` on every mutating
method, and `ConfigService.Validate` for CRDs, let CI, `golemctl` and the
console show errors (with field paths) before anything is written.

## 3. Store

Repository interfaces in `control/store`; one implementation,
`control/store/sql`, that runs unchanged on PostgreSQL 16+ and Aurora
DSQL (ADR 0015). The SQL is written to DSQL's subset:

| DSQL constraint | Consequence in golem |
|---|---|
| Optimistic concurrency only; conflicts fail at commit with SQLSTATE 40001 (OC000 data, OC001 schema); `SELECT ... FOR UPDATE` does not block | Every transaction runs in `store.InTx(ctx, fn)`, which retries the whole function on 40001 with jittered backoff; functions are pure apart from the transaction; etags give clients the same model |
| Repeatable read, snapshot at `BEGIN` | Read-then-write invariants (pool arithmetic) are written so that a concurrent conflicting write touches the same row (e.g. a pool row's `version`), forcing a conflict instead of write skew |
| ≤ 3,000 modified rows, ≤ 10 MiB, ≤ 300 s per transaction | Bulk work (Entra sync of a large group, period rollover) is chunked by the worker into idempotent batches of ≤ 1,000 rows |
| No triggers, no PL/pgSQL, no LISTEN/NOTIFY, no temp tables | Audit and change-log rows are written by the service, in the transaction; change detection by polling (§4) |
| No `SERIAL`; identity columns and sequences only with large caches | Ids are UUIDv7, generated in Go; no ordering is derived from ids across writers |
| One DDL statement per transaction, not mixed with DML; `CREATE INDEX ASYNC` | Migrations are one statement each, applied by a migration Job (Helm hook in M1; the operator from M2) before golem-control rolls out (operator.md §3); a DSQL dialect flag turns `CREATE INDEX` into `ASYNC` and waits on `sys.jobs` |
| JSONB not indexable | Anything queried is a column; JSONB only for opaque payloads (audit diffs, hook config) |
| IAM auth tokens; connections ≤ 60 min | A pgx `BeforeConnect` hook mints tokens (AWS SDK); pool `MaxConnLifetime` 50 min. On Postgres, password or Entra/IAM as the platform offers |
| UTC only, C collation | Times stored UTC; display time zones are a client concern; periods computed in Go from the configured period time zone |

CI runs the store suite against Postgres (containers) on every change and
against a DSQL cluster nightly.

### 3.1 Schema outline

`principals` (users and services), `scopes` (kind, owner scope, Entra
group bindings), `memberships`, `role_bindings`, `cost_centers`, `keys`
(hash, prefix, owner, scope, constraints as columns, expiry, revoked_at),
`grants`, `caps`, `adjustment_requests`, `pools` (per scope per period:
totals and a `version` for conflict forcing), `overrides` (target, change,
expiry, author), `registrations` (self-registered model server endpoints),
`jobs` (the durable job table, ADR 0020), `gateways` (registered instances,
last acked version), `change_log`, `audit_events`; `leases` when there are
two regions. Declarative config is not in the store.

## 4. Distribution

The gateway needs a consistent snapshot, quickly, with no database access
and no LISTEN/NOTIFY to lean on.

- **Change log.** Every transaction writes one `change_log` row: id
  (UUIDv7), commit-ish time (`now()` at write), affected object kinds and
  ids. No global sequence (DSQL has none cheap, and none across regions).
- **Detection.** Each golem-control replica polls `change_log` every
  500 ms for rows newer than its watermark minus a lookback window (the
  maximum transaction age, 300 s), deduplicating by id. A transaction that
  commits late with an early timestamp is therefore still seen. Writes
  made by the replica itself are applied immediately, without waiting for
  the poll.
- **Snapshot versions.** A replica holds the current snapshot in memory
  (built at start from a full read, then updated per change by
  re-reading the affected objects). A version is a content hash plus the
  watermark; gateways treat versions as opaque and compare hashes.
- **Watch.** A gateway calls `Watch(gateway_name, region, have_hash)` on any
  replica of its region. It receives a full snapshot if its hash is
  unknown, then deltas (upserts and deletes by object id) as they are
  built. Deltas are idempotent, so a gateway that reconnects to another
  replica and receives a change twice is unaffected.
- **Selection.** A `Gateway` resource names the config a group of gateways
  receives (by label selector on config objects, e.g. per region or per
  security zone). Entitlements go to all.
- **Revocation latency** is the poll interval plus the stream's latency:
  about a second within a region, a few seconds across regions
  (DSQL's cross-region replication included).
- **Acks.** Gateways ack each applied hash with compile errors, if any;
  control records them per gateway and writes them into the CRDs' status
  (`Applied n/m`).

The snapshot format (`lib/snapshot`, protobuf) is the same one a gateway
reads from a file in development.

## 5. Worker jobs

| Job | Schedule | Notes |
|---|---|---|
| Entra group sync | every 5 min; Graph change notifications in M2 | Chunked; membership diffs as service calls, audited as `system:entra-sync` |
| Period rollover | at each period boundary in the period time zone | Creates the next period's pools from recurring grants and caps; idempotent |
| Lease rebalancing | every 30 s per region | ADR 0016 |
| Key expiry and reminders | hourly | Revokes expired keys; notifies owners 7 days and 1 day ahead |
| Approval expiry and reminders | hourly | Expires stale requests; reminds approvers |
| Ledger reconciliation | daily, and on demand | Compares regional counters and lease use with ClickHouse spend per pool; corrects drift; alerts beyond a threshold |
| Threshold notifications | on lease reports | 50/80/100% at key, member and pool level; email or Teams webhook, plus an audit event |
| Content capture expiry | hourly | Turns off time-boxed capture switches (org-model §3.1) |
| Capture retention | hourly | Finds captured traces past their expiry in the ledger and deletes them from their Langfuse projects in batches (ADR 0018); alerts when behind |

### 5.1 Notifications

Both channels, chosen per event and per recipient preference:

- **Email** through Microsoft Graph `sendMail` from one shared mailbox
  (the worker's application permission is limited to it), or an SMTP
  relay where Graph mail is not allowed. Personal events go by email:
  approval requests to budget managers, decisions to requesters, key
  expiry, cap changes, threshold crossings of one's own caps.
- **Teams**: channel messages through a Workflows webhook URL that each
  team or project can configure (threshold crossings, pending approvals,
  grant changes for the scope). Personal Teams notifications (activity
  feed or a bot) need a Teams app and are Later.
- Messages link to the console (or `golemctl` commands) and to the
  relevant Grafana panel. Delivery failures retry with backoff and are
  visible in audit.

Jobs run behind the worker's `workflow` seam (ADR 0020): a durable job
table in M1, Temporal later for approvals, key rotation and batch jobs.
Two more jobs: **endpoint registrations** (expire self-registered model
server endpoints that stop heartbeating; PRD §5.2.1) and **Copilot usage
ingestion** (daily, from GitHub's usage metrics API into ClickHouse; ADR
0021).

Jobs take a lease row in the store (one runner per job per region) so the
worker can run more than one replica.

## 6. Spend

`SpendService` queries ClickHouse `golem.spend` (and the rollup tables
the ClickHouse DDL defines in `deploy/clickhouse`) with filters derived
from the caller's rights: a member sees their own spend, a manager their
scope's, a budget manager their cost centers'. Live pool usage (this
period, to the minute) comes from the leases' reports rather than
ClickHouse, so a manager's view matches what enforcement sees.

## 7. Multi-region

In each region: golem-control (both roles) against the local endpoint of
a multi-region DSQL cluster, reading that region's CRDs; gateways watch
their own region's control plane.
A region losing its control plane keeps serving on its last snapshot and
its current leases; a region losing DSQL's local endpoint is a region
without changes, not without traffic.

## 8. Testing

- Service-layer tests against an in-memory store (fast) and the SQL
  store (Postgres containers), with the same suite; nightly on DSQL.
- Conflict tests: concurrent grants on one pool, concurrent key minting
  against a user's cap, approval racing withdrawal: each must end in a
  state the org model allows, with retries visible in metrics.
- The change-log detection with late commits, replica failover and
  duplicate deltas is checked against a small model of "every committed
  change reaches every watching gateway, in an order consistent with
  per-object versions" (a good fit for gotogether's simulation and Quint
  tooling).
- API compatibility: `buf breaking` against the last release in CI.
