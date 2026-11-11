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
4. runs one serializable store transaction that writes the change, its
   audit event and the `store_version` increment, retried on
   serialization failures (ADR 0027),
5. returns the new resource with its etag.

Services: `ConfigService` (read, validate a candidate CRD, explain,
overrides), `OrgService` (scopes, memberships, services, role bindings),
`KeyService`, `BudgetService` (grants, caps, adjustment requests),
`SpendService`, `AuditService`, `AdminService`, and the internal
`SnapshotBuilder` (CRDs from the watch, store state when `store_version` moves;
`LeaseService` joins when there are two regions). The worker role calls
the same services as a service principal; it never writes the store
directly.

Validation is shared with clients: `validate_only` on every mutating
method, and `ConfigService.Validate` for CRDs, let CI, `golemctl` and the
console show errors (with field paths) before anything is written.

## 3. Store

PostgreSQL everywhere (ADR 0027): Aurora PostgreSQL Serverless v2 on AWS,
Azure Database for PostgreSQL on AKS, CloudNativePG on-prem. Repository
interfaces in `control/store`; one implementation, `control/store/sql`
(pgx).

- **Transactions**: `SERIALIZABLE`, through `store.InTx(ctx, fn)`, which
  retries the whole function on SQLSTATE 40001 with jittered backoff;
  functions are pure apart from the transaction. Invariants (pool
  arithmetic, a key's cap within its owner's) are plain read-check-write.
- **Migrations**: goose, embedded in the binary, transactional DDL, run by
  a migration Job (a Helm hook in M1, the operator from M2) before
  golem-control rolls out (operator.md §3).
- **Ids**: UUIDv7, generated in Go.
- **Audit**: written by the service in the same transaction as the change.
- **Columns over JSONB** for anything queried; JSONB for opaque payloads
  (audit diffs, hook config).
- **Auth**: IAM database authentication on Aurora (a pgx `BeforeConnect`
  hook mints tokens; `MaxConnLifetime` 10 min), Entra on Azure, a
  Secret-held password on-prem.
- **Times** in UTC; periods computed in Go from the configured period time
  zone.

CI runs the store suite against Postgres containers on every change.

### 3.1 Schema outline

`principals` (users and services), `scopes` (kind, owner scope, Entra
group bindings), `memberships`, `role_bindings`, `cost_centers`, `keys`
(hash, prefix, owner, scope, constraints as columns, expiry, revoked_at),
`grants`, `caps`, `adjustment_requests`, `pools` (per scope per period:
totals), `overrides` (target, change,
expiry, author), `registrations` (self-registered model server endpoints),
`jobs` (the durable job table, ADR 0020), `gateways` (registered instances,
last acked version), `audit_events`, `store_version` (one row); `leases` when there are
two regions. Declarative config is not in the store.

## 4. Distribution

The gateway needs a consistent snapshot, quickly, with no database access.

- **Detection** (ADR 0027). Every write transaction increments the single
  `store_version` row. Each golem-control replica polls it every 500 ms,
  and in the writer's region also `LISTEN`s for the `NOTIFY` each write
  sends on commit, so changes there are seen at once. Every 60 seconds a
  replica rebuilds regardless and compares hashes, as a backstop.
- **Whole snapshots** (ADR 0026). When `store_version` moves or a CRD
  changes, debounced to once a second, a replica rebuilds the whole snapshot
  from the store and its CRD cache (a few thousand rows: milliseconds). Its
  version is its content hash; gateways compare hashes.
- **Watch.** A gateway calls `Watch(pool, have_hash)` on any replica of its
  region and receives the current snapshot (compressed, about 0.5 MB) if
  its hash differs, then each new one. Reconnecting to another replica is
  just another Watch.
- **Selection.** Each gateway pool's snapshot includes the config CRDs
  matching its label selector (e.g. per region, or the edge pool's
  consumers only) and every pool gets the entitlements.
- **Revocation latency** is detection plus the stream: well under a
  second in the writer's region, about a second elsewhere (Aurora Global
  Database's replication lag included).
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

With the second region (M3): Aurora Global Database, writer in us-east-2,
a secondary cluster in us-west-2. Each region runs golem-control (both
roles) reading its own cluster and its own CRDs; us-west-2's writes go to
the writer through the global writer endpoint (or write forwarding; the
M3 spike decides). Gateways watch their own region's control plane. A
region losing its control plane keeps serving on its last snapshot and
its leases; losing the writer region means a managed failover of minutes,
during which control-plane writes wait and model traffic continues.

## 8. Testing

- Service-layer tests against an in-memory store (fast) and the SQL
  store (Postgres containers), with the same suite.
- Conflict tests: concurrent grants on one pool, concurrent key minting
  against a user's cap, approval racing withdrawal: each must end in a
  state the org model allows, with retries visible in metrics.
- Change detection with replica failover, missed notifications and
  replication lag is checked against a small model of "every committed
  change is in a snapshot that reaches every watching gateway within the
  bound" (a good fit for gotogether's simulation and Quint tooling).
- API compatibility: `buf breaking` against the last release in CI.
