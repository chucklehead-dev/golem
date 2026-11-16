# 0050: Scope policy is config, a ScopePolicy CRD; budgets have modes

Status: Accepted. Amends [0005](0005-config-crds-and-overrides.md) (the
ScopePolicy kind, its fields; for M1 the config is a git checkout, ADR
0054, and there is no CRD status) and the accounting decision
([0031](0031-soft-budgets.md): what a budget does at admission). Its
`declaredWorstCaseCheck` field was removed by 0052 (in 0031). Decided by the product owner, as PRD §5.9 and ADR 0005 had
it: declarative config is CRDs, reviewed in GitOps.

## Context

The first control API slice stored a scope's whole policy in the state
document: its models, budget, the declared-worst-case check, in-flight
limits, request ceilings, provider storage and feature mode, set once by
`CreateScope` and changed by nothing. That contradicted PRD §5.9 ("two
writers, one each": config is CRDs, entitlements and overrides the API)
and ADR 0005, which lists `ScopePolicy` among the config kinds. It also
had practical costs:

- **No review, no history.** A budget or a model grant is a policy
  decision with money behind it; in the state it changed by an API call,
  audited but not reviewed, and with no diff to read.
- **Two validation paths.** lib/compile checked a LocalIdentity's scope
  policy field by field (`scopePolicy`); the API stored what it was given
  unchecked, so a negative ceiling was none at the gateway.
- **Pricing checked too late.** A budgeted scope over an unpriced model
  could only be found against the config at creation (`Service.Priced`)
  and, when the state caused it, degraded at publication
  (`Options.Unpriced`), because policy and prices lived apart.
- **No way to change a scope's policy at all**, and no budget modes,
  which PRD §5.5 and §9.1 require: rollouts start in `observe`, and an M1
  exit criterion is "budgets switched from observe to enforce".

## Decision

- **A `ScopePolicy` per scope** (lib/api/v1alpha1), named by the scope's
  ID. Its spec: `models` (route aliases), `budgetMicros`, `budgetMode`,
  `inFlight {principal, scope}`, `rateLimits {member, scope}`,
  `maxOutputTokens`, `maxInputBytes`, `providerStorage` (forbidden |
  allowed), `features` (restricted | compatible). It is checked by the one
  scope-policy check, lib/compile's `scopePolicy`, now also for
  `budgetMode`; a policy whose budget reaches an unpriced model is a
  config problem, found by `golemctl config validate` before a merge.
- **The state keeps entitlements**: a scope's existence (so memberships,
  keys and roles can name it) and the Entra groups bound to it (an
  admin's act, ADR 0047), memberships, keys, revocations, roles and the
  audit outbox. `CreateScope` takes an ID only. A restore no longer
  narrows scope policy: there is none in the state to restore.
- **The compiler joins them by ID.** A scope of the state's with no
  ScopePolicy is published as `DefaultScope`: no models, so its members
  can call nothing. This fails closed, deliberately: a policy is what
  grants anything, and a scope created before its pull request merges
  must not get rights nobody reviewed. A ScopePolicy naming a scope the
  state does not have is not published. Both are reported
  (`Options.Unmatched`; golem-control's `ErrUnmatched`, the
  `scope_policy_unmatched` failure reason and the
  `golem.control.scope_policy.unmatched` gauge) and neither stops the
  publication: the two change apart, in either order, as the unpriced
  scope's handling (now gone) already established. One exception, by
  decision: an edit that deletes the ScopePolicy of a scope the state has
  is refused (the last good config stays published), as it would leave
  that scope's members refused everything by accident; stopping a scope
  is `models: []` in its ScopePolicy, deliberately. The check is against
  the replica's last good config (`policiesRemoved`), so a golem-control
  that starts over a directory already without the policy publishes the
  scope with no models and reports it unmatched. A renewal from a
  published snapshot (no config compiled since a restart) keeps the
  policies it published.
- **Budget modes** (PRD §9.1): `observe` (the default for a scope with a
  budget: admitted and counted, its ledger row marked `over_budget`),
  `warn` (as observe, and the response carries `golem-warning:
  budget.pool_exhausted <n>%`), `enforce` (refused,
  `budget.pool_exhausted`). In-flight limits refuse in every mode. The
  gateway fails closed on what it cannot read: a snapshot's unknown mode,
  and an `admission.Call`'s zero mode, are enforce. A mode with no budget
  is refused as a forgotten budget.
- **Standalone mode** uses ScopePolicy objects too: there is no state, so
  each ScopePolicy is a scope, and a LocalIdentity holds only dev keys,
  users and services naming them. One way to write a policy, whichever
  mode reads it; a laptop config is a ScopePolicy and a LocalIdentity
  instead of one LocalIdentity.
- **The control API keeps a key within its scope's models** by asking
  golem-control's config (`Service.ScopeModels`); while no config has been
  read it checks nothing, and the gateway still intersects a key's models
  with its scope's.

## Consequences

- An operator changes a budget, a mode or a model grant by a pull request
  (operations.md §2, *Changing a scope's policy*), reviewed under the
  platform team's CODEOWNERS, published within a minute of golem-control
  reading it. Creating a scope is still the API's (`golemctl scopes
  create`), and the scope can call nothing until its policy merges.
- In-flight limits left unset now default to lib/compile's 4 per
  principal and 16 per scope everywhere; the state document's own
  defaults (8 and 64) are gone with its policy.
- `golemctl config validate` cannot see a mismatch with the state (it has
  none); golem-control reports it. A CRD status naming the ScopePolicy
  comes with the CRD watch.
- observe and warn never refuse on a budget: their exposure is unbounded
  by design, and the exposure and accounting models check enforce. A
  rollout's observe period is visible in the ledger (`over_budget`).
- Unreleased: the state document, the snapshot and the API change shape
  with no migration.

## Alternatives

- **An `UpdateScope` RPC**, keeping policy in the state: the smallest
  change, and policy changes would be audited. Rejected: no review before
  a change lands and no diff after, a second validation path to keep in
  step with the compiler's, pricing still checked across two stores, and
  it contradicts PRD §5.9 and ADR 0005's split (config in GitOps,
  entitlements and expiring overrides through the API). Incident levers
  that must act faster than a pull request are ADR 0005's overrides, not
  policy edits.
- **The CRD, with the scope's existence in it too** (no `CreateScope`):
  one place for everything about a scope. Rejected: memberships, keys,
  roles and Entra group bindings are entitlements written through the API
  (ADR 0047), and must name a scope the state knows; a scope that existed
  only in config would leave them dangling whenever a file moved.
- **Inline policy in LocalIdentity for standalone**, ScopePolicy for
  production: simplest on a laptop, but two shapes for one policy, two
  sets of docs, and a standalone config that does not carry over to a
  pool.
