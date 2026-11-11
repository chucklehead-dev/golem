# golem: the operator

Status: draft, ninth pass. golem-operator installs and operates golem in
Kubernetes (ADR 0003). It does not handle config:
config CRDs are read by golem-control directly. M1 installs with Helm; the
operator arrives in M2, when ordered upgrades, canaries and Langfuse
provisioning are worth automating.

## 1. CRDs

Group `golem.dev`, version `v1alpha1`.

**Installation** (the operator's; it creates Kubernetes objects from them):

| Kind | Describes | The operator creates |
|---|---|---|
| `ControlPlane` | golem-control for one installation: image version, replicas, store (bucket, KMS key, and the identity to reach it: Pod Identity, workload identity, Roles Anywhere, or a credentials Secret), Entra app registration, ClickHouse and Valkey references, region name | a Deployment, Services, the leader-election Lease and RBAC, PDBs, ServiceAccounts (with workload identity annotations), RBAC to read config CRDs and write their status, AuthorizationPolicies |
| `GatewayPool` | A set of gateways (internal, or `edge` for internet-facing consumers with its ingress, WAF and source ranges; ADR 0018): version, replicas, region, the label selector for the config it receives, Valkey reference, the bucket and the identity to read its snapshots (and write its status), drain grace, waypoint on/off, provider Secrets to mount | Deployment, Service, PDB, ServiceAccount, waypoint and its AuthorizationPolicies, egress ServiceEntries for the providers its backends use; registers the pool with the control plane |

**Config** (golem-control's; ADR 0005): `Backend`, `ModelRoute`,
`ScopePolicy`, `Hook`, `PriceList`. golem-control validates them, writes
`Accepted` (or the validation errors with field paths), `Applied` (`n/m`
gateways in the selecting pools acked the version that contains them) and
`observedGeneration`. Structural rules are CEL validation rules in the
CRDs' schemas, enforced by the API server at `kubectl apply`; semantic
errors appear in status, and an invalid object keeps its last accepted
generation in snapshots (ADR 0005).

Org data (scopes, keys, budgets) and operational overrides are not CRDs:
they change through the control API (ADRs 0006, 0005).

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
| **Upgrade** | Order: golem-control (rolling; it reads the state document's n−1 schema and upgrades it on its first write) → gateways (rolling, `maxUnavailable: 0`). Version skew: gateways read snapshots from n−1 control planes; the control API serves n−1 clients. A failed rollout stops itself and sets `ControlPlane` `Degraded`. |
| **Gateway rollout** | New pods become ready only with a snapshot (gateway.md §6). Old pods drain: readiness off, `preStop` waits for in-flight streams up to `drainGrace` (default 10 min for agent streams), then end them with a dialect error event. `terminationGracePeriodSeconds` derived from it. |
| **Canary** | `GatewayPool.spec.canary`: a second Deployment at the new version with a share of the Service's endpoints (or a waypoint weighted route), promoted when error and latency SLOs hold for a period. |
| **Provider credential rotation** | Secrets are mounted into gateway pods (the operator knows which Backends a pool uses); kubelet refreshes the files and the gateway re-reads them (os.Root, poll). No restart, no secret in the control plane's store. Workload identity needs no rotation. |
| **Backend maintenance** | Planned: `Backend.spec.disabled` or a deployment's `weight: 0` in GitOps. Unplanned: an expiring override (`golemctl overrides create --disable deployment/...`). Routes fail over; status shows the traffic left. |
| **Pause** | A freeze override (control API) stops publication to a pool (incident freeze); gateways keep their snapshot. |
| **Scaling** | Fixed replicas: three gateways per pool per region, one per zone, sized for the design envelope (ADR 0021); the dashboards alert when traffic nears the envelope. |
| **Mesh** | Waypoint for the gateway Service; AuthorizationPolicies: only the waypoint may reach the gateway's data port when identity headers are trusted (ADR 0009); admin ports only from the operator and golem-control. Bucket policies and IAM roles come from the Terraform module (control-plane.md §5). |
| **Langfuse projects** | For each scope with capture set to metadata or content (read from the control API): ensure a Langfuse project through the organisation API, set its retention to the scope's maximum, create its API keys, add them to the `golem-langfuse` Secret mounted into gateways and the worker, and report the destination ready (ADR 0016). Archived scopes' projects are deleted after their longest retention. |
| **Telemetry plumbing** | Applies the ClickHouse DDL (`golem.spend`, rollups, audit) as a Job; publishes Grafana dashboards as ConfigMaps for Grafana's sidecar (or Grafana operator resources); emits the collector routing snippet to merge into the enterprise collector config. |
| **Store** | Bucket versioning and lifecycle rules (set when the `ControlPlane` is created), replication for the second region and the failover runbook (promote the replica bucket); the operator reports the document's size and CAS retry rate. |
| **Support** | `golemctl support-bundle` collects CRDs, statuses, gateway versions and acks, recent errors and a flight-recorder trace from a chosen gateway. |

## 4. Sites

One operator per cluster (ADR 0008): EKS's installs the control plane and
its gateway pools; each AKS or on-prem cluster's installs its own gateway
pools and Valkey, and, if a site keeps its own config CRDs, runs the site
relay (CRDs to `config/<site>/` in the bucket, status back). Fleet-wide
rollouts are the GitOps tool's. No operator reaches another cluster.

## 5. Build

`operator/` uses controller-runtime and the generated control API client;
it imports nothing else of golem (architecture.md §4.2). CRD YAML is
generated from `operator/api/v1alpha1` and shipped in the Helm chart,
which installs only the operator; the operator installs the rest from a
`ControlPlane` and one or more `GatewayPool`s.
