# golem: the gateway (data plane)

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Implementation remains outstanding.

Status: draft, third pass. The system's components and how they fit are in
[architecture.md](architecture.md); this document is the gateway's inside.
The gateway does data-plane work only: it serves model requests. It has no
API for changing anything, no database, no Kubernetes client, and it never
calls the control plane: its input is its pool's snapshots in object
storage (ADR 0006).

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
  A same-dialect request is never decoded into a struct (ADR 0001). Member
  names are compared unescaped (`"mod\u0065l"` is `model`), and a body's
  buffers are released once it has been sent, not when the stream ends
  (docs/spikes/fast-path.md).
- **Stream, don't buffer.** Responses are copied as they arrive and
  flushed through `http.ResponseController`; usage is read from the stream
  by a tee'd scanner, not by buffering it.
- **Immutable config.** One validated snapshot (routes and entitlements)
  behind an `atomic.Pointer`, polled from object storage where the
  control plane publishes it (ADR 0006); a request takes the pointer once and uses
  it to the end. No locks on the request path for config.
- **Standard library first.** Dependencies only where the standard library
  has no answer (OTel, CEL, JWT/JOSE, the RESP client, provider signers),
  each behind a small interface in one package.
- **Context everywhere, goroutines owned.** Every goroutine is started by
  `sync.WaitGroup.Go` or an errgroup owned by its request or the server, and
  ends with it. A goroutine-leak profile is checked in tests.
- **Errors say what to do next** and are rendered in the caller's dialect
  (an OpenAI-shaped error to an OpenAI caller, Anthropic-shaped to an
  Anthropic one).

## 2. Shape

```
request:   ztunnel (mesh mTLS) → listener → frontend (dialect, scan) → authn (Principal, scope)
           → Request hooks → router (alias, pool, deployment) → limits (check: known spend, concurrency, rate, quota)
           → attempt loop (retry, failover; backend dialect) → provider (direct or egress gateway)
response:  provider → backend dialect → usage + cost (reconcile) → frontend encode → caller
inputs:    snapshots/<pool>/current in object storage → fetch by hash → compile → atomic Snapshot (ADR 0006)
           or, standalone (ADR 0023), CRD YAML in a local directory compiled in-process
state:     the site's Valkey (spend since the snapshot, rate windows, concurrency slots, provider quota shares) (ADR 0031)
outputs:   OTLP → collector (ADR 0015); captured traces → the scope's Langfuse project (ADR 0016);
           status/<pool>/<id> in object storage (applied hash, compile errors)
local:     /livez /readyz /healthz /startupz, pprof, the snapshot hash (admin port; explain is golem-control's)
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
   `jsontext.Decoder` scan extracts the routing fields; it scans the complete
   document and rejects duplicate members before forwarding; unknown fields
   are forwarded and logged by name, policy-relevant ones enforced
   (ADR 0037). Hook rewrites
   trigger policy and capability revalidation.
3. **Authenticate** (ADR 0009). The chain yields a `Principal` (user or
   service, scope, key) or an error in the
   caller's dialect. Keys are
   looked up by hash in the entitlements snapshot (no network call); Entra
   JWTs are verified locally against cached JWKS. Effective rights are
   computed from the snapshot: scope policy ∩ membership ∩ key constraints
   (org model §3).
4. **Request hooks**, selected by precompiled CEL programs; they may
   reject, rewrite, or add routing hints (ADR 0014).
5. **Route.** Alias to route; CEL conditions; authorization check; pool
   selection by strategy (affinity key first, then the strategy among
   healthy deployments).
6. **Check** (ADR 0031), in one Valkey round trip: known spend (the
   snapshot's, plus the site's counter since) against the key cap, member
   cap and scope pool, with the request's worst-case cost added in strict
   scopes (bounded by the scope's `max_tokens` and input-size ceilings,
   input tokens by byte count: ADR 0038); a concurrency slot for the
   principal, renewed every 30 seconds while the call is in flight; rate limits and the
   deployment's provider quota share. Refusals are 402 (budgets) or 429
   (limits).
7. **Attempt loop.** For each attempt: build the upstream request (fast
   path: same bytes, spliced model name; else the frontend's decoder and the
   backend's encoder through `canon`), wrap it in the deployment's envelope
   (URL, model placement, auth, stream framing), send it on the deployment's
   `http.Transport`. Retry or fail over on a retryable outcome *before the
   first response byte*; after it, the attempt is committed.
8. **Respond.** Fast path: copy headers through an allowlist, copy the
   body through a tee whose other side is an SSE scanner feeding the usage
   extractor; flush per event. Translated: decode upstream events, encode
   frontend events.
9. **Record**: price the actual usage from the catalog; add it to the
   site's spend counters; release the concurrency slot; record metrics and
   the access log record (which is also the
   ledger's input, ADR 0015); queue `Complete` hooks (bounded queue,
   dropped work counted).

Shadow requests (M2) branch at step 7 onto their own goroutine with a
context detached from the caller's cancellation
(`context.WithoutCancel`) but with its own deadline, and a semaphore
bounding shadow concurrency so mirroring cannot exhaust the pool.

## 4. Packages

All under `gateway/` (architecture.md §4 gives the repository and the
import rules that keep the gateway free of control-plane code):

```
cmd/golem-gateway/     main: flags, signals, wiring; nothing else
gateway/proxy/         the request path (§3): the only package that knows the order of steps
gateway/dialect/       one subpackage per dialect: openai/{chat,responses,embeddings,decisions},
                       anthropic/messages, typesafe/systemone, bedrock/converse; each: the
                       pass-through scan and splice, errors, stream events, usage extraction,
                       and its adapter pair (decode to, encode from the canonical model)
