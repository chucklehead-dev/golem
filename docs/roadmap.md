# golem: roadmap

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Implementation remains outstanding.

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
| Valkey scripts | the check script (known spend, concurrency slot, rate, quota) on ElastiCache for Valkey: latency, behaviour across a failover | ADRs 0012, 0031 |
| SDK retry rules | Which status codes each OpenAI and Anthropic SDK, Claude Code and Codex retry, and their session headers; confirms 402 for budgets | ADRs 0024, 0026 |
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
  `golemctl bootstrap` (control-plane.md §5).
- Benchmarks and the stub-upstream load test as baselines.
- Standalone mode (`--standalone`, ADR 0023) and `make dev-up` (kind,
  MinIO, Valkey, dex, ClickHouse, Langfuse) as the integration-test
  environment.

Exit: a real SDK and Claude Code stream through it, standalone and in
`make dev-up`; a trace with GenAI
attributes and token metrics is in HyperDX; the benchmark meets the
fast-path budget.

## Phase 2 (M1): first team in production (8–10 weeks)

The platform team's own apps and coding agents, us-east-2, EKS.

- **Gateway**: Chat, Responses, Messages, Embeddings, models list; three
  backends with no translation (ADR 0001): Claude on Bedrock (the Messages
  envelope), OpenAI (pass-through), standalone vLLM (pass-through for
  Chat, Completions, Embeddings and Messages; static endpoints, discovered
  models, static concurrency ceilings, per-principal limits, overflow);
  aliases, capability-aware routing (ADR 0028), weighted and least-in-flight
  balancing, failover,
  retries with a retry budget, ejection, prompt-cache affinity; provider
  quota limits per deployment (below); in-process hooks (`Request`,
  `Response`, `Complete`).
- **Policy**: provider-side storage forbidden by default (ADR 0027); the
  threat model reviewed; the model onboarding checklist; CODEOWNERS on
  config.
- **Identity**: virtual keys (`glm_live_`, checksummed, secret-scanning webhook
  revocation), Entra users and services, `golemctl token` for agents with
  a credential helper, minted keys for the rest, `golemctl agent setup` for
  Claude Code, Codex and Copilot BYOK, the emergency denylist.
- **Org**: scopes (teams), members, managers, services, keys with
  constraints, caps, team pools granted by admins; enforcement modes
  (below), starting in `observe`.
- **Limits**: Valkey; soft budgets on known spend with the optional strict
  worst-case check, concurrency slots, rate limits, cut-stream estimates
  (ADR 0031).
- **Control plane**: golem-control (one Deployment, a leader) on the S3
  state document; CRD config with schema rules and offline validation;
  operational overrides; signed snapshot publication; explain; audit
  through the document's outbox.
- **Ledger and dashboards**: ClickHouse `golem.spend` by materialized view,
  with the contract's schema, TTLs and the full price model;
  Grafana dashboards for spend, limits, latency, TTFT; snapshot-age alerts,
  a synthetic request per pool, provider usage reconciliation.
- **Install**: Helm charts through Argo CD, Argo Rollouts for gateway canaries, Crossplane claims for cloud resources (ADRs 0036, 0040).
- **Bypass controls** for the first team: the Bedrock SCP, egress blocks, provider key-creation limits, managed agent settings (ADR 0039).
- **Dedicated provider accounts or projects** for golem (ADR 0038).
- **Testing**: the regime of testing.md for these parts; Quint models of
  accounting (overshoot bound, dedup, reconciliation) and the attempt loop with their mutants; the limiter in
  simulation (gotogether trial 1, testing.md §3.2).

Exit: the platform team uses golem for all its model traffic for two
weeks; ledger within 3–5% of provider usage for those accounts (ADR 0038); outage
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
site, two replicas of golem-control (one leader), one Valkey primary with
a replica per site, the state document at about 1–2 MB, and ClickHouse far
inside its limits. Per-principal limits on local GPU pools are M1, with
those pools.

## Review amendments (ADR 0029)

Phase 0 must validate publication fencing/trusted checkpoints,
duplicate-safe ledger queries, mesh header overwrite,
and Langfuse ingestion permissions. M0 produces the contracts and DDL. M1
requires the M1 acceptance matrix in security-and-correctness.md, including
CRD RBAC, immutable audit archival and authenticated standalone mode. M2
features require their respective gates before activation. Budgets are soft
and eventually consistent by accepted risk (ADR 0031): no durable journal or
site allocations. Stored provider objects
are deferred pending ownership/routing records. Staffing and these spikes
must be measured before the milestone durations are treated as commitments.

## Second security review amendments (ADR 0041)

M0 delivery renders and negative authorization tests cover platform trust
boundaries. M1 gates: incident denylist survival, audit backpressure with
restrictive capacity, request feature modes, composite state/CRD publication
freshness and expanded restrictive-right restore tests. Budget bounds remain
conditional pending counter-handoff, aggregate exposure and failed-renewal
validation. M2 capture gates prove dedicated pipeline destination restrictions
and project reader synchronization within the accepted pool compromise boundary.
See [ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) for acceptance and assumptions.
