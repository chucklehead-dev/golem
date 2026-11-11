# 0003: Components with one job each; layering checked by a test

Status: Accepted (v1 baseline). From archive 0013, 0017, 0025, 0032.

## Decision

| Component | Does | Does not |
|---|---|---|
| **golem-gateway** | Serves model requests | Change anything, serve a management API, call the control plane or Kubernetes |
| **golem-control** | The control API; reads config CRDs; one Deployment, every replica serving the API, a **leader** (Kubernetes Lease) running the jobs (ADR 0019) and signing and publishing snapshots (ADR 0006) | Serve model traffic, reach another cluster |
| **golem-operator** (M2) | Installs and runs golem in its own cluster: upgrades, drains, canaries, credential mounting, mesh policies, Langfuse projects | Handle config, talk to gateways, reach another cluster |
| **golemctl** | CLI for every persona | Anything the API does not offer |
| **golem-console** (M2), **Backstage plugin** (M3) | Small UIs for weekly tasks | Same |

- One Go module and repository; one image per binary.
- Two kinds of change, one path each: declarative config through GitOps
  (CRDs, ADR 0005); entitlements and incident levers through the control
  API (ADR 0022). Every tool is an API client with no private endpoints.
- Inside golem-control: transport → service → store; rules live only in
  the service layer; jobs call services.
- `internal/archtest` (`go list -deps` per binary) fails if the gateway
  depends on control-plane code, a database driver or a Kubernetes client,
  if transport imports the store, or if a client tool imports anything of
  the control plane but generated clients and CRD types.
- M1 installs with Helm; the operator arrives in M2.

## Consequences

The gateway's image and failure modes are its own. A feature spanning both
planes is two changes joined by the API contract.

## Alternatives

One binary with modes (smaller to ship, everything on the request path);
several repositories (stronger walls, heavier cross-cutting changes).
