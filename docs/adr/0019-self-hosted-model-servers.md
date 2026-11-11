# 0019: Self-hosted model servers as services by default, replicas when golem must choose

Status: Proposed

## Context

vLLM and SGLang run on GPU clusters separate from the gateways. Requests
are long, KV-cache locality matters (a prompt prefix already cached on one
replica is much cheaper there), and replicas saturate by queueing rather
than by refusing. Serving platforms (llm-d, Gateway API Inference
Extension, KServe) already do load- and cache-aware replica selection
where they are deployed.

## Decision

- A self-hosted Backend is a **service** (one URL; the platform picks the
  replica) unless configured as **replicas**. golem does not duplicate a
  platform's endpoint picker.
- For replicas, golem's `load` strategy polls each replica's metrics
  (running and waiting requests, KV-cache use) every second over a
  persistent connection and picks the least loaded among those holding the
  prompt prefix (prefix affinity by a hash of the first N tokens' bytes,
  rendezvous hashing so replica changes move few prefixes).
- Queue depth beyond a threshold counts as unavailable for routing, so
  overflow goes to fallback pools before it times out.
- Reached through the mesh's multi-cluster discovery where Gloo Mesh
  spans the GPU clusters; otherwise TLS with a bearer key or client
  certificate, with WAN timeouts and keepalives.
- Usage comes from the servers' own usage fields (streamed with
  `include_usage`); prices from `PriceOverride`.

## Consequences

- No golem code for platforms that already balance; a small, contained
  strategy for plain replica sets.
- Metrics polling is per gateway per replica: fine at tens of replicas;
  beyond that, use a platform balancer.

## Alternatives

- Always route to replicas: duplicates platforms' work and fights it.
- Never: plain replica sets lose cache locality and saturate unevenly.
