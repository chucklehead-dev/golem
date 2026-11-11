# golem: roadmap

Status: draft. Replaces the milestone table in the PRD. Phases are ordered
by dependency and by risk: what could invalidate the design is tried first,
what only adds features comes last. Each phase has an exit criterion that is
observed, not asserted.

## Phase 0: spikes (2–3 weeks, in parallel)

Each answers a question the design depends on; each ends with a short note
in `docs/spikes/` and, where the answer changes the design, an ADR update.

| Spike | Question | Depends on it |
|---|---|---|
| Waypoint identity | Which header carries the caller's SPIFFE ID through a Gloo Mesh ambient waypoint, and can the gateway's port be limited to the waypoint? | ADR 0003 mesh identity (M2) |
| DSQL from Go | pgx with IAM tokens, `InTx` retries on 40001, one-DDL migrations, `CREATE INDEX ASYNC`; a list-append workload through `qhistory` to confirm the isolation DSQL gives | ADR 0015, the store |
| Fast-path budget | jsontext scan and splice, SSE tee, a stub upstream: overhead and allocations per request | PRD §7 targets, ADR 0001 |
| Provider envelopes | Claude on Foundry (Entra scope, `/anthropic/v1/messages`), Anthropic on Bedrock (event-stream framing), Chat→Converse fidelity on tool calls | §5.1.2 translation scope |
| vLLM on the Macs and Sparks | `/v1/messages`, `/metrics`, `--api-key` behind Caddy; adaptive concurrency against one box | ADR 0019 |
| Langfuse | organisation API (project creation, retention), batch trace deletion, per-project OTLP basic auth | ADR 0018 |
| Copilot BYOK | VS Code and Copilot CLI against a stub "OpenAI-compatible" endpoint: request shapes, headers, streaming | ADR 0021 |
| Valkey scripts | reserve/reconcile scripts on ElastiCache for Valkey and Dragonfly: latency, failover behaviour | ADR 0005 |

## Phase 1 (M0): walking skeleton (3–4 weeks)

The gateway end to end with nothing optional.

- Repository layout, the import test (architecture.md §4.2), CI with
  preflight, race, fuzz smoke, goldens.
- Gateway: snapshot from a file; OpenAI Chat and Anthropic Messages
  frontends; one OpenAI-compatible backend and Anthropic; fast path,
  streaming, usage extraction; probes; drain; OTLP to ClickStack.
- Proto skeletons for the control API and distribution.
- Benchmarks and the stub-upstream load test as baselines.

Exit: a real SDK and Claude Code stream through it; a trace with GenAI
attributes and token metrics is in HyperDX; the benchmark meets the
fast-path budget.

## Phase 2 (M1): first team in production (8–10 weeks)

The platform team's own apps and coding agents, us-east-2, EKS.

- **Gateway**: Chat, Responses, Messages, Embeddings, models list; backends
  OpenAI, Anthropic, Foundry (Azure OpenAI and Claude), Bedrock (Anthropic
  envelope, Chat→Converse), standalone vLLM (static endpoints, discovered
  models, adaptive concurrency, prefix affinity, per-principal fair share,
  overflow); Chat→Anthropic
  translation; aliases, weighted and least-in-flight balancing, failover,
  retries with a retry budget, ejection, prompt-cache affinity; provider
  quota limits per deployment (below); in-process hooks (`Request`,
  `Response`, `Complete`).
- **Identity**: virtual keys (format below), Entra users and services,
  `golemctl login`, `golemctl agent setup` for Claude Code, Codex and
  Copilot BYOK.
- **Org**: scopes (teams), members, managers, services, keys with
  constraints, caps, team pools granted by admins; enforcement modes
  (below), starting in `observe`.
- **Limits**: Valkey, reserve and reconcile, rate and concurrency limits.
- **Control plane**: golem-control (api and worker roles) on DSQL, CRD
  config source, operational overrides, distribution, explain, audit.
- **Ledger and dashboards**: ClickHouse `golem.spend` by materialized view;
  Grafana dashboards for spend, limits, latency, TTFT.
- **Install**: Helm (no operator yet).
- **Testing**: the regime of testing.md for these parts; Quint models of
  reserve/reconcile and the attempt loop with their mutants; the limiter in
  simulation (gotogether trial 1, testing.md §3.2).

Exit: the platform team uses golem for all its model traffic for two
weeks; ledger within 1% of provider invoices for that period; a provider
outage drill fails over without caller changes; budgets switched from
`observe` to `enforce` for the team.

## Phase 3 (M2): enterprise default (10–12 weeks)

- **Org**: projects, cost centers, budget managers, grants, adjustment
  requests and approvals (durable job table), notifications by email and
  Teams.
- **Capture**: settings and overrides, Langfuse projects per scope,
  retention deletion.
- **Operator**: `ControlPlane`, `GatewayPool`, ordered upgrades with
  migrations, drains, canaries, credential mounting, mesh policies.
- **Console**; spend APIs; Copilot usage ingestion as showback.
- **Gateway**: Decisions and System One; legacy Completions; embeddings to
  Bedrock; context-window fallback; latency EWMA and quota-headroom
  balancing; shadowing; conditional routing; remote hooks and built-in
  guardrails (`Chunk` phase if a guardrail needs it); credential
  pass-through `forward`; raw pass-through routes.
- **Mesh identity** through the waypoint, if the spike succeeded.
- **Edge pool** for Copilot enterprise custom models and the first SaaS
  consumers.
- **Registration** of model server endpoints (for cloud GPUs and NVL boxes).
- **Testing**: distribution feed model; DSQL semantics in simulation;
  metastability analysis of overflow and retries (gotogether trials 2–4, 6).

Exit: onboarding open to every team; direct provider keys revoked for teams
onboarded; budgets enforced for all of them.

## Phase 4 (M3): resilience and reach

- Multi-region: us-west-2, DSQL peered clusters (us-east-1 witness), the
  store migration, regional leases.
- Temporal for approvals, key rotation and batch jobs, when the platform's
  Temporal is ready (ADR 0020).
- Batch and Files, rerank, Decisions↔System One translation, cost-based
  routing, priority classes, budget downgrade, chargeback export, response
  cache, Backstage plugin, Bedrock from AKS/on-prem, Entra OBO pass-through.

## Later

MCP and A2A fronting; semantic cache; audio, images, Realtime; Vertex.

## Requirements added by this review

Enforcement modes, provider quota limits, the key format and leak
response, SLOs and recovery, the error catalog, the compatibility policy,
price list upkeep, model lifecycle, release engineering, the threat model
and onboarding: each was implicit or missing, and is now a requirement in
PRD §9, scheduled in the phases above.

## Sizing

Settled by the design envelope (PRD §9.12, ADR 0026): three gateways per
region, two api and one worker replicas of golem-control, one Valkey
primary with a replica, and ClickHouse and DSQL far inside their limits.
Per-principal fair share on local GPU pools moves into M1 with those
pools.
