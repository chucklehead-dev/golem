# golem

An LLM gateway for one enterprise, in plain Go.

golem sits between the enterprise's applications and coding agents and the
model providers they call (OpenAI, Anthropic, Azure OpenAI, Bedrock, Vertex,
self-hosted vLLM, ...). It authenticates callers (Entra ID, virtual keys), decides which
backend serves each request, enforces budgets that teams, projects and
budget managers delegate to their members, and reports everything through
OpenTelemetry to ClickStack, ClickHouse and Langfuse. It runs in
Kubernetes (Istio ambient) as separate components: a gateway that only
serves model traffic, a control plane with one API for configuration and
self-service, an operator for day-2, and a CLI and console.

Status: design. Nothing is built yet.

- [docs/prd.md](docs/prd.md): what it does and for whom, the feature set,
  cross-cutting requirements, and the open questions.
- [docs/roadmap.md](docs/roadmap.md): the phases, from spikes to
  multi-region, with their exit criteria.
- [docs/architecture.md](docs/architecture.md): the components, their APIs,
  the repository layout and the layering rules.
- [docs/gateway.md](docs/gateway.md): the data plane: the request path,
  packages, Go facilities, performance.
- [docs/control-plane.md](docs/control-plane.md): golem-control and
  services, the state document in object storage, signed snapshot distribution, jobs, bootstrap.
- [docs/operator.md](docs/operator.md): CRDs and day-2 operations.
- [docs/tools.md](docs/tools.md): golemctl, the console, Backstage.
- [docs/contracts.md](docs/contracts.md): refusals and status codes, error
  origin, the ledger schema, the price model, identifiers and headers,
  provider-side retention, compatibility.
- [docs/canonical-model.md](docs/canonical-model.md): the translation
  model, its adapters, capability-aware routing, deterministic encoding.
- [docs/org-model.md](docs/org-model.md): users, teams, projects, services,
  keys, roles, budgets, grants, approvals, capture and retention.
- [docs/entra.md](docs/entra.md): the Entra ID app registrations.
- [docs/testing.md](docs/testing.md): the testing regime; what to model in
  Quint and where to trial gotogether.
- [model/](model/): literate Quint models of golem's protocols, each
  readable as an explanation and checked by `make` (first: how a
  revocation reaches every gateway).
- [docs/adr/](docs/adr/): the v1 baseline of decisions, one per record
  (their history in docs/adr/archive/).

Comparable products, for reference: LiteLLM, agentgateway, Langfuse AI
Gateway, Envoy AI Gateway, Portkey, Bifrost, Kong AI Gateway, Cloudflare AI
Gateway. golem is not a general-purpose product like them: it serves one
organisation, so it leaves out multi-tenancy, billing, a large UI and most plugin
generality, and keeps the code small enough to read in an afternoon.

Design review amendments and production gates:
[docs/security-and-correctness.md](docs/security-and-correctness.md).
