# 0003: Authenticators in a chain yield one Principal

Status: Proposed

## Context

Callers present virtual keys (`Authorization: Bearer` or `x-api-key`,
depending on SDK) or Entra ID access tokens (users, and services by client
credentials or workload identity federation). golem runs in Gloo Mesh's
ambient mode, where ztunnel terminates mTLS and the pod sees plain HTTP:
the peer certificate is not available to golem. Some routes must forward
the caller's own provider credential.

## Decision

- `Authenticator.Authenticate(ctx, *http.Request) (Principal, Result)`
  where Result is `NotMine`, `Accepted` or `Rejected`. The configured chain
  stops at the first `Accepted` or `Rejected`: a credential that looks like
  a JWT and fails verification is rejected, not retried as a key.
- Credential recognition is by shape (key prefix `glm_`, three-part JWS,
  TLS peer certificate present) before any network call.
- `Principal` carries subject (Entra `oid`, or service id), kind, the
  **scope** it acts in (a key's bound scope; for Entra tokens the
  `x-golem-scope` header or the user's default scope), the key if any, and
  attributes for CEL. Membership and rights come from the entitlements
  snapshot (ADR 0009), not from token claims, so group changes and
  revocations do not wait for token expiry.
- Keys are stored as SHA-256 hashes of high-entropy secrets (no slow hash
  needed: they are random, not passwords). Lookups cached.
- Mesh identity: in ambient mode golem cannot read the peer certificate.
  A waypoint in front of the gateway Service passes the caller's SPIFFE ID
  in a header (XFCC, or a header set by the waypoint's route from the
  peer principal). The gateway trusts the header only when the operator
  has also installed the AuthorizationPolicy that admits nothing but the
  waypoint to the gateway's data port, and strips it otherwise. The
  identity binds keys to workloads and identifies in-mesh services without
  keys (M2). The exact waypoint configuration for the Gloo Mesh version in
  use is an M1 spike: the public docs we found do not describe it, so it
  is verified by test (inspect headers at a stub upstream) before relying
  on it. Fallback if it cannot be done: services use Entra workload
  identity tokens, and the mesh contributes L4 authorization only.
- Pass-through is a route property: `none` | `forward` | `exchange`. With
  `forward`, golem still authenticates the caller by some other method
  unless the route is explicitly `byo-only` (metered by key hash).

## Consequences

- One Principal type feeds authorization, limits, attribution, telemetry.
- Ordering is explicit config; misordering cannot accept a bad credential.

## Alternatives

- Delegate JWT validation to Istio RequestAuthentication at the waypoint:
  possible, but golem still needs the claims and must handle keys, so it
  validates Entra tokens itself (cheap: local signature check, cached
  JWKS).
- Direct mTLS termination in golem: incompatible with ambient's ztunnel
  without opting golem out of the mesh.
