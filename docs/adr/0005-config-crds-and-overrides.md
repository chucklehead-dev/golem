# 0005: Declarative config is CRDs read by golem-control; incidents use expiring overrides

Status: Accepted (v1 baseline). From archive 0002, 0012, 0017, 0023, 0025, 0032.

## Decision

- **Five kinds**, group `golem.dev`: `Backend` (endpoints, dialect and
  envelope, auth, timeouts, egress, deployments, discovery, static provider
  quota shares per site), `ModelRoute` (alias, pools, strategy, affinity,
  fallbacks, shadow, conditions), `ScopePolicy` (allowed models, capture
  ceiling, residency, default limits), `Hook`, `PriceList`.
- They live in the EKS cluster (ADR 0008), selected per gateway pool by
  labels (site, audience). golem-control watches them, compiles them with
  the state (ADR 0006) into snapshots, and writes their status:
  `Accepted` (or errors with field paths), `Applied n/m`,
  `observedGeneration`.
- **One validation path.** Structural and cross-field rules are CEL
  validation rules in the CRDs' schemas, enforced by the API server even
  with golem-control down. Semantic validation is the snapshot compiler, a
  library: `golemctl config validate dir/` runs it offline in CI, and
  golem-control runs it on the live set. An invalid object keeps its last
  accepted generation in snapshots, and its status says so. No admission
  webhook.
- **Operational overrides** are the API's only write to config: disable a
  deployment, set a weight, freeze publication to a pool, force a fallback.
  Each has an author, a reason and an expiry (default 24 h); they are layered
  over the CRDs in snapshots and listed by `golemctl overrides`.
- Provider secrets stay in Kubernetes Secrets in each site's cluster,
  mounted into the gateways that need them (or workload identity where the
  provider supports it); they never enter the state or the snapshots.

## Consequences

GitOps review for routing and policy; an incident lever that cannot
silently become permanent; CRDs usable before golem-control exists.

## Alternatives

Config in the control plane's store through the API (loses GitOps review);
gateways watching CRDs themselves (a Kubernetes client in the data plane).
