# golem

An LLM gateway for one enterprise, in plain Go.

golem sits between the enterprise's applications and coding agents and the
model providers they call (OpenAI, Anthropic, Azure OpenAI, Bedrock, Vertex,
self-hosted vLLM, ...). It authenticates callers, decides which backend serves
each request, enforces cost-based limits, and reports everything through
OpenTelemetry. It runs as a Kubernetes service configured by CRDs or a file.

Status: design. Nothing is built yet.

- [docs/prd.md](docs/prd.md): what it does and for whom, the feature set by
  milestone, and the open questions.
- [docs/design.md](docs/design.md): how it works: the request path, the
  packages, the Go facilities it relies on.
- [docs/adr/](docs/adr/): the decisions, one per record.

Comparable products, for reference: LiteLLM, agentgateway, Langfuse AI
Gateway, Envoy AI Gateway, Portkey, Bifrost, Kong AI Gateway, Cloudflare AI
Gateway. golem is not a general-purpose product like them: it serves one
organisation, so it leaves out multi-tenancy, billing, a UI and most plugin
generality, and keeps the code small enough to read in an afternoon.
