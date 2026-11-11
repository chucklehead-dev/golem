# golem: the operator

Status: draft, fifth pass. golem-operator installs and operates golem in
Kubernetes (ADR 0017 as amended by 0023). It does not handle config:
config CRDs are read by golem-control directly. M1 installs with Helm; the
operator arrives in M2, when ordered upgrades, canaries and Langfuse
provisioning are worth automating.

## 1. CRDs

Group `golem.dev`, version `v1alpha1`.

**Installation** (the operator's; it creates Kubernetes objects from them):

| Kind | Describes | The operator creates |
|---|---|---|
| `ControlPlane` | golem-control's two roles for one region: image version, replicas, store connection (Aurora cluster endpoint and IAM role, or a Postgres DSN Secret), Entra app registration, ClickHouse and Valkey references, region name | Deployments (api, worker), Services, PDBs, ServiceAccounts (with workload identity annotations), RBAC to read config CRDs and write their status, the migration Job, AuthorizationPolicies |
| `GatewayPool` | A set of gateways (internal, or `edge` for internet-facing consumers with its ingress, WAF and source ranges; ADR 0022): version, replicas, region, the label selector for the config it receives, Valkey reference, drain grace, waypoint on/off, provider Secrets to mount | Deployment, Service, PDB, ServiceAccount, waypoint and its AuthorizationPolicies, egress ServiceEntries for the providers its backends use; registers the pool with the control plane |

**Config** (golem-control's; ADR 0023, 0025): `Backend`, `ModelRoute`,
`ScopePolicy`, `Hook`, `PriceList`. golem-control validates them, writes
`Accepted` (or the validation errors with field paths), `Applied` (`n/m`
gateways in the selecting pools acked the version that contains them) and
`observedGeneration`. A validating admission webhook served by
golem-control's api role rejects invalid objects at `kubectl apply`.

Org data (scopes, keys, budgets) and operational overrides are not CRDs:
they change through the control API (ADR 0009, 0023).

## 2. Reconciliation

- **Installation**: create-or-update of owned objects with server-side
  apply; version changes go through the upgrade procedure (§3). A periodic
  resync repairs drift.
- **Langfuse projects**: reads scopes' capture settings from the control
  API and provisions projects (§3).
- The operator authenticates to the control API with its ServiceAccount
  token exchanged for an Entra workload identity token, and holds the
  `golem.operator` app role (read scopes' capture settings, register
  gateway pools, report Langfuse destinations).

## 3. Day-2 operations

| Operation | How |
|---|---|
| **Upgrade** | Order: migration Job (one DDL per step, ADR 0015) → control and worker (rolling) → gateways (rolling, `maxUnavailable: 0`). Version skew: the distribution API supports n−1 gateways, the control API n−1 clients. A failed migration stops the rollout and sets `ControlPlane` `Degraded`. |
| **Gateway rollout** | New pods become ready only with a snapshot (gateway.md §6). Old pods drain: readiness off, `preStop` waits for in-flight streams up to `drainGrace` (default 10 min for agent streams), then end them with a dialect error event. `terminationGracePeriodSeconds` derived from it. |
| **Canary** | `GatewayPool.spec.canary`: a second Deployment at the new version with a share of the Service's endpoints (or a waypoint weighted route), promoted when error and latency SLOs hold for a period. |
| **Provider credential rotation** | Secrets are mounted into gateway pods (the operator knows which Backends a pool uses); kubelet refreshes the files and the gateway re-reads them (os.Root, poll). No restart, no secret in the control plane's store. Workload identity needs no rotation. |
| **Backend maintenance** | Planned: `Backend.spec.disabled` or a deployment's `weight: 0` in GitOps. Unplanned: an expiring override (`golemctl overrides create --disable deployment/...`). Routes fail over; status shows the traffic left. |
| **Pause** | `GatewayPool.spec.paused`, or a freeze override, stops config delivery to a pool (incident freeze); gateways keep their snapshot. |
| **Scaling** | Fixed replicas: three gateways per pool per region, one per zone, sized for the design envelope (ADR 0026); the dashboards alert when traffic nears the envelope. |
| **Mesh** | Waypoint for the gateway Service; AuthorizationPolicies: only the waypoint may reach the gateway's data port when identity headers are trusted (ADR 0003); only gateways may call the distribution API; admin ports reachable only from the operator and the control plane. |
| **Langfuse projects** | For each scope with capture set to metadata or content (read from the control API): ensure a Langfuse project through the organisation API, set its retention to the scope's maximum, create its API keys, add them to the `golem-langfuse` Secret mounted into gateways and the worker, and report the destination ready (ADR 0018). Archived scopes' projects are deleted after their longest retention. |
| **Telemetry plumbing** | Applies the ClickHouse DDL (`golem.spend`, rollups, audit) as a Job; publishes Grafana dashboards as ConfigMaps for Grafana's sidecar (or Grafana operator resources); emits the collector routing snippet to merge into the enterprise collector config. |
| **Store** | Backups and point-in-time restore belong to the store (Aurora, Azure, or CloudNativePG); the operator reports connectivity and migration state, it does not back up. |
| **Support** | `golemctl support-bundle` collects CRDs, statuses, gateway versions and acks, recent errors and a flight-recorder trace from a chosen gateway. |

## 4. Build

`operator/` uses controller-runtime and the generated control API client;
it imports nothing else of golem (architecture.md §4.2). CRD YAML is
generated from `operator/api/v1alpha1` and shipped in the Helm chart,
which installs only the operator; the operator installs the rest from a
`ControlPlane` and one or more `GatewayPool`s.
