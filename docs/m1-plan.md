# M1 plan: first team in production

The working plan for roadmap.md's Phase 2 (M1). Each step names what is
left and the evidence that it is done. Evidence is a test, a model check or
a drill with a written record in docs/acceptance/, never a claim. A test
that shows an invariant is ablated (run with the fix removed, and seen to
fail), as m1-security-gates.md records. Update a step in place when it
lands; dated progress goes in the walkthroughs and the commit log.

## The exit, made measurable

roadmap.md's four exit criteria, each with the measure that decides it.
They are recorded in docs/acceptance/m1-exit.md.

1. **All the team's model traffic through golem for two weeks.** The
   measure:
   - A baseline of the team's provider usage before golem: a month of AWS
     CUR and provider usage by account.
   - Then zero usage on the team's direct provider accounts and roles
     outside golem's dedicated ones (ADR 0038).
   - Bypass attempts (denied Bedrock calls, blocked egress) logged and
     dashboarded (ADR 0039).
2. **Ledger within 3–5% of provider usage.**
   - **What is compared:** cost (and tokens by kind: input, output, cache
     read, cache write 5m/1h) per account × model × UTC day, from
     `golem.spend_dedup`, excluding `adjustment` rows, against CUR (a day
     or two late) and the OpenAI usage API.
   - **Time alignment:** rows are bucketed by `started_at` in UTC for the
     comparison, whatever `-period-zone` is; a stream crossing midnight is
     counted on its start day both sides, or the difference is reported as
     the boundary's share.
   - **Rule:** the whole two weeks within 5%, and every day within 10%.
     Days are previews; ADR 0038's monthly reconciliation is the record.
   - **Estimated and uncertain rows** are reported as their own share. They
     are charged high on purpose (contracts.md), so the ledger may run high
     by that share and still be right. Their share is the reconciliation's
     explanation, not a tolerance to widen.
3. **Outage drills fail over without caller changes:**
   - a Bedrock region to another region or inference profile;
   - a local box to its fallback pool.

   Each is run under synthetic load at the team's peak rate. It passes with
   no caller-visible failures beyond those already in flight (cut streams
   are allowed, and counted), TTFT p95 added under 2 s, and every attempt
   on the ledger.
4. **Budgets from observe to enforce.**
   - **Owner:** the platform team's lead signs off.
   - **Condition:** two weeks of observe with every `over_budget` row
     explained, and budgets sized from that history.
   - **Rollback:** a ScopePolicy PR back to `observe`, live at the next
     publication. It is drilled once before the switch, and the time it
     takes is recorded.

Production also needs the release gate lifted: pool mode refuses to run
without `-accept-unreleased` until golem#7's evidence is in (`releaseGate`,
cmd/golem-gateway; m1-security-gates.md). Step 5 schedules it.

## Built

| Area | What | Evidence |
|---|---|---|
| Frontends | Anthropic Messages and OpenAI Chat, streaming, usage extraction, feature modes, request boundary | `gateway/proxy` `TestFeatureModes`, `TestRequestBoundary`; m1-security-gates.md |
| Backends | Anthropic and OpenAI-compatible upstreams, pass-through, one attempt per call | `gateway/proxy` tests |
| Policy | Provider-side storage refused by default (ADR 0027) | `TestStoreOff` |
| Identity | `glm_live_` keys, Entra users and services, `golemctl login` and `token` (credential helper), minted keys, the emergency denylist; revoked or denied callers' streams ended (`proxy.InFlight`) | `gateway/auth` tests; `gateway/proxy` terminate_test.go, `TestSweepSparesANewerGrant` |
| Org | Scopes, members, managers, services, keys with model constraints, Entra group sync | `control/state`, `control/entrasync` tests |
| Limits | Valkey and Mem stores, soft budgets on known spend, declared worst case, concurrency slots, cut-stream estimates, Valkey outage start, TLS/auth | `gateway/limit` (store differential, Hegel props); model/accounting, model/exposure, `TestExposureModel` |
| Budget modes | ScopePolicy CRD: observe, warn, enforce; a live scope's policy cannot be deleted | `TestRemovedPolicyRefused`; ADR 0050 |
| Control plane | golem-control on the bucket document, lease and fence, signed publication with checkpoints, restore, bounded audit outbox, API drain, last-good config, `golemctl config validate` (offline, one compiler) | `control/*` tests; model/leadership, model/publication (`TestPublicationModel`), model/freshness |
| Ledger | `golem.spend` and `spend_dedup` by materialized view, the price model, the ingestion-lag metric (its alert a rule in operations.md, not yet a rule file), `golem.error.origin` | ClickHouse integration tests; `TestLedgerQueue*` |
| Release | The release gate | `TestReleaseGate` |
| Testing | Five Quint models with exhaustive small-instance checks and mutants; Hegel props in CI | `model/check-*.sh`; CI |

