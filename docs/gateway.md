# golem: the gateway (data plane)

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. What is built: the code's
[orientation](walkthroughs/README.md) and the "Built" notes; release
evidence is tracked in golem#7.

Status: §2–§4a describe what is built (M1); the rest mixes the built and
the planned, each marked. M1's scope is [m1-plan.md](m1-plan.md). The system's components and how they fit are in
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
  has no answer (OTel, JOSE, the RESP client, the AWS SDK's signer; §5),
  each behind a small interface in one package.
- **Context everywhere, goroutines owned.** Every goroutine is started by
  `sync.WaitGroup.Go` or an errgroup owned by its request or the server, and
  ends with it. A goroutine-leak profile is checked in tests.
- **Errors say what to do next** and are rendered in the caller's dialect
  (an OpenAI-shaped error to an OpenAI caller, Anthropic-shaped to an
  Anthropic one).

## 2. Shape

```
request:   ztunnel (mesh mTLS) → listener (guard) → authenticate (Principal, scope) → read and scan the body
           → route (alias, pool) → policy, betas, rate limits → admission (known spend, slots)
           → attempt loop (failover before the commit) → provider (direct, or Bedrock's envelope)
response:  provider → usage read from the stream as it passes → caller; the ledger row; the ticket ended
inputs:    snapshots/<pool>/current in object storage → fetch by hash → compile → atomic snapshot (ADR 0006)
           or, standalone (ADR 0023), config YAML in a local directory compiled in-process
state:     the site's Valkey, over TLS with AUTH: in-flight slots and request rate buckets (ADR 0031);
           known spend is the snapshot's
outputs:   OTLP to the pod's collector sidecar on loopback (ADR 0043): spans, metrics, ledger rows;
           status/<pool>/<id> in object storage (applied hash, compile errors)
local:     /livez /readyz, pprof
```

Hooks (ADR 0014), translation through `canon` (M1.5) and shadow requests
are not built; their places in the path are marked in the code.

## 3. The request path

As built: `gateway/proxy`'s `Handler.ServeHTTP`, a step at a time. The
code's walk through it, with each step's function, is the
[orientation](walkthroughs/README.md#2-a-requests-path-through-the-gateway).

1. **Accept.** `net/http.Server` for HTTP/1.1 and HTTP/2 (h2c). The chart
   has the mesh sidecar connect over HTTP/1.1, which costs about a third of
   h2c's server CPU per streamed event. Headers bounded to 64 KiB; `ReadHeaderTimeout` 10 s; idle
   keep-alives closed after two minutes; HTTP/2 at most 256 streams per
   connection with pings after 30 s of silence. No `WriteTimeout`
   (streams are long): one watchdog per call cuts a write to the caller
   that has been in progress longer than `WriteStall` (30 s; enforced within
   1–1.5 times it), so a client that stops reading is a caller gone. Nothing
   is watched while the provider is silent. Connections are bounded
   by `guard.Listener`: `-max-conns` (8192) open at once, and
   `-max-conns-per-source` (off) from one address.
2. **The floor before authentication** (`gateway/guard`). Each request is
   taken from its source's request rate (`-source-rate`, off by default),
   and each failed authentication from its source's failure budget
   (`-auth-failures`, 20 a second, burst 200). Only a refusal that is the
   credential's fault counts: a key unknown, revoked, expired or of
   another installation, a token invalid or expired, none sent; never
   `gateway.not_ready`, a token refused for golem's copy of Entra's keys
   (too old, or missing the signing key), or a valid credential refused by
   policy. A source past its failures still has a golem key looked up, and
   **a valid key always passes**; its invalid ones and its Entra tokens
   (before their signature check) are refused `limit.source` with a
   Retry-After. The source is the connection's address, or with
   `-source-header` the last value of a header the proxy in front sets.
   Behind a sidecar every connection is the sidecar's, so the gateway
   chart turns the failure budget **off unless `guard.sourceHeader` is
   set**. At most 65,536 sources are kept; past that, new ones are not
   limited (logged at most once a minute) rather than sharing one bucket.
3. **Authenticate** (ADR 0009). A credential sent twice is refused first.
   Golem keys are looked up by hash in the snapshot; Entra JWTs are
   verified locally against cached keys (fetched at start, not required to
   start: until then tokens are `gateway.not_ready` and golem keys work).
   Rights come from the snapshot: scope policy ∩ membership ∩ key
   constraints (org-model.md §3). An expired authorization lease refuses
   every request (`gateway.authorization_expired`).
4. **Read the body, bounded.** A `Content-Length` over the scope's
   `maxInputBytes` is refused unread. The bodies being read at once are
   bounded per gateway (`-max-reading`, 512 MiB of bytes) and per principal
   (`-max-reading-per-principal`, 8 bodies); past either, `limit.concurrency`
   with Retry-After 1. The body is read into a pooled buffer with a cap
   (32 MiB by default; zero is the default, not "no body"), within a body
   timeout.
5. **Scan.** A `jsontext` scan of the whole document extracts the routing
   and policy fields; duplicate members, trailing data and nesting past 128
   are refused, names compared unescaped. Unknown fields are forwarded,
   policy-relevant ones enforced (ADR 0037). **File and item references
   are refused in both dialects**, escaped or nested (`jsonscan.Objects`):
   in Responses a non-null `file_id` anywhere in `input` or
   `instructions`, an `item_reference`, `previous_response_id`,
   `conversation`, `background`, a stored `prompt`, `store: true`; in
   Messages a content block whose source is a file or which names a
   `file_id`. Each is `authz.provider_storage_forbidden`: golem refers to
   nothing a provider keeps (ADR 0027).
6. **Route.** The alias, within the principal's models, to its route; the
   route's feature mode (restricted or compatible: approved betas and API
   versions), then the scope's policy (`maxOutputTokens` over `max_tokens`
   × `n`, `maxInputBytes`, provider storage; server-side tools are
   detected but admitted, ADR 0031). The pool is
   narrowed to the deployments that accept the request's `anthropic-beta`
   values (ADR 0028, betas only); none, `route.no_capable_deployment`,
   before anything is counted.
