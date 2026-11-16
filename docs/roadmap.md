# golem: roadmap

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. What is built: the code's
[orientation](walkthroughs/README.md) and the "Built" notes; release
evidence is tracked in golem#7.

**M1's scope of record is [m1-plan.md](m1-plan.md)**: what is built, what
remains, and what is not being built in M1. Where this page's Phase 2 lists
more, m1-plan.md wins; the rest moves to M2 or later.

Status: draft. Replaces the milestone table in the PRD. Phases are ordered
by dependency and by risk: what could invalidate the design is tried first,
what only adds features comes last. Each phase has an exit criterion that is
observed, not asserted.

## Phase 0: spikes (2–3 weeks, in parallel)

Each answers a question the design depends on; each ends with a short note
in `docs/spikes/` and, where the answer changes the design, an ADR update.

| Spike | Question | Depends on it |
|---|---|---|
| Bucket as the root of trust | S3 `If-Match` behaviour (412, retries, lost responses); KMS asymmetric signing and verification of snapshots and pointers; the bucket policy that denies all other writers; replication to the backup account under Object Lock | ADRs 0006, 0007 |
| Remote sites | Entra-to-AWS federation from AKS and IAM Roles Anywhere from on-prem to the bucket; round trip from each site to us-east-2 (recorded; a Valkey per site assumed) | ADR 0008, before the first remote site |
| Fast-path budget (done: [docs/spikes/fast-path.md](spikes/fast-path.md)) | jsontext scan and splice, SSE tee, a stub upstream: overhead and allocations per request | PRD §7 targets, ADR 0001 |
| Bedrock envelope | Claude Code against Claude on Bedrock through a stub envelope: event-stream framing to SSE, `anthropic_beta` in the body, prompt caching, token counting, inference-profile IDs as alias targets | ADR 0001 |
| vLLM on the Macs and Sparks | `/v1/messages`, `/metrics`, `--api-key` behind Caddy; the concurrency each box sustains (for its static ceiling) | ADR 0017 |
| Langfuse | organisation API (project creation, retention), batch trace deletion, per-project OTLP basic auth | ADR 0016 |
| Copilot BYOK | VS Code and Copilot CLI against a stub "OpenAI-compatible" endpoint: request shapes, headers, streaming | ADR 0020 |
| Valkey scripts | the admission script (known spend, concurrency slots) and rate buckets on ElastiCache for Valkey: latency, behaviour across a failover | ADR 0031 |
| SDK retry rules (SDKs done: [docs/spikes/sdk-compat.md](spikes/sdk-compat.md); agents open) | Which status codes each OpenAI and Anthropic SDK, Claude Code and Codex retry, and their session headers; confirms 402 for budgets | ADRs 0024, 0026 |
| Agent credentials | Claude Code's `apiKeyHelper` and Codex's equivalent returning an Entra access token, with the scope header; which agents need a minted key | ADR 0009 |

## Phase 1 (M0): walking skeleton (3–4 weeks)

The gateway end to end with nothing optional.

- Repository layout, the import test (architecture.md §4.2), CI with
  preflight, race, fuzz smoke, goldens.
- Gateway: snapshot from a file; OpenAI Chat and Anthropic Messages
  frontends; one OpenAI-compatible backend and Anthropic; fast path,
  streaming, usage extraction; probes; drain; OTLP to ClickStack.
- Proto skeletons for the control API and the snapshot format; the error
  codes, headers and ledger schema of contracts.md as code and DDL.
- The Crossplane composition (`GolemInstallation`, `GolemSite`) for the
  bucket, KMS keys, IAM roles, Valkey and Entra registrations, and
  `golemctl bootstrap` (moved to M2: M1 makes them by hand, and
  `golem-control bootstrap` exists for a missing document, control-plane.md §7).
- Benchmarks and the stub-upstream load test as baselines.
- Standalone mode (`-standalone`, ADR 0023) and `make dev-up` (kind,
  MinIO, Valkey, dex, ClickHouse, Langfuse) as the integration-test
  environment.

Exit: a real SDK and Claude Code stream through it, standalone and in
`make dev-up`; a trace with GenAI
attributes and token metrics is in HyperDX; the benchmark meets the
fast-path budget.

## Phase 2 (M1): first team in production (8–10 weeks)

The platform team's own apps and coding agents, us-east-2, EKS. What is
built, the order of the rest and each item's evidence: [m1-plan.md](m1-plan.md).