## Left, in order

Production must start by about week 6 of 8–10, so that two weeks of
observe and reconciliation fit inside the milestone. The order puts first
what could change the design (steps 1–2), then what the team's traffic
needs (3–4), then production in observe (5) and the drills and operations
around it (6), then enforce (7). Each step names the spike that gates it.

### 1. Upstream envelopes and the commit point; reconciliation spike

Today the dialect is the frontend only: `rewrite` splices `model` into the
body, and the provider's status goes to the caller once its headers arrive
(gateway/proxy). Both have to change before Bedrock or retries.

- **Envelope layer** (ADR 0001, gateway.md `gateway/envelope`): a backend
  has a dialect and an envelope. `plain` is today's behaviour. `bedrock`
  wraps Messages: `model` and `stream` taken from the body, the model in
  the path, the beta header moved into the body as `anthropic_beta`, and the
  response reframed. Failover is across pools of one dialect (ADR 0033),
  so Bedrock to the direct Anthropic API stays allowed.
- **Commit point**: an attempt is committed when its first valid event is
  written to the caller, not when its headers arrive. A provider that
  answers 200 and then sends a throttling or overloaded event first is an
  attempt that failed before commit. The envelope tells the loop which it
  was. The first event is buffered up to a bound (it is small); nothing
  else is.
- **Reconciliation spike**: a recorded day of CUR and OpenAI usage for a
  test account, compared with that account's ledger by exit 2's rule.
  Access to CUR and the usage APIs is arranged here, not at the end.

Status:
- **Commit point: built.** `gateway/proxy/commit.go`;
  `TestFailedBeforeCommit` (and the OpenAI case) refuses a 200 whose first
  event is an error as 503 with the provider's Retry-After, uncharged,
  its slot freed; `TestCommitAtFirstEvent` relays every other stream byte
  for byte, one byte a read included. Ablated: committed at headers, both
  refusal tests fail. Allocations per call unchanged (`BenchmarkHandler`).
- **Envelope seam: with its first envelope, in step 3.** Its contract is
  fixed here: an envelope delivers its dialect's framing, a failure before
  output as the dialect's error event, which the commit point reads. A
  seam with only `plain` behind it would be untested abstraction.
- **Reconciliation spike: open,** blocked on access to CUR and the OpenAI
  usage API for a test account.

Done when (open): the spike's note is in docs/spikes/reconciliation.md,
with the day's difference and its causes.

### 2. The attempt loop, on the accounting model

The design risk. Today a call is admitted once, holds one slot and ends
once (`admission.Ticket`). Its ledger row's `attempt_id` and `event_id` are
the request ID (gateway/telemetry/ledger.go). Retries change the accounting,
so the model comes first and extends model/accounting and model/exposure
rather than sitting beside them.

- **Model** (model/attempts.md, on the accounting model):
  - Attempts are made before commit, within a **per-request** retry budget
    (ADR 0002), with an aggregate cap per pool so retries cannot storm.
  - Admission: the budget at the first attempt, as today; each attempt
    checks its deployment's provider quota share and rate counters, and
    takes its own charge and event ID. One slot is held per call.
  - An uncertain attempt (sent, no answer) is not retried: its retry would
    double bill (ADR 0029: commitment is not idempotency). It is charged
    the worst case and holds its tail as today.
  - The overshoot bound is restated for k attempts. Because uncertain
    attempts are not retried, at most one attempt per call is uncertain,
    which keeps ADR 0031's L×W.
  - Ejection: a deployment out after consecutive failures, or for its
    Retry-After, with probation, per gateway. It is kept across snapshot
    swaps by deployment identity.
  - Invariants: at most one attempt commits; every attempt is charged once
    under its own event ID; the overshoot bound; no traffic to an ejected
    deployment except its probe.
  - Mutants: a retry after commit, a retried uncertain attempt, an
    uncharged failed attempt, one event ID shared by attempts, no budget.
