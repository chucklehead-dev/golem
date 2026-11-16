# M1 plan: first team in production

The scope of record for roadmap.md's Phase 2 (M1): what is built, what
remains with its done-when, and what is not being built in M1. Evidence is
a test, a model check or a drill with a written record in docs/acceptance/,
never a claim. A test that shows an invariant is ablated (run with the fix
removed and seen to fail), as m1-security-gates.md records. Update a row or
step in place when it lands; dated progress goes in the commit log.

## The exit, made measurable

roadmap.md's four exit criteria, each with the measure that decides it,
recorded in [acceptance/m1-exit.md](acceptance/m1-exit.md).

1. **All the team's model traffic through golem for two weeks.**
   - A baseline of the team's provider usage before golem: a month of AWS
     CUR and provider usage by account.
   - The endpoint inventory: the team's apps' and agents' endpoints from a
     week of their provider logs, each served or deliberately not.
   - Then zero usage on the team's direct provider accounts and roles
     outside golem's dedicated ones (ADR 0031).
   - Bypass attempts (denied Bedrock calls, blocked egress) logged
     (ADR 0039).
2. **Ledger within 3–5% of provider usage.**
   - **Compared:** cost, and tokens by kind (input, output, cache read,
     cache write 5m and 1h), per account × model × UTC day, from
     `golem.spend_dedup` against CUR (a day or two late) and the OpenAI
     usage API. The comparison is a SQL query with a written record; no
     job writes adjustment rows.
   - **Time alignment:** rows bucketed by `started_at` in UTC whatever
     `-period-zone` is; a stream crossing midnight counted on its start
     day both sides, or the difference reported as the boundary's share.
   - **Rule:** the whole two weeks within 5%, every day within 10%. Days
     are previews; the monthly reconciliation is the record (ADR 0031).
   - **Estimated and uncertain rows** are reported as their own share:
     charged high on purpose (contracts.md), they explain a ledger that
     runs high; they are not a tolerance to widen.
3. **Outage drills fail over without caller changes:** a Bedrock region to
   another region or inference profile; a local box to its fallback pool.
   Each under synthetic load at the team's peak rate; passed with no
   caller-visible failures beyond those in flight (cut streams allowed and
   counted), TTFT p95 added under 2 s, and every attempt on the ledger.
4. **Budgets from observe to enforce.** Owner: the platform team's lead
   signs off. Condition: two weeks of observe with every `over_budget` row
   explained, and budgets sized from that history. Rollback: a ScopePolicy
   PR back to `observe`, live at the next publication, drilled once before
   the switch with its time recorded.

Production also needs the release gate lifted: pool mode refuses to run
without `-accept-unreleased` until golem#7's evidence is in (`releaseGate`,
cmd/golem-gateway; m1-security-gates.md). Step 5 schedules it; the
gateway chart's `acceptUnreleased` runs the observe phase meanwhile, by
recorded decision.

## Built

Steps 1–4 and the built parts of step 5. The code's map:
[orientation](walkthroughs/README.md).

