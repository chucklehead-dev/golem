# golem: testing

Status: draft. How golem is tested, chosen by value: the most effort where
a bug costs money, leaks data, or breaks every caller at once (budgets,
authorization, content capture, streaming, the distribution of config),
and the least where the compiler and a table test suffice.

## 1. What could hurt, and what catches it

| Risk | Example | Main defence |
|---|---|---|
| Overspend or double charge | a retry reserves twice; a crashed gateway's reservation never expires; a lease rebalance races consumption | Quint models of the budget and lease protocols (§3), Hegel state machines against them, simulation with faults |
| Rights wider than intended | a key outlives its owner's membership; a narrow override widens capture | Hegel properties over random org histories; table tests of the resolution rules |
| Content in the wrong place | capture on for a scope whose policy forbids it; a trace not deleted at expiry | Properties of capture resolution; an end-to-end test that checks Langfuse and ClickStack for content |
| Broken streams | usage missed when a stream ends early; a dialect error event malformed; a translated tool call reordered | Goldens from recorded provider streams; fuzzing the SSE parser and scanners; translation properties |
| Stale or inconsistent config | a gateway misses a change (a late DSQL commit); a snapshot built from a half-read change | A model of the change-log feed; Hegel: a snapshot rebuilt after any sequence of changes equals one built from scratch |
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
| Time | retries, backoff, ejection windows, limiter windows, drain, idle-chunk timeouts, lease expiry | `testing/synctest`, no sleeps |
| Store suite | every repository method and transaction, conflicts included | Postgres containers per change, DSQL nightly (control-plane.md §8) |
| Contract tests | each provider backend against its recorded fixtures; each RESP engine (Valkey, Dragonfly) with the real scripts | Containers in CI |
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
| **Regional leases** (ADR 0016) | leases per region, consumption, rebalance, synchronous top-up, partition from the control plane | `Σ leases + spent ≤ available` always; a partitioned region spends only what it holds; after a settle, unused lease returns (eventually, on a settled run) |
| **Change-log distribution** (control-plane.md §4) | transactions committing out of timestamp order, polling with lookback, replica failover, gateway reconnects | every committed change is in a snapshot that reaches every watching gateway; a gateway's snapshot is always one the control plane built; revocation within the bound after a settle |
| **Attempt loop** (ADR 0007) | attempts, first byte, failover, retry budget, ejection | no retry after the first byte; retries bounded by the budget; an ejected deployment receives nothing until probed back |
| **Approvals and grants** (org-model §4.3) | request, approve, reject, withdraw, expire, period rollover | a grant takes effect exactly once; no approval of one's own request; withdrawal and approval cannot both win |

Each starts small (two regions, two gateways, one pool) and stays a
design-level check: the model alone first (scenarios, safety, then
liveness), before any code is bound to it.

### 3.2 Where gotogether can be trialled during development

1. **Limiter and leases in simulation (M1).** Run the limiter, the lease
   client and a reconcile loop on `qenv` with a `qenv.Store`-backed double
   of the RESP store (scripts as atomic transactions), gateways crashing
   and partitions injected; `qtest.System.Explore` judged by the reserve
   and lease models. This is where a bug costs the most and is hardest to
   find otherwise.
2. **The store under DSQL semantics (M1).** The service layer's
   transactions on `qenv.Store` at `SnapshotIsolation` (DSQL's
   optimistic, first-committer-wins behaviour) and with commit faults:
   pool arithmetic and key caps must hold, and the conflict-forcing version
   rows must be shown necessary (ablate them; the test must fail).
   `qhistory` can then check what isolation the real DSQL and Postgres
   actually give, from a list-append workload, before relying on it.
3. **The attempt loop over simulated providers (M1).** The gateway's
   upstream calls through `qenv/qhttp`, with lost responses, slow first
   bytes, 429s and mid-stream cuts as faults, judged by the attempt-loop
   model.
4. **The distribution feed (M1–M2).** golem-control replicas and gateways
   in one simulation, late commits and reconnects as faults, judged by the
   distribution model; a settle checks revocation latency.
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
   `sqlstore`; DSQL untested), so it is a trial, not a dependency, until
   it proves itself.

The trial is judged as any test is: by what it catches. Each model's
checks are proved by planted mutants (a reserve that skips one level, a
rebalance that ignores in-flight use, a poll with no lookback) that the
model must reject; a model whose mutants survive is not worth keeping.
