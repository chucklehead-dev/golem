# 0038: Dedicated provider accounts, monthly reconciliation, renewed slots, bounded worst case

Status: Accepted. Amends [0031](0031-soft-budgets.md), [0012](0012-limits-and-accounting.md) and PRD §7.

## Decision

- **Dedicated provider accounts or projects for golem** (OpenAI projects,
  Anthropic workspaces, AWS accounts or tagged Bedrock roles, Azure
  resources): the precondition for reconciliation, since provider usage
  data is per account or project, not per request.
- **Reconciliation is monthly** by account and model against provider
  usage and cost data (OpenAI and Anthropic usage APIs; AWS Cost and Usage
  Reports, a day or two late; Azure exports), within a 3–5% tolerance, with
  differences beyond it investigated. Daily runs are a preview. Invoice
  differences from credits, discounts and commitments are explained by
  contract terms, not booked as spend.
- **Chargeback** uses the provider's cost per account and model, split
  across scopes in proportion to golem's per-request attribution; golem's
  own prices only need to be good enough for budget checks.
- **Concurrency slots are renewed** every 30 seconds while a call is in
  flight (TTL 90 seconds), so a long agent stream keeps its slot and the
  overshoot bound holds; a crashed gateway's slots expire within the TTL.
- **The worst case is bounded**: a `ScopePolicy` sets a ceiling on
  `max_tokens` and on input size, enforced by the gateway; the strict
  check's worst-case cost uses them, so the bound shown to budget owners
  is a known number. Input tokens are bounded by the body's byte count; no
  tokenizer is needed.

## Consequences

The ledger is reconciled, not exact; the accepted tolerance is stated.
The stated overshoot bound becomes a number per scope.
