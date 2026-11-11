# 0009: Identity: an authenticator chain, Entra, checksummed keys, agents by token

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). From archive 0003, 0032; PRD §9.3. Amended by [0036](0036-no-mesh-identity-no-operator.md) (no mesh identity).

## Decision

- **Chain**: each authenticator recognises its credential by shape (key
  prefix, three-part JWS, a trusted mesh header) and answers `NotMine`,
  `Accepted` or `Rejected`; the first non-`NotMine` decides, so a bad JWT is
  never retried as a key. Everything verifies locally against the
  snapshot and cached JWKS; no network call on the request path.
- **Principal**: a user (Entra `oid`) or a service, the **scope** it acts
  in (a key's bound scope, or the `x-golem-scope` header with an Entra
  token, or the user's default), the key if any, and attributes for CEL.
  Membership and rights come from the snapshot, never token claims.
- **Entra** for users (v2 access tokens for golem's API app) and services
  (client credentials or workload identity federation; `azp`/`appid` maps
  to a service holding the `golem.service` app role).
- **Keys**: `glm_live_` or `glm_dev_` (ADR 0026) + 32 random bytes + a CRC32 checksum, base62; stored as
  SHA-256; bound to one scope; constrained (org-model §3). Registered as a
  GitHub secret-scanning custom pattern; golem-control's webhook receiver
  revokes reported keys. Entra sync revokes disabled users' keys.
- **Agents**: `golemctl token` prints a cached Entra access token for agents
  with a credential helper (Claude Code's `apiKeyHelper` and equivalents),
  with the scope in a header; agents that take only a static key get a
  minted personal key (12-hour default for `golemctl login`).
- **Mesh identity** (M2, after a spike): a waypoint passes the caller's
  SPIFFE ID in a header, trusted only when an AuthorizationPolicy admits
  nothing but the waypoint to the data port; it binds keys to workloads and
  identifies in-mesh services.
- **Credential pass-through** per route: `none` (default), `forward` (the
  caller's own provider credential, after golem authenticated the caller;
  M2), `exchange` (Entra on-behalf-of to Azure; M3).

## Consequences

One Principal feeds authorization, limits, attribution and telemetry.
Users must appear in the state (Entra sync or first API call) before the
gateway admits them.

## Alternatives

Delegating authn to the ingress (no virtual keys); minted keys for every
agent (a control-plane write per login).
