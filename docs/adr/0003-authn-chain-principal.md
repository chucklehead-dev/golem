# 0003: Authenticators in a chain yield one Principal

Status: Proposed

## Context

Callers present virtual keys (`Authorization: Bearer` or `x-api-key`,
depending on SDK), JWTs from the enterprise IdP, opaque OAuth tokens, or
client certificates. Some routes must forward the caller's own provider
credential.

## Decision

- `Authenticator.Authenticate(ctx, *http.Request) (Principal, Result)`
  where Result is `NotMine`, `Accepted` or `Rejected`. The configured chain
  stops at the first `Accepted` or `Rejected`: a credential that looks like
  a JWT and fails verification is rejected, not retried as a key.
- Credential recognition is by shape (key prefix `glm_`, three-part JWS,
  TLS peer certificate present) before any network call.
- `Principal` carries subject, kind, teams, cost center, the matched
  authenticator, and attributes for CEL. Mapping from claims or cert SANs
  is config.
- Keys are stored as SHA-256 hashes of high-entropy secrets (no slow hash
  needed: they are random, not passwords). Lookups cached.
- Behind a mesh, mTLS identity may come from a trusted forwarded header
  (XFCC) only when the listener is configured to trust it.
- Pass-through is a route property: `none` | `forward` | `exchange`. With
  `forward`, golem still authenticates the caller by some other method
  unless the route is explicitly `byo-only` (metered by key hash).

## Consequences

- One Principal type feeds authorization, limits, attribution, telemetry.
- Ordering is explicit config; misordering cannot accept a bad credential.

## Alternatives

- Delegate authn to the ingress (oauth2-proxy, Istio RequestAuthentication)
  and trust headers: fine for JWT, does not cover virtual keys. Supported
  as the XFCC/trusted-header mode, not the default.
