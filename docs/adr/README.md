# Architecture decision records

One decision per file, numbered, in the shape: context, decision,
consequences, alternatives. A superseded record stays, marked so, with a
link to its successor. All are **Proposed** until the first implementation
milestone (M0) confirms them.

| # | Decision | Status |
|---|---|---|
| [0001](0001-passthrough-fast-path.md) | Same-dialect requests are forwarded as bytes; translation only across dialects | Proposed |
| [0002](0002-config-snapshot.md) | One config schema, file and CRD sources, compiled into an immutable snapshot | Proposed |
| [0003](0003-authn-chain-principal.md) | Authenticators in a chain yield one Principal; credential pass-through is per route | Proposed |
| [0004](0004-lifecycle-hooks.md) | Fixed lifecycle phases; hooks in-process or remote, selected by CEL | Proposed |
| [0005](0005-cost-limits-reserve-reconcile.md) | Cost limits reserve an estimate and reconcile with usage; local first, Redis shared | Proposed |
| [0006](0006-not-a-gateway-api-implementation.md) | golem runs behind the enterprise's ingress, not as a Gateway API implementation | Proposed |
| [0007](0007-no-replay-after-first-byte.md) | Retries and failover stop at the first response byte | Proposed |
