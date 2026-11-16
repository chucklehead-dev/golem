# 0004: golem runs behind the enterprise's ingress and mesh

Status: Accepted (v1 baseline).

## Decision

golem is an ordinary Service in Istio (Gloo Mesh, ambient mode), not a
Kubernetes Gateway API implementation. Its CRDs describe LLM concepts, not
listeners or routes. In-mesh traffic is mTLS by ztunnel; the gateway serves
plain HTTP inside it. North-south traffic, TLS and WAF stay with the mesh's
ingress; the internet-facing edge pool (ADR 0018) is exposed through it.
Egress to providers and model servers goes through the mesh's egress
gateway (ServiceEntries per destination). Self-hosted clusters with their
own endpoint pickers (llm-d, an Inference Gateway, KServe) are a single
backend URL to golem (ADR 0017).

## Consequences

No listener management, xDS or conformance suite. Two hops for external
callers (ingress, gateway), negligible at LLM latencies.

## Alternatives

Implementing Gateway API (right for a product, wrong cost here); Envoy
with ext_proc (every feature across a gRPC hop).
