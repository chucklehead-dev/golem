# 0023: A standalone gateway for laptops; no all-in-one control plane

Status: Accepted.

## Context

Developers want to run golem on a laptop or workstation: to point Claude
Code, Codex or an SDK at it with their own provider credentials, to try a
`ModelRoute` or hook against real providers before a PR, and to reproduce a
gateway bug with production code. agentgateway offers a standalone mode
driven by local YAML. The production design (ADRs 0003, 0005, 0006) needs
a bucket, a control plane, Valkey and Kubernetes.

## Decision

- **`golem-gateway --standalone <dir>`**: the production gateway binary,
  running from local files:
  - config: the same CRD YAML as the GitOps repository (`Backend`,
    `ModelRoute`, `ScopePolicy`, `Hook`, `PriceList`), compiled in-process
    by the snapshot compiler that `golemctl config validate` and
    golem-control use, and recompiled when the files change;
  - identity: one implicit local principal and scope; optionally a
    `LocalIdentity` file (keys, scopes, caps), accepted only in standalone
    mode, for testing authentication and limits;
  - limits: the in-memory limiter;
  - provider credentials: the developer's own (environment variables, an
    AWS profile), through the normal envelopes;
  - telemetry: stdout, or a local OTLP endpoint;
  - `golem-gateway --standalone <dir> --explain <request.json>` prints the
    route, deployment and limits a request would get.
- **Guard rails**: binds to `127.0.0.1` by default; prints a "not for
  shared use" banner; refuses edge pools, remote sites, bucket settings and
  credential pass-through to shared endpoints; never reads or writes a
  bucket.
- **Packaging**: the snapshot compiler and the CRD types live in `lib/`
  (`lib/compile`, `lib/api/v1alpha1`) with no dependency on client-go or
  control-plane code, so the gateway may import them; the import test keeps
  it so. The request path is the production one: nothing in it knows it is
  standalone.
- **No all-in-one binary with the control plane.** Running golem-control
  on a laptop would need stand-ins for Entra, the CRD watch, KMS signing,
  the bucket's compare-and-swap and leader election, each a code path
  production never runs. Whole-system development uses `make dev-up`
  instead: kind, MinIO (conditional writes), Valkey, a local OIDC issuer
  (dex), ClickHouse and Langfuse in containers, running production code
  unchanged; it is also the integration-test environment.

## Consequences

- Standalone costs little: the file snapshot source, the in-memory limiter
  and the offline compiler already exist for tests and CI.
- Developers test the exact gateway code that runs in production.
- Self-service, budgets across sites and capture can only be exercised in
  `make dev-up`, not standalone.

## Alternatives

- An all-in-one binary (gateway and control plane with local stand-ins):
  convenient, at the cost of parallel code paths in the control plane.
- No local mode: every route or hook change tested only after merge.
