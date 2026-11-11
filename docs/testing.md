# golem: testing

Status: draft. How golem is tested, chosen by value: the most effort where
a bug costs money, leaks data, or breaks every caller at once (budgets,
authorization, content capture, streaming, the distribution of config),
and the least where the compiler and a table test suffice.

## 1. What could hurt, and what catches it

| Risk | Example | Main defence |
|---|---|---|
| Overspend or double charge | a retry reserves twice; a crashed gateway's reservation never expires; a ceiling rebalance lowers a site below its spend | Quint models of the reservation and ceiling protocols (§3), Hegel state machines against them, simulation with faults |
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
| Fuzzing (`go test -fuzz`) | JSON body scanner and splicer, SSE parser, translators (no panic, output parses, content preserved), key and token parsers, snapshot decoder | Corpora checked in; short runs in CI, long runs nightly; every crash becomes a corpus entry and a table case |
| Property-based and stateful (Hegel, `hegel.dev/go/hegel`) | limiter (reserve, reconcile, expire against a simple reference), snapshot apply, router (never an ejected or capped deployment; affinity stable under membership changes), org service (random sequences of org operations keep the org model's invariants), capture resolution (narrow never widens), translation round trips | Hegel's shrinking turns a failure into a minimal sequence |
| Time | retries, backoff, ejection windows, limiter windows, drain, idle-chunk timeouts, reservation renewal and expiry | `testing/synctest`, no sleeps |
| Store suite | every state operation, lost CAS races included; the blob adapter's conformance (MinIO and a fake); signature verification and the publish-sequence floor | Per change (control-plane.md §8) |
| Contract tests | each provider backend against its recorded fixtures; Valkey with the real scripts | Containers in CI |
| End-to-end | kind with Istio ambient, the operator installing everything, a stub provider, ClickHouse, Langfuse; clients: the OpenAI and Anthropic SDKs, Claude Code, Codex, Copilot CLI with BYOK | Per merge for a smoke set, nightly in full |
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
| **Reserve and reconcile** (ADR 0005) | reservations per level, spend, gateway crash and restart, store loss and rebuild from the ledger, retries before the first byte, streams ending early | never charged twice for one request; every reservation ends as spend or expires; a hard cap exceeded by at most the in-flight estimation error; rebuild converges to the ledger |
| **Site ceilings** (ADR 0031) | ceilings per site, spend, rebalance from a lagging ledger, grant reductions, a site cut off from the bucket | `Σ ceilings ≤ pool total` always; a ceiling never below its site's spend; a cut-off site spends at most its ceiling; after a settle, headroom moves to the busy site |
| **State and distribution** (control-plane.md §3–4) | writers racing with `If-Match` (lost responses included), leader changes, signed publication with a publish sequence, gateway polls and restarts with empty disks, restore | every acknowledged write is in a snapshot every gateway applies; no gateway applies a lower publish sequence or an unsigned snapshot; no write is lost to a race; a restore never revives a revoked key; revocation within the bound after a settle |
| **Attempt loop** (ADR 0007) | attempts, first byte, failover, retry budget, ejection | no retry after the first byte; retries bounded by the budget; an ejected deployment receives nothing until probed back |
| **Approvals and grants** (org-model §4.3) | request, approve, reject, withdraw, expire, period rollover | a grant takes effect exactly once; no approval of one's own request; withdrawal and approval cannot both win |

Each starts small (two regions, two gateways, one pool) and stays a
design-level check: the model alone first (scenarios, safety, then
liveness), before any code is bound to it.

### 3.2 Where gotogether can be trialled during development

1. **Limiter and ceilings in simulation (M1).** Run the limiter, the
   ceiling enforcement and a reconcile loop on `qenv` with a `qenv.Store`-backed double
   of the RESP store (scripts as atomic transactions), gateways crashing
   and partitions injected; `qtest.System.Explore` judged by the reserve
   and ceiling models. This is where a bug costs the most and is hardest to
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
   reservation, attempt and reconcile; `convert` rules turn them into
   model steps, so staging and production runs can be validated against
   the reserve model offline, without new instrumentation.
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
checks are proved by planted mutants (a reserve that skips one level, a
rebalance that ignores in-flight use, a publisher that skips the pointer's version check) that the
model must reject; a model whose mutants survive is not worth keeping.
