# Spike: the fast-path budget

Question (roadmap Phase 0): what do the jsontext scan and splice and the SSE
tee cost per request, against a stub upstream, in latency, allocations and
memory per stream? Can the pass-through path meet PRD §7?

Answer: yes, on every target measured here, with three changes to the
design's details (below). The code is the start of the gateway, not a
throwaway: `gateway/dialect/internal/jsonscan`, `gateway/dialect/anthropic/messages`,
`gateway/dialect/openai/chat`, `gateway/sse`, `gateway/proxy`,
`cmd/golem-gateway`, with the layering rules checked by `internal/archtest`.

## What was built

- **Scan**: `jsonscan.Object` walks a body's top-level members with
  `encoding/json/jsontext` (Go 1.27.3, no experiment flag), reading the
  routing fields and skipping the rest. It reads the **whole** document:
  duplicate names at any depth, invalid UTF-8 and trailing data are refused,
  because a provider's parser could read such a body differently (ADR 0037).
- **Splice**: the model's value is replaced by its byte span; for a streamed
  OpenAI Chat request `stream_options.include_usage` is added or set, since
  without it the stream carries no usage.
- **Tee**: the response is copied to the caller as each read arrives and
  flushed; the same bytes feed an `sse.Parser`, whose events feed the
  dialect's usage reader. Anthropic's content deltas are skipped by event
  name without copying their data.
- **Proxy**: one handler per dialect: body read under a cap, scan, route by
  alias, splice, one upstream attempt with allowlisted headers both ways,
  the tee, a result with usage. Refusals in the caller's dialect.

## Results

On the session's container (4 vCPU, Go 1.27.3); a 200-event Anthropic stream;
"small" is a 1 KB request, "agent" a 200 KB one (system prompt, 40 tools,
120 turns: a coding agent mid-session).

| Measure | Small | Agent | Target |
|---|---|---|---|
| Scan alone | 1.6 µs, 0 allocs | 260–320 µs, 0 allocs (640–790 MB/s) | — |
| Gateway in memory, whole request and stream (stub's own cost subtracted) | ~26 µs, 11 allocs, ~1.5 KB | ~330 µs, 11 allocs, ~1.7 KB | < 50 allocs (gateway.md §7) |
| Per stream event | ~130 ns | | — |
| Loopback, time to first byte, concurrency 1: p50 / p99 overhead | +0.14 ms / +0.26 ms | +0.64 ms / +2.4 ms | p50 < 1 ms, p99 < 5 ms |
| Memory per held stream, the gateway's share | 47–52 KiB | 57–58 KiB | < 64 KiB |

Reproduce: `go test -bench . ./gateway/proxy` and
`GOLEM_SPIKE=1 go test -run 'Overhead|Memory' -v ./gateway/proxy`.

The loopback overhead includes the extra hop's HTTP work and, for the agent
request, sending 200 KB a second time; most of the rest is the scan. At
concurrency 32 the client, gateway and provider shared four cores in one
process and the numbers measured the machine, not the gateway: the
1k req/s-per-core target needs a separate load generator and provider stub
(M0's benchmark rig, gateway.md §7).

## What changed in the design's details

1. **Names are compared unescaped.** `{"model": ...}` is the member
   `model` to every JSON parser. A scan that compared raw bytes would route
   and police by one model while the provider served another. jsonscan
   unescapes names before comparing; jsontext already treats an escaped
   duplicate as a duplicate. (A table test and the fuzz target hold this.)
2. **Request bodies are released once sent, not at the end of the stream.**
   The first version kept the caller's body and the rewritten body for the
   life of the stream: 400 KB per in-flight agent turn, for ten minutes, far
   past the 64 KiB target. The caller's body goes back to its pool after the
   splice; the rewritten one when the transport closes it, with every
   reference dropped (the transport keeps the request until the response
   ends, which kept the buffer alive through a stale pointer: found with a
   heap profile). A retrying attempt loop must keep the rewritten body until
   the attempt commits; that is M0's to measure.
3. **Line splitting uses `bytes.IndexByte`.** A byte loop looking for line
   ends was 40% of the gateway's CPU on a stream.

Smaller: the copy buffer is 8 KB per stream (events are mostly under 1 KB);
a body buffer is sized from Content-Length in one allocation; a pooled
buffer above 1 MB is not kept.

## Found by fuzzing

- An empty write between a chunk ending in `\r` and one starting with `\n`
  lost the pending `\r` and dispatched an event early. Fixed; the input is a
  regression case in `gateway/sse/testdata/fuzz`.
- The fuzz properties that hold: whatever the scan accepts, `encoding/json`
  accepts with the same members; a rewrite changes only the model (and, for
  OpenAI, `stream_options`); usage and events are the same however a stream
  is chunked.

## Open, for M0

- The benchmark rig with separate processes, for throughput per core and
  p99 under load.
- The attempt loop's body retention, and h2 to providers.
- Recorded provider streams (Anthropic, OpenAI, Bedrock's event-stream
  through its envelope, vLLM) instead of synthesised ones; the
  usage readers are tested on the documented shapes only.
- Unknown member names are counted, not yet logged by name (ADR 0037's
  bounded, sanitised logging).
