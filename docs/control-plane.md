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
4. applies the change to a copy of the state, checks the org model's
   invariants, and writes the document with `If-Match`, retrying the
   whole function when another write won (ADR 0028); the audit event is
   emitted after the write succeeds,
5. returns the new resource with its etag.

Services: `ConfigService` (read, validate a candidate CRD, explain,
overrides), `OrgService` (scopes, memberships, services, role bindings),
`KeyService`, `BudgetService` (grants, caps, adjustment requests),
`SpendService`, `AuditService`, `AdminService`, and the internal
`SnapshotBuilder` (CRDs from the watch, the state document when its ETag changes;
`LeaseService` joins when there are two regions). The worker role calls
the same services as a service principal; it never writes the store
directly.

Validation is shared with clients: `validate_only` on every mutating
method, and `ConfigService.Validate` for CRDs, let CI, `golemctl` and the
console show errors (with field paths) before anything is written.

## 3. Store

The control plane's state is **one document in object storage**, updated
by compare-and-swap (ADR 0028).

- **The document**: protobuf, zstd, SSE-KMS, in a versioned bucket; a
  schema version and a monotonic `state_version`. About 1–2 MB within the
  design envelope.
- **Writes**: `store.Update(ctx, fn)`: copy the in-memory state, apply
  `fn`, check invariants, increment `state_version`, `PutObject` with
  `If-Match: <etag>`; on 412 re-read and retry. Every write is serialized.
- **Reads**: from the in-memory state, refreshed by a conditional GET every
  500 ms (and S3 event notifications where configured).
- **Schema evolution**: upgrade functions on load; a replica writes only
  the newest schema it knows; rolling upgrades read n−1.
- **History**: bucket versioning; `golemctl state restore --version …`
  (audited); lifecycle expiry of old versions.
- **Targets**: S3 (EKS, Pod Identity), Azure Blob (AKS, workload
  identity), on-prem Nutanix Objects if it supports conditional PutObject,
  otherwise S3 through IAM Roles Anywhere. One small interface (`Get`,
  `PutIfMatch`, `Put`, `List`) and one conformance suite for every adapter.
- **Not in the document**: audit (to ClickHouse as OTel logs, optionally
  Object Lock archives), registration heartbeats and live counters (Valkey),
  spend (ClickHouse).
- **Fallback**: a PostgreSQL implementation of the same interface (ADR
  0027) if the state outgrows the envelope.

### 3.1 State outline

`principals` (users and services), `scopes` (kind, parent or owner scope,
Entra group bindings), `memberships`, `role_bindings`, `cost_centers`,
`keys` (hash, prefix, owner, scope, constraints, expiry, revoked_at),
`grants`, `caps`, `adjustment_requests`, `pools` (per scope per period:
totals), `overrides` (target, change, expiry, author), `registrations`
(model server endpoints; their heartbeats are in Valkey), `jobs` (the
durable job table, ADR 0020), `gateway_pools`; `leases` when there are two
regions. Declarative config is not in the document (it is CRDs).

## 4. Distribution

Through object storage (ADR 0028); the gateway needs neither the database
nor the control plane to start or to see a change.

- **Publishing**: after a successful write, or a CRD change (debounced to
  once a second), the replica that saw it compiles each gateway pool's
  whole snapshot (ADR 0026) and writes `snapshots/<pool>/<hash>.pb`
  (immutable, content-addressed), then the pointer
  `snapshots/<pool>/current` = `{state_version, crd_generation, hash}`.
- **Ordering without CAS**: gateways ignore a pointer older than the
  snapshot they hold, so a slow replica's stale pointer cannot roll them
  back; the next publication corrects it. Distribution therefore works on
  stores without conditional writes.
- **Polling**: each gateway does a conditional GET on its pointer every
  second; on a change it fetches the snapshot by hash, verifies it,
  compiles, swaps, and stores a last-good copy locally.
- **Selection**: each pool's snapshot holds the config CRDs matching its
  label selector (per region, or the edge pool's consumers only) and every
  pool's holds the entitlements.
- **Status**: each gateway writes `status/<pool>/<gateway-id>` (applied
  hash, compile errors, time); golem-control reads them for the CRDs'
  `Applied n/m` and `golemctl gateways`.
- **Revocation latency**: the write plus at most a one-second poll, about
  two seconds; across regions, plus replication.
- **Access**: gateways may read `snapshots/<pool>/` and write
  `status/<pool>/<own id>` only; golem-control reads and writes all.

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

With the second region (M3): the writer bucket in us-east-2, replicated to
us-west-2 by S3 Cross-Region Replication with Replication Time Control.
us-west-2's golem-control reads the replica and writes to the writer
bucket; its gateways read snapshots from the replica. Losing us-east-2
means promoting the replica to writer (an operator-automated runbook);
writes within the replication lag before the loss may be lost. Model
traffic continues throughout.

## 8. Testing

- Service-layer tests against the in-memory store, and the blob store's
  conformance suite against S3 (or MinIO in CI), Azure Blob (Azurite) and
  a fake; Nutanix Objects in the Phase 0 spike.
- Conflict tests: concurrent grants on one pool, concurrent key minting
  against a user's cap, approval racing withdrawal: each must end in a
  state the org model allows, with retries visible in metrics.
- Writers racing on the document (412 and retry), slow replicas
  publishing stale pointers, and replication lag are checked against a
  small model of "every committed write is in a snapshot every gateway
  applies within the bound, and no gateway ever moves to an older
  snapshot" (a good fit for gotogether's simulation and Quint tooling).
- API compatibility: `buf breaking` against the last release in CI.