| Area | What | Evidence |
|---|---|---|
| Frontends | Anthropic Messages, OpenAI Chat, OpenAI Responses (stateless: store forced false, stateful requests and file or item references refused in both dialects), `GET /v1/models`, `count_tokens` held to a call's checks | `TestFeatureModes`, `TestRequestBoundary`, `TestResponses`, `TestStoreOff`, `TestModels`, `TestCountTokens`, `TestCountTokensChecks`; ADR 0027, 0037 |
| Commit point | an attempt commits at its stream's first event; an error event first is a refusal; a provider error after output marks the call failed | `TestFailedBeforeCommit`, `TestCommitAtFirstEvent`, `TestStreamErrorAfterOutput` |
| Attempts | failover in pool order to an untried deployment, of refusals and unreached attempts only; ejection, probes, health checks, a retry budget per route; a cancelled call is no deployment failure | model/attempts.md and `TestAttemptsModel`; `TestFailover`, `TestFinalAttempts`, `TestCallerGoneIsNoFailure`, `TestRetryBudgetBoundsTheStorm` (the gateway adds at most about 1.25× the SDKs' attempts), `TestFailoverDrill`; ADR 0051 |
| Bedrock | the envelope: InvokeModel and its event stream as Messages SSE, SigV4 per attempt and region, the attempt's attribution as signed request metadata, CountTokens; a request never sent fails over; credentials refusals' text not forwarded | `TestBedrockEndToEnd`, `TestMetadata`, `TestNotSent`, `TestCredentialsRefused`, the envelope's framing tests |
| vLLM boxes | a deployment per box, `maxInFlight`, `maxInFlightPerPrincipal` (a principal at its share told `limit.concurrency`), `healthCheck`, overflow down the pool | `TestPerPrincipalCeiling`, `TestCheckerEjectsAndReadmits`, `TestLocalBox`, `TestOwnShareIsTheCallersLimit` |
| Capability routing | betas only: a deployment that refuses a request's betas is passed over | `TestBetaRouting`; ADR 0028 |
| Before authentication | connection and source limits; only credential faults count against a source; a valid key always passes; off behind a sidecar unless a source header is set; body bytes and bodies being read bounded; JSON depth | `TestInvalidKeyFlood`, `TestEntraKeyTroubleIsNoFailure`, `TestReadingBounds`, `TestReadingClaimOverCeiling`, the guard's and listener's tests |
| Identity | `glm_live_` keys, Entra users and services, `golemctl login` and `token`, minted keys, the emergency denylist; revoked or denied callers' calls in flight ended | `gateway/auth` tests; `TestTerminateStream`, `TestSweepSparesANewerGrant` |
| Org | scopes, members (synced from bound Entra groups), managers, services, keys with model constraints; a manager cannot adopt another team's service | `control/state`, `control/service` (`TestServiceAdoption`), `control/entrasync` tests; ADR 0047 |
| Admission | scope budgets on the snapshot's known spend, in-flight slots per principal (per scope) and per scope, request rate limits, cut-stream estimates, Valkey outages as waivers, TLS and AUTH to Valkey | model/exposure.md and `TestExposureModel`, `TestExposureModelValkey`; `TestStoresAgree`, `TestPrincipalPerScope`, `TestCallerLeavesDuringAdmission`; ADR 0031 |
| Budget modes | ScopePolicy: observe, warn, enforce; a live scope's policy cannot be deleted | `TestBudgetModes`, `TestBudgetWarning`, `TestRemovedPolicyRefused`; ADR 0050 |
| Control plane | one replica claiming the fence; signed publication with checkpoints; config from git-sync's checkout, each snapshot naming its commit, no lease past `-max-config-age`; last-good config; restore; bootstrap; bounded audit outbox; the API served while Entra is down | model/publication.md and `TestPublicationModel`; `TestGuard`, `TestCheckout`, `TestRestartOverBrokenConfig`, `TestRestoreNeverWidens`, `TestOutboxBound`, `TestAPIStartsWithoutEntra`; ADR 0053, 0054 |
| Ledger | rows through the pod's collector sidecar (deploy/otel/collector.yaml, a persistent queue) into spend.sql's tables, deduplicated by event; attempt IDs as UUIDs; price versions, provider accounts, one-hour cache writes; known spend from `golem.spend_dedup` | `TestLedgerReachesClickHouse` (test/ledger), `TestRowMatchesLedgerDDL`, `TestClickHouseLedger`, `TestLedgerQueue*`; ADR 0043 |
| Charts | golem-control (one replica, `Recreate`, git-sync) and a gateway pool (three replicas, PDB, collector sidecar, `acceptUnreleased`, reading bounds); lease and period zone set together; Argo sync waves gateways first, then golem-control | `helm lint`, `helm template`, kubeconform, the flags checked against the binaries, the images run in Docker; deploy/charts/README.md |
| Release | the release gate | `TestReleaseGate` |
| Models | publication, exposure and attempts, each with sampled checks, scenarios and broken designs, and a connect test | `make -C model check`; CI |

## Left, in order

Production must start by about week 6 of 8–10, so that two weeks of
observe and reconciliation fit inside the milestone.

### 5. Production in observe

The team's traffic moves to golem with budgets in observe, on the real
install. Two weeks begin here.

- **Install on EKS** through Argo CD, in operations.md §4's order (the
  bucket and its policy, the KMS key, the IRSA roles, Valkey and
  ClickHouse users, the Secrets and ConfigMaps outside Argo, the config
  repository, the Entra registrations; then gateway pools, then
  golem-control). The chart's drain is the 10 minutes operations.md
  assumes. Cloud resources by hand or Terraform, recorded.
- **golem#7's evidence**, to lift the release gate: provider spending
  limits per account, recorded with who set them; OTLP TLS against the
  real collector (or the sidecar on loopback, recorded); the mTLS owner of
  each hop.
