# 0023: Config is CRDs read by the control plane; the API adds operational overrides

Status: Proposed. Supersedes the write path of 0012 and the config half of 0017.

## Context

ADR 0012 has the operator push config CRDs into the control API, which
stores them in the control plane's database; `golemctl apply` writes there too, and every object
carries an owner so the two writers do not collide. Every deployment target
(EKS, AKS, on-prem) is Kubernetes, so CRDs are always available. The
two-writer design costs an ownership model, a config table, a resync loop,
and an operator on the critical path of every config change, in M1.

## Decision

- **Declarative config is CRDs, only**: `Backend`, `ModelRoute`,
  `ScopePolicy`, `Hook`, `PriceList` (ADR 0025). golem-control watches them
  in its own cluster (a Kubernetes client in the control plane, never in
  the gateway), validates them, writes their status (`Accepted`,
  `Applied n/m`), and compiles them with entitlements from the store into
  the snapshot it distributes.
- **The config API is read and check**: get, list, validate a candidate,
  explain a request, and show what each gateway has applied. It does not
  write declarative config. GitOps or `kubectl` does.
- **Operational overrides** are the API's write side for config: short,
  audited, expiring changes for incidents and maintenance: disable a
  deployment, set a weight, pause a backend, freeze config delivery to a
  pool, force a fallback. Stored in the store, layered over the CRDs in the
  snapshot, listed by `golemctl overrides`, and each with an expiry (default
  24 h) so an incident lever cannot become silent permanent config.
- **The operator does installation and day-2 only** (operator.md):
  `ControlPlane`, `GatewayPool`, upgrades, migrations, drains, canaries,
  credential mounting, mesh policies, Langfuse projects, telemetry
  plumbing. It is not on the path of a config change, and is M2; M1
  installs with Helm.
- **Sites and regions**: one control plane reads the EKS cluster's CRDs for
  every pool, selected by site (ADR 0029, 0030).

## Consequences

- No owner field, no config table, no config resync; one writer per kind of
  state (GitOps for declarations, the API for entitlements and overrides).
- A config change in M1 is: commit, Argo/Flux applies, golem-control sees
  it, gateways ack. The operator can fail without blocking it.
- Non-Kubernetes deployments would need a file source for config;
  none is planned.

## Alternatives

- 0012 as written: one store for everything, at the cost above.
- Config in the store only, via the API: loses GitOps review for routing
  and policy, which operators asked for.
