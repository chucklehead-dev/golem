# golem: testing

What testing exists, what each kind is for, and what is still owed. M1's
evidence is listed in [m1-plan.md](m1-plan.md); each test's invariant and
the function it checks are in the [orientation](walkthroughs/README.md#4-invariants).
The effort goes where a bug costs money, leaks data or breaks every caller
at once: admission, authorization, publication, streaming.

## 1. What runs in CI

`.github/workflows/ci.yml`, on every push:

| Job | What |
|---|---|
| `go` | `gofmt`; `go vet ./...`; `go test -race -shuffle=on ./...` against a real Valkey and a real ClickHouse (service containers); every benchmark run once; `golemctl config validate` on examples/standalone; ci/fuzz-smoke.sh (each fuzz target for 10 s) |
| `models` | `make -C model verify-tangled` (the committed `.qnt` match the literate `.md`), `make -C model check` (each model's typecheck, sampled simulation, scenarios and broken designs), then the connect tests: `TestPublicationModel`, `TestExposureModel`, `TestExposureModelValkey`, `TestAttemptsModel` |
| `compat` | test/compat: the official OpenAI and Anthropic SDKs (Python and Node) through a gateway to a stub provider |

`internal/archtest` (part of `go test`) checks the import rules: the
gateway links nothing of the control plane and no database, ClickHouse or
Kubernetes client; golemctl links `lib/` and the generated clients only.
`lib/contract`'s test fails when the codes and headers drift from
contracts.md; `internal/crdgen`'s when the chart's CRDs, DDL copy or
collector config drift from their sources.

## 2. The kinds of test

| Kind | Where it is used |
|---|---|
| Table and unit tests | everything with inputs and outputs: scanning, rights, pricing, periods, the compiler's problems, error rendering per dialect, the state's invariants, the control API's authorization |
| Fault and race tests | the request path against scripted providers (cuts, silence, error events, a provider killed under load: `TestFailoverDrill`); the store under concurrent writers and lost replies; Valkey down at start and mid-call; shutdown with streams in flight |
| `testing/synctest` | ejection and retry budgets, limiter windows, Entra key refresh, drain |
| Fuzzing | `FuzzObject` (jsonscan), `FuzzRewrite`, `FuzzChunking` and `FuzzStreamChunking` (dialects: chunking never changes usage), `FuzzFilter` (sse), `FuzzAuthenticate`, `FuzzMayHaveUsage`, `FuzzSingleCharError`; corpora checked in |
| Store suites | the limiter's stores (`TestMemStore`, `TestValkeyStore`) and their differential test (`TestStoresAgree`, `TestStoresAgreeVaried`); lib/bucket's conformance suite on the directory bucket and S3's client |
| Integration | ClickHouse: the ledger's DDL and the known-spend query (`TestClickHouseLedger`); test/ledger: real gateway telemetry through deploy/otel/collector.yaml into spend.sql, a duplicate counted once, a malformed row kept aside, a row surviving the collector's kill (skips without Docker and its endpoints) |
| Binaries | cmd/golem-gateway and cmd/golem-control in-process: pool mode over a directory bucket, Valkey with TLS and an ACL user, Entra down at start, the release gate, the drain; the subcommands (keygen, pubkey, restore, bootstrap) |
| Benchmarks | `BenchmarkHandler*`, `BenchmarkProduction` (the OTel SDK as production sets it up), `BenchmarkAdmitComplete` (Mem and Valkey); allocation counts are reviewed when they change, not gated |
| Ablation | every invariant test in m1-security-gates.md and m1-plan.md's Built table was run with its fix removed and seen to fail |

Every failing test names what to do next, as the code's errors do.

## 3. The models

Three literate Quint models in [model/](../model/README.md), each a
design-level check first (sampled simulation of a mid-sized instance: the
design holding every property, every scenario reached, each broken design
caught; simulation, not proof) and then a judge of the code through
gotogether's connect, the model's traces driving the real code:

| Model | Question | Connect test |
|---|---|---|
| [publication.md](../model/publication.md) | after a revocation, how long can a gateway serve the key; no lower publication applied; fencing, restore, lost replies | `TestPublicationModel` (gateway/snapshot): golem-control's `Store.Claim` and `Publisher`, a bucket, each gateway's `Bucket` source |
| [exposure.md](../model/exposure.md) | a scope shared by principals on several gateways, with snapshot lag, Valkey outages and cancelled calls, within its stated bound (ADR 0031) | `TestExposureModel`, `TestExposureModelValkey` (gateway/limit) |
| [attempts.md](../model/attempts.md) | one answer per caller, each attempt charged once, retries within the slots, an ejected deployment seen only by its probe (ADR 0051) | `TestAttemptsModel` (gateway/proxy) |

What the models found, kept as tests: a gateway restarted with an empty
disk refused its own trusted checkpoint (the floor is inclusive); a
publisher numbering from the pointer instead of the record; a probe left
standing after a readmission (`TestReadmissionEndsProbes`). Retired models
(accounting, freshness, leadership) and the exhaustive design matrices
are described in model/README.md; their ground is covered by exposure.md,
by ADR 0054's freshness (tested by `TestCheckout`), and by
control/publish's `TestGuard`.

## 4. Still owed

- **ADR 0041's M1 gates** that need a deployment: the denylist surviving
  Argo sync, prune and restarts; the restore and freshness tests for the
  git checkout against the real install (security-and-correctness.md §10).
- **Against real services**: S3 and KMS in the target account with the
  bucket policy that denies other writers; real Bedrock
  (docs/acceptance/m1-bedrock.md); a real vLLM box (m1-vllm.md); OTLP TLS
  to the real collector (golem#7).
- **Alerts** firing against recorded metrics, and the drills of m1-plan.md
  step 6.
- **Later** (M2): approvals and grants as a model when grants are built
  (ADR 0034); translation round-trip properties and goldens (M1.5); capture
  routing's negative tests (ADR 0041 §2); an end-to-end kind cluster with
  the charts.
