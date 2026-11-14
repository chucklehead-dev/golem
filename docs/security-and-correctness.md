# Security and correctness requirements

Status: accepted design revision (ADRs 0029, 0041); implementation and validation are outstanding.
Decisions: [ADR 0029](adr/0029-security-and-correctness-revision.md), amended
by [ADR 0041](adr/0041-security-boundaries-and-incident-controls.md). ADR 0041 governs the
incident-control, capture, compatibility, delivery and evidence revisions below.
These requirements amend the baseline documents where explicitly referenced.
They describe required behavior, not controls already implemented.

## 1. Trust boundaries and assumptions

One enterprise is still several authorization domains: scopes, internal and
edge pools, operators, and administrators. Neither an internal network nor
a valid Entra token grants access to every scope.

Platform administrators, the signing service, Kubernetes administrators and
the GitOps deployment identity are trusted to change policy. Their compromise
can compromise the installation; signatures do not protect against an
authorized signer publishing malicious policy. A gateway compromise exposes
the credentials and traffic available to its pool. Pool IAM, provider
credentials and capture destinations must minimize that exposure.

Security owners must approve before production: the maximum authorization
staleness, the incident revocation procedure, allowed destinations and data
classes, and recovery permissions. Availability choices that preserve stale
rights are explicit exceptions, never an implicit result of an outage.

## 2. Authentication and identity provenance (M1)

- Verify JWT signature, algorithm allowlist, issuer, tenant, audience,
  expiry and not-before; distinguish delegated user tokens from application
  tokens and require the appropriate scope or app role. Map application
  identity only after token validation. Never infer privileges from an
  unvalidated header or token shape.
- JWKS has a bounded cache and background refresh; an unknown key fails
  closed when refresh is unavailable. Do not accept caller-controlled
  issuer, discovery, jku or x5u URLs.
- No identity is taken from a mesh header (ADR 0036): services use Entra
  workload identity or keys; the mesh contributes L4 authorization only.
  Conflicting explicit credentials are rejected;
  source binding is a constraint on the authenticated principal.
- Keys, tokens, cookies and provider credentials are excluded from logs,
  traces, errors and support bundles. Session IDs and end-user tags are
  untrusted attributes, never authorization inputs.

## 3. Authorization freshness, publication and restore (M1)

Signed pointers bind installation ID, pool/audience, source revision,
publication sequence, issued-at, authorization-valid-until, snapshot hash
and signing-key ID. Snapshots bind the same installation and audience.
Verification checks those bindings, supported schema, hash, signature and
time bounds, including bounded clock skew.

Maximum authorization age is four hours for internal pools (3–4 hours
without an exception) and five minutes for the edge pool, which may not
override it (ADR 0032). Longer internal leases require an explicit
per-pool maximum and security approval. Refresh must incorporate
current authoritative state and CRDs; it cannot merely re-sign stale content.
At expiry reject new model requests, even when routing configuration remains
usable. Existing streams follow a documented termination policy; the default
is termination at authorization expiry, with locally applied emergency
revocations terminating matching streams promptly.

The five-second revocation target applies to healthy connected gateways,
not partitions within the lease. Publish lag, lease expiry, clock skew,
denylist apply latency and active-stream termination are separate metrics.
Emergency denylist changes remain effective until durable revocation and
fresh publication are confirmed at that site. The ConfigMap has an incident
lifecycle outside Argo reconciliation; delivery latency and API-server/kubelet
availability are explicit assumptions (ADR 0041 §1). Entra disablement
and secret-scanning revocation require fresh publication; encoded credential
expiry and a locally applied denylist are the independent paths.

