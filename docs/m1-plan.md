# M1 plan: first team in production

The working plan for roadmap.md's Phase 2 (M1). Each item names what is
built, what is left, and the evidence that it is done. Evidence is a test,
a model check or a drill, never a claim. Update an item in place when it
lands; dated progress goes in the walkthroughs and the commit log.

M1's exit (roadmap.md):

1. The platform team sends all its model traffic through golem for two weeks.
2. The ledger is within 3–5% of provider usage for those accounts.
3. Outage drills fail over without caller changes: a Bedrock region to
   another region or inference profile, and a local box to its fallback
   pool.
4. The team's budgets are switched from `observe` to `enforce`.

Every item below serves one of these. Exits 1 and 2 need the ledger to be
right and the team's real traffic to fit: their providers, endpoints and
agents. Exit 3 needs the attempt loop. Exit 4 needs budget modes and
enough ledger history to trust them.

## Built

| Area | What | Evidence |
|---|---|---|
| Frontends | Anthropic Messages and OpenAI Chat, streaming, usage extraction, feature modes | `gateway/proxy` tests; m1-security-gates.md |
| Backends | Anthropic and OpenAI-compatible dialects, pass-through, one deployment per attempt | `gateway/proxy`, `gateway/dialect` tests |
| Identity | `glm_live_` keys, Entra users and services, `golemctl login`, minted keys, the emergency denylist, revocation by snapshot and stream termination (`proxy.InFlight`) | `gateway/auth`, `TestSweepSparesANewerGrant` |
| Org | Scopes, members, managers, services, keys with model constraints, group sync (Entra) | `control/state`, `control/entrasync` tests |
| Limits | Valkey and Mem stores, soft budgets on known spend, declared worst case, concurrency slots, cut-stream estimates, Valkey outage start, TLS/auth | `gateway/limit` (store differential, hegel props); model/accounting, model/exposure |
| Budget modes | ScopePolicy CRD: observe, warn, enforce; a live scope's policy cannot be deleted | `TestRemovedPolicyRefused`; ADR 0050 |
| Control plane | golem-control on the bucket document, lease and fence, signed publication with checkpoints, restore, bounded audit outbox, API drain, last-good config | `control/*` tests; model/leadership, model/publication, model/freshness |
| Ledger | `golem.spend` and `spend_dedup` by materialized view, the price model, ingestion lag metric and alert | ClickHouse integration tests; `TestLedgerQueue*` |
| Testing | Five Quint models with exhaustive small-instance checks and mutants; Hegel props in CI | `model/check-*.sh`; CI |

## Left, in order

