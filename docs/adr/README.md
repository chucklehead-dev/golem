# Architecture decision records

One record per decision, as it stands. Where a decision was amended
several times, the amendments are folded into one surviving record
("Accepted, consolidated"), and the absorbed records are one-line stubs
pointing at it; their full text, the 35 pre-baseline records that were in
`archive/`, and every earlier version are in git history (`git log --
docs/adr/`). A change to a decision is a new record, or an edit to its
surviving record that says what it absorbed.

**M1 scope**: [m1-plan.md](../m1-plan.md) is the scope of record. Records
marked *M2* below apply from M2 (or later); nothing of them is built for M1.

| # | Decision | |
|---|---|---|
| [0001](0001-dialects-canonical-model-and-envelopes.md) | Same-dialect pass-through; translation through one canonical model; hosting as envelopes (Bedrock built) | |
| [0002](0002-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte | |
| [0003](0003-components-and-layering.md) | Gateway, golem-control, golemctl, console; layering checked by a test | |
| [0004](0004-behind-the-mesh.md) | Behind the enterprise's ingress and mesh, not a Gateway API implementation | |
| [0005](0005-config-crds-and-overrides.md) | Config objects in the cluster and expiring overrides: superseded for M1 by 0054 | M2 |
| [0006](0006-state-and-distribution-in-object-storage.md) | Control-plane state as one CAS-updated document in S3; whole snapshots published there and polled | |
| [0007](0007-bucket-root-of-trust.md) | Signed snapshots, one publish sequence, safe restore, locked writes, offsite backup, emergency denylist | |
| [0008](0008-sites-and-regions.md) | One control plane and one S3 store for every site; cold-standby second region (M3) | |
| [0009](0009-identity-and-authentication.md) | Authenticator chain, Entra, checksummed keys, agents by Entra token | |
| [0010](0010-organisation-scopes-and-roles.md) | Scopes, four roles, rights by intersection | |
| [0011](0011-budgets.md) | Pools funded by grants, caps, three enforcement levels | M2 |
| [0012](0012-limits-and-accounting.md) | Stub: consolidated into 0031 | |
| [0013](0013-site-ceilings.md) | Stub: site allocations, superseded by 0031 | |
| [0014](0014-lifecycle-hooks.md) | Four hook phases, in-process or remote, selected by CEL | M2 |
| [0015](0015-telemetry-and-ledger.md) | OTLP routed by the collector; the ledger and audit in ClickHouse | |
| [0016](0016-capture-and-retention.md) | Content capture per scope, a Langfuse project per scope, per-record retention | M2 |
| [0017](0017-self-hosted-model-servers.md) | Self-hosted servers: each instance a deployment; static ceilings in M1 | |
| [0018](0018-edge-gateways.md) | An edge pool for internet-facing consumers | M2 |
| [0019](0019-background-jobs.md) | Jobs as idempotent scans by golem-control; Temporal later | |
| [0020](0020-github-copilot.md) | Copilot by BYOK, enterprise models through the edge, usage as showback | M2 |
| [0021](0021-design-envelope.md) | The design envelope and what it lets golem leave out | |
| [0022](0022-apis-protobuf-connect.md) | The control API in protobuf, served with Connect | |
| [0023](0023-standalone-gateway.md) | A standalone gateway for laptops from local YAML | |
| [0024](0024-refusals-and-error-origin.md) | Non-retryable refusals are not 429 (budgets are 402); every error has an origin | |
| [0025](0025-ledger-and-price-model.md) | The ledger schema and the price model are contracts | |
| [0026](0026-identifiers-and-headers.md) | Keys carry their environment; request IDs; one session concept; golem's headers | |
| [0027](0027-provider-side-retention.md) | Provider-side storage forbidden unless a scope allows it | |
| [0028](0028-capabilities-and-determinism.md) | Capability-aware routing (betas only in M1); deterministic encoders | |
| [0029](0029-security-and-correctness-revision.md) | Security and correctness review amendments and production gates | |
| [0030](0030-authorization-renewal-and-availability.md) | **Consolidated** (with 0032): a signed authorization lease renewed every minute, four hours internal, five minutes edge; jitter on polls, none on expiry | |
| [0031](0031-soft-budgets.md) | **Consolidated** (with 0012, 0013, 0038, 0042, 0044, 0048, 0052): budgets are soft admission controls on the snapshot's known spend; scope budgets only; provider-account limits the backstop; reconciliation authoritative | |
| [0032](0032-authorization-lease-length.md) | Stub: consolidated into 0030 | |
| [0033](0033-explicit-cross-dialect-conversion.md) | Cross-dialect conversion only where a route asks for it | |
| [0034](0034-budgets-for-m2.md) | M2 budgets are caps and a cost-center mapping | M2 |
| [0035](0035-capture-projects-and-the-collector.md) | A Langfuse project per top-level team; the collector holds the credentials | M2 |
| [0036](0036-no-mesh-identity-no-operator.md) | No mesh-identity header; no operator | |
| [0037](0037-request-policy-and-cache-stability.md) | Forward unknown fields, enforce the policy-relevant ones; semantic cache stability | |
| [0038](0038-accounting-refinements.md) | Stub: consolidated into 0031 | |
| [0039](0039-bypass-controls.md) | Block bypass: SCPs for Bedrock, egress blocks, key-creation limits, managed agent settings | |
| [0040](0040-argo-cd-and-crossplane.md) | Delivery through Argo CD (M1); Argo Rollouts and Crossplane claims from M2 | |
| [0041](0041-security-boundaries-and-incident-controls.md) | Incident denial and audit resilience, delivery privileges, publication freshness (for M1 by 0054), narrowed budget claims; capture boundaries from M2 | |
| [0042](0042-known-spend-by-counter-watermark.md) | Stub: superseded, see 0031 | |
| [0043](0043-ledger-durability.md) | Ledger durability in a collector's persistent queue beside the gateway, not a gateway spool | |
| [0044](0044-budgets-are-controls-not-ceilings.md) | Stub: consolidated into 0031 | |
| [0045](0045-leadership-in-the-state-document.md) | Stub: consolidated into 0053 | |
| [0046](0046-control-api-registration.md) | The control API's own Entra registration; a gateway token cannot call it | |
| [0047](0047-identity-from-entra-groups.md) | Users and memberships from the Entra groups bound to scopes | |
| [0048](0048-known-spend-by-time-boundary.md) | Stub: consolidated into 0031 | |
| [0049](0049-lease-object-and-fence.md) | Stub: consolidated into 0053 | |
| [0050](0050-scope-policy-is-config.md) | Scope policy is config, joined with the state's scopes by ID; budget modes observe, warn, enforce | |
| [0051](0051-gateway-retries-are-failovers.md) | Retries are failovers, of refusals only, to an untried deployment, before any output | |
| [0052](0052-accounting-for-m1.md) | Stub: consolidated into 0031 | |
| [0053](0053-one-golem-control-replica-for-m1.md) | **Consolidated** (with 0045, 0049): one golem-control replica, claiming the state document's fence at start; every leader-only write fenced | |
| [0054](0054-m1-config-from-a-git-checkout.md) | **Consolidated** (with 0005's kinds and validation, 0041 §7.2): config is a git checkout compiled by golem-control, each snapshot naming its commit; freshness by the last fetch | |
