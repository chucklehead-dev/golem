# 0035: M1 serves three providers without any translation

Status: Accepted. Amends the M1 scope of PRD §5.1–5.2 and ADR 0001.

## Context

The first team needs Anthropic models through AWS Bedrock, OpenAI models
through OpenAI's API, and custom and local models on vLLM. The earlier M1
also carried the direct Anthropic API, Microsoft Foundry, and two
translations (OpenAI Chat to Anthropic Messages, OpenAI Chat to Bedrock
Converse): the largest and riskiest code in the project.

## Decision

M1's paths are all same-dialect:

| Caller's dialect | Backend | What golem does |
|---|---|---|
| Anthropic Messages (Claude Code, Anthropic SDKs) | Bedrock, Anthropic models | **Envelope only**: InvokeModel / InvokeModelWithResponseStream with the Messages body; the model in the path (alias to a Bedrock model or inference profile ID); `anthropic_version` set; `anthropic-beta` headers moved into the body's `anthropic_beta` field; AWS event-stream frames re-emitted as Anthropic SSE events; SigV4 from Pod Identity. `count_tokens` through Bedrock's token-counting API where the model supports it, else golem's estimate |
| OpenAI Chat, Responses, Embeddings (OpenAI SDKs, Codex, Copilot BYOK) | OpenAI | Pass-through (fast path) |
| OpenAI Chat, Completions, Embeddings | vLLM | Pass-through |
| Anthropic Messages | vLLM (current versions serve `/v1/messages`) | Pass-through |

- An alias names models of one dialect family per frontend: a Claude alias
  is reachable from the Messages frontend; an OpenAI or vLLM alias from the
  OpenAI frontends (and vLLM models from Messages too). Asking for a model
  through a frontend that cannot reach it is refused, naming the frontend
  that can.
- Translation (Chat → Anthropic, Chat → Converse) moves to M2, with the
  direct Anthropic API, Microsoft Foundry, and Bedrock from other sites.

## Consequences

- No translation code, no intermediate representation, in M1; the one
  non-trivial adapter is Bedrock's envelope, which is small and has a clear
  oracle (the Messages API itself).
- OpenAI-SDK applications that want Claude wait for M2, or use an
  Anthropic SDK.
- Failover in M1 stays within a dialect: Claude on Bedrock fails over across
  Bedrock regions and inference profiles; a vLLM alias reached through the
  OpenAI frontends may fall back to an OpenAI model; Claude cannot fall back
  to an OpenAI model (or the reverse) until translation lands.

## To verify in Phase 0

Bedrock's handling of Claude Code's current beta features in the body,
prompt caching through InvokeModel, token counting on Bedrock, and
inference-profile naming in aliases.