- **ADR 0041's M1 gates** (security-and-correctness.md): the denylist
  surviving Argo sync, prune and restarts; restore and freshness tests for
  the git checkout (ADR 0054).
- **Policy**: the threat model reviewed; the model onboarding checklist;
  branch protection and CODEOWNERS on the config repository.
- **Dedicated provider accounts** and **bypass controls** (ADR 0031,
  0039): the Bedrock SCP, egress blocks, provider key-creation limits,
  managed agent settings.
- **The acceptance runs**: Claude Code streaming through a pool gateway to
  real Bedrock, the ledger row's usage equal to the invocation metrics
  (docs/acceptance/m1-bedrock.md: the request IDs, both usages, the
  difference); one vLLM box serving through golem
  (docs/acceptance/m1-vllm.md); the SDK compatibility suite against the
  install.
- **The baseline and the endpoint inventory** in m1-exit.md.
- **Ledger**: ClickHouse TTLs; the reconciliation query by exit 2's rule,
  its first daily preview.

Done when:
- the charts deploy to the EKS cluster through Argo, recorded;
- `TestReleaseGate`'s gate is lifted by a commit citing golem#7's closed
  evidence;
- each ADR 0041 M1 gate has its test (ablated) in m1-security-gates.md;
- m1-bedrock.md and m1-vllm.md record their runs;
- a direct Bedrock call from the team's role outside golem is denied and
  logged;
- the threat model review is recorded with its sign-off;
- reconciliation's first preview is recorded in m1-exit.md;
- two weeks of observe have run.

### 6. Operations and drills

- **Alerts, as rule files with the charts**: request and upstream error
  rate by `error.origin`; latency and TTFT; retry budget exhaustion and
  ejections; `golem.ledger.dropped` and `failed`; snapshot age and lease
  runway; the config checkout's age; absent metrics; ingestion lag.
- **SLOs** and an on-call rotation for the team's hours; a synthetic
  request per pool.
- **Load test** at three times the team's peak, against stubs on the real
  install: Valkey's and ClickHouse's headroom recorded.
- **Drills**, each written up in docs/acceptance/m1-drills.md: exit 3's
  two failovers; signing-key rotation; the incident denylist, its time to
  apply measured; a state bucket restore; golem-control restarted under
  traffic (gateways ride on their lease); a gateway pool rolled back by
  syncing one pool first.

Done when:
- each alert fires in a test against recorded metrics;
- the synthetic requests are seen per pool;
- each drill's record names its pass criterion and result.

### 7. Enforce

Exit 4's condition and rollback, then the switch: the team's ScopePolicies
move to `enforce` in a reviewed PR, the rollback drill's record linked
from it.

Done when:
- m1-exit.md records all four measures with links to their evidence;
- the published snapshot shows `enforce` for the team's scopes;
- the first refusal in enforce is seen: the `golem.refusals` metric's
  count for `golem.error.code` `budget.pool_exhausted`, and the refused
  request itself, found by its request ID (the 402's `golem-request-id`)
  in the gateway's refusal record: today its span, which carries
  `golem.error.code` (refusals write no ledger row, and the gateway logs
  no refusal line).

## Not building in M1

Decided out, so that nobody builds them for M1. Each is in roadmap.md's M2
unless noted.

- Crossplane claims and their authorization and admission tests; cloud
  resources are made by hand or Terraform and recorded.
- Threshold notifications, and email or Teams notifications of any kind.
- Key, adjustment-request and registration expiry reminders.
- A reconciliation job writing `adjustment` rows: reconciliation is a SQL
  query over `golem.spend_dedup` against CUR and the OpenAI usage API, with
  a written record.
- `SpendService` and `BudgetService`; grants and team pools.
- Per-key and token rate limits, $/min limits, `x-ratelimit-*` headers.
- Member and key caps (ADR 0031: scope budgets only).
- Provider quota shares (a deployment's ceilings and the provider's 429s
  remain).
- The CRD watch, config objects in the cluster, their status, and
  operational overrides (ADR 0054).
- Argo Rollouts canaries; a pool is canaried by syncing it first.
- Dashboards as code.
- Also deferred: weighted and least-in-flight balancing, session affinity,
  in-process hooks, translation (M1.5), `golemctl explain`, `golemctl
  gateways`, restore over the API, the secret-scanning webhook (leaked keys
  revoked by hand), `golemctl agent setup`, Embeddings, Codex and Copilot
  BYOK, remote sites, the console, Langfuse capture.
