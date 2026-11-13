# golem: testing

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Implementation remains outstanding.

Status: draft. How golem is tested, chosen by value: the most effort where
a bug costs money, leaks data, or breaks every caller at once (budgets,
authorization, content capture, streaming, the distribution of config),
and the least where the compiler and a table test suffice.

## 1. What could hurt, and what catches it

| Risk | Example | Main defence |
|---|---|---|
| Overspend or double charge | concurrent calls past a budget beyond ADR 0031's bound; a duplicate delivery counted twice; crash-lost spend never booked | The accounting model (§3), Hegel state machines against it, simulation with faults |
| Rights wider than intended | a key outlives its owner's membership; a narrow override widens capture | Hegel properties over random org histories; table tests of the resolution rules |
| Content in the wrong place | capture on for a scope whose policy forbids it; a trace not deleted at expiry | Properties of capture resolution; an end-to-end test that checks Langfuse and ClickStack for content |
| Broken streams | usage missed when a stream ends early; a dialect error event malformed; a translated tool call reordered | Goldens from recorded provider streams; fuzzing the SSE parser and scanners; translation properties |
| Stale or inconsistent config | a gateway misses a change or rolls back to an older snapshot (a stale pointer, a lagging replica); two writers both believing they won | A model of the state CAS and publication; Hegel: a snapshot rebuilt after any sequence of changes equals one built from scratch |
| Retry storms | failover from a saturated local box to a commercial model amplifies a brownout | Metastability analysis and load simulation (§3.4) |
| API drift | a provider adds a field or changes an event | Recorded contract fixtures; a nightly live smoke test per provider with tiny prompts |
| Performance regression | allocations creep onto the fast path | Benchmarks with allocation budgets in CI |

## 2. The regime

