# Archived decision records

**Archived.** These records were consolidated into the v1 baseline in
[../](../README.md). They are kept as the history of how each decision was
reached; where they disagree with the baseline, the baseline holds.

| Archived | Baseline |
|---|---|
| 0001 | [0001](../0001-dialects-canonical-model-and-envelopes.md) |
| 0002 | [0005](../0005-config-crds-and-overrides.md) |
| 0003 | [0009](../0009-identity-and-authentication.md) |
| 0004 | [0014](../0014-lifecycle-hooks.md) |
| 0005 | [0012](../0012-limits-and-accounting.md) |
| 0006 | [0004](../0004-behind-the-mesh.md) |
| 0007 | [0002](../0002-no-replay-after-first-byte.md) |
| 0008 | [0010](../0010-organisation-scopes-and-roles.md) |
| 0009 | [0006](../0006-state-and-distribution-in-object-storage.md) |
| 0010 | [0015](../0015-telemetry-and-ledger.md) |
| 0011 | [0001](../0001-dialects-canonical-model-and-envelopes.md) |
| 0012 | [0005](../0005-config-crds-and-overrides.md) |
| 0013 | [0003](../0003-components-and-layering.md) |
| 0014 | [0022](../0022-apis-protobuf-connect.md) |
| 0015 | [0006](../0006-state-and-distribution-in-object-storage.md) |
| 0016 | [0013](../0013-site-ceilings.md) |
| 0017 | [0003](../0003-components-and-layering.md) |
| 0018 | [0016](../0016-capture-and-retention.md) |
| 0019 | [0017](../0017-self-hosted-model-servers.md) |
| 0020 | [0019](../0019-background-jobs.md) |
| 0021 | [0020](../0020-github-copilot.md) |
| 0022 | [0018](../0018-edge-gateways.md) |
| 0023 | [0005](../0005-config-crds-and-overrides.md) |
| 0024 | [0010](../0010-organisation-scopes-and-roles.md) |
| 0025 | [0003](../0003-components-and-layering.md) |
| 0026 | [0021](../0021-design-envelope.md) |
| 0027 | [0006](../0006-state-and-distribution-in-object-storage.md) |
| 0028 | [0006](../0006-state-and-distribution-in-object-storage.md) |
| 0029 | [0008](../0008-sites-and-regions.md) |
| 0030 | [0008](../0008-sites-and-regions.md) |
| 0031 | [0013](../0013-site-ceilings.md) |
| 0032 | [0003](../0003-components-and-layering.md) |
| 0033 | [0007](../0007-bucket-root-of-trust.md) |
| 0034 | [0012](../0012-limits-and-accounting.md) |
| 0035 | [0001](../0001-dialects-canonical-model-and-envelopes.md) |

---

# Architecture decision records

One decision per file, numbered, in the shape: context, decision,
consequences, alternatives. A superseded record stays, marked so, with a
link to its successor. All are **Proposed** until the first implementation
milestone (M0) confirms them.

