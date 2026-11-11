# golem: product requirements

Status: draft for discussion. Revised 2026-11-11 (second pass).

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
   team and project managers allocate their budgets, budget managers fund
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

## 3. Non-goals

- Multi-tenant SaaS: one enterprise, one Entra tenant (guest users aside),
  no signup or invoicing.
- A large management UI. Grafana and HyperDX are where people look; the
  self-service API and CLI are where they act. A small self-service web UI
  (keys, allocations, approvals) is M2 (§8 Q4).
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
| Coding-agent user | Their agent's native API (Anthropic Messages, OpenAI Responses) with their Entra identity via `golem login`; streaming without stalls. |
| Team / project manager | Membership, services, allocations to members, adjustment requests, spend by member. |
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
| OpenAI | API key | M1 |
| Anthropic | API key | M1 |
| AWS Bedrock (InvokeModel, Converse, embeddings) | SigV4 from Pod Identity/IRSA, or from Entra via AWS IAM OIDC federation (`AssumeRoleWithWebIdentity`) off-AWS *(added)*; Bedrock API keys | M1 |
| Azure OpenAI / Azure AI Foundry | Entra workload identity *(added)*, API key | M1 |
| OpenAI Decisions | API key | M1 |
| TypeSafe System One | API key | M1 |
| Any OpenAI-compatible (vLLM, SGLang, Ollama, KServe) | none, key, mesh mTLS | M1 |
| Google Vertex AI | workload identity federation | M3, if needed |

Each backend has its own timeouts (connect, first byte, idle between
chunks, total), connection pool, and egress path (direct or the mesh's
egress gateway) *(added)*.

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
| `golem login` for coding agents *(added)* | Entra device-code or browser flow; mints a short-lived personal key in the chosen scope, written where the agent reads it | M1 |
| Mesh identity | In ambient mode the app does not see the peer certificate (ztunnel terminates mTLS). golem's waypoint forwards the caller's SPIFFE ID in a header golem trusts only from the waypoint; used to bind keys to workloads and to identify mesh services without keys | M2 (to confirm, §8 Q2) |
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

- Users (Entra), teams (optionally bound to Entra groups, synced from
  Graph), projects (one primary team plus member teams, services, users),
  services (owned by a team or project), cost centers (external).
- Roles: member, team manager, project manager, budget manager (per cost
  center), service owner, admin.
- Users mint personal keys within their own rights; keys are further
  constrainable (models, spend cap, rates, concurrency, expiry, source
  binding). Service owners mint service keys.
- Budgets per month: budget managers grant from cost centers to teams and
  projects; team managers grant team money to projects; managers allocate
  to members. Recurring ("permanent") and one-time grants and
  adjustments; those drawing on a cost center need a budget manager's
  approval.
- Enforcement per request at three levels: key cap, member allocation,
  scope pool.

| Capability | Milestone |
|---|---|
| Users, teams (Entra-group bound), services, personal and service keys, scope policies, member allocations, team pools granted by admins | M1 |
| Projects, cost centers, budget managers, grants, approvals, adjustments, audit | M2 |
| Self-service web UI; notifications (email/Teams) on thresholds and approvals | M2 |
| Budget downgrade to a cheaper alias; chargeback export per cost center | M3 |

### 5.5 Limits

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