| Kind | Where | Notes |
|---|---|---|
| Table tests | pure logic: body scanning, rights intersection, capture resolution, pricing, period arithmetic, CEL compilation, error rendering per dialect | The default for anything with inputs and outputs |
| Goldens (`-update` to regenerate, reviewed in diffs) | dialect requests and responses, SSE streams in and out, translations, error bodies, compiled snapshots, CLI output, generated CRDs and API descriptors | Fixtures recorded from real providers, scrubbed; one directory per dialect |
| Fuzzing (`go test -fuzz`) | JSON body scanner and splicer, SSE parser, each dialect's decoders, request and stream (no panic, output parses, content preserved), key and token parsers, snapshot decoder | Corpora checked in; short runs in CI, long runs nightly; every crash becomes a corpus entry and a table case |
| Property-based and stateful (Hegel, `hegel.dev/go/hegel`) | adapters (for each dialect, `encode(decode(x))` equals `x` on the supported subset; `decode(encode(c))` equals `c` for canonical values the dialect can express; a feature outside the subset is refused, never dropped), stream adapters (any chunking of the same stream gives the same canonical events), limiter (reserve, reconcile, expire against a simple reference), snapshot apply, router (never an ejected or capped deployment; affinity stable under membership changes), org service (random sequences of org operations keep the org model's invariants), capture resolution (narrow never widens), translation round trips | Hegel's shrinking turns a failure into a minimal sequence |
| Time | retries, backoff, ejection windows, limiter windows, drain, idle-chunk timeouts, concurrency-slot expiry | `testing/synctest`, no sleeps |
| Store suite | every state operation, lost CAS races included; the blob adapter's conformance (MinIO and a fake); signature verification and the publish-sequence floor | Per change (control-plane.md §8) |
| Contract tests | each provider backend against its recorded fixtures; Valkey with the real scripts | Containers in CI |
| End-to-end | kind with Istio ambient, the Helm charts installing everything, a stub provider, ClickHouse, Langfuse; clients: the OpenAI and Anthropic SDKs, Claude Code, Codex, Copilot CLI with BYOK | Per merge for a smoke set, nightly in full |
| Performance | `testing.B.Loop` benchmarks with allocation budgets; a load test against a stub upstream that streams with realistic timing; a soak that checks the goroutine-leak profile | Baselines from M0 |
| Mutation testing | the budget, rights, capture and router packages | Survivors are tests to write; run nightly |
| Race detector | all packages | CI |

Every test that fails names what to do next, as the code's errors do.

## 3. gotogether: what to model, what to trial

gotogether (github.com/chucklehead-dev/gotogether) checks Go code against
Quint models: model-based tests (`connect`), Hegel runs judged by a model
(`qhegel`), recorded or production runs validated against a model
(`qobs`, `binding`, `validate`, `convert` for OTel spans), and
deterministic simulation with fault-injecting doubles (`qenv`, `qtest`).
Its value for golem is in the protocols, not the plumbing.

### 3.1 Worth a Quint model

| Model | State and actions | Properties |
|---|---|---|
| **Accounting** (ADR 0031; written: [model/accounting.md](../model/accounting.md), [model/exposure.md](../model/exposure.md)) | admissions under the soft and worst-case checks, concurrency limits, completions, crashes, Valkey loss, duplicate delivery, reconciliation | overshoot within the stated bound; never counted twice; books match after reconciliation; the worst-case check with one in flight stays within the budget apart from crash losses while every call costs at most its declared worst case (the model's assumption, not the world's: ADR 0044) |
| **State and distribution** (control-plane.md §3–4; written: [model/publication.md](../model/publication.md), [model/freshness.md](../model/freshness.md)) | writers racing with `If-Match` (lost responses included), leader changes, signed publication with a publish sequence, gateway polls and restarts with empty disks, restore | every acknowledged write is in a snapshot every gateway applies; no gateway applies a lower publish sequence or an unsigned snapshot; no write is lost to a race; a restore never revives a revoked key; revocation within the bound after a settle |
| **Attempt loop** (ADR 0002) | attempts, first byte, failover, retry budget, ejection | no retry after the first byte; retries bounded by the budget; an ejected deployment receives nothing until probed back |
| **Approvals and grants** (org-model §4.3; when built, ADR 0034) | request, approve, reject, withdraw, expire, period rollover | a grant takes effect exactly once; no approval of one's own request; withdrawal and approval cannot both win |

Each starts small (two regions, two gateways, one pool) and stays a
design-level check: the model alone first (scenarios, safety, then
liveness), before any code is bound to it.

### 3.2 Where gotogether can be trialled during development

1. **The limiter in simulation (M1; not yet tried: the limiter is checked
   by connect and qhegel against model/exposure.md instead, see Built so
   far).** Run the limiter (known-spend
   checks, concurrency slots, the worst-case check) on `qenv` with a
   `qenv.Store`-backed double of Valkey (scripts as atomic transactions),
   gateways crashing and Valkey failing over; `qtest.System.Explore` judged
   by the accounting model. This is where a bug costs the most and is hardest to
   find otherwise.
2. **The state document under races (M1).** golem-control replicas on
   `qenv` against a blob double with `If-Match` semantics (lost races,
   slow PUTs, lost responses: a PUT that succeeded but whose 200 never
   arrived), judged by the state-and-distribution model. Ablation: a
   double that ignores `If-Match` must make the tests fail on lost
   updates, which shows the CAS is load-bearing.
3. **The attempt loop over simulated providers (M1).** The gateway's
   upstream calls through `qenv/qhttp`, with lost responses, slow first
   bytes, 429s and mid-stream cuts as faults, judged by the attempt-loop
   model.
4. **Distribution (M1–M2).** The leader publishing and gateways polling in
   one simulation, leader changes, replayed old pointers and slow storage as
   faults; a settle checks revocation latency.
5. **Production traces (M2).** golem already emits OTel spans for each
   check, attempt and completion; `convert` rules turn them into model
   steps, so staging and production runs can be validated against the
   accounting model offline, without new instrumentation.
6. **Metastability (M2).** `qmeta` and `qload` on the failover policy:
   capped local boxes overflowing to a commercial pool, retries under a
   provider brownout, `qhegel.FindTrigger` for the smallest spike that
   does not recover. Informs the retry budget and the overflow queue's
   bound.
7. **Configuration coverage.** `qsweep` covering arrays over dialect ×
   backend × stream × translation × hooks × capture setting, so the
   end-to-end suite covers every pair without the full product.
8. **A grist spike for the budget domain (optional).** Grants, allocations
   and approvals are naturally event-sourced and audit-heavy; a branch that
   builds them as a grist app (its model generated from the catalog,
   explored with `gristsim`) would show whether grist's guarantees are worth
   adopting for golem-control. It runs on its own stores (Postgres via
   `sqlstore`), so it is a trial, not a dependency, until
   it proves itself.

The trial is judged as any test is: by what it catches. Each model's
checks are proved by planted mutants (a check that skips the concurrency
limit, a balance that sums duplicates, a publisher that skips the pointer's version check, a stale leader that publishes after losing leadership) that the
model must reject; a model whose mutants survive is not worth keeping.

## Built so far

The first model checks against real code (docs/spikes and ADR 0041's
gates name what is still owed):

| Code | Model | How | Found |
|---|---|---|---|
| `gateway/snapshot.Holder` | model/publication.md | `connect`: the model's traces drive the Holder (publisher actions are the environment), state compared after every step | the floor was strict where the model's is inclusive: a gateway restarted with an empty disk refused its own trusted checkpoint and could not become ready while the publisher was down |
| the same | the same | `qhegel` (`test/props`): Hegel drives the Holder with a stand-in publisher and an attacker replaying any old publication; the model judges each run | planted bugs (no floor check, no expiry check) caught and shrunk to four steps; neither is visible to the connect test, since the model never asks for a forbidden step |
| `gateway/limit.Limiter` | model/exposure.md | `connect`: one Limiter per gateway over one store; slots, waivers, held increments and known spend compared | five of the model's rules shown necessary by ablation (cancel holding its slot, re-acquiring, the hand-off by event, recording waivers, replaying increments) |
| the same | the same | `qhegel`: Hegel drives the limiters, a late ledger, snapshots, outages and failovers | **the model was wrong**: during an outage it judged by the true counter, which no gateway can read; it now keeps each gateway's view. A missing scope limit is caught and shrunk to three steps |
| the same | the same | a benchmark of the admitted path, then the model | **the design was wrong, twice hidden by an atomic model**: matching counter events to snapshots cost O(window) per admission, and deleting included events lost them for gateways still on an older snapshot. ADR 0042 replaced it (numbered counter, ledger watermark: O(1), exact under skew); the model now applies snapshots and recovers per gateway, which also exposed admissions next to unrecovered peers' calls (now waived for a grace period). 32 µs and 23 allocations per admitted request, flat |
| the request path, profiled | — | a performance review: `BenchmarkProduction` (the OTel SDK as production sets it up), `BenchmarkAdmitComplete` (Mem and Valkey), pprof, escape analysis | as production runs it, 75 µs and 89 allocations a call, now 44 µs and 42 (the SDK deduplicated the ledger row's keys, attribute sets were rebuilt per call); OpenAI chunks were scanned in full twice (2.3 µs, now 171 ns); an 8 MiB body pinned 16 MiB per P in the scanner's pool; ledger rows were dropped silently on a full queue; `WAIT` acknowledged nothing; the upstream idle pool was capped at 100 connections in all; a client that stopped reading held its call for ever. Over Valkey a call adds 21 allocations, above the 50 target: next to measure |
| `gateway/dialect/...`, `gateway/sse` | — | fuzzing: scan acceptance equals `encoding/json`'s, rewrites change one field, usage independent of chunking | a split `\r\n` across an empty write |
| the gateway end to end | — | `test/compat`: the official SDKs through it to a stub provider | the injected OpenAI usage chunk crashed the SDKs' usual loops |
| the same, reviewed | — | a Go specialist's and an architect's review, each fix with a test | admission read and acquired in two round trips (now one script); counters spanned months, so every budgeted scope was refused from the second; standalone could be driven by any web page; a cut provider response reached the caller as a clean, shorter one; the filtered OpenAI stream dropped keep-alives; shutdown cut every stream; a key with a missing scope was unlimited |
| `control/watermark` (golem-control's known-spend job) | model/exposure.md | `connect`: at each of the model's publications the job is given the model's ledger, as rows and numbering records, and must compute its watermark and spend; plus a randomised check against brute force (known spend plus the counter above W is the true total, rows and numbers arriving in any order, twice) | a mark taken as the highest number ingested is caught by both; the model's traces publish with a gap rarely (26 of 4,609 publications at 500 samples), so the test asserts that some do |
| `gateway/sse.Filter` | — | fuzzing: dropping nothing is the identity, however the stream is chunked | a `\r\n` split across writes was attributed to the next event, dropped with it |

## Review acceptance gates

The evidence matrix in [security-and-correctness.md](security-and-correctness.md#10-required-evidence-and-review-traceability)
is required at its stated milestones. The models cover fenced publication
and authorization freshness (model/publication.md), composite freshness of
state and config CRDs under stale caches, rollbacks, leader changes and
restore (model/freshness.md), and the soft-budget
bound, deduplication and reconciliation (model/accounting.md).

## Second security review gates (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) adds negative tests for denylist persistence under Argo
sync/self-heal/prune/upgrade/restart; both audit capacity thresholds; capture
sender impersonation, pool-boundary routing and operational-exporter fan-out;
inherited-project readers; restricted/compatible request features and hostile
field-name logging; and Argo/Crossplane privilege escalation. The model side
of CRD freshness, deletion and rollback and of restrictive changes from
either source is model/freshness.md; the implementation tests remain.
The model side of counter handoff, multi-principal scope exposure, failed
renewal and continued billing after cancellation is model/exposure.md
(ADR 0041 §7.6); the implementation tests remain.
These tests and model extensions are required evidence, not completed work.
