# 0025: Smaller surfaces before code

Status: Proposed. Amends 0004, 0009, 0013, 0014, 0016, 0018.

## Context

Five passes of design added features one at a time. Reviewing the whole,
several surfaces are larger than what they must do.

## Decision

1. **One control-plane binary, two roles** (amends 0013). `golem-control
   --role=api|worker` from one image, deployed as two Deployments. The
   gateway remains its own binary and image; the import test still keeps
   worker and API code apart from the gateway and API transport from the
   store.
2. **Five config kinds** (amends 0012/0017 lists): `Backend` (endpoints,
   auth, deployments, discovery), `ModelRoute` (alias, pools, strategy,
   fallbacks, shadow, conditions), `ScopePolicy` (allowed models, capture
   ceiling, residency, default limits: the former LimitPolicy folded in),
   `Hook`, `PriceList` (rates, including self-hosted). Deployment, alias and
   route are not separate resources.
3. **Four hook phases** (amends 0004): `Request` (after auth, before routing:
   reject, rewrite, annotate with routing hints), `Chunk` (streams; M2, only
   when a guardrail needs it), `Response` (complete response or stream end),
   `Complete` (async). Choosing a pool is a routing hint from `Request`;
   upstream headers are Backend config. The former `OnRoute` and
   `OnUpstreamRequest` go.
4. **No long-running operations** (amends 0014). An adjustment request is a
   resource with a `state` and `Approve`, `Reject`, `Withdraw` methods; a
   client waits by polling or by notification.
5. **Leases only when there are two regions** (amends 0016). Single-region
   golem enforces pools directly in Valkey; the `Lease` method is not in the
   M1 API. The limiter interface keeps the seam.
6. **Explain lives in the control plane only.** It holds every snapshot; the
   gateway's local port serves probes, pprof and the snapshot hash it runs.
7. **Capture without sampling at first** (amends 0018). Off, metadata or
   content, retention and the override mode; sample rates when someone
   needs them.
8. **Entitlement delivery**: unchanged (0009), but change detection (since ADR 0027, a version row) covers only
   store-held state (entitlements, overrides); CRD changes reach the
   snapshot builder through the Kubernetes watch.

## Consequences

Fewer resources, phases, binaries and protocol features to build and test
in M1, each with a stated path back if needed.

## Alternatives

Build the larger surfaces now "while we are here": the cost is paid in M1
by every test and every reader.