The order puts first whatever could change the design, then whatever
blocks exit 1 (the team's traffic), then exit 3 (failover), then
operations and install. Items in one step are independent.

### 1. Bedrock envelope and SigV4

The team's Claude traffic goes to Bedrock. Built: nothing.

- A `bedrock` dialect: the Messages body moved into
  `InvokeModelWithResponseStream`, with `anthropic_beta` and
  `anthropic_version` in the body, the event stream reframed as SSE, and
  inference profile IDs as deployment targets.
- SigV4 signing of each attempt, with credentials from the pod's role
  (IRSA or Pod Identity). No AWS SDK in the gateway beyond the signer, if
  the fast-path budget allows it (spikes/fast-path.md).
- Usage taken from Bedrock's `amazon-bedrock-invocationMetrics` and the
  stream's `message_delta`; count_tokens through `CountTokens`.

Done when: Claude Code streams through a standalone gateway to a stub
that checks SigV4 and speaks the event stream (a golden of the framing);
the same against real Bedrock by hand, recorded; usage on the ledger row
equals the invocation metrics. Roadmap spike: "Bedrock envelope".

### 2. The attempt loop: retries, failover, ejection

The design-risk item: an attempt loop interacts with accounting (each
attempt charged once, uncertain ones at worst case), with slots and with
streaming (no retry after the first byte). Built: one attempt
(`proxy.go`'s marked place). Model first.

- **Model**: model/attempts.md. Attempts before the first byte, a retry
  budget per pool (a token bucket, ADR 0024), ejection after consecutive
  failures with probation, and failover to the route's next pool. Its
  invariants: at most one attempt streams to the caller; every attempt is
  charged exactly once (an uncertain one at worst case); the retry budget
  bounds attempts per call and per pool; an ejected deployment gets no
  traffic until probation. Mutants: a retry after the first byte, an
  uncharged failed attempt, an unbounded budget.
- **Code**: the rewritten body kept until an attempt commits; the
  retryable statuses from spikes/sdk-compat.md; `Retry-After` honoured;
  per-deployment health in the gateway (no shared state); failover across
  pools of one dialect only (ADR 0033).
- Weighted and least-in-flight balancing, and prompt-cache affinity (a
  hash of the principal and the prompt prefix) pick the first attempt.
- The in-process hooks (`Request`, `Response`, `Complete`) at the loop's
  three points, so later hooks need no second loop.

Done when: the model's exhaustive check holds and finds each mutant;
connect-driven tests run the proxy's loop against it; a drill with two
stub upstreams, one killed mid-run, shows calls succeeding without caller
changes and the ledger charging each attempt. This is exit 3's drill in
small; the real one is step 6.

### 3. The team's endpoints

What the team's apps and agents call that golem does not serve yet.

- OpenAI Responses (`/v1/responses`), Embeddings (`/v1/embeddings`),
  and models lists (`GET /v1/models` in both dialects: the caller's
  allowed aliases).
- Anthropic `count_tokens` (free, not charged, rate limited).
- vLLM pass-through: static endpoints, discovered models, static
  concurrency ceilings, per-principal limits and overflow to a fallback
  pool (spike: "vLLM on the Macs and Sparks").
- Copilot BYOK and Codex through the OpenAI routes (spike results).

Done when: each endpoint's golden tests and usage extraction tests pass;
the SDK compatibility suite (spikes/sdk-compat.md) runs each endpoint with
the real SDKs against a stub; a vLLM box serves through golem in
`make dev-up` or by hand, recorded.

### 4. Rate limits and provider quotas

Built: concurrency slots; budgets. Left:

- Requests and tokens per minute per key, principal and scope, in the
  Valkey check script with the slot (ADR 0012).
- Provider quota limits per deployment (TPM/RPM shares), refused as
  retryable with `Retry-After`, so the attempt loop fails over.

Done when: the store differential covers the new counters (Valkey vs
Mem); the accounting model's limiter instance includes them; a load test
on the stub shows a deployment held under its quota.

### 5. Control plane operations

- Operational overrides (drain a deployment, pin a route, raise a cap for
  an incident) with expiry, audited, published as snapshot changes.
- `golemctl explain <key|principal> <model>`: the decision the snapshot
  would make, and why.
- `golemctl gateways` (what each gateway has applied), `golemctl state
  restore` over the API, the snapshot's period zone.
- CRD watch in golem-control instead of the directory read.
- `golemctl agent setup` for Claude Code, Codex and Copilot BYOK; the
  secret-scanning webhook that revokes a leaked key.

Done when: each has its test; explain is checked against the gateway's own
decision over random snapshots (a differential: explain says yes exactly
when the gateway admits); the webhook revokes a key in a test with
GitHub's payload, and the gateway refuses it after the next snapshot.

### 6. Install, ledger operations and drills

- Helm charts (gateway, golem-control) through Argo CD; Argo Rollouts for
  gateway canaries; Crossplane claims (`GolemInstallation`, `GolemSite`).
- Grafana dashboards for spend, limits, latency and TTFT; snapshot-age
  alerts; a synthetic request per pool.
- Provider usage reconciliation: a daily job comparing the ledger with
  Bedrock's and OpenAI's usage per account (exit 2's measure).
- Bypass controls (the Bedrock SCP, egress blocks, provider key-creation
  limits, managed agent settings) and dedicated provider accounts (ADRs
  0038, 0039): deployment work, recorded in golem#7.

Done when: the charts render in CI (helm template plus the policy checks)
and install on kind; the reconciliation job runs on a recorded day and
reports its difference; the two outage drills (a Bedrock region, a local
box) run on the real install and are written up in docs/acceptance/
with the ledger's charges for each.

### 7. Production

The team moves its traffic over in observe mode. Reconciliation runs
daily. After two weeks within 3–5%, the team's ScopePolicies go to
enforce. The record of each exit criterion goes in
docs/acceptance/m1-exit.md, as m1-security-gates.md does for the gates.

## Not in M1

Translation between dialects (M1.5), remote hooks and guardrails, remote
sites, the console, Langfuse capture.