gateway/canon/         the canonical model (requests, responses, stream events); internal, never an API
gateway/envelope/      how hosts carry a dialect: anthropic, bedrock (InvokeModel, event-stream
                       framing, SigV4), vertex, foundry, openai, vllm
gateway/auth/          Authenticator, chain, Principal; keys, entra (JWT)
gateway/snapshot/      the compiled Snapshot (routes + entitlements), the object-storage poller,
                       the file source, the on-disk last-good copy, status writes
gateway/route/         aliases, pools, strategies, affinity, ejection, retry budget
gateway/limit/         Limiter: known-spend checks, worst-case cost, concurrency slots, GCRA rate windows; Valkey and in-memory implementations
gateway/price/         model catalog lookups and pricing (the catalog comes in the snapshot)
gateway/hook/          phases, in-process registry, remote client, built-ins
gateway/upstream/      per-deployment transports, provider auth (keys, SigV4, Entra, Roles Anywhere)
gateway/telemetry/     OTel setup, semconv helpers, access log, flight recorder
gateway/local/         probes, pprof, snapshot hash
```

`proxy` depends on interfaces from the others; nothing depends on `proxy`.
Each dialect package depends only on `canon` (for its adapters), never on
another dialect; envelopes depend on neither. A translated request is
`encode(backend, decode(frontend, req))` wrapped by the backend's
envelope, assembled by `proxy`.

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
| `runtime/trace.FlightRecorder` | 1.25 | Snapshot traces of slow requests (PRD §5.8) |
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
(auth/entra), `valkey-go` (limit), `aws-sdk-go-v2`'s SigV4 signer and credential providers only
(upstream/bedrock), `azidentity` (upstream/azure), the blob adapters'
read path (snapshot), protobuf for the snapshot format. No `pgx`, ClickHouse, Graph or
Kubernetes client: the import test (architecture.md §4.2) fails if one
appears. The core (proxy, dialect, route, price) imports none of these
except OTel's API.

## 6. Snapshot lifecycle

- At start the gateway loads its last-good snapshot from disk (an
  `emptyDir` or a small PVC) if present, then polls its pool's pointer in
  object storage. It is ready once it holds a snapshot no older than a
  configured bound (only while its signed authorization lease is valid;
  internal exceptions require an explicit approved maximum).
- Every second (±20% jitter, full-jitter backoff on errors: ADR 0032): a
  conditional GET of `snapshots/<pool>/current`. On a
  change it verifies the pointer's signature against the pinned public key
  and that its `publish_seq` is at least its floor and above the snapshot it
  holds (the trusted publisher checkpoint and atomically persisted local
  checkpoint; status is diagnostic and cannot set the trusted floor), then fetches the snapshot by hash and verifies its signature and
  hash (ADR 0007). Anything unsigned, wrongly signed or older is ignored and
  alarmed. Once the signed authorization lease has expired (five minutes
  by default, ADR 0030) the gateway refuses new model requests.
- An **emergency denylist** (a ConfigMap mounted as a file, read with
  `os.Root`) is applied over every snapshot: a site admin's way to revoke a
  key or principal when the control plane or the bucket is unreachable.
- Each new snapshot is compiled (CEL, lookup maps) off the request path and
  swapped in; a failure to compile keeps the previous snapshot.
- After each apply (or failure) the gateway writes
  `status/<pool>/<gateway-id>`: hash, versions, compile errors, time.
  golem-control reads these for `Applied n/m` and `golemctl gateways`.
- Runtime state that must survive updates (transports, ejection state,
  EWMAs, affinity tables) is keyed by deployment identity, not held in the
  snapshot.

## 6a. Standalone mode

Built (M0, first part): `gateway/snapshot` (the holder: floor and lease
enforced, transports kept across swaps; the directory source through
`os.Root`), `lib/compile` and `lib/api/v1alpha1` (Backend and ModelRoute so
far), and `golemctl config validate`. The bucket source, the on-disk
last-good copy, status writes and the denylist are next.

`golem-gateway --standalone <dir>` (ADR 0023) runs this binary on a laptop
from the same CRD YAML as the GitOps repository, compiled in-process by
`lib/compile` and recompiled on change, with one implicit local principal
(or a `LocalIdentity` file), the in-memory limiter, the developer's own
provider credentials, telemetry to stdout or a local OTLP endpoint, and
`--explain`. It binds to localhost, refuses edge, site and bucket
settings, and changes nothing in the request path.

## 7. Performance notes

- The fast path allocates per request: the pooled body buffer (returned),
  the Principal (cached), one span pair, one access-log record. Target
  well under 50 allocations; tracked by `testing.B.Loop` benchmarks in CI.
- Transports are per deployment with `MaxIdleConnsPerHost` sized to the
  deployment's expected concurrency; HTTP/2 to providers that support it.
- Upstream TLS sessions resumed; DNS cached by the resolver, not golem.
- SSE scanning reads lines from the tee without copying the payload when
  it is not a usage-bearing event (look for the event type first).
- Token estimation for strict scopes' worst-case check uses a cheap byte-count heuristic per
  model family by default; exact counting (provider `count_tokens` or a
  local tokenizer) is opt-in per limit where precision matters.
- Benchmarks against a stub upstream, with `pprof` and the flight recorder,
  belong in M0 so every later feature is measured against the baseline.

## 8. Testing

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
  stream" as the judge. Worth doing once M1's routing settles.
- Budget enforcement is judged by `model/accounting.md`: no double count,
  books matching after reconciliation, overshoot within ADR 0031's bound.
- Limit scripts tested against Valkey containers in CI.
- End-to-end: kind cluster with Istio ambient, the chart, a stub provider,
  ClickHouse, and the real OpenAI and Anthropic SDKs (and a coding agent)
  as clients.
