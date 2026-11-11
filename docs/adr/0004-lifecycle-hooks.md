# 0004: Fixed lifecycle phases; hooks in-process or remote

Status: Proposed

## Context

Guardrails, custom routing, memory and audit all need to see or change
requests at defined points. LiteLLM's callbacks are Python classes;
Envoy-based gateways use ext_proc; agentgateway uses CEL and external
services. golem must stay small, so the extension point must be narrow.

## Decision

- Six phases (PRD §5.6): OnRequest, OnRoute, OnUpstreamRequest,
  OnResponseChunk, OnResponse, OnComplete. Each phase has a Go interface
  taking a narrow view (not the `http.Request`) and returning a decision
  (continue, reject with a dialect-rendered error, rewrite, annotate).
- In-process hooks are Go values registered at build time
  (`hook.Register`), so an enterprise build adds its own hooks without a
  plugin system. Remote hooks are one gRPC service (and an HTTP/JSON
  equivalent) with the same phases.
- Each hook has a CEL `when` condition, a timeout, and a failure mode
  (`open` | `closed`). Hooks in a phase run in config order; OnComplete
  hooks run asynchronously on a bounded queue.
- Chunk hooks see SSE events, not bytes, and are the only hooks that force
  stream parsing on the fast path.

## Consequences

- Guardrails, memory injection and routing overrides share one mechanism
  and one set of telemetry (a span per hook, outcome metrics).
- No WASM or dynamic Go plugins; extending golem in-process means
  rebuilding it, which suits a bespoke enterprise build.

## Alternatives

- Envoy ext_proc protocol compatibility: lets existing ext_proc services
  plug in, but its model is headers/body bytes, not LLM requests. Could be
  a remote-hook adapter later.
- WASM: language-neutral and sandboxed, but large runtime dependency.
