# golem: product requirements

Status: draft for discussion. Date: 2026-11-11.

## 1. Problem

Teams across the enterprise call LLM providers directly, each with its own
keys, retries, logging and (rarely) cost controls. Nobody can answer "what
did we spend on model X last week, by team", rotate a provider key without a
round of redeploys, move traffic off a provider during an outage, or apply a
guardrail everywhere at once. Coding agents (Claude Code, Codex, Cursor and
the like) make this worse: they are high-volume, long-streaming, and each
expects its vendor's native API.

## 2. Goals

1. One endpoint per API dialect that every internal caller uses instead of
   the provider's, with no change to their SDK beyond the base URL and key.
2. Every request attributed (person or service, team, cost center), priced,
   and visible in the enterprise's existing OTel backend.
3. Spend and rate limits in money and tokens, per principal, team and model.
4. Model names decoupled from deployments: aliases, load balancing,
   failover and mirroring, changed by config without caller redeploys.
5. A small, documented hook interface on which guardrails, custom routing,
   memory and the like are built, in-process or out-of-process.
6. Operates like any well-behaved Kubernetes service: probes, graceful
   drain, declarative config, horizontal scaling.
7. Simple, fast Go: the proxy adds well under a millisecond at p50 to a
   non-translated request, and its code stays readable end to end.

## 3. Non-goals

- Multi-tenant SaaS: no organisations, signup, billing or invoicing.
- A management UI. Grafana (or whatever the OTel backend offers) is the
  UI; the admin API and CRDs are the control surface.
- Being a Kubernetes Gateway API implementation. golem runs behind the
  enterprise's ingress or gateway (ADR 0006).
- Prompt management, evaluation, datasets or fine-tuning workflows
  (Langfuse et al. do these; golem emits what they need).
- Every provider and every endpoint. The supported set is chosen (§5.1);
  anything else goes through raw pass-through (§5.1.4).

## 4. Users

| User | Needs |
|---|---|
| Application developer | Base URL and key; their SDK works unchanged; clear errors when limited. |
| Coding-agent user | Their agent's native API (Anthropic Messages, OpenAI Responses) with their SSO identity, streaming without stalls, long timeouts. |
| Platform operator | CRDs/GitOps, probes, dashboards, alerts, safe rollouts, key rotation. |
| FinOps / team lead | Spend by team/person/model, budgets with alerts, chargeback by cost center. |
| Security / compliance | Authn on every call, audit trail, data-residency routing, content logging policy, guardrails, FIPS mode. |

## 5. Features

Milestones: **M1** is the minimum to put real traffic through; **M2** makes
it the default path for the enterprise; **M3** and **Later** are planned but
not committed. Items marked *(added)* were not in the initial list and come
from the comparable products or from the enterprise setting.

### 5.1 API surface (frontends)

A *frontend* is an API dialect golem accepts; a *backend* is one it speaks
to a provider. When frontend and backend share a dialect, golem forwards the
body untouched except for the model name (the fast path, ADR 0001); otherwise
it translates.

| Dialect | Endpoints | Milestone |
|---|---|---|
| OpenAI Chat Completions | `POST /v1/chat/completions` (stream and not) | M1 |
| OpenAI Responses | `POST /v1/responses`, `GET/DELETE /v1/responses/{id}`, streaming events | M1 |
| Anthropic Messages | `POST /v1/messages`, `POST /v1/messages/count_tokens` | M1 |
| OpenAI Embeddings | `POST /v1/embeddings` | M1 |
| Model listing | `GET /v1/models` (OpenAI shape) and Anthropic's, filtered by what the caller may use *(added)* | M1 |
| OpenAI legacy Completions | `POST /v1/completions` | M2 |
| Google Gemini | `generateContent`, `streamGenerateContent` | M2 (to confirm, §8) |
| AWS Bedrock | Converse / ConverseStream, InvokeModel | M2 (backend first) |
| Rerank (Cohere/Jina shape) *(added)* | `POST /v1/rerank` | M3 |
| Audio, images, moderation *(added)* | transcription, speech, image generation, moderations | Later, by demand |
| Batch and Files *(added)* | OpenAI `/v1/batches`, `/v1/files`; Anthropic message batches | M3 |
| Realtime *(added)* | OpenAI Realtime over WebSocket | Later |

