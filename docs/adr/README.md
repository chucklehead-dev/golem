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
| [0005](0005-cost-limits-reserve-reconcile.md) | Cost limits reserve an estimate and reconcile; RESP store (Dragonfly, Valkey, Redis), rebuilt from the ledger | Proposed |
| [0006](0006-not-a-gateway-api-implementation.md) | golem runs behind the enterprise's ingress, not as a Gateway API implementation | Proposed |
| [0007](0007-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte | Proposed |
| [0008](0008-org-and-budget-model.md) | Requests bound to one scope; delegated roles; grants between pools; three-level enforcement | Amended by 0024 |
| [0009](0009-control-plane-and-entitlements.md) | golem-control owns entitlements in a SQL store; the gateway holds a snapshot fed by a stream | Proposed |
| [0010](0010-telemetry-destinations.md) | OTLP only, routed by the collector to ClickStack and the ClickHouse ledger (Langfuse amended by 0018) | Proposed |
| [0011](0011-decision-dialects.md) | OpenAI Decisions and System One as a decision dialect family | Proposed |
| [0012](0012-config-through-the-control-plane.md) | Config reaches gateways through the control plane; CRDs are written by the operator | Superseded by 0023 (write path) and 0028 (delivery) |
| [0013](0013-components-and-layering.md) | Components with one job each; layering checked by a test | Amended by 0025 |
| [0014](0014-connect-protobuf-apis.md) | APIs in protobuf, served with Connect | Amended by 0025 |
| [0015](0015-sql-store-postgres-and-dsql.md) | One SQL store for Aurora DSQL and PostgreSQL | Superseded by 0027 |
| [0016](0016-regional-budget-leases.md) | Budgets enforced through regional leases | Deferred to a second region or remote site (0025, 0029) |
| [0017](0017-operator.md) | An operator for installation and day-2, writing config through the API | Amended by 0023 |
| [0018](0018-capture-and-retention.md) | Content capture per scope with override control; a Langfuse project per scope; retention per record | Amended by 0025 |
| [0019](0019-self-hosted-model-servers.md) | Self-hosted servers: each instance a deployment, discovered or registered, adaptive capacity, prefix affinity, overflow | Proposed |
| [0020](0020-temporal.md) | Durable workflows behind a seam; Temporal for approvals, key rotation and batch jobs when ready | Proposed |
| [0021](0021-github-copilot.md) | Copilot through golem by BYOK, then enterprise custom models; Copilot usage as showback | Proposed |
| [0022](0022-edge-gateways-for-external-consumers.md) | An edge gateway pool for internet-facing consumers (Copilot enterprise models, SaaS apps) | Proposed |
| [0023](0023-config-from-crds-operational-overrides.md) | Config is CRDs read by golem-control; the API adds expiring operational overrides; the operator is install and day-2 only | Proposed |
| [0024](0024-scopes-and-roles.md) | Teams and projects are one Scope entity; four roles; caps unify allocations and key caps | Amended by 0026 |
| [0025](0025-smaller-surfaces.md) | Smaller surfaces: one control binary in two roles, five config kinds, four hook phases, no LROs, leases deferred, explain in the control plane, no sampling at first | Proposed |
| [0026](0026-design-envelope.md) | Designed for the enterprise's size: whole snapshots, fixed replicas, small stores, sub-teams, fair share, capture cost | Amended by 0027 |
| [0027](0027-aurora-postgresql.md) | PostgreSQL everywhere: Aurora PostgreSQL on AWS, Global Database for the second region | Superseded by 0028 (kept as the fallback store) |
| [0028](0028-state-and-snapshots-in-object-storage.md) | Control-plane state as one CAS-updated document in object storage; gateways poll their snapshots there | Accepted; amended by 0029 |
| [0029](0029-one-s3-store-for-every-site.md) | One S3 store and one control plane for every site in the initial phase; remote gateways read S3 by federation; central counters or site leases | Accepted |
