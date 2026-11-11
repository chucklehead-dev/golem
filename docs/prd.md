# golem: product requirements

Status: draft for discussion. Revised 2026-11-11 (sixth pass: the design envelope, ADR 0026; the roadmap in roadmap.md).

## 1. Problem

Teams across the enterprise call LLM providers directly, each with its own
keys, retries, logging and (rarely) cost controls. Nobody can answer "what
did we spend on model X last week, by team", rotate a provider key without a
round of redeploys, move traffic off a provider during an outage, or apply a
guardrail everywhere at once. Coding agents (Claude Code, Codex, Cursor and
the like) make this worse: they are high-volume, long-streaming, and each
expects its vendor's native API. Budgets are set in spreadsheets and
enforced by invoices a month later.

## 2. Goals

1. One endpoint per API dialect that every internal caller uses instead of
   the provider's, with no change to their SDK beyond the base URL and key.
2. Every request attributed to a person or service, a team or project, and
   its funding cost centers; priced; visible in ClickStack, Langfuse and
   Grafana.
3. Self-service within delegated authority: users mint their own keys,
   team and project managers cap their members, budget managers fund
   and approve. Budgets are enforced at request time, not at invoice time.
4. Model names decoupled from deployments: aliases, load balancing,
   failover and mirroring, changed by config without caller redeploys.
5. A small, documented hook interface on which guardrails, custom routing,
   memory and the like are built, in-process or out-of-process.
6. Operates like any well-behaved Kubernetes service in the enterprise's
   Istio (Gloo Mesh, ambient mode): probes, graceful drain, declarative
   config, horizontal scaling.
7. Simple, fast Go: the proxy adds well under a millisecond at p50 to a
   non-translated request, and its code stays readable end to end.
8. Well layered: the gateway does data-plane work only; configuration,
   self-service and administration are a separate control plane behind one
   API; an operator runs day-2; CLI and UI tools are clients of that API
   (architecture.md, ADR 0013).

## 3. Non-goals

- Multi-tenant SaaS: one enterprise, one Entra tenant (guest users aside),
  no signup or invoicing.
- A large management UI. Grafana and HyperDX are where people look;
  golemctl, a small console (M2) and optionally a Backstage plugin (M3)
  are where they act (tools.md).
- Being a Kubernetes Gateway API implementation. golem runs behind the
  mesh's ingress and in the ambient mesh (ADR 0006).
- Prompt management, evaluation, datasets: Langfuse does these; golem
  feeds it.
- Every provider and every endpoint. The supported set is chosen (§5.1);
  anything else goes through raw pass-through (§5.1.4).

## 4. Users

