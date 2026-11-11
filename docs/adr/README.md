# Architecture decision records

One decision per file, numbered, in the shape: context, decision,
consequences, alternatives. A superseded record stays, marked so, with a
link to its successor. All are **Proposed** until the first implementation
milestone (M0) confirms them.

| # | Decision | Status |
|---|---|---|
| [0001](0001-passthrough-fast-path.md) | Same-dialect requests are forwarded as bytes; translation only across dialects | Proposed |
| [0002](0002-config-snapshot.md) | Operator config: one schema, file and CRD sources, compiled into an immutable snapshot | Proposed |
| [0003](0003-authn-chain-principal.md) | Authenticators in a chain yield one Principal and scope; Entra, keys, mesh identity via waypoint | Proposed |
| [0004](0004-lifecycle-hooks.md) | Fixed lifecycle phases; hooks in-process or remote, selected by CEL | Proposed |
| [0005](0005-cost-limits-reserve-reconcile.md) | Cost limits reserve an estimate and reconcile; RESP store (Dragonfly, Valkey, Redis), rebuilt from the ledger | Proposed |
| [0006](0006-not-a-gateway-api-implementation.md) | golem runs behind the enterprise's ingress, not as a Gateway API implementation | Proposed |
| [0007](0007-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte | Proposed |
| [0008](0008-org-and-budget-model.md) | Requests bound to one scope; delegated roles; grants between pools; three-level enforcement | Proposed |
| [0009](0009-control-plane-and-entitlements.md) | golem-control owns entitlements in Postgres; the data plane holds a snapshot fed by a change stream | Proposed |
| [0010](0010-telemetry-destinations.md) | OTLP only, routed by the collector to ClickStack, the ClickHouse ledger and Langfuse | Proposed |
| [0011](0011-decision-dialects.md) | OpenAI Decisions and System One as a decision dialect family | Proposed |