7. **Rate limits**: the member's and the scope's requests-a-minute
   buckets in Valkey (a ScopePolicy's `rateLimits`), taken together once a
   call, `limit.rate` naming which, in every budget mode; with Valkey
   unreachable they fail open. Requests only: token limits are the
   provider's 429s, which the attempt loop fails over on.
8. **The first deployment**: the pool in order, each through its health
   (`route.Health`: `maxInFlight`, `maxInFlightPerPrincipal`, ejection, one
   probe at a time). A call every deployment passes over for its
   principal's own share is told `limit.concurrency`; otherwise
   `upstream.unavailable` with the earliest Retry-After.
9. **Admission** (ADR 0031), one Valkey round trip
   (`gateway/limit`, behind `gateway/admission`'s `Admitter`): the scope's
   known spend, **the snapshot's alone**, against its budget, and the
   principal's (counted per scope) and the scope's in-flight slots, renewed
   every 30 s while the call runs. Under `enforce` a spent budget is
   `budget.pool_exhausted` (402); under `observe` and `warn` the call is
   admitted and its row marked `over_budget`, `warn` adding `golem-warning`.
   In-flight limits refuse in every mode (`limit.concurrency`, 429).
   **There are no member or key caps, no provider quota shares and no
   declared-worst-case check** (ADR 0031). A store call is bounded
   (250 ms) and a caller leaving during it leaves no slot behind. With
   Valkey unreachable the gateway admits on its own in-flight counts, each
   admission a waiver, re-registers its calls when Valkey returns, and is
   ready all the same. Only an admitted call credits the route's retry
   budget. From here the call's ticket ends exactly once.
10. **Attempt loop.** The body's `model` is spliced for the deployment;
    only allowlisted headers cross (an Anthropic call's `anthropic-version`
    and `anthropic-beta`; **an OpenAI call forwards none, so a caller's
    `OpenAI-Organization` is not forwarded**: the Backend's credential
    decides the organisation billed); the deployment's transport (or
    Bedrock's envelope) sends it. Retried, on a deployment the call has not
    tried (ADR 0051), only an attempt the provider never saw (a dial, DNS
    or TLS failure, or **a Bedrock request that never left the gateway**:
    a body, rewrite or signing failure, `envelope.ErrNotSent`) or refused
    before any output (429, 503, 529, a stream whose first event is an
    error), within the route's `maxAttempts` and retry budget. A stream
    commits at its first event; once committed nothing is retried (ADR
    0002). An attempt sent and unanswered ends the call, charged its
    declared worst case as uncertain. **A caller gone, or a call withdrawn
    by a sweep, is no deployment failure**: its attempt's lease is released,
    not failed, so cancelled calls cannot eject a healthy deployment.
11. **Respond.** Headers through an allowlist; the body copied as it
    arrives, the same bytes through the dialect's usage reader, flushed per
    event. **A stream that carried its provider's error event after output
    is a failed call** (`upstream.stream_failed`, of the provider; its
    billing unchanged). A cut stream gets the contract's error event and an
    abort, so no client takes it for whole. A non-streamed response is kept
    up to 1 MiB to read its usage; past it, estimated.
12. **Record.** The usage priced from the snapshot's price list (integer
    micro-units, half-even); the ledger row written; then the ticket ended:
    the slot freed, or, after an ambiguous end (uncertain, cut, abandoned),
    held for the provider's tail (`ProviderTail`, 2 minutes, at most to the
    call's deadline). Usage the provider did not report is estimated high:
    every byte received a token, at most `max_tokens` × `n`; every request
    byte a token (golem#3, golem#4).

A snapshot or denylist entry that withdraws a call's grant (a key
revoked, its principal gone from the scope or from golem, a key or
principal denied) ends that call within seconds in its dialect's error,
charged what was produced, its row `terminated`; lease expiry, a key's own
expiry and a scope losing a model end none.

## 3a. Endpoints beside the calls

- `POST /v1/responses` (`openai.responses`): the Responses API, stateless
  only (security-and-correctness.md §4, whatever the scope allows): store
  written false (an absent store is the provider's true); the refusals of
  §3 step 5, before any provider call; the stored object endpoints
  (`/v1/responses/{id}`, its cancel and input items) refused alike. Usage
  from the terminal event (`response.completed`, `.incomplete`,
  `.failed`) or the whole response; an `error` event first is refused
  before the commit.
- `GET /v1/models`, `GET /v1/models/{id}` (`ServeModels`): the aliases the
  caller's scope may call, of the dialect it calls with, in that dialect's
  shape (Anthropic's when it sends anthropic-version). Another scope's
  model is `route.unknown_model`, not forbidden. Authenticated; nothing
  admitted or charged.
- `POST /v1/messages/count_tokens` (`ServeCountTokens`): **held to a
  call's checks, short of admission**: authenticated, the scope's alias,
  the route's features, the betas check (`route.no_capable_deployment`),
  the scope's input ceiling, a request from its rate limits, and the
  shared health (ejections, ceilings, the principal's share); sent to the
  route's first deployment that takes it, at its count endpoint (Bedrock's
  CountTokens through the envelope, answered in Anthropic's shape). Free:
  no admission, no slot, no ledger row.

## 3b. Local boxes (vLLM)

A vLLM box is a backend of the dialect it serves, each box a deployment
(`spec.network.allowPrivateAddresses` for a private address). Three
per-deployment fields are for a shared box:

- `maxInFlight`: the box's static concurrency. Past it the attempt is not
  sent; the call goes to the pool's next deployment.
- `maxInFlightPerPrincipal`: one principal's share. Past it, the same; a
  principal every deployment passes over for its share is told
  `limit.concurrency`.
- `healthCheck: {path, intervalSeconds}`: one checker per gateway GETs the
  path with the backend's credential (default every 10 s, each check
  bounded by 5 s). A failure ejects the box; a 2xx after the ejection
  readmits it. A pass during an ejection does not end it.

```yaml
apiVersion: golem.dev/v1alpha1
kind: Backend
metadata: {name: dgx}
spec:
  dialect: openai.chat
  network: {allowPrivateAddresses: true}
  auth: {header: Authorization, scheme: Bearer, secretRef: {name: dgx, key: api-key}}
  deployments:
  - name: dgx-1
    url: https://dgx-1.lan:8000/v1/chat/completions
    model: local-coder
    maxInFlight: 32
    maxInFlightPerPrincipal: 8
    healthCheck: {path: /v1/models}
---
apiVersion: golem.dev/v1alpha1
kind: ModelRoute
metadata: {name: coder}
spec:
  alias: coder
  pool:                       # in order: the box, then the hosted fallback
  - {backend: dgx, deployment: dgx-1}
  - {backend: openai, deployment: hosted}
```

## 4. Packages

As built (the [orientation](walkthroughs/README.md#1-the-package-map) says
where to start reading each):

```
cmd/golem-gateway/        flags, wiring, signals, the lifecycle's drain, probes, the release gate
gateway/proxy/            the request path (§3): the only package that knows the order of steps
gateway/guard/            the floor before authentication: sources' requests and failures, connections
gateway/auth/             credential to Principal from the snapshot: golem keys, Entra tokens; the denylist
gateway/dialect/          anthropic/messages, openai/chat, openai/responses: scan, splice, errors, usage;
                          internal/jsonscan (the shared scanner), internal/openaiusage
gateway/sse/              server-sent events from any chunking; a filter that withholds events
gateway/route/            aliases, pools in order, deployment health (ceilings, ejection, probes,
                          health checks), retry budgets
gateway/admission/        the request path's contract with admission: Call, Admitter, Ticket, Refusal (a leaf)
gateway/limit/            Admitter's implementation: snapshot known spend against the scope's budget,
                          in-flight slots, rate buckets; Valkey (Lua) and in-memory stores; outages as waivers
gateway/price/            integer pricing from the snapshot's price list
gateway/canon/            the canonical model (only Usage so far)
gateway/envelope/         an attempt's attribution; envelope/bedrock (InvokeModel, event stream, SigV4)
gateway/snapshot/         the compiled snapshot behind an atomic pointer; the bucket and directory
                          sources; upstream transports and the dial check; status writes
gateway/telemetry/        OTel setup, spans and metrics, the ledger row and its queue
```

Planned, not packages yet: hooks (ADR 0014, from M2), the canonical
model's adapters (M1.5), more envelopes (Vertex, Foundry: M2).
`internal/archtest` checks the rules: nothing but the binary imports
`proxy`; a dialect imports only `canon` and `dialect/internal`; an
envelope imports no dialect; the gateway imports nothing of the control
plane, and no database, ClickHouse or Kubernetes client.

## 4a. The bedrock envelope

`gateway/envelope/bedrock`, an `http.RoundTripper` over the deployment's
transport, so the request path sees an Anthropic provider. A Backend says
`envelope: bedrock` and `aws: {region: ...}`, with no auth; each deployment
names a Bedrock model ID, inference profile ID or ARN, and may name its own
`region` (a failover to another region is another deployment) and `url` (a
VPC endpoint). Per attempt it:

- moves the Messages body into Bedrock's: `model` and `stream` out (the
  model a path segment; `invoke` or `invoke-with-response-stream`),
  `anthropic_version: bedrock-2023-05-31` in, the `anthropic-beta`
  header's values into the body's `anthropic_beta`;
- signs with SigV4 for the deployment's region, with the gateway's
  credentials (the AWS SDK's default chain: IRSA or Pod Identity). **A
  request that never left the gateway** (its body, rewrite or signing
  failed, credentials unavailable included) is `envelope.ErrNotSent`:
  unreached, so free and failed over, not charged as uncertain;
- reads a streamed answer's AWS event stream (CRCs checked) and gives the
  Messages SSE each chunk carries; an exception becomes the dialect's error
  event (throttling a rate limit, unavailability an overload), so the
  commit point refuses one that comes first and the call fails over; one
  after output fails the call (§3 step 11); a bad CRC or a cut is the
  request path's cut;
- returns a Bedrock error answer in Anthropic's shape, its status kept;
  **for a credentials refusal** (an access-denied or other authentication
  or authorization exception, a 401 or 403) the caller gets golem's own
  message, not Bedrock's, which names the gateway's role and account;
  Bedrock's text is logged for the platform team;
- sends the attempt's attribution as Bedrock's signed request metadata
  (`golem_request_id`, `golem_attempt_id`, `golem_principal`,
  `golem_scope`, `golem_key`), so Bedrock's invocation logs join the
  ledger per attempt.

Usage is read as for Anthropic (`message_start`, `message_delta`);
Bedrock's `amazon-bedrock-invocationMetrics` passes through. Tests: the
envelope's `TestRewrite`, `TestRequest`, `TestReframe`, `TestErrorResponse`,
`TestNoCredentials`, `TestNotSent`, `TestCredentialsRefused`, `TestMetadata`,
`TestCountTokens`, and proxy's `TestBedrockEndToEnd`. The gateway's role
needs `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` on
the models and profiles its routes name (operations.md).

## 5. Go 1.27 facilities used

| Facility | Release | Where |
|---|---|---|
| `encoding/json/v2`, `encoding/json/jsontext` | 1.27 | Token-level body scans, SSE payloads, `omitzero` |
| `uuid` package | 1.27 | Request IDs; retried attempts' IDs as UUIDv5s of the request ID |
| `testing/synctest` | 1.25 | Timeouts, ejection, drain and limiter windows on fake time |
| `http.CrossOriginProtection` | 1.25 | Standalone's guard against browser pages |
| `os.Root` | 1.24+ | Reading config, the denylist and mounted secrets without path traversal |
| `sync.WaitGroup.Go`, `errors.AsType` | 1.25 / 1.26 | Goroutine ownership; typed error matching |
| Container-aware GOMAXPROCS, Green Tea GC | 1.25 / 1.26 | Free; GOMAXPROCS is not set in the chart |

Dependencies, from go.mod, each confined to the packages that need it:

- `go.opentelemetry.io/otel` and its SDK and OTLP/HTTP exporters (traces,
  metrics, logs): gateway/telemetry, lib/otlp, golem-control's metrics and
  audit records;
- `github.com/aws/aws-sdk-go-v2` (config, credentials, the event stream,
  KMS, S3) and `github.com/aws/smithy-go`: the Bedrock envelope's SigV4 and
  event stream, lib/bucket/s3bucket, lib/sign/kmssign;
- `github.com/valkey-io/valkey-go`: gateway/limit;
- `github.com/go-jose/go-jose/v4`: lib/entra;
- `connectrpc.com/connect` and `google.golang.org/protobuf`: the control
  API (control/api, cmd/golemctl);
- `golang.org/x/net`: tests only (a DNS server in gateway/snapshot's);
- `sigs.k8s.io/yaml`: reading config YAML (lib/api/v1alpha1);
- `github.com/chucklehead-dev/gotogether`: tests only (the models'
  connect tests).

No CEL, database, ClickHouse, Graph or Kubernetes client in the gateway:
the import test fails if one appears.

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
  alarmed, as is a pointer issued more than `-clock-skew` (two minutes) in
  the gateway's future, or a snapshot whose lease runs further ahead than
  `-max-lease` (four hours, the publisher's `-lease`) plus that skew:
  refused rather than clamped, failing closed, so a publisher's clock a day
  ahead cannot grant a day-long lease. Once the signed authorization lease has expired (four hours
  for internal pools, ADR 0030) the gateway refuses new model requests. A
  snapshot stating another period zone than the gateway's `-period-zone`
  is refused.
- An **emergency denylist** (a ConfigMap mounted as a file, read with
  `os.Root`) is applied over every snapshot: a site admin's way to revoke a
  key or principal when the control plane or the bucket is unreachable.
- Each snapshot applied and each denylist change terminates the calls in
  flight it withdraws (a key revoked, its principal gone from the key's
  scope or from golem, a key or principal denied), within seconds: a
  stream with its dialect's error event, a call awaiting the provider's
  headers with its dialect's error, a whole response already begun by a
  cut; charged what was produced, the slot held for the provider's tail,
  the ledger outcome `terminated`. Lease expiry stops new requests only.
- Each new snapshot is compiled (lookup maps, transports) off the request path and
  swapped in; a failure to compile keeps the previous snapshot.
- After each apply (or failure) the gateway writes
  `status/<pool>/<gateway-id>`: hash, versions, compile errors, time.
  Nothing reads them yet (`Applied n/m` and `golemctl gateways` are M2's).
- Runtime state that must survive updates (transports, ejection state)
  is keyed by deployment identity, not held in the snapshot. Each applied
  snapshot hands its per-scope known spend to the limiter.

## 6a. Standalone mode

Built (M0, first part): `gateway/snapshot` (the holder: floor and lease
enforced, transports kept across swaps; the directory source through
`os.Root`), `lib/compile` and `lib/api/v1alpha1` (Backend, ModelRoute, ScopePolicy,
PriceList, LocalIdentity), golem keys (`lib/key`, `gateway/auth`: by hash
from the snapshot, Bearer or x-api-key, each refusal its own code), Entra
v2 tokens (RS256 only; tenant, audience, expiry with a minute's leeway;
`golem.access` for users by oid, `golem.service` with `idtyp: app` for
services by azp; the gateway's audience only, ADR 0046; keys
cached and refreshed hourly or on an unknown kid (at most once a minute,
a request within it deferred, not dropped), never on the request path; a
gateway started while Entra is unreachable serves golem keys and refuses
tokens as `gateway.not_ready` until a retried fetch succeeds; the scope from `golem-scope`, else the principal's default, rights
from the snapshot; `-entra-tenant`, `-entra-jwks`), the
limiter in the request path (in-memory: 429 for concurrency and rate
limits, cancellation holding the slot for the provider's tail; budgets
never refuse standalone, since nothing publishes known spend, ADR 0031), `golemctl config validate`
and `golemctl keys mint --dev`. Without a LocalIdentity a standalone gateway
has one implicit local principal, which takes only the gateway's own local
key (a dev key generated on first start into a file only its user may
read, printed as an export line; `-local-key`): the browser guard keeps web
pages out, the key keeps out the machine's other programs and users. With
a LocalIdentity, its dev keys are required, each honoured only for a
listed owner of its kind in its scope, the scopes being the config's
ScopePolicies (ADR 0050). Pool mode reads the bucket source,
writes status objects, applies the emergency denylist, and keeps the last
publication applied on disk beside its checkpoint (`last-good.json`):
restarted while the bucket is unreachable, it serves that copy, verified
as a publication from the bucket is, until its lease ends (a copy that
does not compile yet, its Secret not yet mounted, is retried from disk
every 30 seconds until it does or the bucket's publication applies).

`golem-gateway -standalone <dir>` (ADR 0023) runs this binary on a laptop
from the same CRD YAML as the GitOps repository, compiled in-process by
`lib/compile` and recompiled on change, with one implicit local principal
(or a `LocalIdentity` file), the in-memory limiter, the developer's own
provider credentials, telemetry to stdout or a local OTLP endpoint, and
`--explain`. It binds to localhost, refuses edge, site and bucket
settings, and changes nothing in the request path.

Loopback keeps other machines out but not the developer's browser: a page
can POST to localhost (a `text/plain` body needs no CORS preflight), and DNS
rebinding gives a hostile name a loopback address, while the implicit
principal spends the developer's own provider keys. So standalone answers
only to a loopback `Host`, refuses cross-origin requests by the browser's
own headers (`http.CrossOriginProtection`), and requires
`Content-Type: application/json`, which no page can send cross-origin
without a preflight it never gets. SDKs and curl pass all three.

## 7. Performance notes

- The fast path allocates per request: the pooled body buffer (returned),
  the Principal (cached), one span pair, one access-log record. Target
  well under 50 allocations; tracked by `testing.B.Loop` benchmarks in CI.
- Each upstream connection's address is checked as it is made (after the
  name resolved, so a name that resolves elsewhere later is caught):
  cloud metadata, link-local, unspecified and multicast addresses are
  refused always; loopback and private ones (RFC 1918, ULA, CGNAT) unless
  the Backend grants them (`spec.network.allowPrivateAddresses`, for a
  self-hosted or local model server). Telemetry, which carries the
  ledger, goes over TLS unless the collector is on loopback (golem#5); the
  exporters are given their whole URLs, so the `OTEL_EXPORTER_OTLP_*`
  variables cannot turn TLS off or redirect them (the CERTIFICATE ones
  still add trust).
- Transports are per deployment, every setting explicit
  (`snapshot.upstreamTransport`): idle connections bounded per host only
  (DefaultTransport's total of 100 re-handshook connections above 100
  streams), HTTP/2 with pings, no proxy (egress is the mesh's), and no
  response-header timeout (a long non-streamed answer's headers come at its
  end; that bound is the route's).
- Every call is bounded by its route (`ModelRoute` `spec.timeouts`, the
  compiler's defaults written into the snapshot): `maxDuration` for the
  whole call (30 min), `headers` for the response's headers (the whole
  call by default: a non-streamed answer's come at its end) and `idle` for
  a provider's silence once it has begun (5 min, past a reasoning pause).
  They take the usual forms (nginx's `proxy_read_timeout`, Envoy's
  `stream_idle_timeout` and `max_stream_duration`): a context deadline for
  the call, a timer for the headers, and an idle timer reset by every read.
  A bound that fires cancels the upstream request with its cause: before
  the headers, a 503 that names it; after, the cut-stream path (accounted,
  `upstream.stream_failed`, aborted). Renewals stop holding a call's slot
  a minute past its maximum duration.
- Absent response bytes are not absent work (golem#4). A round trip that
  failed where the provider cannot have seen it (a name that did not
  resolve, a connection never made, a TLS handshake that failed) is free.
  Any other failure before the headers is `uncertain`: charged its worst
  case as an estimate until reconciliation (or, if the caller left first,
  its input). After any ambiguous end (that, a cut, an abandoned
  response) the call's slot is held for the provider's tail
  (`ProviderTail`, 2 minutes, at most to the call's deadline), since the
  provider may go on working.
- Upstream TLS sessions resumed; DNS cached by the resolver, not golem.
- SSE scanning reads lines from the tee without copying the payload when
  it is not a usage-bearing event (look for the event type first).
- Benchmarks against a stub upstream, with `pprof` and the flight recorder,
  belong in M0 so every later feature is measured against the baseline.

## 8. Testing

- Unit tests per dialect from recorded provider fixtures (requests,
  responses, SSE streams, errors), including truncated and malformed
  streams.
- Translation (M1.5) to be tested round-trip and against provider SDK
  conformance fixtures.
- `synctest` for anything with time: retry backoff, ejection windows,
  limiter windows, drain, idle-chunk timeouts.
- Fuzzing (`go test -fuzz`) the body scanners and SSE parser.
- The models' connect tests drive the code: `model/exposure.md` the
  limiter (`TestExposureModel`, over the in-memory store, and
  `TestExposureModelValkey`), `model/attempts.md` the attempt loop
  (`TestAttemptsModel`), `model/publication.md` the gateway's snapshot
  source (`TestPublicationModel`).
- Limit scripts tested against a Valkey service in CI (the store
  contract, the two stores' differential test, the exposure model over
  Valkey).
- The real OpenAI and Anthropic SDKs against a gateway (test/compat), and
  the ledger end to end through the collector into ClickHouse
  (test/ledger). A kind cluster with the charts is not built.