| User | Needs |
|---|---|
| Application developer | Base URL and key (or their service's Entra identity); their SDK works unchanged; clear errors when limited. |
| Coding-agent user | Their agent's native API (Anthropic Messages, OpenAI Responses) with their Entra identity via `golemctl login`; streaming without stalls. |
| Manager (of a team or project) | Membership, services, caps for members, adjustment requests, capture settings, spend by member. |
| Budget manager | Fund teams and projects from their cost centers; approve requests; spend against cost center. |
| Platform operator | CRDs/GitOps for backends and routes, probes, dashboards, alerts, safe rollouts, provider key rotation. |
| Security / compliance | Authn on every call, audit trail, data-residency routing, content capture policy, guardrails, FIPS mode. |

## 5. Features

Milestones: **M1** is the minimum to put real traffic through; **M2** makes
it the default path for the enterprise; **M3** and **Later** are planned but
not committed. Items marked *(added)* were not in the initial list.

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
| OpenAI Decisions | `POST /v1/decisions` (non-streaming; predicate, choice, score) | M1 (pass-through) |
| TypeSafe System One (Jev) | `POST /v1/systemone` (non-streaming; yes/no, choice, score) | M1 (pass-through) |
| Model listing | `GET /v1/models`, filtered by the caller's effective rights *(added)* | M1 |
| OpenAI legacy Completions | `POST /v1/completions` | M2 |
| Rerank (Cohere shape) *(added)* | `POST /v1/rerank`, Bedrock rerank models as backend | M3 |
| Batch and Files *(added)* | OpenAI `/v1/batches`, `/v1/files`; Anthropic message batches; Bedrock batch inference | M3 |
| Audio, images, moderation, Realtime *(added)* | | Later, by demand |

5.1.1 **Streaming** (SSE) is first-class for every streaming dialect: bytes
flushed as they arrive, keepalive comments on idle, usage extracted from the
stream without buffering it, and `stream_options.include_usage` set on
OpenAI requests so usage is always reported *(added)*.

5.1.2 **Translation** is scoped to what the backends need (ADR 0001):

| Frontend → backend | Kind | Milestone |
|---|---|---|
| Anthropic Messages → Bedrock (Anthropic models) | envelope only: model in path, `anthropic_version` in body, event-stream framing to SSE | M1 |
| OpenAI Chat → Anthropic Messages | translation | M1 |
| OpenAI Chat → Bedrock Converse / ConverseStream | translation | M1 |
| OpenAI Embeddings → Bedrock (Titan, Cohere embed) | per-model body adapter | M2 |
| OpenAI Chat → Gemini (Vertex), if Vertex is used | translation | M3 |
| OpenAI Decisions ↔ System One | translation of the shared decision shape (predicate/yes-no, choice, score), so one decision alias can fail over between them *(added)* | M3 |

Translation covers text, tool calls, images, system prompts, stop reasons
and usage. Features with no equivalent fail the request with an error naming
the feature, rather than being silently dropped. Other pairs are refused at
config validation.

5.1.3 **Decision dialects** *(added)*: both are billed on input tokens
only and do not stream, so their cost is known before the call: the
reservation (§5.5) is exact, and a `stream: true` request is refused by
golem with the provider's own error shape.

5.1.4 **Provider headers and betas pass through** *(added)*:
`anthropic-beta`, `anthropic-version`, `OpenAI-Beta` and the like, by an
allowlist per backend. Coding agents depend on them.

5.1.5 **Raw pass-through routes** *(added)*: `/{provider}/...` forwarded to
the provider as is, authenticated and metered where usage can be read.

### 5.2 Backends

| Backend | Auth to provider | Milestone |
|---|---|---|
| OpenAI (commercial API: Chat, Responses, Embeddings, Decisions) | project API keys; organisation and project headers per deployment | M1 |
| Anthropic | API key | M1 |
| AWS Bedrock (InvokeModel, Converse, embeddings) | SigV4: EKS Pod Identity (initially); on AKS or on-prem, Entra-to-AWS OIDC federation (`AssumeRoleWithWebIdentity`) or IAM Roles Anywhere with the workload's X.509 identity *(added)*; Bedrock API keys | M1 (EKS), M2 (elsewhere) |
| Microsoft Foundry: Azure OpenAI models (the v1 OpenAI-compatible API: Chat, Responses, Embeddings) | Entra token (workload identity federation from the EKS service account), API key for testing | M1 |
| Microsoft Foundry: Claude (`https://<resource>.services.ai.azure.com/anthropic/v1/messages`) | Entra bearer token (scope to confirm: Microsoft's docs say `https://ai.azure.com/.default`; some models are Entra-only), API key | M1 |
| Microsoft Foundry: other Foundry Models (OpenAI-compatible chat) | Entra token, API key | M2 |
| OpenAI Decisions | API key | M1 |
| TypeSafe System One | API key | M1 |
| Self-hosted vLLM and SGLang, in other clusters or data centers (§5.2.1) | mesh mTLS (multi-cluster), bearer key, or TLS client certificate | M1 |
| Other OpenAI-compatible servers (Ollama, KServe, TGI) | none, key, mTLS | M2 |
| Google Vertex AI | workload identity federation | M3, if needed |

Each backend has its own timeouts (connect, first byte, idle between
chunks, total), connection pool, and egress path (direct or the mesh's
egress gateway) *(added)*.

Same-dialect envelopes rather than translations: Anthropic Messages to
Claude on Foundry and on Bedrock; OpenAI dialects to Azure OpenAI on
Foundry (a base path and auth change).

#### 5.2.1 Self-hosted model servers (ADR 0019)

Today: standalone machines (DGX Spark, Mac mini and Mac Studio) running a
current vLLM behind a TLS proxy, outside Kubernetes and the mesh. Coming:
NVL4 and NVL8 boxes and cloud GPU machines, some of them short-lived.
Models on all of them change over time.

- **Each server instance is a deployment.** A Backend has endpoints, each
  one vLLM (or SGLang) instance: one per small machine; one or several per
  NVL box (a model per instance, or one data-parallel instance).
- **Discovery**, by Backend: a static list of DNS names; DNS (SRV or
  several A records); or **registration**, where an instance (or a small
  sidecar next to it) registers itself with the control API under a
  service identity and heartbeats, which suits cloud GPU machines that
  come and go. Registered endpoints that stop heartbeating leave the
  snapshot.
- **Registration identity**, in order of preference: the platform's
  workload identity where the machine has one (Entra workload identity on
  AKS or Azure VMs; EKS service accounts federated to Entra; an EC2
  instance role, proven by a signed STS `GetCallerIdentity` request that
  golem-control verifies); otherwise an X.509 client certificate from the
  enterprise PKI or SPIRE, the same certificate the machine's TLS proxy
  serves, so one identity covers both directions; for machines without
  either (a Mac on a desk), a one-time bootstrap token per machine pool,
  exchanged at first registration for a short-lived, rotated machine
  credential. Each registration identity is allowed only into named
  Backends, with labels and endpoint names constrained to an allowed
  domain, and the gateway checks the endpoint's TLS name: a registration
  can never point golem at an arbitrary host.
- **Models are discovered, not declared**: golem polls each endpoint's
  `/v1/models`; aliases select deployments by model name or by a label
  the registration carries (`family: qwen3-coder`), so a box changing its
  model moves it between pools without config changes.
- **Capacity**: an adaptive concurrency limit per endpoint (it grows while
  latency holds and shrinks when it rises) discovers what a Mac mini or an
  NVL8 can take, with an optional declared ceiling; routing is least
  in-flight relative to that limit, refined by vLLM's `/metrics` (running
  and waiting requests, KV-cache use) and with **prefix affinity** within
  the limit.
- **Fair share** (ADR 0026): on these scarce pools golem queues per
  principal and serves round-robin across principals, and caps each
  principal's share of a pool's capacity (default half), so the few very
  heavy users cannot occupy every machine.
- **Overflow**: at the limit, or on 429/503, requests go to the alias's
  fallback pool before the first byte, or wait briefly for a slot if the
  route allows (`queue: 2s`).
- **Availability**: health probes every few seconds and passive ejection
  for machines that sleep, reboot or are reclaimed.
- **Reaching them**: TLS at each machine with vLLM's `--api-key` as a
  bearer secret or a client certificate; through the mesh's egress gateway
  (a ServiceEntry per static endpoint; registered endpoints by an allowed
  domain); cloud GPU machines in the same way, or in-mesh where they join a
  cluster.
- **Dialects**: OpenAI Chat, Completions and Embeddings, and Anthropic
  Messages pass-through (current vLLM serves `/v1/messages`), so coding
  agents can use local models without translation.
- **Pricing**: the platform team sets per-token rates through
  `PriceList`, as for commercial models; for cloud GPU machines a rate
  can be derived from the instance's hourly cost and measured throughput
  (M3).
- **Clusters later**: a Backend may be one *service* URL whose own balancer
  (llm-d, an Inference Gateway, KServe) picks the replica.

### 5.3 Authentication and identity

Every request resolves to a **Principal**: a user or a service, the
**scope** (team or project) it is acting in, and the key if any (ADR 0003,
org model §1–3). Authenticators are tried in configured order; the first
that recognises its credential decides.

| Method | Detail | Milestone |
|---|---|---|
| Virtual keys | `glm_...` as bearer or `x-api-key` (OpenAI and Anthropic SDKs both work); hashed; bound to one scope; constrained (org model §3) | M1 |
| Entra ID JWT (users) | v2 access tokens for golem's app registration; JWKS cached; `tid`, `aud`, `iss` checked; `oid` is the user; scope from a header (`x-golem-scope`) or the user's default | M1 |
| Entra ID JWT (services) | client credentials or workload identity federation; `azp`/`appid` maps to a Service | M1 |
| `golemctl login` for coding agents *(added)* | Entra device-code or browser flow; mints a short-lived personal key in the chosen scope, written where the agent reads it | M1 |
| Mesh identity | In ambient mode the app does not see the peer certificate (ztunnel terminates mTLS). A waypoint in front of the gateway passes the caller's SPIFFE ID in a header, trusted only when the operator has restricted the data port to the waypoint (ADR 0003); binds keys to workloads, identifies in-mesh services without keys | Spike in M1, feature in M2 |
| OAuth introspection (non-Entra IdPs) | RFC 7662 | Later, if ever |

**Credential pass-through** (optional, per route): `none` (golem's provider
credential, the default), `forward` (the caller's own provider credential,
after golem has authenticated the caller by key or Entra; metered and
charged as usual), `exchange` (Entra on-behalf-of to an Azure resource
token, so Azure OpenAI sees the user) *(M2)*.

**Authorization**: effective rights are the scope's policy ∩ membership ∩
key constraints, evaluated per request (org model §3). Deny by default.

### 5.4 Organisation, keys and budgets *(added, from the second pass)*

The full model is [org-model.md](org-model.md). In short:

- Users (Entra); scopes, which are teams (optionally bound to Entra
  groups, synced from Graph) or projects (owned by a team; members may be
  teams, services, users); services (owned by a scope); cost centers
  (external). ADR 0024.
- Roles: member, manager (on a scope), budget manager (on a cost center),
  admin.
- Users mint personal keys within their own rights; keys are further
  constrainable (models, spend cap, rates, concurrency, expiry, source
  binding). Managers mint service keys.
- Budgets per month: budget managers grant from cost centers to teams and
  projects; a team's managers grant team money to projects; managers cap
  their members. Recurring ("permanent") and one-time grants and
  adjustments; those drawing on a cost center need a budget manager's
  approval.
- Enforcement per request at three levels: key cap, member cap,
  scope pool.

| Capability | Milestone |
|---|---|
| Users, teams (Entra-group bound), services, personal and service keys, scope policies, member caps, team pools granted by admins, enforcement modes | M1 |
| Projects, cost centers, budget managers, grants, approvals, adjustments, audit | M2 |
| Content capture settings and overrides (org-model §3.1) | M2 |
| golem-console; notifications (email/Teams) on thresholds and approvals | M2 |
| Multi-region budgets through regional leases (ADR 0016) | M2 |
| Backstage plugin; budget downgrade to a cheaper alias; chargeback export per cost center | M3 |

### 5.5 Limits

- **Enforcement modes** per scope: `observe`, `warn`, `enforce` (§9.1).
- **Provider quota limits** per deployment, shared by all gateways (§9.2).
- **Rate limits** in requests/min, tokens/min and $/min, at any of the
  levels of §5.4 and per model or route; **concurrency limits** on
  in-flight streams per principal *(added)*.
- **Estimate then reconcile** (ADR 0005): reserve the estimate at every
  level before the call, correct with actual usage at the end; streams that
  end early are charged what they used.
- Shared state in a Redis-protocol store: Valkey, Redis or **Dragonfly**,
  tested against Valkey and Dragonfly (ADR 0005). M1 may run without it
  (per-instance shares); M2 requires it for budgets.
- Counters are rebuilt from the ClickHouse ledger on store loss and
  reconciled against it daily, so a store failure loses at most the
  in-flight reservations.
- On store outage: rate limits fall back to local shares; budget levels
  are fail-open or fail-closed per scope policy.
- 429 bodies, in the caller's dialect, say which limit, at which level,
  and when it resets; `x-ratelimit-*` headers in the provider's style.

### 5.6 Models, aliases and routing

- **Model catalog** *(added)*: provider, context window, capabilities,
  prices (input, output, cached read, cache write, reasoning, batch,
  input-only for decision models), with enterprise rates overriding a
  vendored price list. Prices are versioned; each spend record names its
  version.
- **Aliases**: a public model name maps to a *pool* of deployments.
- **Load balancing**: weighted random, least in-flight (M1); latency EWMA,
  provider quota headroom from rate-limit headers *(added)* (M2);
  cheapest-first (M3).
- **Prompt-cache affinity** *(added, M1)*: a conversation (session header,
  or a hash of the prompt prefix) sticks to one deployment so provider
  prompt caches hit. Largest cost lever for coding agents after model
  choice.
- **Failover**: ordered fallback pools before the first response byte;
  never after (ADR 0007). **Context-window fallback** *(added, M2)*.
- **Retries** with jittered backoff, `Retry-After`, a retry budget
  *(added)*. **Outlier ejection / cooldown** *(added, M1)*.
- **Shadowing/mirroring** (M2): a percentage of a route's requests also sent
  to a shadow pool; recorded, discarded, charged to a platform account, not
  to the caller. Shadow traces go to Langfuse tagged `shadow`, so a model
  change can be evaluated there on real traffic *(added)*.
- **Conditional routing** *(added, M2)*: CEL over principal, scope and
  headers (data residency, canaries).
- **Priority classes** *(added, M3)*.

### 5.7 Lifecycle hooks

Four phases: `Request` (reject, rewrite, annotate with routing hints),
`Chunk` (streams, M2), `Response`, `Complete` (async) (ADR 0004 as amended
by 0025). In-process (Go, M1) or remote
(gRPC/HTTP, M2), each with a CEL condition, a timeout and a failure mode.
Built-in hooks: deny lists, PII redaction (remote, Presidio-compatible),
prompt-injection classifier, max prompt size, moderation (M2). A decision
model (§5.1.3) is a natural, cheap guardrail classifier: a built-in hook can
ask an OpenAI Decisions or System One question about the prompt and reject
on the answer *(added, M2)*.

### 5.8 Observability *(revised for ClickStack, Langfuse, Grafana)*

All telemetry leaves golem as OTLP to the enterprise OTel collector, which
routes it (ADR 0010):

| Destination | What | Why |
|---|---|---|
| ClickStack (ClickHouse + HyperDX) | All traces, logs, metrics; the access log | Operational search, HyperDX for incidents |
| ClickHouse `golem.spend` | One record per request (org model §4.4), from the access log by materialized view, deduplicated by request id | The ledger: chargeback, budget rebuilds, spend APIs |
| Langfuse (one project per capturing scope, ADR 0018) | GenAI traces with content, or metadata only, as the scope chooses; `langfuse.user.id`, `langfuse.session.id`, tags, scope metadata and golem's own cost | Sessions, prompt/completion review, evaluation, shadow comparisons, with Langfuse access matching the scope |
| Grafana | Dashboards and alerts over ClickHouse (spend, budgets, limits, errors, latency, TTFT) and metrics, provisioned from this repo | Operators, managers, FinOps |

- **Traces**: SERVER span per inbound request, CLIENT span per upstream
  attempt with the OTel GenAI semantic conventions (`gen_ai.operation.name`,
  `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.response.model`,
  `gen_ai.usage.*`, finish reasons), HTTP semconv, hook spans. W3C trace
  context propagated upstream.
- **Metrics**: HTTP server/client duration; `gen_ai.client.token.usage`,
  `gen_ai.client.operation.duration`, `gen_ai.server.time_to_first_token`,
  `gen_ai.server.time_per_output_token`; cost; limit decisions; retries,
  failovers, ejections; hooks; in-flight streams.
- **Content capture** off by default, set per team or project with
  retention, and overridable by keys and services only as far as the
  scope allows (org-model §3.1, ADR 0018). Captured content goes only to
  the scope's own Langfuse project, which golem provisions; per-record
  retention is enforced by deletion, with the Langfuse project's
  retention as a backstop.
- **Request metadata headers** *(added)*: `x-golem-session`,
  `x-golem-tags`, `x-golem-end-user` (or OpenAI's `user`, Anthropic's
  `metadata.user_id`); coding-agent sessions grouped automatically where
  the agent sends a session id.
- **Spend queries**: the self-service API answers "my spend", "my team's",
  "this cost center's" from ClickHouse, with the same numbers Grafana shows.
- **Debug** *(added)*: runtime/trace flight-recorder snapshots of slow
  requests; pprof including the goroutine-leak profile on the admin port.

### 5.9 Components, Kubernetes and the mesh

The system is these deliverables (architecture.md, ADR 0013 as amended by
0025):

| Component | Role | Milestone |
|---|---|---|
| golem-gateway | Data plane only: model requests in, provider calls out | M0 |
| golem-control, `api` role | Control API (config read and check, overrides, org, keys, budgets, spend, audit), the distribution API, the CRD watch | M1 |
| golem-control, `worker` role | Jobs: Entra sync, period rollover, expiries, notifications, ledger reconciliation, retention | M1 |
| golem-operator | Installation and day-2: upgrades, migrations, drains, canaries, credential mounting, mesh policies, Langfuse projects, telemetry plumbing (ADR 0023) | M2 (Helm in M1) |
| golemctl | CLI for every persona, including `login` and coding-agent setup | M1 |
| golem-console | Small web UI for keys, caps, requests and approvals | M2 |
| Backstage plugin | Spend, budgets and keys on Backstage entities | M3 |

- **Two writers, one each**: declarative config is CRDs (GitOps), read by
  golem-control; entitlements and operational overrides change only through
  the control API (protobuf, Connect; ADR 0014, 0023). The CLI, console and
  plugin are API clients; the gateway reads only the distribution stream.
- **Store**: Aurora DSQL (multi-region, preferred) or PostgreSQL (AKS,
  on-prem), one implementation (ADR 0015).
- **Gateway probes**: `/livez`, `/readyz` (a snapshot loaded, a usable
  deployment per required pool, limit store reachable when required),
  `/healthz`, `/startupz`; snapshot dump and `explain` on the admin port,
  read-only.
- **Graceful drain** *(added)*: readiness fails, in-flight streams get a
  long grace, then end with a dialect error event; orchestrated by the
  operator on rollouts (operator.md §3).
- **Mesh**: ambient mode; the gateway serves plain HTTP behind ztunnel;
  the operator installs the gateway's waypoint (optional) and the
  AuthorizationPolicies (only the waypoint to the data port when identity
  headers are trusted; only gateways to the distribution API; admin ports
  to the operator and control plane).
- **Config**: five CRD kinds (`Backend`, `ModelRoute`, `ScopePolicy`,
  `Hook`, `PriceList`; ADR 0025) watched by golem-control, with status
  written back; incident levers as expiring overrides through the API.
  Provider secrets stay in Kubernetes Secrets, mounted into the gateways
  that need them.
- **Multiple instances and regions**: `GatewayPool`s per region or zone;
  in multi-region, a control plane per region over one DSQL cluster.
- Container-aware GOMAXPROCS (runtime default), `GOMEMLIMIT` from the
  limit, PDBs; fixed replicas sized for the design envelope (ADR 0026)
  *(added)*.

### 5.10 Security and compliance

- **FIPS 140-3 mode** via Go's native FIPS module (`GODEBUG=fips140=on`).
- **Audit log** of every entitlement change, approval, key event and
  authorization denial, to ClickHouse with retention by policy.
- **Data residency**: routes constrained to regions by scope policy;
  validation fails if a residency-constrained alias can reach a disallowed
  region.
- Provider secrets by reference (Kubernetes Secrets, or workload identity
  where the provider supports it), rotated without restart.
- Request size, header count and per-phase timeouts. Credentials never
  logged; keys shown once, stored hashed, logged by prefix.

### 5.11 Caching *(added, M3)*

Exact-match response cache for deterministic non-streaming requests, keyed
by scope, model and normalised body, in the Redis-protocol store.
Decision-model answers are good candidates (cheap to store, often
repeated). Semantic caching: Later, if at all.

### 5.12 GitHub Copilot *(added)*

ADR 0021. Users' Copilot clients (VS Code, JetBrains, Copilot CLI, the
Copilot app) can use golem as a BYOK "OpenAI-compatible" provider with
their personal golem key (M1): golem's models, budgets and capture apply,
attributed per user; code completions stay on GitHub. Enterprise custom
models pointing at golem, called from GitHub's side, follow a spike (M2).
Copilot's own usage is ingested from GitHub's usage metrics API into
ClickHouse as showback beside golem spend (M2). External callers
(Copilot's enterprise custom models, SaaS apps with configurable model
endpoints) use the internet-facing edge gateway pool (ADR 0022).