- **Code**:
  - Per-attempt ledger rows and admission, and quota shares and rate limits
    per deployment in the Valkey check script (gated by the Valkey scripts
    spike).
  - The body: kept until commit only for a route that has a fallback,
    counted against a per-gateway memory bound. Past the bound, the call
    gets no retries rather than more memory (fast-path.md's 64 KiB a held
    stream).
  - Retryable statuses: 429, 503, 529 and overloaded events, and failures
    the provider cannot have seen (dial, DNS, TLS). Not timeouts after
    sending.
  - A final 503's Retry-After is the earliest across the pools tried
    (ADR 0024).
- **Retry storms**: a load simulation (testing.md §3.2, qload) of client
  SDK retries (3×) times gateway retries under a provider brownout. It
  shows the aggregate cap holding attempts per second under 1.5× the
  offered load.

Status:
- **Model: built.** model/attempts.md, checked by check-attempts.sh: the
  design's sampled runs, four scenarios, six stories, and the small
  instance's matrix (nine designs, every run to depth 14): the design holds
  all nine properties; each of the eight broken designs breaks what it
  must, and nothing else. Two results changed the plan:
  - The budget need not be checked again per attempt: a call bills at most
    once (refused and unreached attempts are free, an uncertain one ends
    it), so at most `LIMIT` admitted calls are unbilled at once, and L×W
    holds. A switch for the per-attempt check found no run it prevents.
    Quota shares are checked per attempt (without it a failover lands on a
    deployment at its quota).
  - A retry after a commit, or after an uncertain attempt, also breaks the
    budget bound (the call bills twice), not only one answer per caller and
    the provider's work within the slots.

- **Code: built.**
  The loop (`gateway/proxy`, ServeHTTP; `attempt.go`), deployment health
  and retry budgets (`gateway/route/health.go`), a route's
  `spec.retries.maxAttempts` (default 2, at most 5) and a deployment's
  `maxInFlight`. A row per attempt, each its own event. Tests, each
  ablated: `TestFailover` (529, a 200 then an overloaded event, a failed
  dial), `TestFinalAttempts` (uncertain, a cut after the commit, 400, 502:
  no retry), `TestAttemptBudgets` (the call's attempts; the route's retry
  budget), `TestCeilingFailsOver`, `TestKeptBound`, and route's
  `TestEjection`, `TestProbeAbandoned`, `TestCeiling`, `TestRetryBudget`.
  Sixteen ablations, each failing its test. Allocations per admitted call
  unchanged (`BenchmarkHandlerAdmitted`).
