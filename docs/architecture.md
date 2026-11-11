# golem: architecture

Status: draft, third pass. The system as components, their APIs, and the
rules that keep them apart. Each component's inside has its own document:

| Document | Component |
|---|---|
| [gateway.md](gateway.md) | golem-gateway, the data plane |
| [control-plane.md](control-plane.md) | golem-control and golem-worker: APIs, store, distribution, jobs |
| [operator.md](operator.md) | golem-operator: CRDs, deployment, day-2 operations |
| [tools.md](tools.md) | golemctl, golem-console, the Backstage plugin |
| [org-model.md](org-model.md) | the entitlement model all of them share |

## 1. Components

| Component | Does | Does not | State |
|---|---|---|---|
| **golem-gateway** | Serves model requests: authn, routing, limits, hooks, upstream calls, telemetry | Change anything; serve any management API; touch the database or Kubernetes | Caches; limit counters in the RESP store |
| **golem-control** | The control API (config, entitlements, self-service, admin, spend queries) and the distribution API that feeds gateways | Serve model traffic; run background jobs | Postgres or Aurora DSQL (owner); reads ClickHouse |
| **golem-worker** | Background jobs: Entra group sync, period rollover, regional lease rebalancing, key expiry, notifications, ledger reconciliation, approval expiry | Serve any API but probes | The same store, through the same service layer |
| **golem-operator** | Reconciles CRDs: deploys and upgrades the other components, pushes operator config (backends, routes, policies) to the control API, reflects status, runs migrations, rotates credentials | Hold config itself; talk to gateways directly | None (Kubernetes is its state) |
| **golemctl** | CLI for users, managers, budget managers and admins | Anything the API does not offer | Local credentials cache |
| **golem-console** | A small web UI for the same tasks (M2) | Anything the API does not offer | None |
| **Backstage plugin** | Keys, spend and budgets inside Backstage (M3, optional) | Same | None |

External: Entra ID (identity, groups), the providers, the RESP store
(Dragonfly, Valkey or Redis), the OTel collector feeding ClickStack,
ClickHouse and Langfuse, Grafana, Istio (Gloo Mesh, ambient).

## 2. How they connect

```
            users, managers, admins                operators (GitOps)
           golemctl · console · Backstage            CRDs in Kubernetes
                       │                                    │
                       │ control API (Connect, Entra auth)  │ watch
                       ▼                                    ▼
   ┌───────────────── golem-control ◀── control API ── golem-operator ──▶ Deployments,
   │ config · entitlements · self-service · admin       (as a client)       HPA, PDB, waypoint,
   │ spend (reads ClickHouse)                                                AuthorizationPolicy,
   │ distribution API ───────────┐                                           migration Jobs
   └──── store: Postgres | DSQL ─┼── golem-worker (jobs, same service layer)
                                 │ stream: snapshot + deltas, acks/status
                                 ▼
                          golem-gateway  ×N ──▶ providers
                                 │   └──▶ RESP store (limits, leases)
                                 └──▶ OTLP collector ──▶ ClickStack · ClickHouse golem.spend · Langfuse
```

There is exactly one public API for changing anything: the control API.
golemctl, the console, the Backstage plugin and the operator are all its
clients, with no private endpoints. The gateway is a client of only the
distribution API, which is read-only apart from acknowledgements.

## 3. APIs