### 5.13 Agent protocols *(added, Later)*

Fronting MCP servers and A2A with the same principal, scope, budget and
telemetry machinery. Out of scope until the LLM path is done; routes stay
protocol-agnostic so it can be added.

## 6. Milestones

Phases, their contents and exit criteria: [roadmap.md](roadmap.md). In
short: Phase 0 spikes; M0 walking skeleton; M1 the first team in
production (single region, Helm, observe-then-enforce budgets); M2 the
enterprise default (projects, approvals, capture, the operator, console,
edge pool); M3 multi-region, Temporal and the long tail.

## 7. Success measures

- Proxy overhead p50 < 1 ms, p99 < 5 ms on the fast path at 1k req/s per
  core; TTFT overhead < 2 ms; limit check ≤ 1 store round trip.
- Memory per in-flight stream < 64 KiB on the fast path.
- 100% of requests attributed to a principal, a scope and its funding.
- Ledger within 1% of provider invoices; no budget exceeded by more than
  its in-flight reservations' estimation error.
- Key revocation effective on every instance within 5 s.
- A provider outage moves traffic to fallbacks within one ejection window.

## 8. Open questions

Resolved: the decision APIs (§5.1); translation scope (§5.1.2); Entra
(§5.3); Gloo Mesh ambient (§5.9); the telemetry stack (§5.8); store:
Aurora DSQL preferred, PostgreSQL elsewhere (ADR 0015); teams as drafted;
console then Backstage (tools.md); team-to-project grants need no
approval; member caps may oversubscribe; EKS first, AKS and on-prem later;
backends: OpenAI, Foundry, self-hosted vLLM on standalone machines now and
NVL boxes and cloud GPUs later (§5.2.1); content capture and retention
per scope with override control, classes 7, 30, 90, 365 days and 3, 5, 7
years (ADR 0018); notifications by email and Teams; limit store Valkey
(ADR 0005); regions us-east-2, then us-west-2, us-east-1 as witness; the
platform team owns the Entra registrations and grants consent (entra.md);
self-hosted rates through `PriceList`; Temporal later, behind a seam
(ADR 0020); Copilot by BYOK, then enterprise custom models (ADR 0021);
testing (testing.md).

