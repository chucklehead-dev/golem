# 0033: Cross-dialect conversion only where a route asks for it

Status: Accepted. Amends [0001](0001-dialects-canonical-model-and-envelopes.md) and [0002](0002-no-replay-after-first-byte.md).

## Decision

- Translation through the canonical model is used only when a route names
  a target in another dialect: an OpenAI-SDK route whose alias points at
  Claude, or a Messages route pointed at a Chat-only model.
- **No implicit cross-dialect failover.** Fallback pools stay within the
  caller's dialect (another Bedrock region or inference profile, the
  direct API, another vLLM box) unless the route lists a cross-dialect
  fallback explicitly, which is meant for non-agent applications.
- Coding-agent routes never convert: agents depend on their vendor's
  features (thinking signatures, betas, server tools) and on provider
  prompt caches, which a conversion mid-session loses.

## Consequences

The M1.5 adapters serve the explicit routes; no outage silently moves an
agent session to another vendor.