All defined in protobuf under `proto/golem/`, served with Connect
(connect-go on `net/http`), so every API is callable as gRPC, gRPC-Web, or
plain HTTP/JSON (`curl` works), with generated Go and TypeScript clients
(ADR 0014). Resource-oriented in the AIP style: standard methods
(Get, List, Create, Update, Delete), `etag` on every resource for
optimistic concurrency (it maps onto DSQL's), field masks, page tokens,
`request_id` for idempotent creates, and long-running operations for
anything that waits on someone (approvals).

| API | Package | Callers | Resources |
|---|---|---|---|
| Config | `golem.config.v1` | operator, admins, `golemctl apply` | Backend, Deployment, ModelAlias, Route, ScopePolicy, LimitPolicy, Hook, PriceOverride, Gateway (a deployment of gateways, its labels and selected config) |
| Org | `golem.org.v1` | managers, admins | Team, Project, Membership, Service, CostCenter, RoleBinding |
| Keys | `golem.keys.v1` | users, service owners | Key (mint, constrain, rotate, revoke), Login (device and browser flows for `golemctl login`) |
| Budget | `golem.budget.v1` | managers, budget managers | Grant, Allocation, AdjustmentRequest (an operation: approve, reject, withdraw), Pool (read) |
| Spend | `golem.spend.v1` | everyone, within their rights | Spend queries by scope, principal, key, cost center, model, period |
| Audit | `golem.audit.v1` | admins, auditors | AuditEvent (list, filter) |
| Admin | `golem.admin.v1` | admins | Gateways (versions, health), drain, snapshot inspection, explain (which route, deployment, limits a given request would get) |
| Distribution | `golem.distribution.v1` | gateways | `Watch` (server stream: snapshot, then deltas), `Ack` (applied version, compile errors, health), `Lease` (regional budget leases, ADR 0016) |

Authorization is in the service layer, not the transport: every method
checks the caller's role on the resource (org-model §2). The operator acts
as a service principal with the `config-writer` role, scoped to the
objects it manages (ADR 0012).

## 4. Repository and layering

One Go module, one repository; binaries share generated API code and a few
libraries, and nothing else.

```
proto/golem/...            API definitions (buf); the contract between components
gen/                       generated Go (protobuf, connect) and TypeScript clients
cmd/golem-gateway/         each main only wires its component
cmd/golem-control/
cmd/golem-worker/
cmd/golem-operator/
cmd/golemctl/
gateway/...                data plane (gateway.md §4)
control/
  api/                     Connect handlers: decode, call service, encode; no logic
  service/                 domain logic and authorization: org, keys, budget, config, spend
  store/                   repository interfaces
  store/sql/               one SQL implementation for Postgres and Aurora DSQL (ADR 0015)
  distribution/            snapshot building and the Watch/Ack/Lease server
  spend/                   ClickHouse queries
worker/                    job runner and jobs; calls control/service, never the store directly
operator/
  api/v1alpha1/            CRD types (kubebuilder markers)
  controllers/             reconcilers; talk to the control API through gen/ clients
console/                   web UI (Go html/template + htmx), a client of the control API
cli/                       golemctl commands, a client of the control API
plugins/backstage/         TypeScript, generated client (M3)
lib/                       small shared libraries: telemetry setup, Entra token validation,
                           uuid/v7 ids, retry, the snapshot format (types only)
deploy/                    Helm chart (installs the operator and CRDs), ClickHouse DDL, Grafana
docs/
```

### 4.1 Layers

```
 transport (api/, distribution server, console handlers, CLI commands)
     ↓
 service (control/service) — the only place rules live: authz, invariants, transactions
     ↓
 store (control/store interfaces) → store/sql (Postgres | DSQL)
```

The worker and the console sit beside the transport layer: the worker
calls services, the console calls the API. No component reaches past the
layer below it.

### 4.2 Rules, checked by a test

`internal/archtest` runs `go list -deps` per binary and fails when:

- `cmd/golem-gateway` depends on `control/...`, `worker/...`,
  `operator/...`, `pgx`, `clickhouse-go`, `client-go` or
  `controller-runtime`;
- anything outside `control/store/...` imports a database driver;
- `control/api` imports `control/store`;
- `operator`, `cli` or `console` import anything of `control` but the
  generated clients;
- `gen/` or `lib/` import any component.

Each binary also gets its own image and its own dependency list in
`deploy/`, so the gateway's image carries the gateway's code only.

## 5. Data flow, by example

- **An operator adds a model alias.** They commit a `ModelRoute` CRD. The
  operator validates it (schema, plus a dry-run `Update` with
  `validate_only`), writes it through the config API, and sets the CRD's
  status from the response. golem-control commits it, builds the new
  snapshot version, and streams the delta to every gateway whose `Gateway`
  selects it. Each gateway compiles and acks; the operator reports
  `Applied: 12/12 gateways` in the CRD status.
- **A user mints a key.** `golemctl keys create --scope team/platform
  --models 'claude-*' --cap 50/month`. golem-control checks the user's
  membership and rights, stores the hash, writes the audit event, and the
  key reaches the gateways in about a second (ADR 0012).
- **A request is charged.** The gateway reserves at the key, the member
  allocation and the regional lease of the scope's pool (ADR 0016), calls
  the provider, reconciles, and emits the access-log record, which becomes
  the `golem.spend` row in ClickHouse.
- **A budget manager approves a one-time adjustment.** The approval
  completes the long-running operation; the grant takes effect; the worker
  raises the scope pool's regional leases; gateways see the new lease on
  their next lease renewal.

## 6. Deployment topologies

| Topology | Store | Notes |
|---|---|---|
| Development | none: gateway reads a snapshot file | `golemctl snapshot build` renders one from YAML |
| Single region (EKS, initially) | Aurora DSQL single-region, or Postgres | One operator, control, worker; gateways per zone |
| Multi-region | Aurora DSQL multi-region (active-active) | golem-control and worker in each region against the local endpoint; gateways use their region's control; RESP store per region; budgets enforced through regional leases (ADR 0016) |
| AKS or on-prem | Postgres (CloudNativePG or managed) | Same code, same SQL dialect (ADR 0015); Bedrock reached through Entra-to-AWS federation or IAM Roles Anywhere |

The gateway, the worker and golem-control each scale independently; only
the gateway is on the request path.
