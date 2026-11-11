# 0012: Config reaches gateways through the control plane

Status: Proposed. Supersedes 0002. Write path superseded by [0023](0023-config-from-crds-operational-overrides.md): golem-control reads config CRDs itself; the operator does not push config.

## Context

ADR 0002 had gateways watch CRDs. With a control plane that must exist
anyway (entitlements), that gives gateways two inputs, a Kubernetes client,
RBAC on Secrets, and no way to configure golem without Kubernetes. The
data plane should do data-plane work only.

## Decision

- The control plane's store is the single source of truth for operator
  config as well as entitlements. CRDs are one way to write config:
  golem-operator reconciles them into the config API. `golemctl apply`
  is another, for objects not owned by CRDs. Objects carry an owner, and
  only the owner may change them.
- Gateways have one input: the distribution API's `Watch` stream, which
  carries snapshots of both (whole snapshots since ADR 0026). A file with the same format
  replaces it in development and tests.
- The snapshot, its compile step and its atomic swap stay as ADR 0002
  described.
- Provider secrets do not enter the control plane: Backends reference
  Secrets, the operator mounts them into the gateway pods that need them,
  and gateways read files.

## Consequences

- The gateway has no Kubernetes client, no CRD RBAC, no database driver.
- CRD status reflects what gateways actually applied (acks through the
  control plane), not only what the API accepted.
- Config changes need the control plane up; gateways keep serving their
  last snapshot when it is not.

## Alternatives

- Gateways watch CRDs (0002): fewer hops, but two sources of truth and a
  Kubernetes-only configuration story.
- xDS (Envoy's protocol): standard, but its resource model is Envoy's;
  golem's snapshot is small and its own.
