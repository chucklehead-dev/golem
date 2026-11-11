# 0027: Provider-side storage is forbidden unless a scope allows it

Status: Accepted.

## Context

OpenAI's Responses API stores responses by default (`store: true`); file
and batch APIs keep uploads at the provider. Such data escapes golem's
capture and retention rules (ADR 0016).

## Decision

`ScopePolicy.provider_storage: forbidden | allowed`, default forbidden.
When forbidden, golem sets `store: false` (and equivalents) and refuses file
and batch uploads (`authz.provider_storage_forbidden`), so stateful
continuations are unavailable in that scope. When allowed, the ledger
records that the provider holds data, and managers see it in the console.

## Consequences

The default keeps all prompt content inside golem's own retention rules;
scopes that need stateful Responses features opt in, visibly.

## Alternatives

Pass the providers' defaults through (silent retention outside golem's
rules).