- **Gateway**: Chat, Responses (stateless), Messages, models list,
  `count_tokens`; Claude on Bedrock (the Messages envelope), OpenAI and
  vLLM boxes, all pass-through (ADR 0001); aliases, betas-aware routing
  (ADR 0028), failover in pool order with ejection, health checks and a
  retry budget (ADR 0051), static and per-principal ceilings.
- **Policy**: provider-side storage forbidden by default (ADR 0027);
  request feature modes; the threat model reviewed; CODEOWNERS on config.
- **Identity**: `glm_live_` keys, Entra users and services, `golemctl
  token` for agents with a credential helper, minted keys, the emergency
  denylist.
- **Org**: scopes, members (from bound Entra groups), managers, services,
  keys with model constraints; budget modes, starting in `observe`.
- **Limits**: scope budgets on the snapshot's known spend, concurrency
  slots and request rate limits in Valkey, cut-stream estimates (ADR 0031).
- **Control plane**: one golem-control replica (ADR 0053) on the S3 state
  document; config from a git checkout with offline validation (ADR 0054);
  signed snapshot publication; audit through the document's outbox.
- **Ledger**: ClickHouse `golem.spend` through the gateway pod's collector
  (deploy/otel, deploy/clickhouse/spend.sql); reconciliation as a query
  with a written record.
- **Install**: Helm charts through Argo CD; cloud resources by hand or
  Terraform, recorded.
- **Bypass controls** for the first team (ADR 0039) and **dedicated
  provider accounts** (ADR 0031).
- **Testing**: testing.md; the publication, exposure and attempts models,
  each with its connect test.

Exit: the platform team uses golem for all its model traffic for two
weeks; ledger within 3–5% of provider usage for those accounts (ADR 0031); outage
drills fail over without caller changes (a Bedrock region to another
region or inference profile; a local box to its fallback pool); budgets switched from
`observe` to `enforce` for the team.

## Phase 2b (M1.5): translation through the canonical model (4–6 weeks)

- `gateway/canon` and the Chat and Messages adapter pairs (requests,
  responses, streams) (ADR 0001, canonical-model.md), cache-stable
  encoders (ADRs 0028, 0037).
- Explicit conversion routes only (ADR 0033): OpenAI-SDK applications
  routed to Claude on Bedrock (Chat decoder, Messages encoder, the M1
  `bedrock` envelope), and Messages routes pointed at Chat-only models.
  No implicit cross-dialect failover; coding-agent routes never convert.
- golem-placed prompt-cache breakpoints for Chat callers on Claude.
- Testing: adapter round-trip properties, stream-chunking properties,
  decoder fuzzing, goldens from recorded traffic, and a nightly differential
  run against Bedrock and OpenAI with real SDKs.

Exit: the platform team's OpenAI-SDK applications run against Claude on
Bedrock through an explicit route, with tool use and streaming, for a week
without a translation defect.

## Phase 3 (M2): enterprise default (10–12 weeks)

Moved here from M1 (m1-plan.md, *Not building in M1*) and from the control
plane's design, one line each:

- **Config objects in the cluster** and the CRD watch, with 0041 §7.2's
  consistent LIST and status on the objects (ADR 0005), if a second
  golem-control replica or team needs it (ADR 0053, 0054).
- **Operational overrides**: expiring disable, weight, freeze and fallback
  levers through the API (ADR 0005).
- **Grants, team pools, cost centers and adjustment requests** (ADR 0011,
  0034), with `BudgetService`.
- **Member and key caps** (ADR 0034), with known spend per member and key.
- **`SpendService`**: spend queries filtered by the caller's rights.
- **Threshold notifications** (50/80/100%) by email (Graph `sendMail`) and
  Teams webhooks, with idempotency keys in the state.
- **Expiry jobs and reminders** for keys, adjustment requests and
  registrations.
