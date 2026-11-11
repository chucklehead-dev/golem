# Architecture decision records: v1 baseline

The decisions as they stand, one record per decision, without amendment
chains. The 35 records that led here (five design passes and a review) are
in [archive/](archive/), with a mapping from each to its baseline record.
From here on a change to a decision is a new record that supersedes a
baseline one, marked in both.

| # | Decision |
|---|---|
| [0001](0001-dialects-canonical-model-and-envelopes.md) | Same-dialect pass-through; translation through one canonical model (an adapter pair per dialect); hosting as envelopes (Bedrock, Vertex, Foundry); the decision APIs |
| [0002](0002-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte |
| [0003](0003-components-and-layering.md) | Gateway, golem-control (one Deployment, a leader), operator per cluster, golemctl, console; layering checked by a test |
| [0004](0004-behind-the-mesh.md) | Behind the enterprise's ingress and mesh, not a Gateway API implementation |
| [0005](0005-config-crds-and-overrides.md) | Five config CRDs read by golem-control, one validation path, expiring operational overrides |
| [0006](0006-state-and-distribution-in-object-storage.md) | Control-plane state as one CAS-updated document in S3; whole snapshots published there and polled by gateways |
| [0007](0007-bucket-root-of-trust.md) | Signed snapshots, one publish sequence, safe restore, locked writes, offsite backup, audiences, emergency denylist |
| [0008](0008-sites-and-regions.md) | One control plane and one S3 store for every site; no cross-cluster access; cold-standby second region |
| [0009](0009-identity-and-authentication.md) | Authenticator chain, Entra, checksummed keys, agents by Entra token, mesh identity, pass-through |
| [0010](0010-organisation-scopes-and-roles.md) | Scopes (teams with sub-teams, projects), four roles, rights by intersection |
| [0011](0011-budgets.md) | Pools funded by grants, approvals, three enforcement levels, observe/warn/enforce |
| [0012](0012-limits-and-accounting.md) | Valkey at every site; expected-cost reservations, renewal, cut streams, quotas, reconciliation |
| [0013](0013-site-ceilings.md) | Site allocations (superseded by 0031) |
| [0014](0014-lifecycle-hooks.md) | Four hook phases, in-process or remote, selected by CEL |
| [0015](0015-telemetry-and-ledger.md) | OTLP routed by the collector; the ledger and audit in ClickHouse |
| [0016](0016-capture-and-retention.md) | Capture per scope with override control, a Langfuse project per scope, per-record retention (M2) |
| [0017](0017-self-hosted-model-servers.md) | Self-hosted servers: each instance a deployment; static ceilings in M1, adaptive in M2 |
| [0018](0018-edge-gateways.md) | An edge pool for internet-facing consumers (M2) |
| [0019](0019-background-jobs.md) | Jobs as idempotent scans by the leader; Temporal later |
| [0020](0020-github-copilot.md) | Copilot by BYOK, enterprise models through the edge, usage as showback |
| [0021](0021-design-envelope.md) | The design envelope and what it lets golem leave out |
| [0022](0022-apis-protobuf-connect.md) | The control API in protobuf, served with Connect |
| [0023](0023-standalone-gateway.md) | A standalone gateway for laptops from local CRD YAML; whole-system development in `make dev-up`, not an all-in-one binary |
| [0024](0024-refusals-and-error-origin.md) | Non-retryable refusals are not 429 (budgets are 402); every error has an origin |
| [0025](0025-ledger-and-price-model.md) | The ledger schema and the price model are contracts |
| [0026](0026-identifiers-and-headers.md) | Keys carry their environment; request IDs; one session concept; golem's headers |
| [0027](0027-provider-side-retention.md) | Provider-side storage forbidden unless a scope allows it |
| [0028](0028-capabilities-and-determinism.md) | Capability-aware routing; deterministic encoders |

| [0029](0029-security-and-correctness-revision.md) | Proposed security and correctness review amendments and production gates |
| [0030](0030-authorization-renewal-and-availability.md) | Authorization renewed every minute within a five-minute lease; the control plane, bucket and KMS on the availability path; the accounting spool spike |
| [0031](0031-soft-budgets.md) | Budgets are soft and eventually consistent; overshoot bounded by concurrency limits and an optional strict worst-case check; no journal, reservations or site allocations |
| [0032](0032-authorization-lease-length.md) | Internal pools: a four-hour authorization lease, renewed every minute; jitter on polls and retries, none on expiry |
| [0033](0033-explicit-cross-dialect-conversion.md) | Cross-dialect conversion only where a route asks for it; no implicit cross-dialect failover |
| [0034](0034-budgets-for-m2.md) | M2 budgets are caps and a cost-center mapping; grants and approvals wait for demand |
| [0035](0035-capture-projects-and-the-collector.md) | A Langfuse project per top-level team, inheritable; retention per project; the collector holds the credentials |
| [0036](0036-no-mesh-identity-no-operator.md) | No mesh-identity header; no operator (Helm, GitOps, Argo Rollouts) |
| [0037](0037-request-policy-and-cache-stability.md) | Forward unknown fields, enforce the policy-relevant ones; semantic cache stability |
| [0038](0038-accounting-refinements.md) | Dedicated provider accounts, monthly reconciliation within tolerance, renewed concurrency slots, bounded worst case |
| [0039](0039-bypass-controls.md) | Block bypass: SCPs for Bedrock, egress blocks, provider key-creation limits, managed agent settings |
| [0040](0040-argo-cd-and-crossplane.md) | Delivery through Argo CD and Argo Rollouts; cloud resources through the platform's Crossplane (`GolemInstallation`, `GolemSite`) |
