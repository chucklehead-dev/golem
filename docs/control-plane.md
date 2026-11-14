# golem: the control plane

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Implementation remains outstanding.

Status: draft, ninth pass. golem-control: one Deployment, every replica
serving the API, one leader running jobs and publishing snapshots (ADRs
0003, 0019), over one state document in object storage (ADRs 0006, 0007).
Components and APIs at a glance: architecture.md §1–3.

## 1. Responsibilities

- Hold the source of truth for **entitlements** (org-model.md) and
  **operational overrides** (ADR 0005) in the state document; read
  **declarative config** from the EKS cluster's CRDs (`Backend`,
  `ModelRoute`, `ScopePolicy`, `Hook`, `PriceList`) and write their status.
- Enforce every rule of the org model on writes: who may do what, budget
  arithmetic, approvals, key constraints within the owner's rights.
- Compile, sign and publish each gateway pool's **snapshot** (§4).
- Publish **known spend** per key, member and scope for the period (the
  deduplicated ledger balance) in every snapshot renewal, so every site
  checks budgets against the same numbers (ADR 0031).
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
`SnapshotBuilder` (which also publishes known spend). Jobs call the same services; nothing
writes the document except through them.

**Built (slice 1):** `control/service` with the org and key methods:
`CreateScope` (admins), `AddMember` and `RemoveMember` (the scope's
managers or admins; a removal revokes the member's keys in the scope and
its role there), `BindManager` (admins), `MintKey` (a member its own keys,
a scope's managers its services' keys, admins any; within the scope's
models; the plaintext returned once, the hash stored) and `RevokeKey`
(the key's owner, its scope's managers, admins). Each writes its audit
event with its change in one update; the store numbers events and refuses
an outbox that loses anything but its oldest or skips a number. The
transport is `control/api`: `golem.org.v1.OrgService` and
`golem.keys.v1.KeyService` (proto/golem, generated into gen/), served by
connect-go as gRPC, gRPC-Web and HTTP/JSON, every call authenticated by an
Entra token (`EntraAuth`: the token's user or service is the principal
with that Entra ID; `golem.admin` makes it an admin; one with no principal
yet is `entra:<id>`), errors mapped to Connect codes with the field path.
`golem-control -listen` serves it, and only with `-entra-tenant`. Reads:
`Whoami`, `ListScopes` and `GetScope` (members, managers, admins; not found
for anyone else) and `ListKeys` (owner, the scope's managers, admins).

`validate_only` on every mutating method lets `golemctl` and the console
show errors with field paths before anything is written. Config CRDs are
validated by their schemas' CEL rules and offline by `golemctl config
validate` (the snapshot compiler as a library), not by a live API call
(ADR 0005).

## 3. Store: the state document

- **The document** (ADR 0006): protobuf, zstd, SSE-KMS with Bucket Keys, in
  a versioned bucket in us-east-2 shared by every site (ADR 0008); a
  schema version and a monotonic `state_version`. About 1–2 MB within the
  design envelope.
- **Writes**: `store.Update(ctx, fn)`: copy the in-memory state, apply
  `fn`, check invariants, increment `state_version`, `PutObject` with
  `If-Match: <etag>`; on 412 re-read and retry. Every write is serialized.
- **Reads**: from memory, refreshed by a conditional GET every 500 ms.
- **Audit outbox** (ADR 0006): a write's audit events go into the document
  with it; the leader ships them to ClickHouse as OTel log records and
  trims them only after durable separate-account Object Lock archival.
  Collector acceptance is not the durability boundary. Built:
  `control/audit`'s Shipper, run by the leader (`golem-control
  -audit-archive`, with `-telemetry` for the log records): each event
  archived create-only as `audit/<seq>.json` (a retry after a crash finds
  it archived and goes on; an archived object that differs stops shipping,
  untrimmed), then emitted as a `golem.audit` log record (at least once:
  ClickHouse keeps one row per seq, and fills a gap from the archive),
  then trimmed from the outbox up to the last event shipped.
- **Schema evolution**: additive protobuf changes need nothing. A breaking
  change takes two releases: release N reads the new schema and keeps
  writing the old; release N+1 writes the new. Mixed replicas during a
  rolling upgrade therefore always read what the others write.
- **History**: bucket versioning in place, replication to a backup
  account under Object Lock (ADR 0007). `golemctl state restore` writes an
  old content as a new version after preserving all subsequent restrictive
  security changes; missing history requires quarantined recovery and review.
- **Write access**: golem-control's role only, by bucket policy; a
  break-glass role exists, audited, MFA, two-person (ADR 0007).
- **Not in the document**: shipped audit (ClickHouse), counters and
  concurrency slots (each site's Valkey), spend (ClickHouse).
- **Interface**: `Get`, `PutIfMatch`, `Put`, `List` over S3, with one
  conformance suite (MinIO in CI and a fake). Other adapters (Azure Blob,
  Nutanix Objects) when a site needs its own store; PostgreSQL (ADR 0006)
  if the state outgrows the envelope.

- **Built (slice 1):** `control/state`, the store over `lib/bucket` as
  above (reads from memory, refreshed by a conditional get every 500 ms;
  `Update` on a copy, invariants checked, `state_version` incremented,
  `If-Match`, a re-read and retry on 412), holding each pool's
  publication record (`publications`), which no write may remove or move
  back. JSON, decoded strictly, for now; the rest of §3.1, protobuf with
  zstd, the audit outbox and restore come as they are built.
  Entitlements (slice 2): `principals` (kind, Entra ID, memberships,
  default scope), `scopes` (policy), `keys` (by hash: owner, scope, model
  narrowing, expiry, revocation) and the append-only `revocations`. Every
  write keeps: a key's owner exists and is a member of its scope; a
  principal's scopes exist and its default is one; no two principals share
  an Entra ID; a revocation is never dropped and its key never revived.
  `Document.Entitlements` is the snapshot's view, a scope's models limited
  to the routes that exist. Nothing writes them yet but tests: the control
  API's first endpoints are next.

### 3.1 State outline

`principals` (users and services), `scopes` (kind, parent or owner scope,
Entra group bindings), `memberships`, `role_bindings`, `cost_centers`,
`keys` (hash, prefix, owner, scope, constraints, expiry, revoked_at),
`grants`, `caps`, `adjustment_requests`, `pools` (per scope per period:
totals), `overrides` (target, change,
expiry, author), `registrations` (model server endpoints, with TTL),
`gateway_pools` (site, audience, selector), `notifications_sent`
(idempotency keys), `revocations` (append-only, for safe restore),
`audit_outbox`. Declarative config is not in the document: it is CRDs.

## 4. Distribution

Through the bucket, signed (ADRs 0006, 0007); the gateway never calls the
control plane.

- **Publisher**: the leader. After a write or a CRD change (debounced to
  once a second) it compiles each pool's whole snapshot (ADR 0021), signs
  it with the KMS publishing key, writes `snapshots/<pool>/<hash>.pb`, then
  the signed pointer `snapshots/<pool>/current` = `{publish_seq, issued_at,
  hash}` by CAS on the pointer.
- **Selection**: a pool's snapshot holds the config CRDs matching its
  selector (site, audience) and the entitlements its audience allows:
  internal pools get internal principals and keys; the edge pool gets its
  external consumers' service keys only. Every snapshot carries known
  spend for the period (ADR 0031).
- **Gateways** poll the pointer every second (conditional GET), verify its
  signature and that `publish_seq` is at least their floor and above the snapshot they hold (the trusted publisher checkpoint and atomically persisted local
  checkpoint; gateway status is diagnostic), fetch and verify the
  snapshot, compile, swap, and write `status/<pool>/<gateway-id>`.
- **Built (slice 1, no cloud dependencies):** `lib/bucket` (the
  interface, with S3's conditional semantics, and a directory
  implementation), `lib/sign` (Ed25519; KMS is the next implementation),
  `lib/snapshot`'s signed `Pointer`, `control/publish` (the publisher) and
  `gateway/snapshot.Bucket` (the gateway's source). As built:
  - the publisher numbers from the pool's **publication record** in the
    state document, won by a state update that checks the lease's holder
    and epoch in the same write and fenced by state version, not from the
    pointer, which anyone who can write the pool's objects can replay;
    then it writes the pool's **trusted checkpoint**
    (`control/<pool>/checkpoint`), then the pointer, each only forward
    (conditionally on the object read, never over a later publication's),
    so a publisher that dies between them leaves the record ahead until
    the next publication, and one paused between them cannot set either
    back;
  - the pointer and the checkpoint are signed, each with its own context
    string; the pointer carries an `expires` (ten minutes), past which a
    gateway that sees nothing newer reports it overdue and goes on serving
    to its snapshot's lease end; a published pool's missing checkpoint is
    not floor zero; the snapshot itself is not signed: it is checked
    against the pointer's hash and must carry the pointer's sequence;
  - the gateway persists the last sequence applied to a local checkpoint
    file, and at start raises its floor to the higher of it and the
    trusted checkpoint; with neither (an empty disk and an unreadable
    bucket) it does not start. Gateways do not read the state document,
    which holds key hashes and principals;
  - the checkpoint is also protected by where it is: the bucket policy must let
    only golem-control's role write `state/` and `control/` (and
    `snapshots/`), and a pool's credentials write only its `status/`.
  - golem-control: `control/controller` and `cmd/golem-control`. While it
    leads, it compiles the config directory and publishes every pool when
    the config changes, when it becomes leader, and every minute to renew
    the authorization lease; a config that does not compile is reported
    and the last good one renewed. Every pool gets the same snapshot, its
    entitlements the state document's, republished at the next step when
    they change; pool selectors and the CRD watch come later. Its
    end-to-end test runs two replicas and a gateway source over one
    bucket: a change reaches the gateway, a renewal extends its lease, a
    broken edit renews the last good config, a new leader publishes and a
    paused one is fenced;
  - leadership: `control/lead`, the lease in the state document (ADR
    0045): taken when lapsed at the next epoch, renewed by its holder,
    given up by its holder's own clock before another may take it; the
    publisher publishes at the epoch its elector believes;
  - slice 2: `lib/bucket/s3bucket` (S3, with its conditional requests:
    If-None-Match on GetObject, If-Match and If-None-Match: * on
    PutObject; 304, 404, 412 and 409 mapped to the bucket's errors; reads
    bounded) and `lib/sign/kmssign` (the publishing key in KMS,
    ECC_NIST_P256 for SIGN_VERIFY, signing SHA-256 digests under
    ECDSA_SHA_256; the key's ARN is the pointer's key ID). Gateways verify
    with `sign.ECDSAKeys` and the key's public key (`kmssign.PublicKey`
    fetches it at bootstrap), never calling KMS. One conformance suite
    (`lib/bucket/buckettest`) runs against the directory bucket and, through
    the real S3 client, an S3 stand-in;
  - open: neither binary
    is wired to the bucket (the release gate, golem#7), and nothing tests
    against real S3 and KMS yet (an integration run in the target
    account, with the bucket policy that denies other writers).
- **Status**: golem-control reads the status objects for the CRDs'
  `Applied n/m`, `golemctl gateways`, and the per-gateway snapshot age
  alert.
- **Revocation latency**: the write plus the one-second poll, about two
  seconds.
- **Freshness** (ADRs 0029, 0030, 0032): every snapshot carries a signed
  authorization lease: four hours for internal pools, five minutes for the
  edge pool. The leader renews every pool's authorization every 60 seconds
  from current state and CRDs; a gateway refuses new requests once its
  lease has expired, and alerts warn hours before.

## 5. Identity, keys and bootstrap

- **Users** enter the state through Entra group sync (members of bound
  groups) or on their first control-API call; a user unknown to the
  snapshot cannot use the gateway yet (seconds after their first sign-in).
- **Coding agents**: `golemctl token` prints a cached Entra access token for
  agents with a credential helper (ADR 0009); others get a minted personal
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
- **Bootstrap**: a `GolemInstallation` claim on the platform's Crossplane
  (ADR 0040) composes the bucket (versioning, lifecycle, policy,
  replication to the backup account), the KMS keys (encryption, signing),
  the IAM roles (control plane, each gateway pool, break-glass), Valkey and
  the Entra registrations; `golemctl bootstrap` writes the first empty document
  with `If-None-Match: *`, registers the gateway pools, and creates the
  first scopes; the first admin is whoever holds the `golem.admin` Entra
  app role.

## 6. Jobs

Run by the leader; each an idempotent scan.

| Job | Schedule | Notes |
|---|---|---|
| Entra group sync | every 5 min; change notifications in M2 | Membership diffs as service calls in batches; revokes disabled users' keys |
| Period rollover | at each period boundary in the period time zone | Creates the next period's pools from recurring grants and caps |
| Known spend | with every authorization renewal | Reads per-key, member and scope spend for the period from ClickHouse into the snapshot (ADR 0031) |
| Audit shipping | every few seconds | Archive before outbox trim; ClickHouse is a query projection |
| Authorization renewal | every 60 s | Rebuilds and republishes every pool's snapshot from current state, extending its authorization lease (ADR 0030) |
| Key, request and registration expiry; reminders | hourly | Expires keys, adjustment requests, registrations past TTL; notifies 7 days and 1 day ahead |
| Provider reconciliation | monthly (daily preview) | The ledger against provider usage and cost data per dedicated account and model; differences booked as adjustments, unattributed where needed, within the 3–5% tolerance (ADRs 0031, 0038) |
| Threshold notifications | with known spend | 50/80/100% at key, member and pool level |
| Capture expiry and Langfuse projects (M2) | hourly | Switch expiry; create a Langfuse project when a scope first needs its own and add its credentials to the collector's Secret (ADR 0035) |
| Copilot usage ingestion (M2) | daily | GitHub usage metrics into ClickHouse (ADR 0020) |

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

- **Sites** (ADR 0008): one control plane on EKS in us-east-2 serves
  every site. AKS and on-prem clusters run gateway pools and a Valkey;
  their gateways read the us-east-2 bucket (Entra-to-AWS federation, IAM
  Roles Anywhere). Their config CRDs live in the EKS cluster, selected by a
  `site` label. Budgets reach them as known spend in snapshots (ADR 0031); provider quotas
  are split statically per site (ADR 0012). No cluster reaches another's
  API server.
- **Second region** (M3, ADR 0008): the bucket replicated to us-west-2;
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
  applies a lower `publish_seq` or an unsigned object"), and accounting
  (`model/accounting.md`: no double count, books match after
  reconciliation, overshoot within ADR 0031's bound).
- API compatibility: `buf breaking` against the last release in CI.

## Security protocol amendments (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) requires source freshness for both CAS state and the complete
accepted CRD set before lease renewal, by a consistent LIST of the config
CRDs and a GET of the state document on every renewal (§7.2). It also reserves bounded audit capacity
for restrictive mutations and moves emergency denial outside Argo ownership.
The composite protocol is modelled in [model/freshness.md](../model/freshness.md):
any restriction from either source is honoured within one lease of that
source's change, under stale watch caches, Git rollbacks, leader changes and
restore, given consistent reads, the earliest-read anchor, fencing on both
source versions and restrictive-preserving restore. The guarantee is per
source: a snapshot can pair reads that never coexisted (freshness.md §6.2).
The publication spike and its tests must still show the implementation does
the same. Capture provisioning is limited to named destination Secrets.
