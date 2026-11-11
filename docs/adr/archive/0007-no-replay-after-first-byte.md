# 0007: Retries and failover stop at the first response byte

Status: Proposed

## Context

A streaming response that fails midway could be retried on another
deployment, but the caller has already received tokens; a replay would
duplicate or contradict them, and charge twice.

## Decision

An attempt is committed when the first response byte is written to the
caller. Before that, connection errors, timeouts to first byte, 429 and
5xx are retryable within the retry budget, on the same or a fallback
pool. After it, an upstream failure ends the stream with an error event in
the caller's dialect (Anthropic `error` event, OpenAI error chunk), and
usage seen so far is charged.

Non-streaming requests may optionally buffer the full upstream response
before committing, which makes mid-body upstream failures retryable at the
cost of latency; off by default.

## Consequences

- Failover is invisible to callers before the stream starts and explicit
  after; agents already handle stream errors by retrying the turn.
- Time-to-first-byte timeouts must be set per deployment so a hung
  provider fails over before the caller gives up.

## Alternatives

- Resume on another deployment by re-prompting with the partial output as
  a prefill: model- and provider-specific, changes semantics; rejected.
