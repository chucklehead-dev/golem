# 0002: One config schema, compiled into an immutable snapshot

Status: Superseded by [0012](0012-config-through-the-control-plane.md) (the snapshot and its atomic swap stand; config now reaches gateways through the control plane, not by gateways watching CRDs)

## Context

This record covers **operator config**: backends, model routes and
aliases, scope policies' model sets, limit policies, hooks. Entitlements
(users, teams, projects, keys, budgets) change by self-service many times
a day and are not config: ADR 0009.

Operators want CRDs and GitOps; developers and tests want a single YAML
file. Config changes (a new alias, a rotated key) must apply without
restarts and without a half-applied state on the request path.

## Decision

- One set of Go types (`api/v1alpha1`) is the schema for both sources: the
  CRDs are generated from them, and the file format is a list of the same
  objects (`kind:` + `spec:`), read with `sigs.k8s.io/yaml` so JSON tags are
  the single source of field names.
- A `Source` (file, or Kubernetes informers) delivers the current set of
  objects. `config.Compile` resolves references (Secrets, aliases to
  backends), precompiles CEL, validates everything (field paths in errors),
  and produces a `*Snapshot`: immutable, with lookup maps built once.
- The proxy holds `atomic.Pointer[Snapshot]`. A request loads it once.
  Invalid input is rejected, reported (log, metric, CRD status condition)
  and the previous snapshot stays.
- Per-deployment runtime state (transports, ejection state, EWMA) lives
  outside the snapshot, keyed by deployment identity, so a config change
  does not reset health or drop connection pools.
- The proxy watches CRDs itself (no separate controller) in M1. If the
  RBAC footprint or fan-out becomes a problem, a controller can compile
  snapshots and serve them (xDS-like) with no change to the data path.

## Consequences

- Config reload is a pointer swap; no locks on the request path.
- The CRD source needs read RBAC on golem's CRDs and the Secrets they
  reference; instances select their objects by label.
- `golem config check file.yaml` and the admin config dump use the same
  compile, so CI can validate GitOps repos.

## Alternatives

- Separate controller rendering a ConfigMap: less RBAC in the data plane,
  slower propagation, one more component. Deferred.
- Gateway API resources with policy attachment (Envoy AI Gateway,
  agentgateway): see ADR 0006.
