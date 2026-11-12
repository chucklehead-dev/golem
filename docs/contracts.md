# golem: contracts

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Implementation remains outstanding.

Status: v1 baseline. The interfaces other people and systems will depend
on, written down before the first caller does: refusals and their status
codes, error origin, the ledger, the price model, identifiers and headers,
and provider-side retention. Decisions: ADRs 0024–0027.

## 1. Refusals and status codes (ADR 0024)

Every SDK golem serves (OpenAI's and Anthropic's, and the agents built on
them) retries 429, 408, 409 and 5xx automatically with backoff. A refusal
that retrying cannot fix must therefore not use those codes, or clients
retry pointlessly and users wait through backoff before seeing why.

| Situation | Status | Golem code (examples) | Retryable |
|---|---|---|---|
| Rate or concurrency limit; provider quota share full | 429 + `Retry-After` | `limit.rate`, `limit.concurrency`, `limit.provider_quota` | yes, after the delay |
| Budget or cap reached by known spend (in `enforce` mode) | 402 | `budget.pool_exhausted`, `budget.member_cap`, `budget.key_cap` | no |
| Not authenticated (missing, unknown, expired, revoked key or token) | 401 | `auth.missing`, `auth.key_revoked`, `auth.token_expired` | no |
| Authenticated, not allowed (model, scope, dialect, capture policy, provider storage) | 403 | `authz.model_not_allowed`, `authz.scope_not_member`, `authz.provider_storage_forbidden` | no |
| A malformed request (not one JSON object, duplicate members, a routing field of the wrong type, unreadable) or one over the size limit | 400 or 413 | `request.invalid`, `request.unreadable`, `request.too_large` | no |
| Request golem cannot route (no deployment with the capabilities used, unknown alias, frontend that cannot reach the model) | 400 or 404 | `route.unknown_model`, `route.no_capable_deployment`, `route.wrong_frontend` | no |
| A feature the translation cannot express | 400 | `translate.unsupported_feature` | no |
| Every deployment failed before the first byte | 503 + `Retry-After` | `upstream.unavailable` | yes |
| Upstream failure after the first byte | an error event in the stream | `upstream.stream_failed` | the client decides |
| The gateway's authorization lease has expired (ADR 0030) | 503 + `Retry-After` | `gateway.authorization_expired` | yes, once renewed |
| The gateway has no configuration yet | 503 + `Retry-After` | `gateway.not_ready` | yes |
| Golem's own fault | 500 | `gateway.internal` | yes |

- The body is the caller's dialect's error shape (OpenAI's `error` object,
  Anthropic's `type: error`), with golem's `code`, a message that says what
  to do next and who can change it (the scope's managers, a budget
  manager), and the request ID.
- `warn` mode lets the request through and adds `golem-warning:
  budget.member_cap 92%`; `observe` adds nothing to the response.
- The codes are a stable, documented list: this table, defined as code in
  `lib/contract` (the gateway can send no other), and checked against the
  table by a test in both directions. The ledger's DDL
  (`deploy/clickhouse/spend.sql`) is checked against §3 the same way.
- The SDKs' retry rules are pinned by the SDK compatibility tests (§7):
  `test/compat` checks every status above against the official Python and
  TypeScript SDKs in CI (docs/spikes/sdk-compat.md).

## 2. Error origin (ADR 0024)

Every failed or degraded request carries `golem.error.origin`:

| Origin | Meaning |
|---|---|
| `client` | The caller's request or credentials (4xx golem returned for the caller's reasons) |
| `policy` | golem refused by rule (budget, limit, authorization) |
| `provider` | The provider or model server failed or refused (its 4xx/5xx, timeouts, cut streams) |
| `gateway` | golem itself failed |

It is an attribute on the span, the access log and the ledger row, and a
label on the error metrics. SLOs (PRD §9.4) count `gateway` errors against
golem and `provider` errors against the provider; ejection and failover
act only on `provider`; dashboards and support start from it.

## 3. The ledger (ADR 0025)