Publication must prevent source regression after the authoritative publication
record advances. The record is won in the same write that checks the lease's
holder and epoch (ADR 0045), so a deposed leader is refused whether or not its
successor has published, and the checkpoint and pointer only move forward; a
leader whose own view is stale is bounded by source-read expiry (ADR 0041 §5). Pointer CAS
alone is insufficient. A fencing generation and source revision are checked
against an authoritative publication record; losing leadership or CAS requires
reloading inputs and rebuilding, never just renumbering old content. The
mechanism is a Phase 0 spike and an M1 gate. The source manifest binds the
state revision and complete accepted config set, including deletions, to a
monotonic config revision. Renewal requires freshness evidence for both
inputs; Kubernetes resourceVersion is opaque. See ADR 0041 §5.

Gateway status is diagnostic, not an authoritative rollback checkpoint.
Bootstrap and replacement gateways obtain a trusted publication floor from
the authoritative publisher record; if no trustworthy floor can be obtained,
they fail closed. Local checkpoints are atomically persisted. Pool-scoped
status credentials cannot alter the authoritative floor. Signed freshness
bounds apply even to a correctly signed historical snapshot. Signing-key
rotation includes overlap, explicit retirement and a compromised-key runbook.

Restore writes a new version but cannot silently widen rights. Compare the
candidate with current authoritative security state and preserve subsequent
key revocations, principal/service deletions, membership and role removals,
constraint tightening, capture/storage restrictions and emergency denials.
Re-granting any removed right is a separate, explicitly authorized and audited
change. If history or current state is unavailable, restoration remains
quarantined until an administrator reviews the security diff. Replica
promotion fences the former writer and verifies the security high-water mark;
replication alone does not establish a zero-loss recovery point.

## 4. Request validation and stored provider objects (M1)

Scan the entire bounded JSON document before forwarding; reject malformed
JSON, trailing values and duplicate object members. Extract every
policy-relevant field regardless of order, including model, capabilities,
output limits, storage controls, object references and tools. Amended by
ADR 0041 §3 and §7.1: restricted mode rejects unapproved fields, API
versions and betas and is the default for the edge pool, non-agent
applications and sensitive or residency-bound scopes; compatible mode
forwards unknown features and is the default for coding-agent routes under
one standing platform and security approval. Known policy fields (contracts.md §8) are enforced
in both modes. Logging is bounded and sanitized or keyed-hashed; nightly
detection is not a remediation-time guarantee. Provider behavior and
contractual data controls still require review.

Hooks may rewrite content or add hints, but cannot grant rights. After each
rewrite, recompute capability, residency, storage and size checks before
routing. Mandatory security/redaction hooks fail closed; general hooks may
fail open only when policy explicitly permits it.

M1 supports stateless POST Responses with store:false only. Reject stored
retrieval/deletion, previous_response_id, background execution and references
to provider-managed files or conversations. Return a stable non-retryable
policy error. The restriction applies even when a scope requests storage.

Stored objects are deferred until an object registry binds each identifier
to its creating principal, scope, provider account and deployment. Every
read, delete, continuation and file reference checks explicit sharing rights
and resolves through that binding rather than the current alias. IDs are
not bearer credentials. State the lifecycle, deletion, revocation and
provider-retention behavior before enabling this feature.

## 5. Outbound access, raw routes and hooks (before each feature ships)

Use one outbound destination policy for providers, discovery, registrations
and remote hooks. Validate scheme, hostname, port, path and every resolved
address at connection time. Disable redirects; reject URL userinfo,
ambiguous encodings and unsupported schemes. Deny loopback, link-local,
metadata and unapproved private destinations, including IPv6 forms.
Private model servers need explicit address-range and TLS-identity grants.
DNS changes cannot bypass destination checks; enforce egress policy as a
second boundary.

Select credentials by approved deployment identity, never caller-controlled
Host or URL. Strip inbound authorization, cookies, forwarding identity,
hop-by-hop and golem-only headers; create provider authorization afresh.
Credential pass-through requires a separate explicit channel for the caller's
provider credential and must not forward their golem credential.

