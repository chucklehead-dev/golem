# golem: the control plane

Status: draft, ninth pass. golem-control: one Deployment, every replica
serving the API, one leader running jobs and publishing snapshots (ADR
0032), over one state document in object storage (ADR 0028, 0033).
Components and APIs at a glance: architecture.md §1–3.

## 1. Responsibilities

- Hold the source of truth for **entitlements** (org-model.md) and
  **operational overrides** (ADR 0023) in the state document; read
  **declarative config** from the EKS cluster's CRDs (`Backend`,
  `ModelRoute`, `ScopePolicy`, `Hook`, `PriceList`) and write their status.
- Enforce every rule of the org model on writes: who may do what, budget
  arithmetic, approvals, key constraints within the owner's rights.
- Compile, sign and publish each gateway pool's **snapshot** (§4).
- Set every site's **budget ceilings** from the ledger (ADR 0031).
- Answer spend queries from ClickHouse.
- Record every change as an audit event, atomically with it (§3).

## 2. Service layer

`control/service` holds the rules; `control/api` is transport only. Each
service method:

1. authenticates the caller (an Entra token validated by `lib/entra`;
   jobs run as a service principal),
2. loads what it needs and checks authorization against org-model §2,
3. validates the change (a key's models within its scope's policy; a cap's
   member in the scope; an override naming a deployment that exists),
4. applies the change to a copy of the state, appends its audit events to
   the document's outbox, checks the org model's invariants, and writes the
   document with `If-Match`, retrying the whole function when another
   write won,
5. returns the new resource with its etag.

Services: `ConfigService` (effective config, explain, overrides),
`OrgService` (scopes, memberships, services, role bindings), `KeyService`,
`BudgetService` (grants, caps, adjustment requests), `SpendService`,
`AuditService`, `AdminService` (gateways, state history and restore),
`RegistrationService` (model server endpoints), and the internal
`SnapshotBuilder` and `CeilingService`. Jobs call the same services; nothing
writes the document except through them.

