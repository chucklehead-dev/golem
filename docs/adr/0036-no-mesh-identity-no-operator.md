# 0036: No mesh-identity header; no operator

Status: Accepted; delivery and infrastructure detailed by [0040](0040-argo-cd-and-crossplane.md). Amends [0009](0009-identity-and-authentication.md) and [0003](0003-components-and-layering.md).

## Decision

- **Mesh identity is dropped.** golem does not trust a waypoint-written
  identity header. Services authenticate with Entra workload identity or a
  service key; an Istio `AuthorizationPolicy` still limits which
  namespaces and service accounts can reach golem at all. Revisit only for a
  service that cannot use Entra.
- **No golem-operator.** Installation and day-2 are Helm charts deployed by
  Argo CD (Argo Rollouts for gateway canaries from M2, ADR 0040);
  drains are the gateway's `preStop` and `terminationGracePeriodSeconds`;
  mesh policies and mounted Secrets are chart templates; Langfuse
  provisioning is a golem-control job (ADR 0035). docs/operations.md
  replaces operator.md.

## Consequences

One fewer component to build, review and run, and no identity path whose
correctness depends on mesh configuration.
