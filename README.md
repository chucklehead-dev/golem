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
self-service, Helm and GitOps for day-2, and a CLI and console.

Status: M1, the first team in production, built and awaiting its install
and acceptance runs: [docs/m1-plan.md](docs/m1-plan.md) is the scope of
record, with what is built, what remains and what M1 does not build. The
gateway serves Anthropic Messages, OpenAI Chat and Responses, Bedrock and
vLLM boxes, standalone from local YAML (`golem-gateway --standalone
examples/standalone`) or as a pool from signed snapshots in a bucket
(`golem-gateway -pool <name> -bucket … -keys … -valkey … -site …`, behind
`-accept-unreleased` until golem#7's deployment evidence is in).
golem-control, one replica, compiles a git checkout of the config and the
state document into those snapshots (`golem-control keygen ./keys`, then
`golem-control -config ./config -bucket ./bucket -signing-key
file:keys/publishing.key`; `-bucket s3://…` and `-signing-key kms:…` in
AWS), and with `-listen` and `-entra-tenant` serves the org and key APIs
that `golemctl` calls. In M1 budgets are per scope, on the known spend each
snapshot carries.

- [docs/walkthroughs/](docs/walkthroughs/README.md): the code's orientation:
  the package map, a request's path, the control plane's loop, the
  invariants with their enforcing functions and tests. Start here to read
  the code.
- [docs/prd.md](docs/prd.md): what it does and for whom, the feature set,
  cross-cutting requirements, and the open questions.
- [docs/roadmap.md](docs/roadmap.md): the phases, from spikes to
  multi-region, with their exit criteria.
- [docs/architecture.md](docs/architecture.md): the components, their APIs,
  the repository layout and the layering rules.
- [docs/gateway.md](docs/gateway.md): the data plane: the request path,
  packages, Go facilities, performance.
- [docs/control-plane.md](docs/control-plane.md): golem-control as built:
  the state document, the publication loop, the fence, known spend, the
  audit outbox, restore and bootstrap, the control API.
- [docs/operations.md](docs/operations.md): installation and day-2 with Argo CD
  (Argo Rollouts and Crossplane from M2).
- [docs/tools.md](docs/tools.md): golemctl, the console, Backstage.
- [docs/contracts.md](docs/contracts.md): refusals and status codes, error
  origin, the ledger schema, the price model, identifiers and headers,
  provider-side retention, compatibility.
- [docs/canonical-model.md](docs/canonical-model.md): the translation
  model, its adapters, capability-aware routing, deterministic encoding.
- [docs/org-model.md](docs/org-model.md): users, teams, projects, services,
  keys, roles, budgets, grants, approvals, capture and retention.
- [docs/entra.md](docs/entra.md): the Entra ID app registrations.
- [docs/testing.md](docs/testing.md): what testing exists and what is owed.
- [model/](model/README.md): three literate Quint models (publication,
  exposure, attempts), each checked by `make` and driving the code through
  its connect test.
- [docs/adr/](docs/adr/README.md): the decisions, one record each, amendment
  chains folded into their surviving record (earlier text in git history).

Comparable products, for reference: LiteLLM, agentgateway, Langfuse AI
Gateway, Envoy AI Gateway, Portkey, Bifrost, Kong AI Gateway, Cloudflare AI
Gateway. golem is not a general-purpose product like them: it serves one
organisation, so it leaves out multi-tenancy, billing, a large UI and most plugin
generality, and keeps the code small enough to read in an afternoon.

Design review amendments and production gates:
[docs/security-and-correctness.md](docs/security-and-correctness.md).

Second security review: [ADR 0041](docs/adr/0041-security-boundaries-and-incident-controls.md).
