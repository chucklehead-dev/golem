# golem

An LLM gateway for one enterprise, in plain Go.

golem sits between the enterprise's applications and coding agents and the
model providers they call (OpenAI, Anthropic, Azure OpenAI, Bedrock, Vertex,
self-hosted vLLM, ...). It authenticates callers (Entra ID, virtual keys), decides which
backend serves each request, enforces budgets that teams, projects and
budget managers delegate to their members, and reports everything through
OpenTelemetry to ClickStack, ClickHouse and Langfuse. It runs in
Kubernetes (Istio ambient), configured by CRDs or a file, with a small
control plane for self-service keys, budgets and approvals.

Status: design. Nothing is built yet.

- [docs/prd.md](docs/prd.md): what it does and for whom, the feature set by
  milestone, and the open questions.
- [docs/org-model.md](docs/org-model.md): users, teams, projects, services,
  keys, roles, budgets, grants and approvals.
- [docs/design.md](docs/design.md): how it works: the request path, the
  packages, the Go facilities it relies on.
- [docs/adr/](docs/adr/): the decisions, one per record.

Comparable products, for reference: LiteLLM, agentgateway, Langfuse AI
Gateway, Envoy AI Gateway, Portkey, Bifrost, Kong AI Gateway, Cloudflare AI
Gateway. golem is not a general-purpose product like them: it serves one
organisation, so it leaves out multi-tenancy, billing, a large UI and most plugin
generality, and keeps the code small enough to read in an afternoon.