- **Registrations** of model server endpoints, with a TTL.
- **A reconciliation job** writing `adjustment` rows (M1's is a query).
- **Per-key and token rate limits, $/min limits, `x-ratelimit-*` headers**;
  provider quota shares per site.
- **Pool selectors**: a snapshot per pool by site and audience; gateway
  status read back (`Applied n/m`, `golemctl gateways`).
- **Argo Rollouts canaries, Crossplane claims, dashboards as code**
  (ADR 0040).
- **The secret-scanning webhook** revoking leaked keys; `golemctl agent
  setup`; restore over the API (`golemctl state restore`); `golemctl
  explain`.
- **Weighted and least-in-flight balancing**, session affinity, in-process
  hooks (ADR 0014).

The rest of M2:

- **Org**: projects and sub-teams, monthly caps per scope set by admins or
  parent managers, cost-center mapping for chargeback, threshold
  notifications by email and Teams (ADR 0034); grants and approvals only
  when a budget manager asks.
- **Capture**: settings and down-only overrides; a Langfuse project per
  top-level team, inherited or overridden by sub-teams and projects;
  retention per project; credentials in the collector (ADR 0035).
- **Console**; spend APIs; Copilot usage ingestion as showback.
- **Local GPU routing**: adaptive concurrency, metrics polling, prefix
  affinity, round-robin queueing across principals.
- **Gateway**: more envelopes (the direct Anthropic API, Microsoft Foundry for
  Azure OpenAI and Claude) and the Converse adapter pair; Decisions and
  System One; legacy Completions; embeddings to Bedrock; context-window fallback; latency EWMA and quota-headroom
  balancing; shadowing; conditional routing; remote hooks and built-in
  guardrails (`Chunk` phase if a guardrail needs it); credential
  pass-through `forward`; raw pass-through routes.
- **Edge pool** for Copilot enterprise custom models and the first SaaS
  consumers.
- **Registration** of model server endpoints (for cloud GPUs and NVL boxes).
- **Remote sites** (AKS, on-prem) as gateway pools on the shared S3 store
  (ADR 0008): a Valkey per site, budgets by known spend in snapshots
  (ADR 0031), static provider quota shares, Bedrock through federation,
  the same charts through GitOps; after the residency review (PRD §8).
- **Bypass controls** for every onboarded team (ADR 0039).
- **Testing**: the state and distribution model; the state document under races in simulation;
  metastability analysis of overflow and retries (gotogether trials 2–4, 6).

Exit: onboarding open to every team; direct provider keys revoked for teams
onboarded; budgets enforced for all of them.

## Phase 4 (M3): resilience and reach

- Second region: us-west-2 by replication of the bucket, gateways reading
  the replica, a golem-control there scaled to zero, the failover runbook
  and a drill (ADR 0008).
- Temporal for approvals, key rotation and batch jobs, when the platform's
  Temporal is ready (ADR 0019).
- Batch and Files, rerank, Decisions↔System One translation, cost-based
  routing, priority classes, budget downgrade, chargeback export, response
  cache, Backstage plugin, Entra OBO pass-through.

## Later

MCP and A2A fronting; semantic cache; audio, images, Realtime; Vertex.

## Requirements added by this review

Enforcement modes, provider quota limits, the key format and leak
response, SLOs and recovery, the error catalog, the compatibility policy,
price list upkeep, model lifecycle, release engineering, the threat model,
onboarding, watching golem and stored-format upgrades: each was implicit or missing, and is now a requirement in
PRD §9, scheduled in the phases above.

## Sizing

Settled by the design envelope (PRD §9.12, ADR 0021): three gateways per
site, one golem-control replica in M1 (ADR 0053), one Valkey primary with
a replica per site, the state document at about 1–2 MB, and ClickHouse far
inside its limits. Per-principal limits on local GPU pools are M1, with
those pools.

## Review amendments (ADR 0029)

Phase 0 must validate publication fencing/trusted checkpoints,
duplicate-safe ledger queries, mesh header overwrite,
and Langfuse ingestion permissions. M0 produces the contracts and DDL. M1
requires the M1 acceptance matrix in security-and-correctness.md, including
immutable audit archival and authenticated standalone mode. M2
features require their respective gates before activation. Budgets are soft
and eventually consistent by accepted risk (ADR 0031): no durable journal or
site allocations. Stored provider objects
are deferred pending ownership/routing records. Staffing and these spikes
must be measured before the milestone durations are treated as commitments.

## Second security review amendments (ADR 0041)

M0 delivery renders and negative authorization tests cover platform trust
boundaries. M1 gates: incident denylist survival, audit backpressure with
restrictive capacity, request feature modes, composite state/CRD publication
freshness (for M1's git checkout, ADR 0054) and expanded restrictive-right
restore tests. Budget bounds are stated, not proven (ADR 0031). M2 capture gates prove dedicated pipeline destination restrictions
and project reader synchronization within the accepted pool compromise boundary.
See [ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) for acceptance and assumptions.
