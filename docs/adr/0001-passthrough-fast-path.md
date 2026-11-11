# 0001: Same-dialect requests are forwarded as bytes

Status: Proposed

## Context

LiteLLM and most gateways decode every request into one canonical format
and re-encode it for the provider. That costs CPU and allocations per
request, and it loses fields the canonical format lacks: a new provider
parameter or beta feature disappears until the gateway learns it. Coding
agents use new provider features within days of release.

Most enterprise traffic is same-dialect: OpenAI SDKs to OpenAI or Azure,
Anthropic SDKs and Claude Code to Anthropic, Bedrock or Vertex (the last
two need a thin envelope change, not a translation of content).

## Decision

When a request's frontend dialect equals its backend's, golem forwards the
body as received, with only these byte-level edits: the model name
replaced (spliced at the offset the `jsontext` scan recorded) and, for
OpenAI streaming, `stream_options.include_usage` inserted if absent.
Responses are copied through, with an SSE scanner on a tee reading usage.

Translation goes through an intermediate representation (`dialect/ir`) and
is used only when dialects differ. Each translation pair is written and
tested on its own; unsupported features fail loudly (PRD §5.1.2).

## Consequences

- New provider fields work on the fast path the day they ship.
- Hooks that need the parsed request (guardrails) pay for a full decode
  only when selected; they get a lazily decoded view.
- Two code paths per dialect (pass-through and translate) to test.
- Envelope-only differences (Anthropic on Bedrock/Vertex: model in the
  path, `anthropic_version` in the body) are handled as "same dialect,
  different envelope", not as translation.

## Alternatives

- Canonical format for everything (LiteLLM): one path, but slower and lossy.
- Pass-through only, no translation (Langfuse gateway at launch): simplest;
  rules out serving Anthropic-dialect agents from OpenAI-compatible
  self-hosted models. Kept as the M1 scope; translation is M2.
