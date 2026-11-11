# golem: design

Status: draft. Read with [prd.md](prd.md); decisions are in [adr/](adr/).

## 1. Principles

The style is that of Filippo Valsorda's "Asynchronous Networking Patterns"
(GopherCon 2018) and the small proxy written from it
(github.com/posilva/go-proxy): a goroutine per connection, blocking code
that reads top to bottom, `io.Copy` where bytes need no inspection, and
exactly as much parsing as the decision needs (that proxy reads the TLS
ClientHello with `cryptobyte` for the SNI and nothing more). Updated:

- **Parse only what routing needs.** The request body is scanned with
  `encoding/json/jsontext` for `model`, `stream`, `max_tokens` and a few
  fields, not unmarshalled. The model name is rewritten by splicing bytes.
  A same-dialect request is never decoded into a struct (ADR 0001).
- **Stream, don't buffer.** Responses are copied as they arrive and
  flushed through `http.ResponseController`; usage is read from the stream
  by a tee'd scanner, not by buffering it.
- **Immutable config.** One validated snapshot behind an
  `atomic.Pointer`; a request takes the pointer once and uses it to the
  end. No locks on the request path for config (ADR 0002).
- **Standard library first.** Dependencies only where the standard library
  has no answer (OTel, CEL, JWT/JOSE, Redis, Kubernetes client), each behind
  a small interface in one package.
- **Context everywhere, goroutines owned.** Every goroutine is started by
  `sync.WaitGroup.Go` or an errgroup owned by its request or the server, and
  ends with it. A goroutine-leak profile is checked in tests.
- **Errors say what to do next** and are rendered in the caller's dialect
  (an OpenAI-shaped error to an OpenAI caller, Anthropic-shaped to an
  Anthropic one).

## 2. Shape

```
request:   listener (TLS/mTLS) → frontend (dialect, scan) → authn (Principal)
           → OnRequest hooks → router (alias, pool, deployment) → limits (reserve)
           → attempt loop (retry, failover; backend dialect) → provider
response:  provider → backend dialect → usage + cost (reconcile) → frontend encode → caller
admin:     /livez /readyz /healthz /startupz, pprof, config dump, explain, key API
config:    file | Kubernetes CRDs → compile + validate → atomic snapshot
state:     Redis/Valkey (limits, budgets, cache; M2), Postgres (keys, ledger; M2)
telemetry: OTLP traces, metrics, logs → the enterprise collector
```

## 3. The request path

1. **Accept.** `net/http.Server` with `Protocols` set for HTTP/1.1 and
   HTTP/2 (h2c behind a mesh). Header limits including
   `Server.MaxHeaderValueCount` (1.27). `ReadHeaderTimeout` set; no
   `WriteTimeout` (streams are long), instead per-phase deadlines through
   `ResponseController.SetWriteDeadline`, renewed per chunk.
2. **Frontend.** `ServeMux` method-and-path patterns select the dialect
   (`POST /v1/chat/completions`, `POST /v1/messages`, ...). The body is read
   into a pooled buffer with a size cap (`http.MaxBytesReader`). A
   `jsontext.Decoder` scan extracts the routing fields; it stops early once
   it has them unless a hook or translation needs the whole tree.
3. **Authenticate** (ADR 0003). The chain yields a `Principal` or an error
   in the caller's dialect. Results for keys and opaque tokens are cached
   by credential hash with a TTL bounded by the credential's expiry.
4. **OnRequest hooks**, selected by precompiled CEL programs.
5. **Route.** Alias to route; CEL conditions; authorization check; pool
   selection by strategy (affinity key first, then the strategy among
   healthy deployments).
6. **Reserve** the estimated cost against every limit that applies, in one
   call to the limiter (local, or one Redis script round trip in M2).
7. **Attempt loop.** For each attempt: build the upstream request (fast
   path: same bytes, spliced model name; else translate), sign or
   authenticate it for the provider, send it on the deployment's
   `http.Transport`. Retry or fail over on a retryable outcome *before the
   first response byte*; after it, the attempt is committed.
8. **Respond.** Fast path: copy headers through an allowlist, copy the
   body through a tee whose other side is an SSE scanner feeding the usage
   extractor; flush per event. Translated: decode upstream events, encode
   frontend events.
9. **Reconcile** the reservation with actual usage; price it from the
   catalog; record metrics and the access log; queue `OnComplete` hooks and
   the ledger record (bounded queue, dropped records counted).

Shadow requests (M2) branch at step 7 onto their own goroutine with a
context detached from the caller's cancellation
(`context.WithoutCancel`) but with its own deadline, and a semaphore
bounding shadow concurrency so mirroring cannot exhaust the pool.

## 4. Packages

```
cmd/golem/            main: flags, signals, wiring; nothing else
internal/config/      the snapshot types (one schema for file and CRDs), compile, validate
internal/config/file  file source (fsnotify-free: poll or ConfigMap symlink swap, via os.Root)
internal/config/kube  CRD source (client-go informers), Secret watching
internal/dialect/     one subpackage per dialect: openai/chat, openai/responses,
                      anthropic/messages, gemini, bedrock; each: scan, errors, SSE events,
                      usage extraction; translation via ir/
internal/dialect/ir   the intermediate representation used only for translation
internal/auth/        Authenticator interface, chain, Principal; keys, jwt, introspect, mtls
internal/route/       aliases, pools, strategies, affinity, ejection, retry budget
internal/limit/       limiter interface; local GCRA/sliding windows; redis implementation
internal/price/       model catalog, pricing, the vendored price list
internal/hook/        phases, in-process registry, remote (gRPC/HTTP) client, built-ins
internal/upstream/    per-deployment transports, provider auth (keys, SigV4, Entra, GCP)
internal/proxy/       the request path (§3): the only package that knows the order of steps
internal/telemetry/   OTel setup, semconv attribute helpers, access log, flight recorder
internal/admin/       probes, admin API, config dump, explain
internal/ledger/      async spend records to Postgres or OTel logs
api/v1alpha1/         CRD Go types (kubebuilder markers), generated CRD YAML
deploy/helm/          chart
```

