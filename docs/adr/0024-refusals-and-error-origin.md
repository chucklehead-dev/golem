# 0024: Refusals that retrying cannot fix are not 429; every error has an origin

Status: Accepted.

## Context

The SDKs golem serves, and the agents built on them, retry 429, 408, 409
and 5xx automatically with backoff. An exhausted budget returned as 429
makes clients retry for nothing and hides the reason behind the backoff.
Separately, SLOs, failover, ejection and support all need to know whose
fault a failure was, and that is painful to add to the ledger and metrics
later.

## Decision

- 429 with `Retry-After` only for limits that time will lift (rate,
  concurrency, provider quota shares); 503 with `Retry-After` when every
  deployment failed before the first byte.
- Budget, cap and ceiling exhaustion in `enforce` mode: **402**; not
  authenticated: 401; not allowed: 403; not routable or not translatable:
  400 or 404. None of these is retried by the SDKs.
- Bodies in the caller's dialect's error shape, with a stable golem code,
  what to do next, who can change it, and the request ID (contracts.md §1).
- Every failed or degraded request carries `golem.error.origin`: `client`,
  `policy`, `provider` or `gateway` (contracts.md §2), on spans, the access
  log, the ledger and error metrics. SLOs, ejection and failover use it.
- The SDK compatibility tests pin each SDK's retry rules, so a change in an
  SDK's behaviour fails a test rather than a user.

## Alternatives

429 for everything golem refuses (provider-like, but retried pointlessly);
deriving origin later from status codes (ambiguous: a provider 400 and a
golem 400 look alike).
