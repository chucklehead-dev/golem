# golem: architecture

Status: draft, fifth pass (simplified by ADRs 0023–0025). The system as components, their APIs, and the
rules that keep them apart. Each component's inside has its own document:

| Document | Component |
|---|---|
| [gateway.md](gateway.md) | golem-gateway, the data plane |
| [control-plane.md](control-plane.md) | golem-control, api and worker roles: APIs, store, distribution, jobs |
| [roadmap.md](roadmap.md) | the phases and their exit criteria |
| [operator.md](operator.md) | golem-operator: CRDs, deployment, day-2 operations |
| [tools.md](tools.md) | golemctl, golem-console, the Backstage plugin |
| [org-model.md](org-model.md) | the entitlement model all of them share |
| [entra.md](entra.md) | the Entra ID app registrations they need |
| [testing.md](testing.md) | the testing regime, and where gotogether is trialled |

## 1. Components

| Component | Does | Does not | State |
|---|---|---|---|
| **golem-gateway** | Serves model requests: authn, routing, limits, hooks, upstream calls, telemetry | Change anything; serve any management API; call the control plane or Kubernetes | Caches; limit counters in Valkey; reads its snapshots from object storage |
| **golem-control**, `api` role | The control API (config read and check, overrides, entitlements, self-service, admin, spend), the watch on config CRDs and their status, snapshot publication to object storage | Serve model traffic; run background jobs | The state document in object storage (owner, ADR 0028); reads ClickHouse and Valkey |
| **golem-control**, `worker` role | Background jobs: Entra sync, period rollover, expiries, notifications, ledger reconciliation, retention deletion | Serve any API but probes | The same store, through the same service layer |
| **golem-operator** (M2) | Installs and upgrades the other components, migrations, drains, canaries, credential mounting, mesh policies, Langfuse projects | Handle config; talk to gateways | None (Kubernetes is its state) |
| **golemctl** | CLI for users, managers, budget managers and admins | Anything the API does not offer | Local credentials cache |
| **golem-console** | A small web UI for the same tasks (M2) | Anything the API does not offer | None |
| **Backstage plugin** | Keys, spend and budgets inside Backstage (M3, optional) | Same | None |

One image serves both golem-control roles; the gateway has its own.

External: Entra ID (identity, groups), the providers, the RESP store
(Valkey), the OTel collector feeding ClickStack,
ClickHouse and Langfuse, Grafana, Istio (Gloo Mesh, ambient).

## 2. How they connect

```
   users, managers, admins                       platform team (GitOps)
   golemctl · console · Backstage                 config CRDs ──────────────┐
            │                                     (Backend, ModelRoute,     │ watch,
            │ control API (Connect, Entra)         ScopePolicy, Hook,       │ status
            ▼                                      PriceList)               │
   golem-control  api role ◀────────────────────────────────────────────────┘
     entitlements · overrides · spend · audit · explain
   golem-control  worker role (jobs, same service layer)
            │  state document: CAS (If-Match)        golem-operator (M2): installs,
            │  snapshots: snapshots/<pool>/<hash>    upgrades, drains, mesh policies,
            ▼                                        Langfuse projects
   object storage (S3 · Azure Blob · Nutanix Objects)
            ▲  poll pointer, fetch by hash; write status/<pool>/<id>
            │
   golem-gateway ×N ──▶ providers, model servers
            │   └──▶ Valkey (limits, quotas)
            ├──▶ OTLP collector ──▶ ClickStack · ClickHouse golem.spend, audit
            └──▶ captured traces ──▶ Langfuse (a project per capturing scope)
```

Two kinds of change, one path each: declarative config through GitOps
(CRDs, reviewed as code); entitlements and incident levers through the
control API. golemctl, the console and the Backstage plugin are API
clients with no private endpoints. The gateway's only input is its pool's
snapshots in object storage: it never calls the control plane.

## 3. APIs

All defined in protobuf under `proto/golem/`, served with Connect
(connect-go on `net/http`), so every API is callable as gRPC, gRPC-Web, or
plain HTTP/JSON (`curl` works), with generated Go and TypeScript clients
(ADR 0014). Resource-oriented in the AIP style: standard methods
(Get, List, Create, Update, Delete), `etag` on every resource for
optimistic concurrency, field masks, page tokens,
`request_id` for idempotent creates. Requests that wait on someone
(approvals) are resources with a state, not long-running operations.

| API | Package | Callers | Resources |
|---|---|---|---|
| Config | `golem.config.v1` | admins, CI, golemctl | Read the effective config (CRDs plus overrides) per gateway pool; `Validate` a candidate CRD; `Explain` a request; `Override` (create, list, expire) |
| Org | `golem.org.v1` | managers, admins | Scope, Membership, Service, CostCenter, RoleBinding |
| Keys | `golem.keys.v1` | users, service owners | Key (mint, constrain, rotate, revoke), Login (device and browser flows for `golemctl login`) |
| Budget | `golem.budget.v1` | managers, budget managers | Grant, Cap, AdjustmentRequest (`Approve`, `Reject`, `Withdraw`), Pool (read) |
| Spend | `golem.spend.v1` | everyone, within their rights | Spend queries by scope, principal, key, cost center, model, period |
| Audit | `golem.audit.v1` | admins, auditors | AuditEvent (list, filter) |
| Admin | `golem.admin.v1` | admins | Gateways and pools (applied snapshot hashes from their status objects, health), drain, snapshot inspection, state history and restore; `Lease` for gateways with the second region (ADR 0016) |
| Registration | `golem.registration.v1` | model servers | `Register`, `Heartbeat` (PRD §5.2.1) |

