# 0051: The gateway's retries are failovers, of refusals only

Status: Accepted. Refines [0002](0002-no-replay-after-first-byte.md)'s
per-request retry budget and [0024](0024-refusals-and-error-origin.md)'s
retryable refusals for the attempt loop (model/attempts.md,
m1-plan.md step 2). Decided by the product owner.

## Context

An LLM call is not idempotent in cost. A provider that saw a request may
be generating, and billing, after the connection is gone; a retry then
pays twice for one answer. Other gateways retry by default on more than
refusals: Portkey on 429, 500, 502, 503, 504 (and 529), up to five times;
LiteLLM per failure kind, timeouts included; the official SDKs on 408,
409, 429 and 5xx, twice, and on connection errors (docs/spikes/
sdk-compat.md). agentgateway's retries are opt-in per route, by status.

Two things golem's first attempt loop (34613c2) did beyond what only a
gateway can do:

- It retried a call on the deployment it had just tried (a pool of one
  retried on itself). The client's SDK, and agents such as Claude Code,
  already retry the same endpoint, with backoff: the gateway's same-
  deployment retry multiplied attempts and added nothing. The retry-storm
  simulation (route's `TestRetryBudgetBoundsTheStorm`) found the
  gateway's layer does not decide whether a brownout recovers; the SDK's
  retries do.
- It retried a 500. No provider documents a 500 (or a 529) as unbilled,
  and a 500 may come after the model has worked.

## Decision

- **A retry is a failover.** An attempt goes only to a deployment of the
  pool the call has not tried. A pool of one makes one attempt; the
  client retries, as it would without golem. `spec.retries.maxAttempts`
  (default 2) bounds the deployments a call tries.
- **Only refusals are retried**: the provider never saw the request
  (DNS, dial, TLS), or refused it before any output: 429, 503, 529, or a
  200 whose first event is an error (the commit point, commit.go). Not
  500, 502 or 504, not a timeout or reset after the request went out
  (uncertain: it ends the call, charged its worst case), and nothing after
  the commit.
- The rest of the loop stands: one admission per call, a retry budget per
  route (a fifth of its calls), ceilings and ejection per deployment, each
  attempt its own ledger event.

## Consequences

- golem never pays twice for a call by its own choice: what it retries
  the provider did not work on.
- A deployment's transient refusals are the client's to retry when the
  pool has no other deployment; a route that wants the gateway to absorb
  them needs a second deployment (another region, inference profile or
  account), which is also what an outage drill needs (M1's exit).
- A 500 fails the call to its caller, who may retry it (the SDKs do): the
  choice, and its cost, are the caller's.
- model/attempts.md takes the rule (an attempt only to an untried
  deployment, in every design); its checks and the gateway's connect test
  hold it. Its search also found a probe left standing after a
  readmission, now ended by the readmission (route's
  `TestReadmissionEndsProbes`).