Six phases: `OnRequest`, `OnRoute`, `OnUpstreamRequest`, `OnResponseChunk`,
`OnResponse`, `OnComplete` (ADR 0004). In-process (Go, M1) or remote
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
| Langfuse | GenAI spans (and content where the scope's policy allows), with `langfuse.user.id`, `langfuse.session.id`, tags, scope and project metadata, and golem's own cost | Sessions, prompt/completion review, evaluation, shadow comparisons |
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
- **Content capture** off by default; per scope policy, sampled, redacted
  by hooks first, sent only to Langfuse (not to ClickStack).
- **Request metadata headers** *(added)*: `x-golem-session`,
  `x-golem-tags`, `x-golem-end-user` (or OpenAI's `user`, Anthropic's
  `metadata.user_id`); coding-agent sessions grouped automatically where
  the agent sends a session id.
- **Spend queries**: the self-service API answers "my spend", "my team's",
  "this cost center's" from ClickHouse, with the same numbers Grafana shows.
- **Debug** *(added)*: runtime/trace flight-recorder snapshots of slow
  requests; pprof including the goroutine-leak profile on the admin port.

### 5.9 Kubernetes and the mesh

- Two deployments: **golem** (data plane) and **golem-control** (control
  plane: self-service/admin API, Entra group sync, approvals; ADR 0009).
- Ports: data, admin (probes, pprof, config dump). In ambient mode golem
  serves plain HTTP; ztunnel provides mTLS. A waypoint for golem's service
  is optional (M2), for L7 authorization and the caller identity header.
- `/livez`, `/readyz` (config and entitlements snapshot loaded, a usable
  deployment per required pool, limit store reachable when required),
  `/healthz`, `/startupz`.
- **Graceful drain** *(added)*: readiness fails, new requests stop,
  in-flight streams get a long grace, then end with a dialect error event.
- **Backend health probes** *(added, M2)*.
- Container-aware GOMAXPROCS (runtime default), `GOMEMLIMIT` from the
  limit, PDB and HPA on in-flight streams in the Helm chart *(added)*.
- **Operator config** (backends, model routes and aliases, scope policies'
  model sets, limit policies, hooks) by CRDs or one YAML file, compiled into
  an immutable snapshot (ADR 0002). **Entitlements** (users, teams,
  projects, services, keys, budgets) are not CRDs: they are data managed
  through golem-control (ADR 0009).
- Multiple instances (e.g. per region) select their CRDs by label and share
  golem-control and the limit store, or run their own.

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

### 5.12 Agent protocols *(added, Later)*

Fronting MCP servers and A2A with the same principal, scope, budget and
telemetry machinery. Out of scope until the LLM path is done; routes stay
protocol-agnostic so it can be added.

## 6. Milestones

| Milestone | Contents | Exit criterion |
|---|---|---|
| M0 | Skeleton: config snapshot, file source, one OpenAI-compatible backend, fast-path proxy, probes, OTLP to ClickStack | Streams a chat completion with a trace and token metrics in HyperDX |
| M1 | §5.1/§5.2 M1 dialects and backends incl. Bedrock and the decision APIs; Chat→Anthropic and Chat→Converse translation; Entra users and services; personal and service keys; `golem login`; teams, scopes, member allocations; aliases, balancing, failover, ejection, cache affinity; limits (Dragonfly/Valkey if present); in-process hooks; CRD source; ClickHouse ledger; Langfuse export; Grafana dashboards; drain; Helm | One team's apps and coding agents in production through it |
| M2 | Projects, cost centers, grants, approvals, adjustments, audit; self-service UI and notifications; mesh identity via waypoint; shadowing; conditional routing; remote and built-in hooks; embeddings to Bedrock; context-window fallback; Entra OBO pass-through | Default path for the enterprise; direct provider keys revoked |
| M3 | Batch/Files, rerank, Decisions↔System One translation, cost-based routing, priority classes, budget downgrade, chargeback export, response cache, Vertex | By demand |

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

Resolved in the second pass: "decisions" is OpenAI's Decisions API and
"systemone" TypeSafe's System One (§5.1); translation is what Bedrock needs
plus Chat to all (§5.1.2); Entra for end users (§5.3); Gloo Mesh ambient
(§5.9); ClickHouse, ClickStack, Langfuse, Grafana available (§5.8).

1. **Where entitlements live.** "Virtual keys can be external": read here as
   "golem does not need Kubernetes Secrets for keys; an external store is
   fine". ADR 0009 proposes PostgreSQL (CloudNativePG or managed) for
   golem-control's transactional data, because ClickHouse is not suited to
   approvals and key revocation. Is a Postgres acceptable? If you instead
   meant keys issued by an *external system* (e.g. an existing API-key
   service), golem's `KeyStore` would call it, and we need its API.
2. **Mesh identity in ambient mode.** Confirm whether Gloo Mesh's waypoint
   in your version can forward the caller's SPIFFE identity (XFCC or a
   custom header) to golem; otherwise services authenticate with Entra
   tokens or keys only, and the mesh contributes L4 authorization alone.
3. **Team ↔ Entra groups.** Should teams be strictly Entra groups (golem
   reads membership, cannot edit it), or golem-managed with optional group
   binding (as drafted)? golem-control needs Graph `GroupMember.Read.All`
   either way for groups-claim overage.
4. **Self-service UI.** CLI and API in M1, a small server-rendered UI in M2
   (as drafted), or is the API enough with a Backstage plugin or similar?
5. **Team-to-project grants.** Drafted as needing no approval (the team's
   money was already approved); confirm, or require a budget manager for
   those too.
6. **Oversubscription** of member allocations, default on as drafted?
7. **Limit store.** Dragonfly, Valkey or Redis: any preference? Dragonfly
   needs all script keys declared (golem's scripts do) and runs fine
   single-node; Redis Cluster would need hash-tagged keys per scope.
8. **AWS identity.** Does golem run on EKS (Pod Identity for Bedrock) or in
   Azure/on-prem (Entra → AWS IAM OIDC federation)?
9. **Content capture policy**: who may enable it per scope, Langfuse
   retention, and whether Langfuse is the only place content may go.
