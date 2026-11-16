# 0014: Lifecycle hooks: four phases, in-process or remote, selected by CEL

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). **Applies from M2**: nothing of this record is built or planned for M1 (m1-plan.md).

## Decision

- Phases: **Request** (after authentication, before routing: reject,
  rewrite, annotate, add routing hints), **Chunk** (stream events; M2, only
  when a guardrail needs it, the one phase that forces stream parsing on
  the fast path), **Response** (the complete response or stream end),
  **Complete** (asynchronous, on a bounded queue: audit, memory writes,
  evaluation sampling).
- Each phase is a Go interface over a narrow view of the request (the
  canonical model, ADR 0001, decoded lazily when a hook needs it), returning
  continue, reject (rendered in the caller's dialect), rewrite or annotate.
- In-process hooks are registered at build time; remote hooks (M2) are one
  gRPC service and an HTTP/JSON equivalent with the same phases. Each hook
  has a CEL `when`, a timeout and a failure mode (open or closed); a span
  and outcome metrics per hook.
- Built-ins (M2): deny lists, PII redaction (Presidio-compatible), a
  prompt-injection classifier, prompt size limits, moderation, and a
  decision-model classifier (ADR 0001's decision APIs as a cheap guardrail).
- Upstream headers are Backend config, not a hook.

## Alternatives

Envoy ext_proc compatibility (byte-level, not LLM-level); WASM (a large
runtime for little gain in a bespoke build).