5.1.1 **Streaming** (SSE) is first-class for every dialect: bytes flushed as
they arrive, keepalive comments on idle, usage extracted from the stream
without buffering it, and `stream_options.include_usage` set on OpenAI
requests so usage is always reported *(added)*.

5.1.2 **Translation** between dialects (e.g. Anthropic Messages frontend to an
OpenAI-compatible vLLM backend) covers text, tool calls, images, system
prompts, stop reasons and usage. Features with no equivalent (Anthropic
thinking signatures, Responses' server-side state) fail the request with an
error naming the feature, rather than being silently dropped.

5.1.3 **Provider headers and betas pass through** *(added)*: `anthropic-beta`,
`anthropic-version`, `OpenAI-Beta` and the like, by an allowlist per
backend. Coding agents depend on them.

5.1.4 **Raw pass-through routes** *(added)*: `/{provider}/...` forwarded to
the provider as is, authenticated and metered where usage can be read, for
endpoints golem does not model.

### 5.2 Backends

| Backend | Auth to provider | Milestone |
|---|---|---|
| OpenAI | API key | M1 |
| Anthropic | API key | M1 |
| Azure OpenAI / Azure AI Foundry | API key, Entra ID (workload identity) *(added)* | M1 |
| Any OpenAI-compatible (vLLM, SGLang, Ollama, TGI, KServe, LiteLLM) | none, key, mTLS | M1 |
| AWS Bedrock | SigV4 from IRSA / Pod Identity *(added)* | M2 |
| Google Vertex AI (Gemini, Anthropic on Vertex) | workload identity *(added)* | M2 |

Each backend has its own timeouts (connect, first byte, idle between
chunks, total), connection pool, egress proxy and CA bundle *(added)*.

### 5.3 Authentication and identity

Every request resolves to a **Principal**: subject, kind (person or
service), teams, cost center, and the policies that apply. Authenticators
are tried in configured order; the first that recognises its credential
decides (accepts or rejects), so a bad JWT is not retried as a key.

| Method | Detail | Milestone |
|---|---|---|
| Virtual keys | `glm_...` bearer or `x-api-key` (so OpenAI and Anthropic SDKs both work); stored hashed; carry owner, team, cost center, allowed models, limits, expiry | M1 |
| JWT | Verified against JWKS (cached, rotated), issuer/audience checks; claims mapped to Principal by config (e.g. `groups` to teams) | M1 |
| OAuth 2 | Opaque tokens by introspection (RFC 7662), cached by token hash for its lifetime | M2 |
| mTLS | Client cert verified against configured CAs; SPIFFE ID or SAN to Principal; works for mesh-issued certs | M1 |
| Login for coding agents *(added)* | Device-code or browser OIDC flow that mints a short-lived personal virtual key, so agents carry the user's SSO identity | M2 |

**Credential pass-through** (optional, per route): the caller's own
provider credential is forwarded instead of golem's, after golem has
authenticated the caller by another means, or as the only credential in a
"BYO key" route that is still metered. Modes: `none` (golem's credential,
the default), `forward` (the caller's header to the provider), and
`exchange` (OAuth token exchange, RFC 8693, to a provider-scoped token)
*(added, M3)*.

**Authorization** *(added)*: which principals may use which models and
routes, by team or claim; deny by default; evaluated before any upstream
call.

### 5.4 Models, aliases and routing

- **Model catalog** *(added)*: each model's provider, context window, max
  output, capabilities (tools, vision, reasoning, caching) and prices (input,
  output, cached read, cache write, reasoning, batch). Seeded from a
  vendored price list, overridden by config with enterprise-negotiated
  rates. Prices are needed for limits (§5.5) and for spend (§5.7).
- **Aliases**: a public model name (`claude-sonnet`, `default-chat`,
  `gpt-large`) maps to a *pool* of deployments (provider + model + region +
  credential). The caller never needs to know which.
- **Load balancing** across a pool, by strategy: weighted random (M1),
  least in-flight (M1), latency EWMA (M2), quota headroom from provider
  rate-limit headers *(added, M2)*, cheapest-first *(added, M3)*.
- **Session affinity for prompt caching** *(added, M1)*: requests in one
  conversation (by a session header, or a hash of the prompt prefix) stick
  to one deployment, so provider prompt caches hit. For coding agents this
  is the largest cost lever after model choice.
- **Failover**: ordered fallback pools, tried on connection errors, 429,
  5xx and timeouts before the first response byte. Not after it: a stream
  that has started is never replayed elsewhere (it would duplicate tokens).
  Plus **context-window fallback** *(added, M2)*: a request too large for
  the primary (by estimate) goes to a larger-context pool.
- **Retries** with jittered backoff, honouring `Retry-After`, bounded by a
  per-request retry budget *(added)* so retries cannot amplify an outage.
- **Outlier ejection / cooldown** *(added, M1)*: a deployment returning
  errors is taken out of rotation for a period and probed back in.
- **Shadowing/mirroring**: a percentage of a route's requests also sent to
  a shadow pool; the shadow's response is recorded (latency, usage, cost,
  optionally the content) and discarded; never affects the caller, never
  counted against the caller's limits but costed to a separate account.
  M2.
- **Conditional routing** *(added, M2)*: a route chosen by CEL expression
  over the principal, headers and request metadata (e.g. EU teams to EU
  regions for data residency; a header to opt into a canary pool).
- **Priority classes** *(added, M3)*: interactive vs batch traffic; batch
  yields capacity (PTU, self-hosted) to interactive under contention.

### 5.5 Limits and budgets

- **Rate limits** in requests/min, tokens/min, and **cost per window**
  ($/minute, $/day, $/month), scoped to principal, team, cost center,
  model, route, or combinations.
- **Estimate then reconcile**: a request reserves its estimated cost
  (input tokens counted or estimated, output at `max_tokens` or a
  configured default) before going upstream, and the reservation is
  corrected with actual usage when the response ends. Streams that end
  early are charged for what they used.
- **Budgets** *(added)*: long-window spend caps (monthly per team) with
  soft thresholds that alert (an OTel metric and log, optionally a
  webhook) and a hard threshold that rejects, or downgrades to a cheaper
  alias if configured *(added, M3)*.
- **Concurrency limits** *(added)*: max in-flight streams per principal,
  so one runaway agent cannot hold every connection.
- Limit state is local per instance in M1 (each replica enforces its
  share) and shared through Redis/Valkey in M2 (ADR 0005). On a shared
  store outage, limits fall back to local enforcement; budgets are
  configurable fail-open or fail-closed.
- Responses carry the remaining allowance (`x-ratelimit-*` headers in the
  provider's style) and a 429 whose body says which limit, its scope, and
  when it resets.

### 5.6 Lifecycle hooks

Hooks are the extension point for guardrails, custom routing, memory,
request enrichment and audit (ADR 0004). Phases:

1. `OnRequest` after auth, before routing: may reject, rewrite the
   request, or annotate it (tags, routing hints).
2. `OnRoute` after the route is chosen: may change the pool or deployment.
3. `OnUpstreamRequest` per attempt: may add headers.
4. `OnResponseChunk` for streams: may observe, redact or stop.
5. `OnResponse` when the response is complete: may rewrite (non-stream),
   or flag.
6. `OnComplete` after the caller has its response: async, for audit,
   memory writes, evaluation sampling. Cannot affect the response.

Hooks run **in-process** (Go, compiled in, M1) or **out-of-process** (gRPC
or HTTP, M2), each with a timeout and a failure mode (`open` or `closed`).
A CEL condition selects which requests a hook sees. Built-in hooks *(added)*
give the common cases without writing any: regex/keyword deny lists, PII
redaction (Presidio-compatible remote), prompt-injection classifier call,
max prompt size, and provider moderation endpoints (M2).

### 5.7 Observability

- **Traces**: a SERVER span per inbound request, a CLIENT span per upstream
  attempt with the OTel GenAI semantic conventions (`gen_ai.operation.name`,
  `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`,
  `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`,
  finish reasons, ...), HTTP semconv on both, hook spans as children.
  W3C trace context propagated upstream.
- **Metrics**: HTTP server/client duration; GenAI `gen_ai.client.token.usage`,
  `gen_ai.client.operation.duration`, `gen_ai.server.time_to_first_token`,
  `gen_ai.server.time_per_output_token`; plus golem's own: cost (USD)
  by principal/team/model, limit decisions, retries, failovers, ejections,
  hook latency and outcomes, in-flight streams.
- **Logs**: one structured access log record per request (slog, bridged to
  OTel logs), with principal, route, deployment, usage, cost, outcome.
- **Content capture** *(added)*: prompts and completions are off by default;
  on per route as GenAI events, sampled, with redaction hooks applied first.
- **Spend ledger** *(added, M2)*: an append-only record of each request's
  attributed cost written asynchronously to Postgres (or as OTel logs to the
  warehouse), the source of truth for chargeback; metrics are for
  dashboards, not invoices.
- **Request metadata headers** *(added)*: `x-golem-session`, `x-golem-tags`,
  `x-golem-end-user` (or OpenAI's `user`, Anthropic's `metadata.user_id`)
  become attributes, for grouping agent sessions and attributing service
  traffic to end users, as Langfuse's gateway does.
- **Debug** *(added)*: a runtime/trace flight recorder snapshot written
  when a request exceeds a latency threshold; pprof (including the
  goroutine-leak profile) on the admin port.

### 5.8 Kubernetes operation

- Ports: data (TLS or plain behind the mesh), admin (probes, metrics if
  pulled, pprof, config dump), both configurable.
- `/livez` (process alive), `/readyz` (config loaded, at least one usable
  deployment per required pool, shared state reachable when required),
  `/healthz` (detail, for humans), `/startupz` *(added)*.
- **Graceful drain** *(added)*: on SIGTERM, readiness fails, new requests
  stop, in-flight streams get a configured grace (coding-agent streams run
  minutes) before being closed with a provider-style error event.
- **Backend health probes** *(added, M2)*: optional cheap probes per
  deployment feeding ejection, and visible on `/healthz`.
- Container-aware GOMAXPROCS (Go runtime default), `GOMEMLIMIT` from the
  memory limit, a PodDisruptionBudget and HPA on in-flight streams in the
  Helm chart *(added)*.
- **Configuration** by CRDs (`Backend`, `ModelRoute`, `VirtualKey`
  references, `RateLimitPolicy`, `Hook`) or by one YAML file (ConfigMap),
  both compiled into one validated, immutable snapshot swapped atomically
  (ADR 0002). Invalid config is rejected with field paths and the last good
  snapshot kept. Secrets only by reference to Kubernetes Secrets or files.
- Multiple instances: several golem deployments (e.g. per region or per
  security zone) each select their CRDs by label.

### 5.9 Security and compliance *(added)*

- **FIPS 140-3 mode** via Go's native FIPS module (`GODEBUG=fips140=on`),
  for environments that require it.
- **Audit log** of admin actions (key create/revoke, config changes) and
  of authorization denials.
- **Data residency**: routes constrained to regions by principal attribute
  (§5.4 conditional routing), and a config validation that fails if a
  residency-constrained alias can reach a disallowed region.
- **Upstream secret rotation** without restart (watched Secrets).
- Request size limits, header count limits, and timeouts on every phase.
- The data path never logs credentials; keys are hashed at rest and in
  logs show only a prefix.

### 5.10 Admin API *(added)*

On the admin port, authenticated by the enterprise's OIDC with an admin
group: virtual key create/list/revoke/rotate, spend queries by scope and
period, effective config dump (secrets redacted), and a dry-run endpoint
that shows which route, deployment and limits a given request would get.
A small CLI (`golem key create ...`, `golem route explain ...`) wraps it.

### 5.11 Caching *(added, M3)*

Exact-match response cache for non-streaming, deterministic requests
(temperature 0, or an explicit opt-in header), keyed by principal scope,
model and normalised body, in Redis. Semantic caching is Later, if at all:
its hit rates on enterprise traffic rarely justify an embedding call per
request and the false-hit risk.

### 5.12 Agent protocols *(added, Later)*

agentgateway and LiteLLM now front MCP servers (authn, tool allowlists,
federation, audit) and A2A. The same principal, policy and telemetry
machinery applies. Out of scope until the LLM path is done; the design keeps
routes protocol-agnostic so an MCP frontend can be added without reshaping
the core.

## 6. Milestones

| Milestone | Contents | Exit criterion |
|---|---|---|
| M0 | Skeleton: config snapshot, file source, one OpenAI-compatible backend, fast-path proxy, probes, OTel wiring | Streams a chat completion with a trace and token metrics |
| M1 | §5.1 M1 dialects, §5.2 M1 backends, virtual keys + JWT + mTLS, aliases, weighted/least-in-flight, failover, ejection, prompt-cache affinity, local cost limits, in-process hooks, CRD source, drain, Helm chart | One team's apps and coding agents in production through it |
| M2 | Translation, Bedrock/Vertex/Gemini, OAuth introspection, agent login, shared limits and budgets, spend ledger, shadowing, conditional routing, remote hooks, built-in guardrails, admin API/CLI | Default path for the enterprise; direct provider keys revoked |
| M3 | Batch/Files, rerank, cost-based routing, priority classes, budget downgrade, token exchange, response cache | By demand |

## 7. Success measures

- Proxy overhead (golem's own time, excluding upstream) p50 < 1 ms, p99 <
  5 ms on the fast path at 1k req/s per core; TTFT overhead < 2 ms.
- Memory per in-flight stream < 64 KiB on the fast path.
- 100% of requests attributed to a principal and a cost center.
- Spend from the ledger within 1% of provider invoices.
- A provider outage moves traffic to fallbacks within one ejection window,
  with no caller change.

## 8. Open questions

1. **"decisions" and "systemone"** in the initial list: I could not match
   these to an API. Did you mean OpenAI *embeddings* (or the legacy
   *completions*)? And is "systemone" Google *Gemini*, AWS *Bedrock
   Converse*, or an internal system? §5.1 assumes Gemini at M2.
2. **Virtual key storage**: Kubernetes Secrets (one per key, GitOps
   friendly, fine to a few thousand keys) or Postgres (needed anyway for
   the ledger; suits self-service key minting)? Recommendation: Postgres
   once the admin API exists, CRD/Secret-referenced keys for services.
3. **Shared state**: is a managed Redis/Valkey available in every cluster
   golem runs in? Shared limits and budgets need it (or Postgres-backed
   counters, slower).
4. **Translation priority**: which frontend-to-backend pairs actually
   matter? Likely Anthropic Messages to Bedrock/Vertex (same model family,
   small translation) and OpenAI Chat to everything. Full any-to-any is
   the most code in the project.
5. **Content logging policy**: who may turn on prompt capture, where it is
   stored, and retention.
6. **CRDs vs a file**: is the operator comfortable with golem's pods
   watching CRDs directly (RBAC to read its CRDs and referenced Secrets)?
7. **Ingress**: what sits in front (Istio, an ingress controller, an
   enterprise API gateway)? Determines whether golem terminates TLS/mTLS or
   reads a forwarded client identity (XFCC) from the mesh.
8. **MCP**: is fronting MCP servers wanted in the first year?
