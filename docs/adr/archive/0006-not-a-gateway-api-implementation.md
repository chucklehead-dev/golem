# 0006: golem runs behind the enterprise's ingress

Status: Proposed

## Context

Envoy AI Gateway, agentgateway and kgateway implement the Kubernetes
Gateway API, with LLM behaviour attached as policy. The Gateway API
Inference Extension (InferencePool, endpoint picker) adds model-server-
aware routing for self-hosted models.

## Decision

golem is an ordinary Service behind whatever ingress or mesh the
enterprise runs. Its own CRDs describe LLM concepts (backends, model
routes, limits, hooks), not listeners or HTTPRoutes. For self-hosted model
pools, a backend may point at an InferencePool's endpoint picker, or at a
plain Service, rather than golem reimplementing KV-cache-aware routing.

## Consequences

- Much less code: no listener management, no xDS, no conformance suite.
- TLS, WAF and north-south policy stay where the enterprise already has
  them (Gloo Mesh's ingress). In-mesh traffic is mTLS by ztunnel (ambient
  mode); golem serves plain HTTP inside it, and caller identity from the
  mesh, if used, arrives through a waypoint (ADR 0003).
- Two hops for external callers (ingress, golem); acceptable at LLM
  latencies.

## Alternatives

- Implement Gateway API: right for a general product; wrong cost for one
  enterprise.
- Envoy plus ext_proc for LLM logic: performance from Envoy, but the
  logic lives in a sidecar service and every feature crosses a gRPC hop.
