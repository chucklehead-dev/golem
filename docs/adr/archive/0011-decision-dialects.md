# 0011: Decision APIs are a dialect family of their own

Status: Proposed. Scheduled for M2 by the roadmap (pass-through), M3 (translation).

## Context

OpenAI's Decisions API (`POST /v1/decisions`) and TypeSafe's System One
(`POST /v1/systemone`, the Jev model) answer typed questions (predicate or
yes/no, choice, score) about an input instead of generating text. Both are
non-streaming and billed on input tokens only.

## Decision

- Both are frontends and backends in M1, same-dialect pass-through only.
- They form a **decision** family: an alias of decision models may contain
  only decision deployments; config validation refuses mixing them with
  chat models.
- The estimate is exact (input only), so reservations equal the final
  cost; `stream: true` is refused by golem in the dialect's error shape.
- M3: translation between the two through a small decision IR (questions
  with id, type, instructions, criteria; answers with id, value,
  probability), so one alias can fail over or be cost-routed between
  providers. Fields one side lacks fail the request.
- Decision models are offered to hooks as a cheap classifier for
  guardrails (PRD §5.7).

## Consequences

- Two small dialect packages, no streaming code.
- Request and response schemas must be taken from the providers'
  references when implemented; the vendors' speed and cost claims are not
  relied on.

## Alternatives

- Treat them as chat with structured output: loses their typed answers and
  input-only pricing.