Authorization is in the service layer, not the transport: every method
checks the caller's role on the resource (org-model §2).

## 4. Repository and layering

One Go module, one repository; binaries share generated API code and a few
libraries, and nothing else.

```
proto/golem/...            API definitions (buf); the contract between components
gen/                       generated Go (protobuf, connect) and TypeScript clients
cmd/golem-gateway/         each main only wires its component
cmd/golem-control/         --role=api|worker
cmd/golem-operator/
cmd/golemctl/
gateway/...                data plane (gateway.md §4)
control/
  api/                     Connect handlers: decode, call service, encode; no logic
  service/                 domain logic and authorization: org, keys, budget, config, spend
  store/                   repository interfaces
  store/doc/               the state document over a blob CAS interface (ADR 0028)
  store/blob/              adapters: S3, Azure Blob, Nutanix Objects, in-memory
  config/                  the CRD watch, validation, status, overrides layering
  distribution/            snapshot building and publication to object storage
  spend/                   ClickHouse queries
worker/                    job runner and jobs; calls control/service, never the store directly
api/v1alpha1/              CRD types for config and installation (kubebuilder markers)
operator/
  controllers/             installation and day-2 reconcilers
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
 transport (api/, console handlers, CLI commands)
     ↓
 service (control/service) — the only place rules live: authz, invariants, transactions
     ↓
 store (control/store interfaces) → store/doc (state document, blob CAS)
```

The worker and the console sit beside the transport layer: the worker
calls services, the console calls the API. No component reaches past the
layer below it.

### 4.2 Rules, checked by a test

`internal/archtest` runs `go list -deps` per binary and fails when:

- `cmd/golem-gateway` depends on `control/...`, `worker/...`,
  `operator/...`, `pgx`, `clickhouse-go`, `client-go` or
  `controller-runtime`;
- anything outside `control/store/...` writes the state document;
- `control/api` imports `control/store`;
- `operator`, `cli` or `console` import anything of `control` but the
  generated clients and `api/v1alpha1`;
- `control/api` imports `worker/` or the reverse;
- `gen/` or `lib/` import any component.

Each binary also gets its own image and its own dependency list in
`deploy/`, so the gateway's image carries the gateway's code only.

## 5. Data flow, by example

- **The platform team adds a model alias.** They open a PR with a
  `ModelRoute` CRD; CI runs `golemctl config validate` against the control
  API. After merge, Argo or Flux applies it (the admission webhook rejects
  it if invalid). golem-control sees it through its watch, builds the new
  snapshot, and publishes it for every gateway pool that
  selects it; the pools' gateways pick it up within a second, compile it and
  write their status; golem-control writes
  `Applied: 12/12` in the CRD's status.
- **An incident.** A provider deployment misbehaves; an admin runs
  `golemctl overrides create --disable deployment/openai-east --for 2h
  --reason INC-123`. It reaches the gateways in about a second and expires
  on its own; the audit log has who, why and when.
- **A user mints a key.** `golemctl keys create --scope team/platform
  --models 'claude-*' --cap 50/month`. golem-control checks the user's
  membership and rights, stores the hash, writes the audit event, and the
  key reaches the gateways in about a second (ADR 0012).
- **A request is charged.** The gateway reserves at the key, the member
  cap and the scope's pool (with two regions, its regional lease: ADR 0016), calls
  the provider, reconciles, and emits the access-log record, which becomes
  the `golem.spend` row in ClickHouse.
- **A budget manager approves a one-time adjustment.** The request's state
  becomes `approved`; the grant takes effect in the same transaction; the
  pool's new total reaches gateways in the next snapshot.
- **The control plane is down.** Gateways keep polling object storage;
  they start, serve and stay current with whatever was last published.
  Nothing can change until golem-control is back, except that keys and
  overrides expire on their own.

## 6. Deployment topologies

| Topology | Store | Notes |
|---|---|---|
| Development | none: gateway reads a snapshot file | `golemctl snapshot build` renders one from YAML |
| Single region (EKS in us-east-2, first) | The state document and snapshots in an S3 bucket in us-east-2 | golem-control (api, worker), gateways across zones; ElastiCache for Valkey; Helm in M1, the operator from M2 |
| Multi-region (later: us-east-2 and us-west-2) | Writer bucket in us-east-2, Cross-Region Replication to us-west-2 | golem-control in each region reading its local bucket and that region's CRDs, writing to the writer bucket; gateways read their region's bucket; Valkey per region; budgets through regional leases (ADR 0016) |
| AKS or on-prem | Azure Blob; on-prem Nutanix Objects, or S3 through IAM Roles Anywhere (ADR 0028) | Same code, a different blob adapter; Bedrock reached through Entra-to-AWS federation or IAM Roles Anywhere |

The gateway and golem-control's two roles each scale independently; only
the gateway is on the request path.

**Going multi-region later.** Adding us-west-2 is enabling Cross-Region
Replication to a bucket there and deploying golem-control and gateways:
no migration, no pause. Writes from us-west-2 cross to the writer bucket,
which is fine at golem's write rate; failover promotes the replica.
