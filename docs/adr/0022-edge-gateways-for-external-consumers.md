# 0022: An edge gateway pool for internet-facing consumers

Status: Proposed

## Context

Some callers are outside the network: GitHub Copilot's enterprise custom
models (called from GitHub's side), and SaaS applications that let a
customer configure their own OpenAI- or Anthropic-compatible endpoint.
golem should give them models while controlling which models, how much,
and what is captured. Internal gateways should not become internet-facing
to do it.

## Decision

- A separate `GatewayPool`, **edge**, runs the same gateway binary with
  its own config selection, exposed through the mesh's ingress (Gloo) on
  its own hostname. Internal pools stay internal. A noisy or attacked edge
  cannot starve internal traffic.
- Each external caller is a **consumer**: a Service (org-model §1) marked
  external, owned by the team or project that pays for the SaaS app, with
  - credentials: a service key, or Entra client credentials (an app
    registration per consumer, where the SaaS supports OAuth) when it can;
  - allowed source ranges (the ingress enforces them; for GitHub, from
    GitHub's published meta ranges, refreshed by the worker and applied by
    the operator);
  - allowed dialects and models, hard caps, rate and concurrency limits;
  - capture settings like any scope, defaulting to off;
  - no credential pass-through, no raw provider routes.
- End-user identifiers a consumer sends (OpenAI's `user`, Anthropic's
  `metadata.user_id`, documented headers) are recorded as unauthenticated
  attributes for attribution, never as identity, unless the consumer signs
  them (a JWT from the consumer's own issuer, configured per consumer).
- Ingress protections: TLS, WAF rules for the API paths only, request size
  limits, and per-consumer rate limits at the ingress as a first line.

## Consequences

- Copilot's enterprise custom models and SaaS integrations use one
  mechanism; each is a consumer with its own budget and audit.
- Per-user attribution for external consumers is only as good as what they
  send.

## Alternatives

- Expose the internal pool with stricter routes: one bad config away from
  exposing internal-only models and auth modes.
- A separate product for external access: duplicates the gateway.
