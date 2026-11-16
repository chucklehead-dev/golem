# Helm charts

M1's install (docs/m1-plan.md step 5): Argo CD syncs these, golem-control
first, then a gateway pool per site.

- `golem-control`: two replicas (one leads, by a lease in the state
  bucket), the control API, `/livez` and `/readyz` on `-probes`, and a
  git-sync sidecar keeping the config repository checked out (an init
  container makes the first checkout). Needs: the bucket, the KMS
  publishing key, the config repository's deploy key (a Secret), the
  control API's Entra tenant; ClickHouse and the Entra group sync are
  optional.
- `golem-gateway`: one pool at one site, three replicas spread across
  zones, each pod with its own OpenTelemetry collector (ADR 0043's in-pod
  variant: a native sidecar, the gateway exporting to 127.0.0.1:4318 in
  plaintext, the collector's file_storage queue on an emptyDir, its
  config deploy/otel/collector.yaml from a ConfigMap, ClickHouse's
  endpoint and password as its env; `collector.enabled: false` and
  `telemetry: host:port` for a remote collector over TLS, with
  `caBundle` for a private CA as `SSL_CERT_FILE`), a PDB, the provider Secrets mounted for the pool's backends, the
  emergency-denylist ConfigMap (managed outside Argo), the publishing keys
  it trusts (a ConfigMap), the site's Valkey (TLS, an ACL user, the
  password from a Secret), and an Istio AuthorizationPolicy when
  principals are given. Pool mode is behind the gateway's release gate
  (golem#7): `acceptUnreleased: true` passes `-accept-unreleased`, for the
  observe-phase site only and with a recorded decision; left false, the
  pods exit naming the missing evidence. The grace period is the drain (10 minutes by
  default, as operations.md assumes) plus the readiness window.

Two settings are set in both charts and must agree: golem-control's
`lease` must not exceed the gateways' `maxLease` (a longer lease makes
every gateway refuse every snapshot: raise `maxLease` in every pool first),
and `periodZone` must be the same (each snapshot states golem-control's,
and a gateway of another zone refuses it, naming both).

Each ServiceAccount takes its AWS role by annotation (IRSA or Pod
Identity).

- The config CRDs are in golem-control's `crds/` (Helm installs them
  first), generated from `lib/api/v1alpha1` by internal/crdgen, whose test
  fails when they are not current (`GOLEM_UPDATE_CRDS=1 go test
  ./internal/crdgen/`).
- golem-control's `clickhouse.ddl` runs the ledger's DDL as a pre-install
  and pre-upgrade Job (its copy, `files/spend.sql`, is checked against
  deploy/clickhouse/spend.sql).
- The images: the repository's Dockerfile, `--build-arg
  BINARY=golem-gateway` or `golem-control` (static, distroless, non-root).
- deploy/argocd/golem.yaml: a sample Application for golem-control and an
  ApplicationSet of gateway pools by site.

Not here yet: Argo Rollouts (canaries are by hand in M1), dashboards,
egress ServiceEntries.

Checked: `helm lint`, `helm template`, kubeconform (every resource but
the CRDs and the Istio policy, which have no schema offline), every flag
against the binaries, and the images run in Docker: golem-control
published to a directory bucket and the gateway verified and applied it,
served ready, and refused an unauthenticated call. Not on a cluster: kind
cannot run in this sandbox.

Required values fail the render with their name. Large numbers in values
are quoted: Helm reads YAML numbers as floats and would render 33554432 as
3.3554432e+07.