- **Connect: built.** `TestAttemptsModel` replays model/attempts.md's
  `gateway` and `gatewayWide` instances (§11: the gateway's own choices,
  `ORDERED`) on the real request path against scripted providers: 50
  traces each and a witness for each of five required states (a
  failover, an ejection and its probe, the route's retries spent, a
  call's attempts spent, a probe passed by another call). The deployment
  each attempt takes and when a call gives up are the code's, checked
  against the model's. Of nine ablations of the loop and its health, eight
  fail it; the ninth (a failed probe not re-ejected) is a matter of time,
  which the model leaves out, and `TestEjection` catches it.

- **Retries are failovers, of refusals only (ADR 0051).** After a review
  of what other gateways retry and what costs twice: an attempt goes only
  to a deployment the call has not tried (a pool of one makes one attempt;
  the client's SDK retries the same endpoint), and 500 is no longer
  retried. The model takes the rule; its connect test found a probe left
  standing after a readmission, now ended (`TestReadmissionEndsProbes`).
- **Drill in small: built.** `TestFailoverDrill` (gateway/proxy): two
  providers over real HTTP, the gateway's own server in front, eight
  callers streaming for two seconds, the first provider killed at 0.7 s.
  Every call started after the kill succeeded whole, with no change at the
  caller; the calls cut by the kill ended with the contract's error event;
  one ledger row per attempt, each its own event, the free attempts at the
  dead provider charged nothing; the dead provider ejected. Ablated: no
  failover (no call fails over) and no ejection (calls after the kill get
  503 once the route's retry budget is spent: ejection, not the budget,
  is what keeps an outage's calls succeeding).
- **Storm simulation: built, and narrower than planned.** route's
  `TestRetryBudgetBoundsTheStorm` (qload, the real `route.Health` budget
  inside the SDKs' three retries with backoff, a provider at a third of its
  speed for two minutes): the gateway adds 0.12 retries per call, against
  0.48 for a gateway that always retries once, so the gateway's layer
  adds at most about 1.25x the SDK's attempts (the plan's bound was 1.5x).
  Every budgeted run recovers. Ablated (no cap): 0.48, and the test fails.
  The recovery claim the plan expected does not hold up: where the
  provider's queue outlasts the timeout, the SDK's own retries keep every
  policy down, the SDK's alone included, and near the tipping point
  recovery is a coin toss with no ordering between the policies. The
  gateway's layer bounds its amplification; it does not decide recovery.
  (The real gateway retries no timeout, so its share in a brownout is
  smaller still than the simulation's.)

Done when:
- ~~the model's exhaustive check holds, and finds each mutant~~ (done);
- ~~each mutant's code counterpart fails a test (ablated)~~ (done);
- ~~connect-driven tests run the proxy's loop against the model~~ (done);
- ~~the storm simulation holds its bound~~ (done: the amplification bound; recovery is not the gateway's to decide);
- ~~two stubs, one killed under load, show calls succeeding and one ledger
  row per attempt~~ (done: `TestFailoverDrill`).

### 3. Bedrock and SigV4

Status:
- **Built:** the envelope (`gateway/envelope/bedrock`; gateway.md §4a) and
  its config (a Backend's `envelope: bedrock`, `aws.region`, a
  deployment's `region` and optional `url`), signed with the AWS SDK's
  default credential chain. Tests: the framing (each chunk to its SSE
  event, one byte a read), a bad CRC and a cut refused as read errors, an
  exception mapped to its error event, error answers in Anthropic's
  shape, the signature reproduced by re-signing, and end to end
  (`TestBedrockEndToEnd`: config, two regions, a throttle failed over, a
  stub verifying every signature, usage on the row); ablated: unsigned,
  one region for all, exceptions dropped. Allocations per admitted call
  unchanged on other envelopes.
- **Attribution: built.** Each attempt's attribution (golem's request and
  attempt IDs, principal, scope, key display ID) goes to Bedrock as its
  signed request metadata, so Bedrock's invocation logs join the ledger
  per request: reconciliation (exit 2) can compare call by call. The AWS
  side (CUR 2.0 caller identity, metadata-only invocation logging) is in
  operations.md. Tests: `TestMetadata`, and `TestBedrockEndToEnd`'s stub
  refusing unsigned metadata and checking each attempt's IDs against its
  row; ablated (set after signing), both fail.
- **Open:** the run against real Bedrock (docs/acceptance/m1-bedrock.md,
  needing an AWS account with model access), non-streaming calls on real
  Bedrock, and `count_tokens` (step 4's endpoints).

- The `bedrock` envelope's response side:
  - the AWS event stream (prelude and message CRCs, headers, base64
    `bytes` payloads, exception message types) reframed as SSE;
  - non-streaming `InvokeModel`;
  - inference profile IDs as deployment targets.
- SigV4 per attempt, re-signed on each retry, with region and host from
  the deployment, so failover to another region signs for it.
- Credentials from the pod's role (IRSA or Pod Identity), refreshed through
  the SDK's credential providers (gateway.md allows them); the signer and
  providers only, no Bedrock client.
- Usage from `message_delta` and `amazon-bedrock-invocationMetrics`, cache
  read and write tokens mapped. A cut stream without metrics is estimated
  as today.
- `count_tokens` through Bedrock's CountTokens.

Done when:
- golden tests of the framing, including a bad CRC refused and an
  exception event mapped to its status;
- a stub checks the signature (ablated: an unsigned or stale-signed
  request is refused);
- Claude Code streams through a standalone gateway to real Bedrock, with
  the ledger row's usage equal to the invocation metrics, recorded in
  docs/acceptance/m1-bedrock.md (the request IDs, both usages, the
  difference).

### 4. The team's endpoints

Status:
- **Built:** `GET /v1/models` and `/v1/models/{id}` in both dialects (the
  scope's aliases only; another scope's model unknown, not forbidden), and
  Anthropic's `POST /v1/messages/count_tokens`, free, to the deployment's
  count endpoint or Bedrock's CountTokens (gateway.md §3a). Tests:
  `TestModels` (ablated: the scope's filter), `TestCountTokens` in proxy
  and in the bedrock envelope.
- **The team uses** the OpenAI Responses API and vLLM (not Embeddings,
  Codex or Copilot BYOK, which are deferred).
- **Built: Responses**, stateless (gateway.md §3a): `TestResponses` (a
  stream byte for byte, usage from response.completed, store false added;
  previous_response_id, background, a provider file and store true each
  refused before any call; an error first refused before the commit), and
  the dialect's tests; ablated: the stateful refusal, the store rewrite.
- **Built: vLLM boxes.** A vLLM box is a backend of the dialect it
  serves (`openai.chat`, or `anthropic.messages` for vLLM's Messages
  endpoint), each box a deployment (`spec.network.allowPrivateAddresses`
  for a private address). Per deployment:
  `maxInFlight` (the box's static concurrency), `maxInFlightPerPrincipal`
  (one principal cannot fill a shared box), and `healthCheck` (a GET, for
  vLLM `/v1/models`, with the backend's credential, every
  `intervalSeconds`): a failed check ejects the box before a call finds it,
  a pass after its ejection readmits it, and a pass never cuts an ejection
  short. Overflow is the pool's order: past a ceiling, or ejected, the call
  goes to the next deployment (a hosted model as the fallback). Tests:
  `TestPerPrincipalCeiling`, `TestCheckerEjectsAndReadmits`, `TestLocalBox`;
  ablated: the per-principal refusal, a failed check's ejection, a pass
  during the ejection. Open: models discovered from the box (they are
  configured), `/v1/completions` (not used), the run on a real box
  (docs/acceptance/m1-vllm.md).
- **Built: limits before authentication** (gateway.md §3 step 1).
  Connections (`-max-conns`, `-max-conns-per-source`), each source's
  requests (`-source-rate`) and failed authentications (`-auth-failures`;
  past them `limit.source`, before the credential or body is read), the
  source the connection's address or `-source-header`'s; request JSON
  nested past 128 refused. SSE events were bounded already (`sse.MaxEvent`,
  1 MiB). Tests: `TestInvalidKeyFlood`, the guard's and the listener's,
  `TestRequestDepth`, `TestRefusals`; ablated: failures not recorded, the
  depth check, escaped quotes. Behind a sidecar the source is the pod's
  until the mesh sets a header.
- **Built: member and key caps** (org-model.md §4.2). A call is charged to
  up to three accounts: its key's cap (state, or a LocalIdentity key's
  `capMicros`), its member's (state, else the ScopePolicy's
  `defaultMemberCapMicros`), and the scope's budget, checked in that order
  in the one admission script and added to by the one completion write;
  the first reached refuses (`budget.key_cap`, `budget.member_cap`,
  `budget.pool_exhausted`) under the scope's mode, and observe and warn
  name the level. Known spend per member and per key comes from the
  ledger (spend_by_event gains principal and key) into the snapshot.
  Model: exposure.md's member accounts (`memberCaps` holds everything to
  depth 7; `memberSnapshotOnly`, a member seen through snapshots alone,
  breaks `memberWithinBound` in 4 steps). Tests: `TestCaps` (limiter and
  auth), the store suite's `accounts` on Mem and Valkey, `TestCapModes`,
  `TestWithKnownSpend`, `TestEntitlementsCaps`; ablated: no member
  account, the Lua checking the scope alone. Set through the control API
  (`SetMemberCap`, `SetKeyCap`, a cap on `MintKey`; audited, widening)
  and golemctl (`members cap`, `keys cap`, `keys create --cap`, amounts in
  the currency): `TestCaps` (service: who may, the bounds, removal with
  the membership), `TestGolemctl`, `TestAmounts`. Open: team pools
  (grants).
- **Built: provider quota shares** (gateway.md §3 step 6). Per deployment,
  requests and tokens a minute, a bucket shared by the site's gateways in
  Valkey, charged per attempt (an estimate corrected to usage), a full
  deployment passed over, every one full `limit.provider_quota` with a
  Retry-After, failing open with Valkey down. Tests: `TestQuotaFailover`,
  `TestQuotaTokens`, the store suite's rate buckets on Mem and Valkey,
  `TestQuotaFailsOpen`; ablated: the per-attempt check, the correction.
  Open: per-member and per-scope rate limits; `x-ratelimit-*` headers.

Only what the team uses. First, list their apps' and agents' endpoints in
docs/acceptance/m1-exit.md from a week of their provider logs.

- Responses (`/v1/responses`), refusing stored retrieval,
  `previous_response_id`, background execution and provider file
  references (security-and-correctness.md §4).
- `GET /v1/models` in both dialects (the caller's allowed aliases);
  Anthropic `count_tokens`, not charged, rate limited.
- vLLM pass-through (Chat, Completions, Messages):
  - static endpoints with discovered models;
  - static concurrency ceilings, per-principal limits;
  - health probes and passive ejection;
  - overflow to a fallback pool.

  Gated by the vLLM spike.
- Capability-aware routing (ADR 0028): a route's pool filtered by what the
  request needs.
- Member and key caps, and team pools granted by admins (roadmap.md, Org).
- Embeddings, Codex and Copilot BYOK if the team uses them. Copilot is
  gated by its spike, the agents by sdk-compat.md's open half.
- Request limits before authentication: per-source connections and
  request rate, JSON depth, SSE event size, invalid-token floods
  (security-and-correctness.md §8).

Done when:
- each endpoint's golden and usage extraction tests pass;
- the SDK compatibility suite runs each with the real SDKs against a stub;
- each refusal above has its test (ablated);
- one vLLM box serves through golem, recorded in
  docs/acceptance/m1-vllm.md.

### 5. Production in observe

The team's traffic moves to golem with budgets in observe, on the real
install. Two weeks begin here.

- **Install**: Helm charts for the gateway and golem-control through Argo
  CD, on EKS. The chart sets drain to the 10 minutes operations.md assumes
  (the binary's `-drain` defaults to 30 s). Argo Rollouts and Crossplane
  claims are deferred: canaries by hand, cloud resources by hand or Terraform,
  recorded.
- **golem#7's evidence**, to lift the release gate:
  - provider spending limits per account, recorded with who set them;
  - OTLP TLS against the real collector;
  - the mTLS owner of each hop.
- **ADR 0041's M1 gates** (security-and-correctness.md):
  - the denylist surviving Argo sync, prune and restarts;
  - negative Argo and Crossplane authorization and admission tests;
  - implementation tests of composite freshness and restore.
- **Policy**: the threat model reviewed; the model onboarding checklist;
  CODEOWNERS on config, with RBAC on CRD writes as the enforcement.
- **Dedicated provider accounts** and **bypass controls** (ADRs 0038,
  0039): the Bedrock SCP, egress blocks, provider key-creation limits,
  managed agent settings.
- **Ledger**: ClickHouse TTLs; a reconciliation job by exit 2's rule, daily
  previews and the month's record.

Done when:
- the charts deploy to the EKS cluster through Argo, recorded;
- `TestReleaseGate`'s gate is lifted by a commit citing golem#7's closed
  evidence;
- each ADR 0041 gate has its test (ablated) in m1-security-gates.md;
- a direct Bedrock call from the team's role outside golem is denied and
  appears on the dashboard;
- the threat model review is recorded with its sign-off;
- reconciliation's first preview is published.

### 6. Operations and drills

- **Alerts, as rule files with the charts**:
  - request and upstream error rate by `error.origin`;
  - latency and TTFT;
  - retry budget exhaustion and ejections;
  - `golem.ledger.dropped` and `failed`, since a dropped row undercounts
    exit 2;
  - snapshot age;
  - absent metrics;
  - the leader's CAS retries;
  - ingestion lag.
- **SLOs** and an on-call rotation for the team's hours.
- Grafana dashboards for spend, limits, latency and TTFT; a synthetic
  request per pool.
- **Load test** at the team's peak volume times three, against stubs on the
  real install: Valkey's and ClickHouse's headroom recorded.
- **Drills**, each written up in docs/acceptance/m1-drills.md:
  - the two failover drills of exit 3 (a second region or inference profile
    with model access and quota, allowed under the SCP);
  - signing-key rotation;
  - the incident denylist, with its time to apply measured;
  - a state bucket restore;
  - a gateway canary rolled back.

Done when:
- each alert fires in a test against recorded metrics;
- the dashboards show the synthetic requests;
- each drill's record names its pass criterion and result.

### 7. Enforce

Exit 4's condition and rollback, then the switch. The team's ScopePolicies
move to `enforce` in a reviewed PR, and the rollback drill's record is
linked from it.

Done when:
- m1-exit.md records all four measures with links to their evidence;
- the published snapshot shows `enforce` for the team's scopes;
- the first refusal in enforce appears on the ledger as `402`.

## Deferred past M1

- Argo Rollouts automation and Crossplane claims (by hand in M1).
- Weighted and least-in-flight balancing (round robin with ejection is
  enough for one team).
- Session affinity: when it comes, keyed by the session header (ADR 0026),
  not a prompt hash.
- In-process hooks.
- `golemctl explain`, `golemctl gateways`, restore over the API, the CRD
  watch (with its watch-sync barrier, ADR 0041) and operational overrides:
  the directory read and PRs serve one team. Incidents use the denylist.
- The secret-scanning webhook (leaked keys revoked by hand meanwhile).
- `golemctl agent setup`.
- Translation (M1.5), remote hooks and guardrails, remote sites, the
  console, Langfuse capture.
