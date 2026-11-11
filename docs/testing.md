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
| **Accounting** (ADR 0031; written: [model/accounting.md](../model/accounting.md)) | admissions under soft and strict checks, concurrency limits, completions, crashes, Valkey loss, duplicate delivery, reconciliation | overshoot within the stated bound; never counted twice; books match after reconciliation; strict with one in flight is a hard ceiling apart from crash losses |
| **State and distribution** (control-plane.md §3–4; publication and freshness written: [model/publication.md](../model/publication.md)) | writers racing with `If-Match` (lost responses included), leader changes, signed publication with a publish sequence, gateway polls and restarts with empty disks, restore | every acknowledged write is in a snapshot every gateway applies; no gateway applies a lower publish sequence or an unsigned snapshot; no write is lost to a race; a restore never revives a revoked key; revocation within the bound after a settle |
| **Attempt loop** (ADR 0002) | attempts, first byte, failover, retry budget, ejection | no retry after the first byte; retries bounded by the budget; an ejected deployment receives nothing until probed back |
| **Approvals and grants** (org-model §4.3; when built, ADR 0034) | request, approve, reject, withdraw, expire, period rollover | a grant takes effect exactly once; no approval of one's own request; withdrawal and approval cannot both win |

Each starts small (two regions, two gateways, one pool) and stays a
design-level check: the model alone first (scenarios, safety, then
liveness), before any code is bound to it.

### 3.2 Where gotogether can be trialled during development

1. **The limiter in simulation (M1).** Run the limiter (known-spend
   checks, concurrency slots, the strict worst-case check) on `qenv` with a
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

## Review acceptance gates

The evidence matrix in [security-and-correctness.md](security-and-correctness.md#10-required-evidence-and-review-traceability)
is required at its stated milestones. The models cover fenced publication
and authorization freshness (model/publication.md) and the soft-budget
bound, deduplication and reconciliation (model/accounting.md).

## Second security review gates (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) adds negative tests for denylist persistence under Argo
sync/self-heal/prune/upgrade/restart; both audit capacity thresholds; capture
sender impersonation, pool-boundary routing and operational-exporter fan-out;
inherited-project readers; restricted/compatible request features and hostile
field-name logging; and Argo/Crossplane privilege escalation. Extend publication
models for CRD freshness/deletion and restrictive role/service/policy changes.
Extend accounting scenarios for counter handoff, multi-principal scope exposure,
failed renewal and continued billing after cancellation before asserting bounds.
These tests and model extensions are required evidence, not completed work.
