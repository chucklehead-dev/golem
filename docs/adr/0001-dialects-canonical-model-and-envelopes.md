# 0001: Same-dialect pass-through; translation through one canonical model; hosting as envelopes

Status: Accepted (v1 baseline). From archive 0001, 0011, 0035. Amended by [0033](0033-explicit-cross-dialect-conversion.md) (conversion only where a route asks), [0037](0037-request-policy-and-cache-stability.md) (semantic cache stability).

## Context

Callers speak OpenAI Chat Completions, OpenAI Responses, Anthropic
Messages, and others; providers host models behind the same few API
shapes, but each hosting adds its own wrapping: Claude is the Messages API
directly, on Bedrock (InvokeModel), on Vertex (rawPredict) and on
Microsoft Foundry (`/anthropic/v1/messages`); OpenAI models are the OpenAI
API directly and on Foundry. Converting every caller format to every
provider format pairwise grows as N×M and duplicates each provider's
quirks in every pair. Most traffic is same-dialect and must stay untouched
(new provider features work on the day they ship).

## Decision

Two independent axes:

1. **Dialect**: the shape of a request, response and stream (OpenAI Chat,
   OpenAI Responses, Anthropic Messages, Bedrock Converse, and the decision
   APIs, below).
2. **Envelope**: how a host carries a dialect: the URL and model
   placement, required body fields, auth, and stream framing. Messages has
   envelopes `anthropic` (direct), `bedrock` (InvokeModel; model in the
   path, `anthropic_version`, `anthropic_beta` in the body, AWS event-stream
   framing re-emitted as SSE, SigV4), `vertex` and `foundry`; OpenAI Chat
   has `openai`, `foundry` and `vllm`.

A deployment is a dialect plus an envelope. Requests take one of two paths:

- **Same dialect: pass-through.** The body is forwarded as bytes with only
  the model name spliced (and `stream_options.include_usage` added for
  OpenAI streams, the extra chunk stripped if the caller did not ask);
  the envelope does its wrapping. No decoding into structs.
- **Different dialects: through the canonical model.** Each dialect has
  one adapter pair, `decode` (dialect → canonical) and `encode`
  (canonical → dialect), for requests, complete responses and stream
  events. Translation is `encode_backend(decode_frontend(x))`, and the
  backend's envelope then wraps it. Adding a dialect is one adapter, not a
  row of pairs; adding a host of an existing dialect is one envelope.

So Chat → Claude on Bedrock is the Chat decoder, the Messages encoder and
the `bedrock` envelope: the Bedrock part is the same envelope M1 already
uses for Messages callers, and costs nothing more.

### The canonical model

Close to the richer of the families (Messages-like content blocks), in
`gateway/canon`:

- a request: system content, messages of typed content blocks (text,
  image, document, tool use, tool result, thinking with its signature),
  tools (JSON Schema), tool choice, sampling parameters, stop sequences,
  output limits, response format (JSON schema), cache hints, metadata;
- a response: content blocks, stop reason, usage (input, output, cache
  read, cache write, reasoning);
- a stream: message start, block start, block delta (text, tool input
  JSON, thinking), block stop, message delta (stop reason, usage), error.
  Stream adapters are incremental state machines (an OpenAI tool-call
  argument delta becomes a tool-input JSON delta, and back).

Rules:

- A feature one side cannot express **fails the request** with an error
  naming the feature (OpenAI `n > 1`, `logprobs`, an Anthropic thinking
  signature into a dialect that cannot return it), unless it is
  semantically inert (metadata), which is dropped and reported in a
  response header.
- Cache hints: a Chat caller has no `cache_control`; when the backend is
  Claude, golem may place cache breakpoints itself (on the system prompt
  and the conversation prefix) by route policy, the largest cost lever for
  translated traffic.
- Dialect-specific extras with no canonical meaning ride in a per-dialect
  extension map, used only when encoding back to the same dialect.

### Timing

M1 needs no translation (Claude on Bedrock for Messages callers, OpenAI
and vLLM pass-through). **M1.5** adds the canonical model with the Chat and
Messages adapters in both directions, so Chat callers reach Claude on
Bedrock and Messages callers reach Chat-only models (OpenAI, vLLM).
Converse, Responses-as-translation and the decision APIs' shared shape
follow by demand.

### Decision APIs

OpenAI Decisions (`/v1/decisions`) and TypeSafe System One
(`/v1/systemone`) are a separate family: typed questions (predicate or
yes/no, choice, score), non-streaming, billed on input only (so the
reservation is exact). Pass-through in M2; a small canonical decision
shape lets one alias fail over between them later. An alias never mixes
decision and chat models.

## Consequences

- N adapters plus M envelopes instead of N×M converters; Bedrock, Vertex
  and Foundry hosting of Claude are envelopes over one Messages adapter.
- Translation tests are per adapter: round trips (`encode(decode(x)) ≈ x`
  on the supported subset), goldens from recorded provider traffic,
  fuzzing of decoders, and properties over the canonical model; pairs come
  for free.
- The canonical model is golem's to evolve; it is internal (never an API).

## Alternatives

- Pairwise converters: simplest for two formats, quadratic after.
- Translate everything, always (LiteLLM): one path, but slow and lossy for
  same-dialect traffic.
- Adopt one provider's format as the canonical model verbatim: ties golem
  to that provider's evolution; the canonical model is Messages-shaped,
  not Messages.
