# 0025: The ledger schema and the price model are contracts

Status: Accepted.

## Decision

- `golem.spend` has the columns, units, rounding and row kinds of
  contracts.md §3: integer token counts by kind (input, output, cache
  read, cache write at 5 minutes and 1 hour, reasoning), other billed units
  in a map (server-side tools, images), cost in micro-units of one
  currency, the price version used, `actual`, `estimated` and `adjustment`
  rows that are never edited, its own schema version, and the error origin.
- A request is charged to the period of its **start**, in the period time
  zone.
- `PriceList` entries price input, output, reasoning, cache reads, cache
  writes by TTL, prompt-length tiers, batch, regional and inference-profile
  premiums, server-side tool calls and images, from an effective date;
  versions are immutable once used (contracts.md §4).

## Consequences

The 1% invoice target is reachable only because every billed dimension is
in the model from the start; chargeback consumers can rely on the schema.

## Alternatives

Tokens-only pricing extended later (wrong from the first invoice that has
cache writes or a long-context tier).
