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
row before its numbering record). The neighbouring walkthroughs are
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
  non-streamed response is kept, up to 1 MiB, to read its usage at the end.
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
| `gateway/proxy/proxy.go` | `Handler.ServeHTTP`, the request path top to bottom; refusals, policy, admission, the response relay, `settlement` (the ticket's one end) |
| `gateway/proxy/dialect.go` | everything that differs by dialect: `Dialect`, `scanned`, scan/rewrite/usage dispatch, `estimate`, error shapes |
| `gateway/proxy/exchange.go` | the pooled `exchange` (copy buffer, scanners, usage readers, writer state), body pools, `upstreamBody` |
| `gateway/proxy/bounds.go` | the route's time bounds over the upstream call: reusable `alarm`s, the idle reader |
| `gateway/admission` | the contract with the limiter: `Call`, `Admitter`, `Ticket`, `Refusal` (implemented by `gateway/limit`) |
| `gateway/dialect/openai/chat` | Chat Completions: `Request.Scan`/`Rewrite` (injects `include_usage`), `StreamUsage`, `UsageOnly` |
| `gateway/dialect/anthropic/messages` | Messages: `Request.Scan`/`Rewrite`, server-tool detection, `StreamUsage` |
| `gateway/dialect/internal/jsonscan` | the shared top-level member scanner (`Object`, `Elements`), spans and splices (`Splice`, `Apply`) |
| `gateway/sse` | `Parser` (events from arbitrary chunking) and `Filter` (the same, withholding events) |
| `gateway/route` | `Table` (alias to `Route`), `Route` (pool and bounds), `Target` (one deployment) |
| `gateway/price` | `Cost` and `Worst`: integer micro-units, half-even rounding, saturation at `MaxCost` |
| `gateway/canon` | `Usage`, the one canonical type the path needs so far |

`proxy` is imported only by the binary (`internal/archtest` checks it).
The binary mounts one `Handler` per dialect (`cmd/golem-gateway/main.go:87`)
behind `localOnly` (`cmd/golem-gateway/guard.go`), which refuses
non-loopback hosts, cross-origin browser requests and non-JSON bodies.

## A worked example: an OpenAI streaming request

Follow `POST /v1/chat/completions` with
`{"model":"gpt","stream":true,"max_tokens":100,"messages":[...]}` from a key
whose scope has a budget, through to the ledger.

**Identity and configuration.** `ServeHTTP` (`proxy.go:107`) starts the
span and mints a UUIDv7 request ID, formatted into a stack buffer and set on
the response at once (`proxy.go:111-115`), so every refusal carries it.
`authenticate` (`proxy.go:368`) takes the current snapshot once, used to the
end of the call (gateway.md §1): an expired lease refuses with
`gateway.authorization_expired`, a missing snapshot with
`gateway.not_ready`, and a snapshot with keys hands the headers to
`Keys.Authenticate` (`gateway/auth/auth.go:198`).

**The body.** `getExchange` (`exchange.go:113`) takes the pooled working
memory, and `readBody` (`proxy.go:387`) reads into a pooled buffer sized
from `Content-Length` in one allocation, under a read deadline
(`BodyTimeout`, so a trickling client cannot hold a buffer before
admission) and `MaxBody`.

**Scan and route.** `ex.scan` (`dialect.go:47`) runs `chat.Request.Scan`
(`chat/chat.go:62`) over `jsonscan.Object` (`jsonscan/jsonscan.go:81`),
which compares member names unescaped (`"model"` is `model`) and
records each value's span. Policy members set flags; unknown members are
counted. `route` (`proxy.go:408`) looks the alias up without allocating
(`route/route.go:54`), refuses an alias of the other dialect, and checks the
principal may use it. `features` (`proxy.go:429`) applies restricted mode
(unknown members, betas and API versions refused unless the route is an
agent route), and `policy` (`proxy.go:461`) the scope's ceilings and
storage rule (`store: true` is rewritten to `false` where storage is
forbidden).

**Admission.** `rt.Pick` takes the first deployment. `price.Worst`
(`price/price.go:33`) prices the declared worst case (every input byte a
token, the output cap times `n`); a product that would overflow is refused.
`admit` (`proxy.go:492`) fills an `admission.Call` with the scope's limits
and calls the `Admitter`: golem's is `Limiter.Admitter`
(`gateway/limit/limit.go:674`). A `*admission.Refusal` is rendered with its
code and Retry-After; an error with the caller gone is a silent end. The
admitted ticket goes into a `settlement` (`proxy.go:175`, type at
`proxy.go:291`) with a context that is never cancelled, and a deferred
guard (`proxy.go:182`) cancels it at its worst case if a panic gets there
before its end.

**The upstream request.** `ex.rewrite` (`dialect.go:70`) runs
`chat.Request.Rewrite` (`chat/chat.go:218`): the model spliced and, since
the caller did not ask for usage, `"stream_options":{"include_usage":true}`
added after it. The caller's body goes back to its pool at once
(`proxy.go:193`): a long stream must not hold a 200 KB agent request. The
row's fields are gathered into a `telemetry.Call` (`proxy.go:196`).
`lim.bound` (`bounds.go:91`) derives the upstream context and arms the
call's and the headers' alarms; `upstreamRequest` (`proxy.go:518`) builds
the request with allowlisted caller headers and the target's credential, its
body an `upstreamBody` (`exchange.go:166`) that returns the buffer to the
pool when the transport closes it.

**If the round trip fails** (`proxy.go:212-228`), `chargeUnanswered`
(`proxy.go:324`) decides what the provider may have seen: a DNS, dial or TLS
failure (`reachedProvider`, `proxy.go:353`) is free and completes the
ticket at zero; a caller who left first is charged its input; anything else
is `uncertain`, charged its worst case (with 8192 output tokens if it set no
`max_tokens`) and its slot held for the provider's tail. The row is written
before the ticket ends, and the caller gets `upstream.unavailable`, naming
the route's timeout if one fired.

**The response.** `gotHeaders` (`bounds.go:104`) stops the headers alarm
and arms the idle one, reset by every read (`bounds.go:125`). Allowlisted
headers are copied and the status written. `relay` (`proxy.go:563`) is the
copy loop. For this request `startStream` (`dialect.go:81`) sees that golem
injected `include_usage` and routes the stream through `ex.filter`, whose
`Drop` is `dropUsageOnly` (`exchange.go:111`, over `chat.UsageOnly`): the
usage-only chunk is read but withheld, because SDK loops that read
`choices[0]` of every chunk crash on it. Each read goes through
`ex.forward` (`exchange.go:91`) into `Filter.Pass` (`sse/sse.go:213`),
whose `Parser.Write` (`sse.go:58`) finds line ends with `bytes.IndexByte`
and dispatches each event (`sse.go:170`) to `chat.StreamUsage.OnEvent`
(`chat/chat.go:288`). That checks `mayHaveUsage` (`chat.go:267`) before
scanning, since every chunk but the last carries `"usage":null`. What the
filter passes is written by `ex.send` (`exchange.go:67`) under a write
deadline (`WriteStall`) and flushed; a failed or stalled write marks the
caller gone and ends the loop. At EOF `Filter.End` forwards an unterminated
last event, and `ex.usage` (`dialect.go:111`) returns the usage with any
malformed-report error joined to the cut.

**Usage, estimate, cost.** The caller is gone if a write failed or its
request ended (`proxy.go:239`); the response was cut if `relay` returned an
`errUpstreamCut` (a provider error, an idle or duration bound, or more
than `MaxResponse` bytes). For a 200, `ex.estimate` (`dialect.go:130`)
keeps the provider's report if it was final; otherwise output is every byte
received, capped at `max_tokens × n`, and input, if unreported, every
request byte; `EstimatedFraction` records the estimated share of the cost.
`price.Cost` (`price.go:14`) prices it.

**The row and the ticket.** `outcome` (`proxy.go:546`) names the ledger
outcome (`completed`, `failed`, `cancelled`) with its error origin and code.
`Telemetry.Ended` (`gateway/telemetry/telemetry.go:185`) records the span
and metrics and writes the `golem.spend` row (`telemetry/ledger.go:37`).
Only then does the ticket end (`proxy.go:260-271`): `complete` with the
cost, or, for an abandoned or cut response, `cancel` with the cost seen and
the slot held for `ProviderTail` (at most to the call's deadline), because
the provider may go on working after the connection closes. The limiter's
numbering record follows the row (ADR 0043).

**A cut stream.** If the provider's response was cut and the caller is
still there, the stream gets the contract's `upstream.stream_failed` event
(`streamError`, `dialect.go:188`) and the handler panics with
`http.ErrAbortHandler` (`proxy.go:284`): the connection is reset, not ended,
so no client can mistake a short answer for a whole one (contracts.md §1).

An Anthropic stream differs only inside the dialect seam: no usage is
injected, so the plain parser runs with `StreamUsage.Want`
(`messages/messages.go:184`) skipping every event but `message_start` and
`message_delta` without copying their data; `message_start` gives the input
side and each `message_delta` the cumulative output (`messages.go:193`).

## Invariants, where they are enforced and tested

| Invariant | Enforced | Tested |
|---|---|---|
| Every admitted ticket ends exactly once, whatever ends the call (a panic included) | `settlement` and its deferred guard (`proxy.go:175-186`); `admission.Ticket`'s contract | `TestAdmitterContract` (`limits_test.go:69`), `TestConcurrencyAndCancel` (`auth_test.go:124`) |
| The row is written before the ticket numbers its event | `Telemetry.Ended` before `complete`/`cancel` (`proxy.go:220`, `proxy.go:259`) | `TestLedgerRows` (`telemetry_test.go:21`) |
| Golem routes by exactly what the provider will read | `jsonscan.Object`: whole document, unescaped names, duplicates and trailing data refused | `TestObject` (`jsonscan_test.go:27`), `chat`'s and `messages`' `TestScan*` |
| Work the provider may have done is never free; estimates err high | `chargeUnanswered`, `estimate` | `TestEstimates` (`estimate_test.go:65`), `TestUncertainUpstream` (`uncertain_test.go:23`) |
| A cut response is never taken for whole | `streamError` and the abort (`proxy.go:275-285`) | `TestUpstreamCut` (`proxy_test.go:240`) |
| An ambiguous end holds the slot for the provider's tail, at most to the deadline | `settlement.cancel` (`proxy.go:308`) | `TestConcurrencyAndCancel`, `TestUncertainUpstream` |
| The injected usage chunk never reaches the caller | `startStream` and `sse.Filter` | `TestInjectedUsageWithheld` (`proxy_test.go:111`), `TestFilter` (`sse/filter_test.go:24`) |
| Only allowlisted headers cross; golem's own never reach the provider | `forwardRequest`, `forwardResponse`, `copyHeaders` | `TestHeaders` (`proxy_test.go:61`) |
| Refusals use contract codes, in the caller's dialect, with the request ID | `fail`, `refuse`, `appendError` | `TestRefusals` (`proxy_test.go:139`), `TestWrongFrontend` |
| A slow client or a silent provider is bounded | write deadlines in `send`; `bounds` | `TestStalledClient`, `TestSilentProviderKeepsClient`, `TestRouteBounds` (`proxy_test.go:311-460`), `TestRequestBoundary` |
| A fired alarm never cancels a later call | `alarm.disarm`, `stopAlarm` replaces a fired one | `TestBoundsReuse`, `TestBoundsRace` (`bounds_test.go`) |
| Policy: ceilings, storage, server tools, restricted features | `policy`, `features`, the dialects' flags | `TestPolicy`, `TestStoreOff`, `TestPolicyUnits`, `TestFeatureModes` |
| Prices are integers, rounded once, and never wrap | `price.numerator`, `product`, `halfEven`, `MaxCost` | `TestCost` (`price_test.go:11`) |

## Performance choices, and how they are measured

- **The pooled exchange** (`exchange.go:22`) holds one request's scanners,
  usage readers, SSE parser and filter, an 8 KiB copy buffer, the response
  writer's state and the route's alarms. The request bodies are pooled
  separately and released as soon as they are sent, so a long stream holds
  about the copy buffer and the parser's bounded buffers (PRD §7: under
  64 KiB a stream). `sse.Parser.Reset` and `Filter.Reset` keep buffers up to
  16 KiB; the body pool drops buffers over 1 MiB.
- **Bound method values.** The usage readers' methods are bound once, when
  the exchange is made (`exchange.go:51`): a method value allocates each
  time it is taken.
- **Reusable alarms** (`bounds.go:39`). Each bound is a `time.AfterFunc`
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
  content deltas' data entirely.
- **Measured** by the benchmarks in `gateway/proxy/bench_test.go`:
  `BenchmarkScan` (the scan alone, 0 allocations), `BenchmarkHandler`
  (the in-memory fast path with a 200-event stream: 21 allocations a call),
  `BenchmarkHandlerAdmitted` (with a key, an alias check and the in-memory
  limiter: 24), `BenchmarkStub` (the stub's share, to subtract),
  `BenchmarkLoopback` (over real sockets, direct against proxied) and
  `prod_bench_test.go`'s `BenchmarkProduction` (with real telemetry
  exporters), and `chat`'s `BenchmarkChunkNullUsage` (0 allocations).
  `TestOverheadPercentiles` and `TestMemoryPerStream` are spike
  measurements, run with `GOLEM_SPIKE=1`. Run them before and after a
  change: `go test -run '^$' -bench . -benchmem ./gateway/proxy/`.

## Extending it: a new dialect

Everything that differs by dialect is in `gateway/proxy/dialect.go`, as
methods of the exchange; the rest of the path is dialect-free. To add one:

1. Write its package under `gateway/dialect/<provider>/<api>`, on
   `jsonscan`: a `Request` with `Scan` (model, stream, max tokens, policy
   flags, unknown members) and `Rewrite` (the model spliced, anything golem
   needs asked for), and a `StreamUsage` with `OnEvent` (and `Want`, if
   some events can be skipped unread) and `Response`, setting `Final` only
   for a complete report and `Err` for a malformed one.
2. Add a `Dialect` constant and its `Name` (the Backend's `spec.dialect`).
3. Give the exchange fields for its request and usage reader (bind their
   methods in the pool's `New`), and a branch in each dialect method:
   `scan`, `forbidStorage`, `rewrite`, `startStream`, `readWhole`, `usage`,
   `estimate`, `appendError` and `streamError`.
4. Mount it in `cmd/golem-gateway/main.go`, and add it to the tests that
   run per dialect (`TestUpstreamCut`, `TestEstimates`, `TestRefusals`).

## Known gaps

- One attempt only: no retries or failover before the first byte, no
  request hooks, and `Route.Pick` takes the first deployment
  (gateway.md §3 steps 4, 5 and 7).
- No cross-dialect conversion: a route speaks its frontend's dialect or is
  refused (`route.wrong_frontend`); `canon` holds only `Usage`.
- A non-streamed response over 1 MiB keeps only part of itself, so its
  usage (usually at the end) is lost and the call is estimated from its
  bytes.
- Estimates are byte counts: they do not bound images, audio or files, or
  units billed outside tokens (ADR 0044), and the provider's work past a cut
  is unseen until reconciliation.
- `Handler.MaxBody` has no default: a zero cap refuses every body.
