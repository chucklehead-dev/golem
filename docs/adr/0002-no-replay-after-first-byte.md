# 0002: Retries and failover stop at the first response byte

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). Amended by [0033](0033-explicit-cross-dialect-conversion.md) (no implicit cross-dialect failover).

## Decision

An attempt is committed when the first response byte reaches the caller.
Before it, connection errors, timeouts to first byte, 429 and 5xx are
retried within a per-request retry budget, on the same pool or a fallback
pool (jittered backoff, `Retry-After` honoured). After it, an upstream
failure ends the stream with an error event in the caller's dialect, and
the usage produced so far is charged (ADR 0012). Failover crosses
dialects only through the canonical model (ADR 0001): before M1.5, a
Claude pool falls back only to Claude pools.

Non-streaming requests may opt in to buffering the whole upstream
response before committing, which makes mid-body failures retryable at the
cost of latency.

## Consequences

Failover is invisible before a stream starts and explicit after; agents
already retry a turn on a stream error. First-byte timeouts are set per
deployment so a hung provider fails over before the caller gives up.

## Alternatives

Resuming elsewhere by re-prompting with the partial output: model- and
provider-specific, changes semantics; rejected.
