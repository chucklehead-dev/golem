# 0034: Accounting correctness: estimates, cut streams, reconciliation

Status: Accepted. Amends 0005, 0007, 0031.

## Context

The review found the money path underspecified exactly where coding
agents stress it: large `max_tokens` with mostly cached input, ten-minute
streams, client disconnects, gateway crashes, lost counter updates, and
provider-side state.

## Decision

- **Estimates are expected, not worst case.** A reservation prices the
  input at the principal's recent cache-hit ratio (an EWMA kept in the
  site's Valkey) and the output at the smaller of `max_tokens` and the
  principal's recent p95 output for that model; a scope may choose worst
  case. Overshoot stays bounded by caps and ceilings, and reconciliation
  corrects every request.
- **Long streams renew their reservation** every minute while open
  (default TTL 5 minutes), so a ten-minute stream never loses it and a
  crashed gateway's reservations lapse within five.
- **Cut streams are charged what was produced.** If a stream ends before
  the provider's final usage event (upstream failure, client disconnect,
  gateway shutdown), the gateway counts output tokens from the deltas it
  saw (a per-family tokenizer approximation) and records the request with
  `usage_estimated=true`.
- **Client disconnect closes the upstream** at once (no paying for tokens
  nobody reads), except for non-streaming requests a scope marks
  `complete_on_disconnect` (batch-like work).
- **Provider reconciliation.** A daily job compares the ledger with each
  provider's usage or cost API (OpenAI and Anthropic organisation usage
  APIs, AWS Cost and Usage data, Azure cost exports) by account and model,
  and reports drift beyond 1%. Drift from crashes (provider billed, ledger
  missing) shows up there; the ledger is corrected with an adjustment
  record, never edited.
- **Counter repair waits for the pipeline.** Rebuilding site counters from
  the ledger (after a Valkey failover lost writes) and the daily
  correction use only ledger data older than the collector's maximum
  queue delay (one hour), so records still in flight are not double
  counted.
- **No surprise chunks.** When golem adds `stream_options.include_usage`
  to an OpenAI request the client did not ask for, it strips the extra
  usage chunk from the client's stream.
- **Provider-side state pins.** A Responses API request with
  `previous_response_id`, or a `GET`/`DELETE` of a stored response, goes to
  the deployment that holds that response (a response-id-to-deployment map
  in the site's Valkey, kept for the provider's retention); failover is not
  attempted for it, and residency follows that deployment.

## Consequences

- Heavy users are not refused by worst-case arithmetic; the cost is that a
  burst can overshoot a cap by its estimation error before reconciliation.
- The ledger has estimated rows, flagged, and adjustment rows; invoices
  reconcile against both.