To be decided (TBD):

1. **Waypoint identity header** (ADR 0003): the M1 spike.
2. **Never-capture data** and who may enable multi-year retention
   (drafted: platform admins, by scope policy).

Resolved this round: the enterprise is migrating to GitHub Enterprise
Managed Users (login mapping through SAML identities until then, ADR
0021); internet-facing access through an edge gateway pool for GitHub and
SaaS apps (ADR 0022); registration identity by workload identity, then
PKI certificates, then bootstrap tokens (§5.2.1).

New:

3. **SaaS consumers**: which SaaS apps first, and whether they support
   OAuth client credentials or only static keys.

Resolved in the sixth pass: SaaS consumers use static service keys for
now (ADR 0022); the size of the enterprise and its organisation set the
design envelope (§9.12, ADR 0026), which adds sub-teams to the org model.

## 9. Cross-cutting requirements

### 9.1 Enforcement modes

Per scope: `observe` (record what would be refused), `warn` (allow, add a
warning header and notify), `enforce`. Rollouts start in `observe` so a
mis-set budget cannot stop a team's work on day one.

### 9.2 Provider quota limits

Per deployment TPM/RPM limits shared by every gateway (in Valkey), so
golem stays inside each provider account's quota instead of discovering it
by 429s, and balancing knows the headroom.