`golem.spend` in ClickHouse is the chargeback contract. Immutable events per upstream attempt, with distinct estimate, settlement
and adjustment identities (ADR 0029); rows are never edited. Deduplicate
by event identity before sums and rollups; background merges alone do not
prevent double counting.

| Column | Type | Notes |
|---|---|---|
| `schema_version` | UInt16 | this table's contract version |
| `row_kind` | Enum: `attempt`, `adjustment` | attempt: one per upstream attempt, at its end (`estimated_fraction` > 0 when a cut stream was counted from deltas); adjustment: from provider reconciliation (ADR 0031) |
| `request_id`, `attempt_id`, `event_id` | UUID | logical request, upstream attempt, stable accounting-event identity (ADR 0029) |
| `provider_request_id` | String | the provider's own ID, for disputes |
| `started_at`, `ended_at` | DateTime64(3, UTC) | |
| `period` | Date | the **start** time's period in the period time zone: a stream crossing a period boundary is charged to the period it started in |
| `site`, `gateway_pool` | LowCardinality(String) | |
| `principal_kind`, `principal_id`, `key_id` | | user or service; key prefix and id, never the key |
| `scope_id`, `scope_path` | | `scope_path` is the team/sub-team/project chain at request time |
| `funding` | Array(Tuple(source, share)) | the scope's funding sources and shares at request time |
| `frontend_dialect`, `backend_dialect`, `envelope` | LowCardinality(String) | |
| `alias`, `model`, `deployment`, `provider_account` | | `model` as the provider names it (inference profile ID on Bedrock) |
| `tokens_input`, `tokens_output`, `tokens_cache_read`, `tokens_cache_write_5m`, `tokens_cache_write_1h`, `tokens_reasoning` | UInt64 | |
| `units` | Map(String, UInt64) | other billed units: `web_search`, `code_execution_seconds`, `images` |
| `cost_micros` | Int64 | in the configured currency's millionths; computed by golem |
| `currency` | FixedString(3) | |
| `price_version` | String | the `PriceList` version used |
| `outcome`, `error_origin`, `error_code` | | §1, §2 |
| `estimated_fraction` | Float32 | 0 for actual rows |

- Rounding: per row, cost is computed in micro-units from integer token
  counts and prices in micro-units per million tokens, rounded half-even
  once, at the end.
- Adjustment rows (from provider reconciliation, ADR 0012) carry the
  period, account and model they correct and a reason.
- Adjustments are attributed where the provider's usage data allows, else
  booked to an `unattributed` principal under the provider account, so
  totals match invoices even when a crash lost a few attempts' rows
  (ADR 0031).
- Balances and rollups are computed from a view deduplicated by
  `event_id`; no query sums the raw table (ADR 0029).
- Schema changes are additive within a version; a breaking change is a new
  table version with a view keeping the old shape for one release.

## 4. The price model (ADR 0025)

A `PriceList` entry prices a model on a provider account (or a whole
provider), from an effective date, in micro-units per million tokens or
per unit:

- input; output; reasoning (if billed separately);
- cache read; cache write by TTL (5 minutes, 1 hour);
- tiers by prompt length (for example above 200k input tokens), each tier
  with its own rates;
- batch discount (a factor or explicit rates);
- regional or inference-profile premiums (Bedrock cross-region profiles,
  regional endpoints);
- per-call units for server-side tools (web search, code execution) and
  per-image inputs where priced per image;
- for self-hosted models, internal rates set by the platform team.

Versions are immutable once used by a ledger row; a change is a new
version with a new effective date. The vendored list is refreshed by a
scheduled PR (PRD §9.7); enterprise rates override it.

## 5. Identifiers and headers (ADR 0026)

- **Keys**: `glm_live_…` (production installation) and `glm_dev_…`
  (standalone and `make dev-up`), then 32 random bytes and a CRC32 checksum
  in base62. A production gateway refuses `glm_dev_` keys and a standalone
  one refuses `glm_live_` keys. Both prefixes are registered for secret
  scanning.
