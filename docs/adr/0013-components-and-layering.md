# 0013: Components with one job each, and layering checked by a test

Status: Proposed. Amended by [0025](0025-smaller-surfaces.md): golem-control and the worker are one binary in two roles.

## Context

Gateways that also host admin APIs, background jobs and UIs end up with
the dependencies, failure modes and attack surface of all of them on the
request path. The requirement is a data plane that does data-plane work
only, a separate control plane, an operator for day-2, and CLI/UI tools.

## Decision

- Six deliverables (architecture.md §1): golem-gateway, golem-control,
  golem-worker, golem-operator, golemctl, golem-console, plus an optional
  Backstage plugin. One Go module and repository; one image per binary.
- Exactly one API changes state: the control API. The operator, CLI,
  console and plugin are its clients with no private endpoints.
- Inside the control plane: transport → service → store. Rules live only
  in the service layer; the worker calls services.
- `internal/archtest` enforces the import rules (architecture.md §4.2)
  with `go list -deps`, so the gateway binary cannot acquire a database
  driver, a Kubernetes client or control-plane code by accident.

## Consequences

- More deployables than a monolith; each small and independently scaled.
- A feature that needs both planes is two changes joined by the API
  contract (protobuf, checked by `buf breaking`).

## Alternatives

- One binary with modes (LiteLLM-style): simpler to ship, but the
  gateway's image and dependency graph include everything.
- Several repositories: stronger separation, heavier changes across the
  API; the import test gives the separation without it.