`validate_only` on every mutating method lets `golemctl` and the console
show errors with field paths before anything is written. Config CRDs are
validated by their schemas' CEL rules and offline by `golemctl config
validate` (the snapshot compiler as a library), not by a live API call
(ADR 0032).

## 3. Store: the state document

- **The document** (ADR 0028): protobuf, zstd, SSE-KMS with Bucket Keys, in
  a versioned bucket in us-east-2 shared by every site (ADR 0029); a
  schema version and a monotonic `state_version`. About 1–2 MB within the
  design envelope.
- **Writes**: `store.Update(ctx, fn)`: copy the in-memory state, apply
  `fn`, check invariants, increment `state_version`, `PutObject` with
  `If-Match: <etag>`; on 412 re-read and retry. Every write is serialized.
- **Reads**: from memory, refreshed by a conditional GET every 500 ms.
- **Audit outbox** (ADR 0032): a write's audit events go into the document
  with it; the leader ships them to ClickHouse as OTel log records and
  trims them once accepted. Optionally also archived under Object Lock.
- **Schema evolution**: additive protobuf changes need nothing. A breaking
  change takes two releases: release N reads the new schema and keeps
  writing the old; release N+1 writes the new. Mixed replicas during a
  rolling upgrade therefore always read what the others write.
- **History**: bucket versioning in place, replication to a backup
  account under Object Lock (ADR 0033). `golemctl state restore` writes an
  old content as a new version and re-applies revocations and removals
  made since.
- **Write access**: golem-control's role only, by bucket policy; a
  break-glass role exists, audited, MFA, two-person (ADR 0033).
- **Not in the document**: shipped audit (ClickHouse), counters and
  reservations (each site's Valkey), spend (ClickHouse).
- **Interface**: `Get`, `PutIfMatch`, `Put`, `List` over S3, with one
  conformance suite (MinIO in CI and a fake). Other adapters (Azure Blob,
  Nutanix Objects) when a site needs its own store; PostgreSQL (ADR 0027)
  if the state outgrows the envelope.

### 3.1 State outline

`principals` (users and services), `scopes` (kind, parent or owner scope,
Entra group bindings), `memberships`, `role_bindings`, `cost_centers`,
`keys` (hash, prefix, owner, scope, constraints, expiry, revoked_at),
`grants`, `caps`, `adjustment_requests`, `pools` (per scope per period:
totals), `ceilings` (per pool per site), `overrides` (target, change,
expiry, author), `registrations` (model server endpoints, with TTL),
`gateway_pools` (site, audience, selector), `notifications_sent`
(idempotency keys), `revocations` (append-only, for safe restore),
`audit_outbox`. Declarative config is not in the document: it is CRDs.

## 4. Distribution

Through the bucket, signed (ADR 0028, 0033); the gateway never calls the
control plane.

- **Publisher**: the leader. After a write or a CRD change (debounced to
  once a second) it compiles each pool's whole snapshot (ADR 0026), signs
  it with the KMS publishing key, writes `snapshots/<pool>/<hash>.pb`, then
  the signed pointer `snapshots/<pool>/current` = `{publish_seq, issued_at,
  hash}` by CAS on the pointer.
- **Selection**: a pool's snapshot holds the config CRDs matching its
  selector (site, audience) and the entitlements its audience allows:
  internal pools get internal principals and keys; the edge pool gets its
  external consumers' service keys only. Each site's snapshot carries that
  site's budget ceilings.
- **Gateways** poll the pointer every second (conditional GET), verify its
  signature and that `publish_seq` exceeds their floor (the larger of the
  local last-good copy and their own status object), fetch and verify the
  snapshot, compile, swap, and write `status/<pool>/<gateway-id>`.
- **Status**: golem-control reads the status objects for the CRDs'
  `Applied n/m`, `golemctl gateways`, and the per-gateway snapshot age
  alert.
- **Revocation latency**: the write plus the one-second poll, about two
  seconds.
- **Freshness**: the leader re-signs the pointer at least hourly; a
  gateway alarms when its pointer is more than a day old.

## 5. Identity, keys and bootstrap

- **Users** enter the state through Entra group sync (members of bound
  groups) or on their first control-API call; a user unknown to the
  snapshot cannot use the gateway yet (seconds after their first sign-in).
- **Coding agents**: `golemctl token` prints a cached Entra access token for
  agents with a credential helper (ADR 0032); others get a minted personal
  key, 12-hour TTL by default for `golemctl login`, pruned at expiry.
- **Disabled users**: Entra sync (every 5 minutes; change notifications in
  M2) revokes the keys and memberships of disabled or deleted users.
- **Leaked keys**: GitHub secret scanning's `secret_scanning_alert`
  webhooks go to golem-control's receiver (through the edge ingress,
  signature verified), which revokes the key and notifies its owner.
- **Emergency revocation without the control plane or S3**: each site has
  an `emergency-denylist` ConfigMap (key prefixes or hashes, principals)
  mounted into its gateways and read as a file, applied over the snapshot.
  A site admin edits it with `kubectl`; it is cleared once the control
  plane has the revocation.
- **Bootstrap**: a Terraform module creates the bucket (versioning,
  lifecycle, policy, replication to the backup account), the KMS keys
  (encryption, signing), and the IAM roles (control plane, each gateway
  pool, break-glass); `golemctl bootstrap` writes the first empty document
  with `If-None-Match: *`, registers the gateway pools, and creates the
  first scopes; the first admin is whoever holds the `golem.admin` Entra
  app role.

## 6. Jobs

Run by the leader; each an idempotent scan.

| Job | Schedule | Notes |
|---|---|---|
| Entra group sync | every 5 min; change notifications in M2 | Membership diffs as service calls in batches; revokes disabled users' keys |
| Period rollover | at each period boundary in the period time zone | Creates the next period's pools from recurring grants and caps |
| Ceiling rebalance | every 30 s | ADR 0031: per-site spend from ClickHouse, ceilings moved within each pool |
| Audit shipping | every few seconds | Outbox to ClickHouse, trimmed when accepted |
| Pointer refresh | hourly | Re-signs every pool's pointer |
| Key, request and registration expiry; reminders | hourly | Expires keys, adjustment requests, registrations past TTL; notifies 7 days and 1 day ahead |
| Ledger and provider reconciliation | daily | Counters against the ledger (data older than an hour); the ledger against provider usage APIs (ADR 0034) |
| Threshold notifications | with the ceiling rebalance | 50/80/100% at key, member and pool level |
| Capture expiry and retention (M2) | hourly | Switch expiry; deletion of expired traces in Langfuse (ADR 0018) |
| Copilot usage ingestion (M2) | daily | GitHub usage metrics into ClickHouse (ADR 0021) |

### 6.1 Notifications

- **Email** through Microsoft Graph `sendMail` from one shared mailbox (the
  application permission limited to it), or an SMTP relay where Graph mail
  is not allowed: approval requests, decisions, key expiry, cap changes,
  one's own threshold crossings.
- **Teams**: channel messages through a Workflows webhook URL each scope can
  configure. Personal Teams notifications need a Teams app: Later.
- Each notification has an idempotency key recorded in the state, so a
  scan repeated after a leader change does not send twice.

## 7. Spend

`SpendService` queries ClickHouse `golem.spend` and its rollups, filtered by
the caller's rights: a member sees their own spend, a manager their
scope's, a budget manager their cost centers'. Live pool usage is the
ledger's too (seconds behind); golem-control reads no site's Valkey.

## 8. Sites and regions

- **Sites** (ADR 0029, 0030): one control plane on EKS in us-east-2 serves
  every site. AKS and on-prem clusters run gateway pools and a Valkey;
  their gateways read the us-east-2 bucket (Entra-to-AWS federation, IAM
  Roles Anywhere). Their config CRDs live in the EKS cluster, selected by a
  `site` label. Budgets reach them as ceilings (ADR 0031); provider quotas
  are split statically per site (ADR 0032). No cluster reaches another's
  API server.
- **Second region** (M3, ADR 0032): the bucket replicated to us-west-2;
  us-west-2's gateways read the replica; a golem-control Deployment there
  scaled to zero, started by the failover runbook (promote the replica,
  scale up) within the 30-minute RTO. One active control plane at a time.

## 9. Testing

- Service-layer tests on the in-memory store; the blob conformance suite
  (MinIO, a fake); the store's own tests for lost CAS races, lost
  responses (a PUT that succeeded but whose reply was lost), and restore.
- Conflict tests: concurrent grants on one pool, concurrent key minting
  against a user's cap, approval racing withdrawal: each must end in a
  state the org model allows.
- Models (testing.md §3): the state document and publication ("every
  acknowledged write is in a snapshot every gateway applies; no gateway
  applies a lower `publish_seq` or an unsigned object"), and the site
  ceilings ("Σ ceilings ≤ total; a ceiling never below its site's spend").
- API compatibility: `buf breaking` against the last release in CI.