| # | Decision | Status |
|---|---|---|
| [0001](0001-passthrough-fast-path.md) | Same-dialect requests are forwarded as bytes; translation only across dialects | Proposed |
| [0002](0002-config-snapshot.md) | Operator config: one schema, file and CRD sources, compiled into an immutable snapshot | Superseded by 0012 |
| [0003](0003-authn-chain-principal.md) | Authenticators in a chain yield one Principal and scope; Entra, keys, mesh identity via waypoint | Proposed |
| [0004](0004-lifecycle-hooks.md) | Fixed lifecycle phases; hooks in-process or remote, selected by CEL | Amended by 0025 |
| [0005](0005-cost-limits-reserve-reconcile.md) | Cost limits reserve an estimate and reconcile; Valkey (0032), rebuilt from the ledger | Proposed |
| [0006](0006-not-a-gateway-api-implementation.md) | golem runs behind the enterprise's ingress, not as a Gateway API implementation | Proposed |
| [0007](0007-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte | Proposed |
| [0008](0008-org-and-budget-model.md) | Requests bound to one scope; delegated roles; grants between pools; three-level enforcement | Amended by 0024 |
| [0009](0009-control-plane-and-entitlements.md) | A control plane owns entitlements; the gateway holds a snapshot and never writes | Amended by 0028 |
| [0010](0010-telemetry-destinations.md) | OTLP only, routed by the collector to ClickStack and the ClickHouse ledger (Langfuse amended by 0018) | Proposed |
| [0011](0011-decision-dialects.md) | OpenAI Decisions and System One as a decision dialect family | Proposed (M2) |
| [0012](0012-config-through-the-control-plane.md) | Config reaches gateways through the control plane; CRDs are written by the operator | Superseded by 0023 (write path) and 0028 (delivery) |
| [0013](0013-components-and-layering.md) | Components with one job each; layering checked by a test | Amended by 0025 |
| [0014](0014-connect-protobuf-apis.md) | APIs in protobuf, served with Connect | Amended by 0025, 0028 |
| [0015](0015-sql-store-postgres-and-dsql.md) | One SQL store for Aurora DSQL and PostgreSQL | Superseded by 0027 |
| [0016](0016-regional-budget-leases.md) | Budgets enforced through regional leases | Superseded by 0031 |
| [0017](0017-operator.md) | An operator for installation and day-2, writing config through the API | Amended by 0023 |
| [0018](0018-capture-and-retention.md) | Content capture per scope with override control; a Langfuse project per scope; retention per record | Amended by 0025 |
| [0019](0019-self-hosted-model-servers.md) | Self-hosted servers: each instance a deployment, discovered or registered, adaptive capacity, prefix affinity, overflow | Proposed |
| [0020](0020-temporal.md) | Durable workflows behind a seam; Temporal for approvals, key rotation and batch jobs when ready | Proposed |
| [0021](0021-github-copilot.md) | Copilot through golem by BYOK, then enterprise custom models; Copilot usage as showback | Proposed |
| [0022](0022-edge-gateways-for-external-consumers.md) | An edge gateway pool for internet-facing consumers (Copilot enterprise models, SaaS apps) | Proposed |
| [0023](0023-config-from-crds-operational-overrides.md) | Config is CRDs read by golem-control; the API adds expiring operational overrides; the operator is install and day-2 only | Proposed |
| [0024](0024-scopes-and-roles.md) | Teams and projects are one Scope entity; four roles; caps unify allocations and key caps | Amended by 0026 |
| [0025](0025-smaller-surfaces.md) | Smaller surfaces: one control binary in two roles, five config kinds, four hook phases, no LROs, leases deferred, explain in the control plane, no sampling at first | Proposed |
| [0026](0026-design-envelope.md) | Designed for the enterprise's size: whole snapshots, fixed replicas, small stores, sub-teams, fair share, capture cost | Amended by 0027, 0028, 0032 |
| [0027](0027-aurora-postgresql.md) | PostgreSQL everywhere: Aurora PostgreSQL on AWS, Global Database for the second region | Superseded by 0028 (kept as the fallback store) |
| [0028](0028-state-and-snapshots-in-object-storage.md) | Control-plane state as one CAS-updated document in object storage; gateways poll their snapshots there | Accepted; amended by 0029, 0032, 0033 |
| [0029](0029-one-s3-store-for-every-site.md) | One S3 store and one control plane for every site in the initial phase; remote gateways read S3 by federation; central counters or site leases | Accepted |
| [0030](0030-no-cross-cluster-kubernetes-access.md) | Object storage is the only cross-site channel; multicluster-runtime not adopted for now; a pull-based site relay if sites keep their own config | Accepted |
| [0031](0031-site-ceilings.md) | Budgets across sites as monotonic site ceilings in snapshots, consumption from the ledger | Accepted |
| [0032](0032-control-plane-simplified.md) | A smaller control plane: one Deployment and a leader, no job table, registration TTLs, one validation path, Valkey only, static quota shares, audit outbox, cold standby, Entra tokens for agents | Accepted |
| [0033](0033-integrity-of-the-bucket.md) | The bucket is the root of trust: signed snapshots, one publisher and sequence, safe restore, locked writes, offsite backup, edge pools see only their consumers | Accepted |
| [0034](0034-accounting-correctness.md) | Accounting correctness: expected-cost reservations, renewal, cut streams, provider reconciliation, Responses state pinning | Accepted |
| [0035](0035-m1-providers-without-translation.md) | M1 serves Claude on Bedrock (envelope), OpenAI and vLLM (pass-through), with no translation | Accepted |