Raw routes require exact allowed methods and normalized paths, scope rights
and known metering/storage semantics. No arbitrary provider account-management
or object operations. Unsupported or unmetered operations are refused in
enforced scopes. Remote hooks are content recipients: reviewed identity,
mTLS or equivalent authentication, residency approval, minimum payload,
bounded response/body/time/concurrency, and no arbitrary URL in hook output.
Shadows obey the original scope's data restrictions even when platform-funded.

## 6. Capture, retention and administrative access (M2)

Gateway pods must not hold Langfuse project credentials. Amended by ADR
0035: the enterprise OTel collector holds them and routes captured traces
to the project the snapshot names for the scope; it accepts captured
traces only from authenticated gateway workloads and only for projects allowed
to that pool. A compromised shared gateway can relabel captures among those
projects: the pool is the compromise boundary (ADR 0041 §2). Narrower
isolation requires separate workloads/pools and destinations. Inherited projects
are shared read boundaries requiring explicit reader approval. Capture uses a
dedicated receiver/pipeline without operational-exporter fan-out.

Required redaction occurs before every authorized content export. Capture
off forbids content in operational telemetry, error bodies, flight-recording
exports and support bundles. Bound and scrub metadata, including session,
tag and end-user fields. Retention covers collector queues, failed deliveries,
backups and exports as well as live traces; legal holds and deletion evidence
have named administrators and audit records.

## 7. Configuration, audit and local mode

Kubernetes RBAC restricts config spec writes to GitOps and audited break-glass
identities; CODEOWNERS alone is not enforcement. Controllers receive only
needed read/status rights. Direct changes alert and GitOps reconciles drift.
Anyone who can redirect a Backend or Hook is a content-egress administrator.
Keep control API authorization in services, including list/search/filter
operations and field-mask updates; Grafana links do not confer data isolation.
Console sessions require secure cookies, CSRF protection and no tokens in URLs.

Security audit events have stable IDs and monotonically ordered sequence
numbers. Archive them durably under separate-account Object Lock before
trimming the state outbox; collector acceptance alone is insufficient.
ClickHouse is a query projection. Verify sequence gaps and duplicate
delivery; archive access, retention and break-glass actions are audited.
This protects retained history, not the honesty of a compromised event producer.
Bound outbox growth. Widening mutations fail closed before ordinary audit
capacity is exhausted; reserve bounded audit capacity for restrictive changes.
At emergency-capacity exhaustion use the separately audited local denylist,
not unaudited writes or blocked revocation. See ADR 0041 §1.

Standalone binds only to loopback unless explicitly configured, rejects
unexpected Host/Origin values and browser cross-site requests, and requires
a generated local credential for model operations. Loopback alone is not
authentication against malicious web pages or DNS rebinding. Dev credentials
and development issuers are refused by production.

## 8. Resource exhaustion (M1; edge additions before M2 edge)

Apply source rate and connection limits before authentication and body
buffering. Authenticate headers before reading large bodies where possible.
Enforce body byte/read-time limits, JSON depth, SSE event size, output bytes,
stream duration/idle time, and global plus per-principal concurrency.
Bound hook, shadow, telemetry and capture queues. On overload shed work with
a stable retry policy rather than growing memory. Limit snapshot compressed
and expanded sizes, collection counts and CEL compilation work before swap.
Test slow bodies, endless streams, malformed events and invalid-token floods.
Ingress protection is an additional layer; the gateway enforces its own bounds.

## 9. Accounting, budgets and retries (M1)

Amended by [ADR 0031](adr/0031-soft-budgets.md): budgets are soft and
eventually consistent, with the overshoot bounded and stated, by accepted
risk. The durable journal, money reservations, uncertain-liability recovery
and site allocations are not required. What remains required:

