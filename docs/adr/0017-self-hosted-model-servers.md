# 0017: Self-hosted model servers: each instance a deployment

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). From archive 0019, 0032.

## Decision

- Each vLLM (later SGLang) instance is a deployment: one per DGX Spark, Mac
  mini or Mac Studio; one or several per NVL4/NVL8 box. Endpoints come from
  a static list, DNS, or **registration** through the control API under a
  service identity (workload identity, else a PKI or SPIRE certificate, else
  a bootstrap token exchanged for a rotated machine credential), with a
  24-hour TTL renewed by re-registering, confined to named Backends and an
  allowed domain; the gateway checks each endpoint's TLS name.
- **Models are discovered** from each endpoint's `/v1/models`; aliases
  select by model name or registered label.
- **M1**: a static concurrency ceiling per endpoint and a per-principal
  in-flight limit per pool (default half its capacity); overflow at the
  ceiling or on 429/503 goes to the fallback pool before the first byte, or
  waits briefly if the route allows. **M2**: adaptive concurrency, vLLM
  metrics polling, prefix affinity, round-robin queueing across principals.
- Liveness is the gateways' health probes and ejection, as for every
  endpoint; no heartbeats.
- Reached over TLS (a terminating proxy such as Caddy in front of vLLM's
  `--api-key`) through the mesh's egress gateway. Dialects: OpenAI Chat,
  Completions and Embeddings, and Anthropic Messages pass-through.
- Rates set by the platform team in `PriceList`.
- Clusters with their own balancer (llm-d, an Inference Gateway, KServe)
  are one service URL.

## Alternatives

Always routing to replicas (duplicates platforms' endpoint pickers); never
(no protection for scarce machines).