### 9.3 Key format

`glm_` + 32 random bytes + a CRC32 checksum, base62, so secret scanners
can verify candidates offline; registered as a GitHub secret-scanning
custom pattern with push protection, and a leaked key reported by scanning
is revoked automatically.

### 9.4 SLOs and recovery

Gateway availability 99.95% (it serves from snapshots through
control-plane outages); control plane 99.9%; store RPO by DSQL PITR; the
ledger's ClickHouse backups; RTO for a region's control plane 30 min.

### 9.5 Error catalog

Every refusal has a stable golem code (`limit.pool_exhausted`,
`auth.key_revoked`, `route.no_deployment`, ...) rendered in the caller's
dialect, documented with what to do next.

### 9.6 Compatibility policy

The control API and the snapshot format are versioned; gateways accept
snapshots from n−1 control planes; `buf breaking` gates CI; CRDs move
v1alpha1 → v1beta1 before the enterprise default.

### 9.7 Price list upkeep

A vendored price list refreshed by a scheduled job that opens a PR (diffs
reviewed by the platform team), enterprise rates in `PriceList` CRDs.

### 9.8 Model lifecycle

Deprecation dates in the catalog; aliases pointing at a model within 30
days of retirement warn their scopes' managers.

### 9.9 Release engineering

Signed images (cosign) with SBOMs, a FIPS build variant, semantic versions
per component.

### 9.10 Threat model

Before M1 production: key theft and replay, SSRF through registration and
raw routes, the edge pool, insider access to captured content, the
operator's and control plane's RBAC.

### 9.11 Onboarding

A guide per SDK and agent, `golemctl agent setup`, and an `observe`-mode
period per team before enforcement.

### 9.12 Design envelope

Under 2,000 users (bi- or tri-modal: under 100 heavy, under 10 very heavy),
under 10 top-level teams with 3–10 sub-teams each, 4–10 projects per team.
golem is designed for ten times that (ADR 0026: about 10,000 concurrent
streams, 500 requests per second, 50,000 keys, a 20 MB snapshot), with
dashboards that alert as traffic approaches it. Within it: whole-snapshot
distribution, fixed gateway replicas, a single Valkey primary with a
replica. The heavy tail drives cost and local GPU demand, so per-principal
fairness on scarce pools and per-user concurrency limits are M1.
