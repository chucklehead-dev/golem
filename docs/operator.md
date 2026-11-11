# golem: the operator

Status: draft, third pass. golem-operator installs, configures and
operates golem in Kubernetes. It is a client of the control API like any
other (architecture.md §3); it holds no config of its own.

## 1. CRDs

Group `golem.dev`, version `v1alpha1`. Two kinds of resource:

**Installation** (the operator creates Kubernetes objects from them):

| Kind | Describes | The operator creates |
|---|---|---|
| `ControlPlane` | golem-control and golem-worker for one region: image version, replicas, store connection (Postgres DSN Secret, or DSQL cluster endpoint and IAM role), Entra app registration, ClickHouse reference, region name | Deployments, Services, PDBs, ServiceAccounts (with workload identity annotations), the migration Job, AuthorizationPolicies |
| `GatewayPool` | A set of gateways: version, replicas or autoscaling, region, the config selector its `Gateway` resource uses, RESP store reference, drain grace, waypoint on/off, provider Secrets to mount | Deployment, Service, HPA (or KEDA ScaledObject), PDB, ServiceAccount, waypoint and its AuthorizationPolicies, egress ServiceEntries for the providers its backends use, and the `Gateway` resource in the control API |

**Config** (the operator writes them through the config API and reports
the result in status; ADR 0012):

| Kind | Config API resource |
|---|---|
| `Backend` | a provider endpoint, its auth (Secret reference, workload identity, Roles Anywhere profile), timeouts, egress path, and its deployments (model, region, quota hints) |
| `ModelRoute` | an alias: pools of deployments with weights, strategy, affinity, fallbacks, shadow pool, conditions |
| `ScopePolicy` | the model sets, content capture ceiling, data class and residency a team or project may use (bound by name or label) |
| `LimitPolicy` | rate, concurrency and default member caps, by scope selector |
| `Hook` | an in-process hook's config or a remote hook's endpoint, phase, condition, timeout, failure mode |
| `PriceOverride` | negotiated prices per model |

Org data (teams, projects, keys, budgets) is not CRDs: it is self-service
through the API (ADR 0009). A `Service` CRD for platform-managed services
declared in GitOps is a Later option.

Status on every config CRD: `Accepted` (the control API accepted it, or
the validation errors with field paths), `Applied` (`n/m` gateways in the
selecting pools acked the version that contains it), `observedGeneration`.

Objects written by the operator are owned by it in the control plane
(`owner: operator`): the API refuses changes to them from other callers,
naming the CRD to edit. Objects created through the API are not touched by
the operator.

## 2. Reconciliation

- **Config**: on CRD change, `validate_only` first; then `Update` with the
  last seen etag; on conflict, re-read and retry. On delete, a finalizer
  removes the object from the control plane first. A periodic resync
  repairs drift (an object deleted out of band).
- **Installation**: standard create-or-update of owned objects with
  server-side apply; version changes go through the upgrade procedure
  (§3).
- The operator authenticates to the control API with its ServiceAccount
  token exchanged for an Entra workload identity token (or, in-mesh, by its
  SPIFFE identity where the waypoint path is in use), and holds the
  `config-writer` role.

## 3. Day-2 operations

| Operation | How |
|---|---|
| **Upgrade** | Order: migration Job (one DDL per step, ADR 0015) → control and worker (rolling) → gateways (rolling, `maxUnavailable: 0`). Version skew: the distribution API supports n−1 gateways, the control API n−1 clients. A failed migration stops the rollout and sets `ControlPlane` `Degraded`. |
| **Gateway rollout** | New pods become ready only with a snapshot (gateway.md §6). Old pods drain: readiness off, `preStop` waits for in-flight streams up to `drainGrace` (default 10 min for agent streams), then end them with a dialect error event. `terminationGracePeriodSeconds` derived from it. |
| **Canary** | `GatewayPool.spec.canary`: a second Deployment at the new version with a share of the Service's endpoints (or a waypoint weighted route), promoted when error and latency SLOs hold for a period. |
| **Provider credential rotation** | Secrets are mounted into gateway pods (the operator knows which Backends a pool uses); kubelet refreshes the files and the gateway re-reads them (os.Root, poll). No restart, no secret in the control plane's store. Workload identity needs no rotation. |
| **Backend maintenance** | `Backend.spec.disabled` or a deployment's `weight: 0`; routes fail over; status shows the traffic left. |
| **Pause** | `GatewayPool.spec.paused` stops config delivery to a pool (incident freeze); gateways keep their snapshot. |
| **Scaling** | HPA on CPU by default; optional KEDA on in-flight streams read from the control plane's admin API. |
| **Mesh** | Waypoint for the gateway Service; AuthorizationPolicies: only the waypoint may reach the gateway's data port when identity headers are trusted (ADR 0003); only gateways may call the distribution API; admin ports reachable only from the operator and the control plane. |
| **Langfuse projects** | For each scope with capture set to metadata or content (read from the control API): ensure a Langfuse project through the organisation API, set its retention to the scope's maximum, create its API keys, add them to the `golem-langfuse` Secret mounted into gateways and the worker, and report the destination ready (ADR 0018). Archived scopes' projects are deleted after their longest retention. |
| **Telemetry plumbing** | Applies the ClickHouse DDL (`golem.spend`, rollups, audit) as a Job; publishes Grafana dashboards as ConfigMaps for Grafana's sidecar (or Grafana operator resources); emits the collector routing snippet to merge into the enterprise collector config. |
| **Store** | Backups and PITR belong to the store (DSQL, or CloudNativePG); the operator reports connectivity and migration state, it does not back up. |
| **Support** | `golemctl support-bundle` collects CRDs, statuses, gateway versions and acks, recent errors and a flight-recorder trace from a chosen gateway. |

## 4. Build

`operator/` uses controller-runtime and the generated control API client;
it imports nothing else of golem (architecture.md §4.2). CRD YAML is
generated from `operator/api/v1alpha1` and shipped in the Helm chart,
which installs only the operator; the operator installs the rest from a
`ControlPlane` and one or more `GatewayPool`s.