- Known spend (the deduplicated ledger balance published in snapshots,
  plus the site's counter since then) is the only input to budget checks;
  once it reaches a budget, every further call is refused on every site.
- Per-principal concurrency limits are enforced; scopes with the
  declared-worst-case check admit on the declared worst case (no cache discount,
  `max_tokens` × `n`, no server-side tools). Budgets are controls, not
  ceilings: no estimate or declared worst case is claimed to bound real
  spend, and the provider account's limit is the backstop (ADR 0044).
- Numeric aggregate bounds are not established by the single-principal
  accounting model. ADR 0041 §6 (and §7.3 for failed renewal) requires scope/multi-site bounds,
  counter handoff and failed-renewal scenarios before such claims are made.
- Reconciliation is monthly with daily previews and ADR 0038's 3–5%
  tolerance; unexplained larger differences are investigated.

Separate logical request_id, attempt_id and accounting event_id. An attempt
records deployment/account, provider ID, pinned price version, usage, cost and
certainty. Re-check known spend and quota before each fallback;
multiple attempts may be billed. No bytes delivered to a caller does not
prove that upstream execution never happened. Replay only operations whose
retry policy explicitly accepts that ambiguity or has verified provider
idempotency; disable automatic replay of side-effecting/stateful operations.
After the first caller byte, never transparently retry.

The ledger is an immutable event projection. Each attempt row and each
adjustment has a distinct stable event ID. ReplacingMergeTree merges are asynchronous:
deduplicate by event identity before sums and before rollups; ordinary
materialized sums over raw deliveries are not duplicate-safe. Duplicate
delivery, multiple adjustments and pre-merge queries must produce the same
balance. Exact DDL is an M0 deliverable.

## 10. Required evidence and review traceability

Each item is a release gate for its stated milestone, not merely a test to
add later. Record spike results and approval of exceptions in docs/spikes/.

| Review finding | Required evidence |
|---|---|
| Bypass | Direct provider calls from outside golem are blocked or visible (ADR 0039) |
| Stale auth and rollback | Partition, restart, empty disk, forged status, retired signing key and clock-skew tests; expiry refuses traffic |
| Stale publisher | Paused old leader resumes after revocation; publication cannot restore the revoked right |
| Restore widening rights | Random restrictive changes followed by restore cannot widen rights without separate approval |
| Stored response authorization | M1 rejects every stored-object path; later cross-principal/scope/account tests deny access |
| Capture boundaries | No gateway holds Langfuse credentials; pool allowlists and dedicated pipelines prevent outside-pool exports; within-pool relabeling is an explicit compromise assumption (ADR 0041) |
| Scanner and hook bypass | Duplicate/late/nested fields, unknown features and rewrites cannot evade policy |
| SSRF and raw routes | Redirects, rebinding, IPv6, metadata and encoded paths cannot escape approved destinations |
| GitOps bypass and audit | Unauthorized spec writes denied; audit archive survives collector outage and query-store deletion |
| DoS and standalone | Bounded memory under slow/flooded traffic; hostile web origin and Host requests refused locally |
| Budget exposure | Establish aggregate bounds, snapshot/counter handoff and failed slot-renewal behavior before claiming bounded exposure (ADR 0041) |
| Reconciliation | Monthly account/model reconciliation with daily previews, explained adjustments and ADR 0038's tolerance |
| Replay and ledger dedup | Duplicate events and multiple adjustments keep balances stable |

The canonical encoder's prefix property applies to provider cache-relevant
conversation blocks, not complete JSON request bytes (whose closing delimiters
and mutable fields change). Unsupported semantic fields fail translation;
any permitted drops must be explicitly classified as inert.

Roadmap estimates are planning placeholders until staffing, required spikes,
accounting durability and publication fencing are measured. The first
production slice may use one team, site and native backend; broader M1
features cannot waive these gates.

Additional M1 gates from [ADR 0041](adr/0041-security-boundaries-and-incident-controls.md): denylist survival
through Argo sync/prune/restarts; restrictive writes during audit backpressure;
negative Argo/Crossplane authorization tests; composite state/config freshness
and restrictive-right restore scenarios. Capture-pipeline and human-access
checks are M2 gates. Model extensions remain outstanding.
