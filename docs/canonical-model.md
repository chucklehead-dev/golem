# golem: the canonical model

Status: v1 baseline, for M1.5. The internal representation that dialect
adapters translate through (ADR 0001), the capability matrix that routing
uses (ADR 0028), and the determinism rule that keeps provider prompt
caches working (ADR 0028). Internal to the gateway: never an API.

## 1. Shape

Package `gateway/canon`.

- **Request**: system content (blocks); messages (role, content blocks);
  tools (name, description, JSON Schema input); tool choice (auto, none,
  required, a named tool); sampling (temperature, top-p, top-k); stop
  sequences; output limit; response format (text, JSON schema); reasoning
  (on/off, budget or effort); cache hints (breakpoints with TTL); metadata
  (end user, session); stream flag.
- **Content blocks**: text; image (URL or bytes, media type); document
  (PDF and text, with citations flag); tool use (id, name, input as JSON);
  tool result (tool use id, content blocks, error flag); thinking (text,
  signature) and redacted thinking.
- **Response**: id, model, content blocks, stop reason (end, max tokens,
  stop sequence, tool use, refusal, pause), usage (input, output, cache
  read, cache write by TTL, reasoning, server-tool units).
- **Stream events**: message start (id, model, input usage); block start
  (index, kind); block delta (text, tool-input JSON fragment, thinking text,
  signature); block stop; message delta (stop reason, output usage); error;
  keepalive.
- **Extensions**: a per-dialect map of fields with no canonical meaning,
  carried only when encoding back into the same dialect, and dropped (and
  reported) otherwise.

The model is Messages-shaped (the richer family) but not Messages: it is
golem's to evolve, versioned in code (`canon.Version`), and adapters
declare the version they implement.

## 2. Adapters

Each dialect implements, in its package:

| Function | From → to |
|---|---|
| `DecodeRequest`, `EncodeRequest` | dialect request ↔ canonical request |
| `DecodeResponse`, `EncodeResponse` | dialect response ↔ canonical response |
| `NewStreamDecoder`, `NewStreamEncoder` | incremental: dialect events ↔ canonical events, as state machines |
| `Capabilities` | what the dialect can express (§3) |

Translation is `EncodeRequest(backend, DecodeRequest(frontend, req))`,
then the backend's envelope. Responses and streams go the other way.

Mapping notes for the M1.5 pair:

- **Chat → canonical**: `system` and `developer` messages become system
  content; `tool_calls` become tool-use blocks (arguments parsed as JSON;
  invalid JSON is refused); `tool` messages become tool-result blocks;
  `response_format: json_schema` becomes the canonical response format;
  `reasoning_effort` becomes reasoning effort; `n > 1`, `logprobs`,
  `logit_bias`, `seed` are refused (`translate.unsupported_feature`).
- **Canonical → Messages**: system content to `system`; consecutive
  same-role messages merged; tool results placed in the user turn that
  follows the tool use; the response format as Messages' structured output
  where the model supports it, else refused; cache hints as
  `cache_control`.
- **Messages → canonical → Chat**: thinking blocks with signatures cannot
  round-trip through Chat; they are dropped from history sent to a Chat
  backend (reported), and a request that requires them is refused.
- **Streams**: Chat's tool-call argument deltas (by index) become
  tool-input JSON deltas on the matching block, and back; usage arrives in
  Chat's final chunk (requested by golem, stripped if the caller did not
  ask) and in Messages' `message_delta`.

## 3. Capabilities and capability-aware routing (ADR 0028)

The model catalog records, per model (and per deployment where they
differ): text, images, documents, tools, parallel tool calls, reasoning,
JSON-schema output, prompt caching (and TTLs), context window, maximum
output, server-side tools.

- The frontend's decoder (or, on the pass-through path, the scan) records
  which capabilities a request **uses**: images present, tools present,
  reasoning requested, a JSON schema, its estimated input size.
- The router filters each pool's deployments by those capabilities before
  choosing one; fallback pools are filtered the same way, so a failover can
  never land on a deployment that cannot serve the request.
- If no deployment fits: `route.no_capable_deployment`, naming the missing
  capability and the alias.
- The capability matrix of the dialects themselves (what each can
  express) gates translation: a request whose features the backend
  dialect cannot express is refused before any upstream call.

## 4. Determinism (ADR 0028)

Provider prompt caches match on exact prefixes. An encoder that varies its
output for the same input (map order, whitespace, number formatting, field
order, merged-message boundaries) turns every translated request into a
cache miss, which on coding-agent traffic can double its cost.

- Encoders are pure functions of the canonical value: the same canonical
  conversation prefix always produces the same bytes, independent of the
  rest of the request.
- JSON is written by golem's own encoder in a fixed field order with no
  map iteration; numbers in a canonical form; tool input JSON re-emitted in
  its original form where it was received (kept as raw bytes), not
  re-serialised.
- Cache breakpoints placed by golem (for Chat callers on Claude) are put at
  stable positions: the end of the system content and the last stable turn.
- A property test: for random conversations and random extensions of them,
  the encoded cache-relevant blocks of the prefix (system content, tools,
  earlier turns) are byte-identical in the encoding of the extended
  conversation, for every encoder. Whole request bodies are not prefixes of
  each other (closing delimiters and per-request fields change); the
  property is about the blocks providers cache.

## 5. Tests

- Per adapter: `EncodeRequest(DecodeRequest(x)) == x` on the supported
  subset (byte-identical where the dialect is byte-stable, structurally
  otherwise); `DecodeRequest(EncodeRequest(c)) == c` for canonical values
  the dialect can express; features outside the subset refused, never
  dropped.
- Streams: any chunking of the same upstream stream produces the same
  canonical events and the same client output.
- Determinism (§4) and capability filtering (§3) as properties.
- Decoders fuzzed; goldens from recorded traffic per pair; a nightly
  differential run with real SDKs against Bedrock and OpenAI.
