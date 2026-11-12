# Spike: the SDKs' retry rules, and the SDKs through the gateway

Question (roadmap Phase 0, "SDK retry rules"): which status codes do the
official SDKs retry, and is 402 right for budget refusals (ADR 0024)? And do
the SDKs work unchanged through the gateway's fast path?

`test/compat` answers both, in CI: a Go test starts a stub provider, the
gateway compiled from CRD YAML through the standalone path, and a status
server, and drives the official Python and TypeScript SDKs (Anthropic and
OpenAI; versions pinned in `test/compat/clients`).

## Retry rules

Attempts per status, two retries allowed, Anthropic Python 2.8.0 and
TypeScript 1.8.0, OpenAI Python 3.49.0 and TypeScript 7.55.0: all four
identical.

| Status | Attempts | |
|---|---|---|
| 400, 401, 402, 403, 404, 413, 422 | 1 | not retried |
| 408, 409, 429, 500, 502, 503, 504 | 3 | retried |

So contracts.md §1 holds as written: budget refusals as 402 are final in
every SDK, limits as 429 with `Retry-After` are retried, and a gateway
fault (500) or an expired lease (503) is retried. The test fails if an SDK
release changes any of it.

Not yet covered: the agents (Claude Code, Codex, Copilot's BYOK clients) and
their session headers; they need their binaries and, for a live run, keys.

## Through the gateway

Create, stream, and an unknown model's error, in both SDKs and languages:
text, usage and errors as the provider would give them; the error's
message (and, for OpenAI, its `code`) are golem's.

**One finding, fixed.** The gateway adds `stream_options.include_usage` to
an OpenAI stream the caller sent without it, since a stream without usage
cannot be priced. The provider then ends the stream with a usage-only chunk
whose `choices` is empty, and the ordinary loop both SDKs document,
`chunk.choices[0].delta`, crashed on it (Python `IndexError`, Node
`TypeError`). The gateway now withholds that chunk when it was golem that
asked for it, re-serialising the stream event by event in that case only
(OpenAI streams carry only `data:` lines, so this is exact), and still reads
the usage. A caller that asked for usage gets the chunk as before.
