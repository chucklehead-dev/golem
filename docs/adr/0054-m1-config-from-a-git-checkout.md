# 0054: Config is a git checkout, compiled by golem-control

Status: Accepted, consolidated 2026-11-16. This record is golem's config
decision for M1. It absorbs what still holds of
[0005](0005-config-crds-and-overrides.md) (the kinds, one validation path,
the secrets rule; 0005 is now a stub listing what it defers to M2) and
replaces [0041](0041-security-boundaries-and-incident-controls.md) §7.2's
freshness by a consistent LIST. It records what was built: golem-control
has read its config from a git-sync checkout since its first chart
(deploy/charts/golem-control). 0005's in-cluster objects, CRD watch and
operational overrides apply from M2, if at all (roadmap.md).

## Context

ADR 0005 has the config kinds (`Backend`, `ModelRoute`, `ScopePolicy`,
`Hook`, `PriceList`) as objects in the cluster, synced there by Argo CD
from the GitOps repository, watched by golem-control, which writes their
status. ADR 0041 §7.2 proves each renewal's config fresh by a quorum LIST
of those objects, anchoring the lease to the earlier of that read and the
state document's.

What runs is different. golem-control has no Kubernetes client: a
git-sync sidecar keeps the GitOps repository checked out beside it
(`--link=current`, a new worktree for each commit), and golem-control
compiles the directory every step (`-config`). Nothing reads the cluster's
config objects or writes their status, the CRDs carry no CEL rules, and
the Argo sample installed the CRDs (from the chart's `crds/`) and
documented syncing the objects into the cluster, where nothing used them.
golem-control renewed its last checkout for as long as it ran, so a
stalled git-sync had its config renewed indefinitely, though it might
miss a restriction merged since.

## Decision

- **Five kinds**, group `golem.dev` (lib/api/v1alpha1): `Backend`
  (endpoints, dialect and envelope, auth, network, deployments and their
  ceilings), `ModelRoute` (alias, pool in order, retries, timeouts),
  `ScopePolicy` (one per scope: models, budget and mode, limits, request
  ceilings, provider storage, feature mode; ADR 0050), `PriceList`, and
  `LocalIdentity` for standalone; `Hook` comes with hooks (M2). Decoded
  strictly: an unknown field is refused, naming file and field.
- **One validation path**: the snapshot compiler (lib/compile), a library:
  `golemctl config validate` runs it in CI, golem-control on the checkout,
  the standalone gateway on its directory.
- **Secrets stay in Kubernetes Secrets** in each site's cluster, mounted
  into the gateways that need them (or workload identity); a config names
  them, and they never enter the state or the snapshots.
- **The config is the GitOps repository at a commit.** The platform
  team's repository (CODEOWNERS, review) holds the config YAML; CI runs
  `golemctl config validate` on it (lib/compile, the one validation path
  above); git-sync checks out the branch's head every `period`
  (30 seconds); golem-control compiles the checkout and publishes it
  (control/controller). A merge reaches the gateways within a sync period
  and a step.
- **No config objects in the cluster.** Argo CD does not sync the config
  repository into the cluster (deploy/argocd/golem.yaml says so), and
  golem-control's Application skips the chart's CRDs (`skipCrds`): they
  are generated (internal/crdgen) and kept current by their test, for the
  later CRD watch, but nothing in M1 installs or reads them. Plain Helm
  installs pass `--skip-crds`. With no objects there is no status:
  golem-control's log and metrics say whether an edit was applied
  (`config not applied`, `golem.control.config.renewing`).
- **Each publication names its commit.** golem-control reads the
  directory through git-sync's link resolved once a step (`-config-link`),
  so the files and their revision are one checkout, and states the
  worktree's name, the commit's hash, as the snapshot's
  `Source.ConfigRevision`, logged at each publication and each config
  change. A renewal of the bucket's publication (no config compiled since
  a start) keeps that publication's revision.
- **Freshness, for a git revision** (restating 0041 §7.2). A publication
  binds the state version it was built from, the config's digest and its
  commit. The state is read consistently at each step (a GET of the
  document, as before). The config is as fresh as git-sync's last
  successful fetch of the branch, which rewrites the repository's
  `FETCH_HEAD` whether or not the branch moved: golem-control reads its
  modification time (`-config-fetched`), exports the age
  (`golem.control.config.age`), and once it is older than
  `-max-config-age` (30 minutes) renews no lease. A change of the
  entitlements or the epoch is still published then, with the lease the
  pool's last publication gave and never later, so a revocation still
  reaches the gateways; nothing extends a lease on a stale checkout. An
  unreadable `FETCH_HEAD` counts as stale. So a restriction merged at time
  T is served by every gateway from T plus a sync period, a step and a
  poll while git-sync works, and a gateway serves a checkout that may
  miss it for at most `-max-config-age` plus one lease (four hours for
  internal pools) after the last fetch. The bound is by golem-control's
  clock against the file's time, both on one node.
- **What is not ordered.** As 0053 notes, nothing compares commits: a
  revert is a new commit, and a checkout that went back (a force-push to
  the branch) is published as the config. The branch's protection (no
  force-push, reviews required) is the repository's, outside golem.

## Consequences

- No admission-time validation: an invalid edit is caught by CI's
  `golemctl config validate`, else refused by golem-control at load
  (`config not applied, the last good one is published`), as before.
- Incident levers that 0005 put on the config (overrides: disable a
  deployment, set a weight, freeze a pool) do not exist in M1; the levers
  are a ModelRoute or Backend pull request, the gateways' ejection of a
  failing deployment, and the emergency denylist (operations.md §2).
- A stalled git-sync now ends in the gateways' leases running out (an
  outage after `-max-config-age` plus a lease) rather than in serving an
  old config indefinitely; the *Config checkout old* alert pages well
  before.
- Revisit with the second team or two golem-control replicas (0053):
  the CRD watch reads the cluster's one copy, with 0041 §7.2's consistent
  LIST; or a config reader that publishes only a commit descending from
  the last published one.

## Alternatives

- **Sync the objects into the cluster as well, unread**: two copies of
  the config, the cluster's not the one published, and a status that
  never comes. Rejected.
- **git-sync's touch file or link time as the age**: both change only
  when the commit does, so an unchanged branch would read as stale and a
  stalled sync of an unchanged branch could not be told from a healthy
  one. `FETCH_HEAD` changes at every fetch.
- **The commit's time as the age**: measures how long since the last
  merge, not since the last fetch.