- **Request IDs**: golem's request ID (UUIDv7) is returned on every
  response as `golem-request-id`, and in error bodies; an incoming
  `x-request-id` is recorded as the caller's. The provider's request ID
  (`request-id`, `x-request-id`, `x-amzn-requestid`) is recorded in the
  span and the ledger.
- **Sessions**: one session concept drives prompt-cache affinity and
  Langfuse sessions. golem takes, in order: `golem-session`, then what each
  agent already sends (Claude Code's session identifier in
  `metadata.user_id` or its session header, Codex's, OpenAI's `user` with a
  conversation prefix hash as a last resort). The mapping per agent is
  checked by the SDK compatibility tests.
- **Scope selection** with Entra tokens: `golem-scope: team/platform`.
- **Tags and end users**: `golem-tags`, `golem-end-user` (attributes, never
  identity).
- Response headers golem adds: `golem-request-id`, `golem-warning`,
  `golem-route` (alias and deployment, when the scope allows it),
  `golem-translated` (when the canonical model was used), and provider-style
  `x-ratelimit-*`.
- Header names have no `x-` prefix (RFC 6648), and golem never forwards its
  own headers upstream.

## 6. Provider-side retention (ADR 0027)

Some provider APIs keep data on the provider's side: OpenAI's Responses
API stores responses by default (`store: true`), and file and batch APIs
keep uploads. Data stored there escapes golem's capture and retention
rules.

- A `ScopePolicy` sets `provider_storage: forbidden | allowed`, default
  `forbidden`.
- When forbidden, golem sets `store: false` on Responses requests (and
  equivalents) and refuses file and batch uploads with
  `authz.provider_storage_forbidden`; stateful continuations
  (`previous_response_id`) are therefore unavailable in such scopes.
- M1 remains stateless even when allowed is configured: stored operations,
  continuations, background work and managed-object references are refused
  pending an ownership registry. Later, each object operation checks sharing
  rights and records provider storage (ADR 0029).

## 7. SDK and API-version compatibility

- A matrix of supported `anthropic-version` values, Anthropic beta
  features, OpenAI API features, and SDK and agent versions (OpenAI and
  Anthropic SDKs for Python, TypeScript, Go; Claude Code, Codex, Copilot
  BYOK clients), each tested nightly against the gateway with recorded and
  live providers.
- The tests pin each SDK's retry behaviour (§1) and each agent's session
  and header behaviour (§5). Built for the SDKs (`test/compat`); the agents
  are next.
- golem never changes a stream's shape for a caller: where it asks a
  provider for more than the caller did (OpenAI's `include_usage`), it
  withholds the extra from the caller.
- When a provider ships a breaking change, the pass-through path requires
  policy compatibility review before enabling new provider features; the adapters and envelopes get a fix and a matrix
  row, and the release notes name it.

## 8. Policy-relevant request fields (ADR 0037)

ADR 0041 §3 and §7.1 add feature modes. Restricted (unapproved API
versions, fields and betas rejected) is the default for the edge pool,
non-agent applications and sensitive or residency-bound scopes; compatible
(unknown features forwarded, exposure accepted until remediation) is the
default for coding-agent routes under one standing approval. Names are bounded/sanitized or keyed-hashed before logging;
values are never logged. Nightly diffs do not bound remediation time. These are enforced
wherever they appear, and the list grows with the nightly review:

| Field or family | Policy |
|---|---|
| `store`, `previous_response_id`, `background`, conversation identifiers | provider-side storage (ADR 0027; M1 refuses stateful use) |
| file, container and upload identifiers | provider-side storage |
| `mcp_servers`, server-side tools with network access (web search, fetch, code execution) | egress and data movement; billing units |
| `n`, `max_tokens` above the scope's ceiling, input above the scope's size ceiling | cost (ADR 0038) |
| `metadata.user_id`, `user`, session fields | attribution only, never identity |

## 9. Reconciliation (ADR 0038)

golem's providers run on dedicated accounts or projects. Each month the
ledger is compared with provider usage and cost per account and model;
differences within 3–5% are booked as adjustments, larger ones
investigated. Chargeback uses the provider's cost per account and model,
split across scopes in proportion to golem's per-request attribution.

