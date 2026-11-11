# 0017: An operator for installation and day-2, writing config through the API

Status: Proposed. Amended by [0023](0023-config-from-crds-operational-overrides.md): installation and day-2 only, from M2; config CRDs are golem-control's.

## Context

Running golem means ordered upgrades with schema migrations, long stream
drains, mesh policies, credential mounting, telemetry plumbing, and
config managed by GitOps. Helm alone renders objects but does not
sequence, observe or report.

## Decision

- golem-operator (controller-runtime) owns installation CRDs
  (`ControlPlane`, `GatewayPool`) and config CRDs (`Backend`,
  `ModelRoute`, `ScopePolicy`, `Hook`, `PriceList`)
  (operator.md §1).
- Config CRDs are written to the control API (ADR 0012) and their status
  reports acceptance and gateway application.
- Day-2 procedures (operator.md §3) are code in the operator: upgrade
  ordering with migrations, drains, canaries, credential mounting, mesh
  policies, ClickHouse DDL and Grafana dashboards.
- The Helm chart installs only the operator and CRDs.

## Consequences

- A failed upgrade stops itself and says why in status.
- GitOps users change CRDs; self-service users change data through the
  API; neither can overwrite the other's objects.

## Alternatives

- Helm only: no sequencing, no status beyond "deployed".
- Operator that also holds config (CRDs as the store): rejected in ADR 0009
  and 0012.
