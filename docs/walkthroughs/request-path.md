# Walkthrough: the request path

This is a guided reading of the gateway's request path: what happens
between a model request arriving at `POST /v1/chat/completions` or
`POST /v1/messages` and its ledger row being written. It covers
`gateway/proxy` and the small packages it is built from (`admission`,
`dialect`, `sse`, `canon`, `route`, `price`). It is for an engineer new to
the repository; read it with the code open. Line numbers are as of this
writing.

The design is in [gateway.md](../gateway.md) §1 (principles), §3 (the
request path, step by step) and §7 (performance notes), with the external
contracts in [contracts.md](../contracts.md): §1 refusals, §3 the ledger,
§4 the price model, §5 identifiers and headers, §8 policy-relevant request
fields. The decisions behind its shape are ADRs
[0001](../adr/0001-dialects-canonical-model-and-envelopes.md) (dialects and
the pass-through fast path), [0002](../adr/0002-no-replay-after-first-byte.md)
(no replay after the first byte), [0012](../adr/0012-limits-and-accounting.md)
(accounting from what was seen), [0027](../adr/0027-provider-side-retention.md)
(provider-side storage), [0031](../adr/0031-soft-budgets.md) and
[0044](../adr/0044-budgets-are-controls-not-ceilings.md) (admission and the
declared worst case), [0037](../adr/0037-request-policy-and-cache-stability.md)
(request policy, duplicate members), [0041](../adr/0041-security-boundaries-and-incident-controls.md)
§7.6 (the provider's tail) and [0043](../adr/0043-ledger-durability.md) (the
row written at the call's end). The neighbouring walkthroughs are
[gateway-edges.md](gateway-edges.md) (auth, snapshots, telemetry, the
binary) and [accounting.md](accounting.md) (the limiter behind admission).

## What it does, and why it is shaped this way

The gateway is a pass-through proxy that must know three things about each
call: which route serves it, whether the scope may afford it, and what it
cost. Everything else about the body is the provider's business. So the
path reads only what those decisions need and forwards bytes unchanged:

- **Scan, don't decode.** A request body is scanned once with
  `encoding/json/jsontext` for `model`, `stream`, `max_tokens` and the
  policy-relevant members; the model name is spliced in place. The whole
  document is still read, so a body with duplicate names or trailing data
  is refused: an upstream parser could read it differently (ADR 0037).
- **Stream, don't buffer.** The response is copied to the caller as it
  arrives, and the same bytes go to an SSE parser that reads usage. Only a
  non-streamed response is kept, up to 1 MiB, to read its usage at the end;
  one larger than that has its usage unread and is estimated.
- **One attempt, accounted once.** There is no retry or failover yet (the
  places are marked in `ServeHTTP`); after the first response byte there
  never will be (ADR 0002). Each admitted call's ticket ends exactly once,
  however the call ends.
- **Err high when unsure.** A call whose provider never reported usage is
  charged from what was seen, by rules that over-count rather than average:
  never zero for work the provider may have done (golem#4).
- **The hot path allocates almost nothing.** Per-request working memory is
  pooled (the *exchange*), timers are reused, and the dialect code is
  written so that an ordinary stream allocates nothing per event.

## The packages at a glance

| Where | What it is |
|---|---|
| `gateway/proxy/proxy.go` | `Handler.ServeHTTP`, the request path top to bottom; refusals, features and policy, admission, the response relay, `settlement` (the ticket's one end) |
| `gateway/proxy/dialect.go` | everything that differs by dialect: `Dialect`, the headers each forwards and checks, `scanned`, scan/rewrite/usage dispatch, `estimate`, error shapes |
| `gateway/proxy/exchange.go` | the pooled `exchange` (copy buffer, scanners, usage readers, writer state), body pools, `upstreamBody` |
| `gateway/proxy/bounds.go` | the route's time bounds over the upstream call: reusable `alarm`s, the idle reader |
| `gateway/admission` | the contract with the limiter: `Call`, `Admitter`, `Ticket`, `Refusal` (implemented by `gateway/limit`) |
| `gateway/dialect/openai/chat` | Chat Completions: `Request.Scan`/`Rewrite` (injects `include_usage`), `StreamUsage`, `UsageOnly` |
| `gateway/dialect/anthropic/messages` | Messages: `Request.Scan`/`Rewrite`, server-tool detection, `StreamUsage` |
| `gateway/dialect/internal/jsonscan` | the shared top-level member scanner (`Object`, `Elements`), spans and splices (`Splice`, `Apply`) |
| `gateway/sse` | `Parser` (events from arbitrary chunking) and `Filter` (the same, withholding events) |
| `gateway/route` | `Table` (alias to `Route`), `Route` (pool, bounds, approved betas), `Target` (one deployment) |
| `gateway/price` | `Cost` and `Worst`: integer micro-units, half-even rounding, saturation at `MaxCost` |
| `gateway/canon` | `Usage`, the one canonical type the path needs so far |

`proxy` is imported only by the binary (`internal/archtest` checks it).
The binary mounts one `Handler` per dialect (`cmd/golem-gateway/main.go:312`),
each wrapped by the lifecycle's drain tracking and, in standalone mode
only, by `localOnly` (`cmd/golem-gateway/guard.go:24`), which refuses
non-loopback hosts, cross-origin browser requests and non-JSON bodies.

## A worked example: an OpenAI streaming request

Follow `POST /v1/chat/completions` with
`{"model":"team-chat","stream":true,"max_tokens":100,"messages":[...]}`
from a key whose scope has a budget, through to the ledger.

**Identity and configuration.** `ServeHTTP` (`proxy.go:117`) starts the
span and mints a UUIDv7 request ID (`newRequestID`, `proxy.go:278`,
formatted into a stack buffer), set on the response at once
(`proxy.go:122`), so every refusal carries it. `authenticate`
(`proxy.go:498`) takes the current snapshot once, used to the end of the
call (gateway.md §1): an expired lease refuses with
`gateway.authorization_expired`, a missing snapshot with
`gateway.not_ready`, and a snapshot with keys hands the headers to
`Keys.Authenticate` (`gateway/auth/auth.go:207`).

**The body.** `getExchange` (`exchange.go:68`) takes the pooled working
memory, and `readBody` (`proxy.go:517`) reads into a pooled buffer sized
from `Content-Length` in one allocation, under a read deadline
(`BodyTimeout`, default 30 s, so a trickling client cannot hold a buffer
before admission) and `MaxBody` (zero means the default, 32 MiB; the
binary's `-max-body` flag sets it).

**Scan and route.** `ex.scan` (`dialect.go:101`) runs `chat.Request.Scan`
(`chat/chat.go:72`) over `jsonscan.Object` (`jsonscan/jsonscan.go:81`),
which compares member names unescaped (`"model"` is `model`) and
records each value's span. A null `stream` or `max_tokens` is the member
absent. `stream_options` and its `include_usage` are located by
`scanStreamOptions` (`chat.go:175`) for the rewrite. Policy members set
flags; unknown members are counted. `store` must be a boolean or null: any
other value (`"true"`, `1`) is refused as `request.invalid` naming it
(`ErrStore`), since read as not storing it would let the storage policy
fail open on a wrong type (TestScanStore, TestStoreNotBoolean). `route` (`proxy.go:539`) looks the
alias up without allocating (`route/route.go:54`), refuses an alias of the
other dialect, and checks the principal may use it. `features`
(`proxy.go:415`) applies restricted mode: unknown members are refused
unless the route is an agent route or the scope compatible, and
`Dialect.restrictedHeaders` (`dialect.go:63`) checks the dialect's own
feature headers, which for OpenAI is none (an Anthropic call's betas must
be the route's, its `anthropic-version` one of `approvedVersions`,
`dialect.go:57`). `policy` (`proxy.go:578`) applies the scope's ceilings
and storage rule (`store: true` is rewritten to `false` where storage is
forbidden).

**Admission.** `rt.Pick` takes the first deployment. `price.Worst`
(`price/price.go:33`) prices the declared worst case (every input byte a
token, the output cap times `n`); a product that would overflow is refused.
`admit` (`proxy.go:609`) fills an `admission.Call` with the scope's limits
and calls the `Admitter`: golem's is `Limiter.Admitter`
(`gateway/limit/limit.go:765`). A `*admission.Refusal` is rendered with its
code and Retry-After; an error with the caller gone is a silent end. The
admitted ticket goes into a `settlement` (`proxy.go:197`, type at
`proxy.go:412`) with a context that is never cancelled, and a deferred
guard (`proxy.go:177`) cancels it at its worst case if a panic gets there
before its end.

**The upstream request.** `ex.rewrite` (`dialect.go:118`) runs
`chat.Request.Rewrite` (`chat/chat.go:241`): the model spliced and, since
the caller did not ask for usage, `usageEdit` (`chat.go:258`) adds
`"stream_options":{"include_usage":true}` after it (or fills in an
existing `stream_options`). The caller's body goes back to its pool at
once (`proxy.go:215`): a long stream must not hold a 200 KB agent request.
The row's fields are gathered into a `telemetry.Call` (`proxy.go:218`).
`lim.bound` (`bounds.go:114`) derives the upstream context and arms the
call's and the headers' alarms; `upstreamRequest` (`proxy.go:521`) builds
the request with the dialect's allowlisted caller headers
(`Dialect.forwardRequest`, `dialect.go:38`: `OpenAI-Organization` here,
never an `anthropic-*` header) and the target's credential, its body an
`upstreamBody` (`exchange.go:178`) that returns the buffer to the pool when
the transport closes it.

**Registered in flight.** With a `Handler.InFlight` (golem-gateway's, one
for both dialects), the call is registered right after `lim.bound`, with
the `auth.Grant` its authentication filled and the upstream context's
cancel, and taken off just before that context is released
(`InFlight.add`, `remove`; the entry lives in the exchange, nothing is
allocated). A snapshot that revokes its key, or removes its principal, or a
denylist that names it, cancels that context with the refusal as its cause
(`InFlight.Sweep`, gateway-edges.md §1). The caller's own context is
untouched, so such an end is never taken for a caller gone: `terminatedBy`
reads the cause. A call withdrawn as it registers (`add` rechecks one that
authenticated before a sweep it missed) is refused with that code at once
and costs nothing: the provider never had it.

**If the round trip fails** (`proxy.go:235-219`), `chargeUnanswered`
(`proxy.go:308`) decides what the provider may have seen: a DNS, dial or TLS
failure (`reachedProvider`, `proxy.go:483`) is free and completes the
ticket at zero; a caller who left first is charged its input; anything else
is `uncertain`, charged its worst case (with 8192 output tokens if it set no
`max_tokens`) and its slot held for the provider's tail. The row is written
before the ticket ends, and the caller gets `upstream.unavailable`, naming
the route's timeout if one fired (`unavailableMessage`, `proxy.go:309`).
A round trip a sweep cancelled (the call withdrawn while it waited for the
provider's headers) is ended as a caller gone is, charged its input and
its slot held for the tail, its row's outcome `terminated` with the
refusal's code; the caller gets that refusal in its dialect's error shape
(`auth.key_revoked`, `authz.scope_not_member` or `auth.denied`).

**The attempts.** The call is admitted once, at its first deployment, and
then makes up to `Route.MaxAttempts` attempts (default 2) before one
commits (`proxy.go:267`), the design model/attempts.md checks. `pick`
(`attempt.go:28`) takes the pool in order, a deployment not yet tried
first, each only if `route.Health` (`gateway/route/health.go`) gives it a
lease: under its `maxInFlight` ceiling, and not ejected (one probe at a
time once its ejection is over). An attempt the provider never saw (a
dial, DNS or TLS failure), or refused before any output (429, 500, 503,
529, or a 200 whose first event is an error), is free: its lease reports
the failure, its row is written (`Telemetry.Retried`, its own
`attempt_id` and `event_id`, cost 0), and the call goes on, if its body
was kept (`Handler.MaxKept`), it has attempts left, its caller is there,
nothing withdrew it, and the route's retry budget (a fifth of its calls,
`Health.Retry`) has one. An attempt that may have reached the provider
and got no answer ends the call, as does a 4xx, a 502 or 504, or a cut
after the commit. Enough failures in a row eject a deployment, for longer
each time (or the provider's longer Retry-After); a committed attempt
readmits it.

**The response.** `gotHeaders` (`bounds.go:138`) stops the headers alarm
and arms the idle one, whose clock every read stamps (`bounds.go:165`).
A stream is not committed yet: `firstEvent.await` (`commit.go:47`) holds
its bytes, at most 64 KiB, until its first event is complete. A provider
that answered 200 and then an error as that event (Anthropic's
`overloaded_error`) has produced nothing and told the caller nothing: the
call is refused as `upstream.unavailable` (503, the provider's
Retry-After), charged nothing, its slot freed (`TestFailedBeforeCommit`).
Otherwise the held bytes are replayed ahead of the body. This is where the
attempt loop will retry (m1-plan.md step 2). Then allowlisted headers are
copied and the status written. `relay`
(`proxy.go:563`) is the copy loop. For this request `startStream`
(`dialect.go:129`) sees that golem injected `include_usage` and routes the
stream through `ex.filter`, whose `Drop` is `dropUsageOnly`
(`exchange.go:139`, over `chat.UsageOnly`): the usage-only chunk is read
but withheld, because SDK loops that read `choices[0]` of every chunk
crash on it. Each read goes through `ex.forward` (`exchange.go:116`) into
`Filter.Pass` (`sse/sse.go:213`), whose `Parser.Write` (`sse.go:58`) finds
line ends with `bytes.IndexByte` and dispatches each event (`sse.go:170`)
to `chat.StreamUsage.OnEvent` (`chat/chat.go:326`). That checks
`mayHaveUsage` (`chat.go:305`) before scanning, since every chunk but the
last carries `"usage":null`. A usage object is the provider's final
report only with both `prompt_tokens` and `completion_tokens` present and
not null (`readInt` reports presence): a null count is not reported, and
the call is estimated rather than charged nothing
(`TestNullCountsAreNotFinal`). What the filter passes is written by
`ex.send` (`exchange.go:92`) under a write deadline (`WriteStall`) and
flushed; a failed or stalled write marks the caller gone and ends the
loop. At EOF `Filter.End` forwards an unterminated last event, and
`ex.usage` (`dialect.go:176`) returns the usage with any malformed-report
error joined to the cut.

A non-streamed response takes the other branch of `forward`: it is written
unflushed and appended to `ex.kept`. Once a read would take the copy past
1 MiB, `overflow` is set and nothing after it is kept, though a later read
would fit (`exchange.go:131`): a head and a tail spliced together could
read as a body with another usage. `readWhole` (`dialect.go:150`) then
records `errResponseUnread` instead of reading the copy, and the call is
estimated.

**Usage, estimate, cost.** The caller is gone if a write failed or its
request ended (`proxy.go:257`); the response was cut if `relay` returned an
`errUpstreamCut` (a provider error, an idle or duration bound, or more
than `MaxResponse` bytes, default 64 MiB). For a 200, `ex.estimate`
(`dialect.go:193`) keeps the provider's report if it was final; otherwise
output is every byte received, capped at `max_tokens × n`, and input, if
unreported, every request byte; `EstimatedFraction` records the estimated
share of the cost. `price.Cost` (`price.go:14`) prices it.

**The row and the ticket.** `outcome` (`proxy.go:663`) names the ledger
outcome (`completed`, `failed`, `cancelled`, or `terminated` for a cut a
sweep made: `Result.Terminated`) with its error origin and code.
`Telemetry.Ended` (`gateway/telemetry/telemetry.go:196`) records the span
and metrics and writes the `golem.spend` row (`Ledger.Ended`,
`telemetry/ledger.go:36`). Only then does the ticket end
(`proxy.go:415-427`): `complete` with the cost, or, for an abandoned or cut
response, `cancel` with the cost seen and the slot held for `ProviderTail`
(default 2 minutes, at most to the call's deadline: `settlement.cancel`,
`proxy.go:429`), because the provider may go on working after the
connection closes. The row's end is stamped before the counter's minute,
so a snapshot's boundary puts the call on one side only (ADR 0048).

**A cut stream.** If the provider's response was cut and the caller is
still there, the stream gets the contract's `upstream.stream_failed` event
(`streamError`, `dialect.go:248`) and the handler panics with
`http.ErrAbortHandler` (`proxy.go:397`): the connection is reset, not ended,
so no client can mistake a short answer for a whole one (contracts.md §1).
A call the gateway ended (its authorization withdrawn) takes the same path,
cancel and abort included, but its stream's event carries the refusal's
code and message instead (`errorEvent`); a whole response already begun is
cut alone, as any cut whole response is.

An Anthropic stream differs only inside the dialect seam: no usage is
injected, so the plain parser runs with `StreamUsage.Want`
(`messages/messages.go:189`) skipping every event but `message_start` and
`message_delta` without copying their data; `message_start` gives the input
side and each `message_delta` the cumulative output (`messages.go:198`).
Its `Rewrite` (`messages.go:172`) is the model splice alone
(`jsonscan.Splice`); its `anthropic-version` and `anthropic-beta` headers
are checked in restricted mode and forwarded.

## Invariants, where they are enforced and tested

| Invariant | Enforced | Tested |
|---|---|---|
| Every admitted ticket ends exactly once, whatever ends the call (a panic included) | `settlement` and its deferred guard (`proxy.go:197-180`); `admission.Ticket`'s contract | `TestAdmitterContract` (`limits_test.go:69`), `TestConcurrencyAndCancel` (`auth_test.go:125`) |
| The row is written before the ticket numbers its event | `Telemetry.Ended` before `complete`/`cancel` (`proxy.go:239`, `proxy.go:261`) | `TestLedgerRows` (`telemetry_test.go:20`) |
| Golem routes by exactly what the provider will read | `jsonscan.Object`: whole document, unescaped names, duplicates and trailing data refused | `TestObject` (`jsonscan_test.go:27`), `FuzzObject`, `TestRefusals` (`proxy_test.go:163`), `messages`' `TestScan` (`messages_test.go:13`) and `chat`'s `TestScanMaxTokens`, `TestScanPolicyFields` |
| A null `stream` or `max_tokens` is the member absent | the dialects' `Scan` | `TestScan`'s null case (`messages_test.go:38`) |
| Work the provider may have done is never free; estimates err high | `chargeUnanswered`, `estimate` | `TestEstimates` (`estimate_test.go:65`), `TestUncertainUpstream` (`uncertain_test.go:23`) |
| A whole response's usage is read from all of it or not at all | `forward`'s `overflow`, `readWhole`'s `errResponseUnread` | `TestKeptNotSpliced` (`exchange_test.go:14`) |
| A cut response is never taken for whole | `streamError` and the abort (`proxy.go:388-278`) | `TestUpstreamCut` (`proxy_test.go:266`) |
| A call whose key is revoked, or whose key or principal is denied, ends within seconds in its dialect's error (an event, a refusal, a cut), as a caller gone is charged and held, its row `terminated`; other calls, the lease's expiry, a key's own expiry and a scope losing a model end none (by decision: they stop new requests only) | `InFlight` (`inflight.go`), `terminatedBy`, `outcome`, `chargeUnanswered` | `TestTerminateStream`, `TestTerminateWhole`, `TestLeaseExpiryEndsNoCall`, `TestTerminateRace` (`terminate_test.go`) |
| An ambiguous end holds the slot for the provider's tail, at most to the deadline | `settlement.cancel` (`proxy.go:429`) | `TestTailClampedToDeadline` (`settlement_test.go:21`), `TestConcurrencyAndCancel`, `TestUncertainUpstream` |
| The injected usage chunk never reaches the caller | `startStream` and `sse.Filter` | `TestInjectedUsageWithheld` (`proxy_test.go:135`), `TestFilter` (`sse/filter_test.go:24`) |
| Only allowlisted headers cross, each API's own; golem's never reach the provider | `Dialect.forwardRequest` (`dialect.go:38`), `forwardResponse` (`proxy.go:87`), `copyHeaders` | `TestHeaders` (`proxy_test.go:61`), `TestOpenAIHeaders` (`proxy_test.go:90`) |
| Refusals use contract codes, in the caller's dialect, with the request ID | `fail`, `refuse` (`proxy.go:735`, `proxy.go:627`), `appendError` (`dialect.go:225`) | `TestRefusals` (`proxy_test.go:163`), `TestWrongFrontend` (`proxy_test.go:237`) |
| A body cap of zero is the default (32 MiB), not "no body" | `readBody` (`proxy.go:518`) | `TestRefusals`' two default-cap cases |
| A slow client or a silent provider is bounded | write deadlines in `send`; `bounds` | `TestStalledClient`, `TestSilentProviderKeepsClient`, `TestRouteBounds` (`proxy_test.go:337-485`), `TestRequestBoundary` (`boundary_test.go:70`) |
| A fired alarm never cancels a later call | `alarm.disarm`, `stopAlarm` replaces a fired one | `TestBoundsReuse`, `TestBoundsRace`, `TestIdleAlarm` (`bounds_test.go`) |
| Policy: ceilings, storage, server tools, restricted features | `policy`, `features`, `restrictedHeaders`, the dialects' flags | `TestPolicy`, `TestStoreOff`, `TestPolicyUnits` (`policy_test.go`), `TestFeatureModes` (`boundary_test.go:18`), `TestOpenAIHeaders` |
| Prices are integers, rounded once, and never wrap | `price.numerator`, `product`, `halfEven`, `MaxCost` | `TestCost` (`price_test.go:11`) |

## Performance choices, and how they are measured

- **The pooled exchange** (`exchange.go:22`) holds one request's scanners,
  usage readers, SSE parser and filter, an 8 KiB copy buffer, the response
  writer's state and the route's alarms. The request bodies are pooled
  separately and released as soon as they are sent, so a long stream holds
  about the copy buffer and the parser's bounded buffers (PRD §7: under
  64 KiB a stream). `sse.Parser.Reset` and `Filter.Reset` keep buffers up to
  16 KiB; the body pool drops buffers over 1 MiB, and `release` drops a
  kept copy that grew past that.
- **Bound method values.** The usage readers' methods are bound once, when
  the exchange is made (`exchange.go:58`): a method value allocates each
  time it is taken.
- **Reusable alarms** (`bounds.go:48`). Each bound is a `time.AfterFunc`
  timer armed per call; only one that fired is replaced, since its callback
  may still be running. A call allocates its cancellable context and
  nothing else for its bounds.
- **No-allocation scanning.** `jsonscan` decodes from a pooled
  `jsontext.Decoder` over a `bytes.Buffer` it reads in place (from another
  reader it would copy the body, and the pool would keep the copy:
  `TestNoRetention`). `jsonscan.String` copies a model name with no escapes
  straight into a reused slice; `Route.Lookup` uses the `string(bytes)` map
  lookup the compiler does not allocate for; `chat.mayHaveUsage` skips the
  scan of every chunk with `"usage":null`; the Anthropic parser skips
  content deltas' data entirely. `jsonscan.Apply` sorts its few edits by
  insertion, so they do not escape, and `chat`'s spliced literals are
  package-level: an OpenAI scan and rewrite allocate once (the quoted model
  name, which `jsontext.AppendQuote` makes escape).
- **Measured** by the benchmarks in `gateway/proxy/bench_test.go`:
  `BenchmarkScan` (the scan alone, 0 allocations), `BenchmarkHandler`
  (the in-memory Anthropic fast path with a 200-event stream: 21
  allocations a call), `BenchmarkHandlerAdmitted` (with a key, an alias
  check and the in-memory limiter: 25; both with the in-flight registry,
  which adds none: `BenchmarkInFlight`, its add and remove from every core,
  about 60 ns and 0 allocations), `BenchmarkStub` (the stub's share,
  to subtract), `BenchmarkLoopback` (over real sockets, direct against
  proxied) and `prod_bench_test.go`'s `BenchmarkProduction` (with real
  telemetry exporters), and `chat`'s `BenchmarkChunkNullUsage` (0
  allocations). `TestOverheadPercentiles` and `TestMemoryPerStream` are
  spike measurements, run with `GOLEM_SPIKE=1`. Run them before and after a
  change: `go test -run '^$' -bench . -benchmem ./gateway/...`.

## Extending it: a new dialect

Everything that differs by dialect is in `gateway/proxy/dialect.go`, as
methods of `Dialect` or of the exchange; the rest of the path is
dialect-free. To add one:

1. Write its package under `gateway/dialect/<provider>/<api>`, on
   `jsonscan`: a `Request` with `Scan` (model, stream, max tokens, policy
   flags, unknown members; null read as absent) and `Rewrite` (the model
   spliced, anything golem needs asked for), and a `StreamUsage` with
   `OnEvent` (and `Want`, if some events can be skipped unread) and
   `Response`, setting `Final` only for a complete report and `Err` for a
   malformed one.
2. Add a `Dialect` constant and its `Name` (the Backend's `spec.dialect`),
   its caller headers in `forwardRequest`, and any of them restricted mode
   must approve in `restrictedHeaders`.
3. Give the exchange fields for its request and usage reader (bind their
   methods in the pool's `New`), and a branch in each dialect method:
   `scan`, `forbidStorage`, `rewrite`, `startStream`, `readWhole`,
   `reported`, `appendError` and `streamError`.
4. Mount it in `cmd/golem-gateway/main.go`, and add it to the tests that
   run per dialect (`TestUpstreamCut`, `TestEstimates`, `TestRefusals`).

## Known gaps

- No request hooks; the pool is taken in order, with no weights,
  least-in-flight or affinity (gateway.md §3 steps 4 and 5).
- A call's final 503 carries its own Retry-After (the provider's, or the
  default), not the earliest of the pools it tried.
- Retries are not yet driven by the attempt model (connect), nor its storm
  bound simulated (m1-plan.md step 2).
- No cross-dialect conversion: a route speaks its frontend's dialect or is
  refused (`route.wrong_frontend`); `canon` holds only `Usage`.
- A non-streamed response over 1 MiB has its usage unread
  (`errResponseUnread`), so the call is estimated from its bytes even when
  the provider reported usage at the end.
- Estimates are byte counts: they do not bound images, audio or files, or
  units billed outside tokens (ADR 0044), and the provider's work past a cut
  is unseen until reconciliation.
- A terminated whole response whose headers were already forwarded can
  only be cut: its caller sees a reset, not the refusal's code (a stream
  gets the code as its error event). By decision, termination ends
  nothing a snapshot narrows short of the key or principal (a model or
  route taken from a scope ends no call already on it), nor a key's own
  expiry or the lease's: those stop new requests only.
- Unknown request members are counted, not logged by name (the sanitized
  names ADR 0037 describes are not built).
