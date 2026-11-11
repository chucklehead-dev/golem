# 0026: Identifiers and headers

Status: Accepted. Amends 0009 (key format).

## Decision

- Keys carry their environment: `glm_live_` (an installation) and
  `glm_dev_` (standalone and `make dev-up`); each kind of gateway refuses
  the other's.
- golem's request ID (UUIDv7) on every response (`golem-request-id`) and in
  error bodies; the caller's `x-request-id` and the provider's request ID
  recorded beside it.
- One session concept for prompt-cache affinity and Langfuse sessions,
  taken from `golem-session`, else what each agent already sends, mapped
  per agent and pinned by the compatibility tests.
- golem's headers (`golem-scope`, `golem-tags`, `golem-end-user`,
  `golem-warning`, `golem-route`, `golem-translated`) carry no `x-` prefix
  and are never forwarded upstream (contracts.md §5).

## Alternatives

One key prefix (a laptop key indistinguishable from a production one);
separate session notions for caching and tracing (two answers to one
question).