`proxy` depends on interfaces from the others; nothing depends on `proxy`.
Each dialect package is independent of the others and of `ir` except for its
translation file, so the fast path never links the translation code's
behaviour into a same-dialect request.

## 5. Go 1.27 facilities used

| Facility | Release | Where |
|---|---|---|
| `encoding/json/v2`, `encoding/json/jsontext` (GA; v1 now backed by v2) | 1.27 | Token-level body scans, streaming SSE payload decode, `omitzero` on generated types |
| Generic methods | 1.27 | Typed accessors on the snapshot (e.g. `Snapshot.Lookup[T]`) where they replace an interface switch |
| `uuid` package | 1.27 | Request IDs, key IDs (no third-party UUID module) |
| `testing/synctest` (`Test`, `Wait`, `Sleep`) and `httptest.NewTestServer` in-memory network | 1.25 / 1.27 | Timeouts, retries, ejection, drain and limiter windows tested on fake time, no sleeps |
| HTTP/1 response body drained on close | 1.27 | Better upstream connection reuse when a caller disconnects mid-response |
| `Server.MaxHeaderValueCount`; HTTP/2 RFC 9218 priorities | 1.27 | Inbound limits; priority classes (M3) |
| Green Tea GC (default), size-specialized small allocations | 1.26 / 1.27 | Free: lower GC cost for many small per-request objects |
| Container-aware GOMAXPROCS | 1.25 | Free; do not set GOMAXPROCS in the chart |
| `runtime/trace.FlightRecorder` | 1.25 | Snapshot traces of slow requests (§5.7 of the PRD) |
| `goroutineleak` profile (GA) | 1.27 | Exposed on admin pprof; checked in tests |
| Goroutine labels in tracebacks | 1.27 | Request ID and route as pprof labels show up in crash dumps |
| `sync.WaitGroup.Go`, `errors.AsType`, `slog.NewMultiHandler` | 1.25 / 1.26 | Goroutine ownership; typed error matching; slog to stderr and OTel at once |
| `new(expr)` | 1.26 | Optional pointer fields in config and API types |
| `os.Root` | 1.24+ | Reading config and mounted secrets without path traversal |
| `http.CrossOriginProtection` | 1.25 | Admin API (browser-reachable via port-forward) |
| `httputil.ReverseProxy.Rewrite` (Director deprecated) | 1.26 | Raw pass-through routes only (§5.1.4); the main path is its own loop |
| Hybrid post-quantum TLS key exchange default; ML-DSA in TLS 1.3, `crypto/mldsa` | 1.26 / 1.27 | Inbound and upstream TLS, nothing to configure |
| FIPS 140-3 native module | 1.24+ | `fips140=on` mode for regulated deployments |
| `runtime/secret` | experimental | Not used until GA; noted for zeroing upstream keys |

Extended libraries: `golang.org/x/sync` (errgroup, singleflight for JWKS
and introspection fetches, semaphore for shadow concurrency),
`golang.org/x/net/http2` only if a server knob is missing from net/http.

Third-party, each confined to one package: OpenTelemetry Go SDK and
`otelslog` bridge (telemetry), `cel-go` (route, hook), `go-jose/v4`
(auth/jwt), `valkey-go` or `rueidis` (limit/redis), `pgx/v5` (ledger,
keys), `client-go` and `sigs.k8s.io/yaml` (config/kube, config). The data
path's core (proxy, dialect, route, price) imports none of them except
OTel's API.

## 6. Performance notes

- The fast path allocates per request: the pooled body buffer (returned),
  the Principal (cached), one span pair, one access-log record. Target
  well under 50 allocations; tracked by `testing.B.Loop` benchmarks in CI.
- Transports are per deployment with `MaxIdleConnsPerHost` sized to the
  deployment's expected concurrency; HTTP/2 to providers that support it.
- Upstream TLS sessions resumed; DNS cached by the resolver, not golem.
- SSE scanning reads lines from the tee without copying the payload when
  it is not a usage-bearing event (look for the event type first).
- Token estimation for reservations uses a cheap byte-count heuristic per
  model family by default; exact counting (provider `count_tokens` or a
  local tokenizer) is opt-in per limit where precision matters.
- Benchmarks against a stub upstream, with `pprof` and the flight recorder,
  belong in M0 so every later feature is measured against the baseline.

## 7. Testing

- Unit tests per dialect from recorded provider fixtures (requests,
  responses, SSE streams, errors), including truncated and malformed
  streams.
- Translation tested round-trip and against provider SDK conformance
  fixtures.
- `synctest` for anything with time: retry backoff, ejection windows,
  limiter windows, drain, idle-chunk timeouts.
- Fuzzing (`go test -fuzz`) the body scanners and SSE parser.
- The router, limiter and failover loop are a good fit for deterministic
  simulation with fault injection (lost responses, 429 storms, slow first
  byte, a provider recovering): gotogether's qenv doubles can drive them,
  with a small Quint model of "never double-charge, never replay a started
  stream, never exceed a hard budget by more than one in-flight
  reservation" as the judge. Worth doing once M1's routing settles.
- End-to-end: kind cluster with the chart, a stub provider, and the real
  OpenAI and Anthropic SDKs (and a coding agent) as clients.
