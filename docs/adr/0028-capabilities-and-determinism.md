# 0028: Capability-aware routing; deterministic encoders

Status: Accepted. Detailed in canonical-model.md.

## Decision

- **Capabilities**: the model catalog records what each model (and
  deployment) supports: text, images, documents, tools, parallel tool
  calls, reasoning, JSON-schema output, prompt caching and TTLs, context
  window, maximum output, server-side tools. Each request records what it
  uses. The router filters every pool, fallback pools included, by those
  capabilities before choosing; nothing fitting is
  `route.no_capable_deployment`, naming the missing capability. The
  dialects' own capability matrix gates translation before any upstream
  call.
- **Determinism**: every encoder is a pure function of the canonical
  value, writes JSON in a fixed order without map iteration, keeps tool
  input JSON as the raw bytes received, and places golem's cache
  breakpoints at stable positions, so the same conversation prefix always
  encodes to the same bytes and provider prompt caches hit. A property test
  checks that the encoding of a prefix is a byte prefix of the encoding of
  any extension.

## Consequences

Failover can never land on a deployment that cannot serve the request;
translated traffic keeps prompt-cache economics.

## Alternatives

Routing by alias alone (failures after the call, or worse, silently
degraded answers); encoders without a determinism rule (cache misses that
double the cost of agent traffic).
