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
  zones, a PDB, the provider Secrets mounted for the pool's backends, the
  emergency-denylist ConfigMap (managed outside Argo), the publishing keys
  it trusts (a ConfigMap), the site's Valkey (TLS, an ACL user, the
  password from a Secret), and an Istio AuthorizationPolicy when
  principals are given. The grace period is the drain (10 minutes by
  default, as operations.md assumes) plus the readiness window.

Each ServiceAccount takes its AWS role by annotation (IRSA or Pod
Identity). Not here yet: the config CRDs' definitions, Argo Rollouts
(canaries are by hand in M1), the ClickHouse DDL Job, dashboards, and
egress ServiceEntries.

Required values fail the render with their name. Large numbers in values
are quoted: Helm reads YAML numbers as floats and would render 33554432 as
3.3554432e+07.
