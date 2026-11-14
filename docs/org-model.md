# golem: organisation, keys and budgets

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. Built so far: see the walkthroughs
(docs/walkthroughs) and the "Built" notes; release evidence is tracked in
golem#7.

Status: draft, fifth pass. The entitlement model behind PRD §5.3–§5.5;
decided in ADR 0010 (the model), 0010 (scopes and roles), 0006 (where it
lives) and 0013 (multi-region enforcement).

## 1. Entities

| Entity | Identity | Notes |
|---|---|---|
| **User** | Entra `oid` (+ `tid`) | Created by the Entra group sync when they are a member of a group bound to a scope, or by an admin; their `oid` and UPN are Graph's, never a manager's statement (ADR 0047). |
| **Scope** | golem id; `kind: team \| project` | Where spend is charged and rights come from. A **team** may have a parent team (a sub-team; depth at most two, ADR 0021) and may be bound to Entra groups (membership synced from Graph) and/or managed in golem. A **project** is owned by exactly one team (its primary team); its members may be other teams (all their members), services and users. |
| **Service** | golem id; Entra app (client id); service keys | Owned by one scope. A non-human principal. May be added to other projects. |
| **Cost center** | external code (e.g. `CC-4410`) | Not managed by golem: a funding source referenced by budgets, with its budget managers. |
| **Key** | `glm_` + 32 random bytes, stored as SHA-256 | Belongs to a user or a service; bound to one **scope**. |

A team that is a member of a project contributes its members; it does not
fund the project unless it also gives the project a budget.

## 2. Roles

Roles are assigned in golem and audited; the Entra app role
`golem.admin` bootstraps administrators.

Four roles (ADR 0010):

| Role | Held on | May |
|---|---|---|
| Member | a scope | Use the scope's models within their cap; mint personal keys in the scope; set capture and retention on their own keys, as far as the scope permits (§3.1). |
| Manager | a scope (a binding may be narrowed to one service) | Manage membership granted in golem (members from bound Entra groups are Entra's; a manager adds existing principals and never binds an Entra ID, ADR 0047); create services; set caps for members and services; grant the scope's money to projects (a team's managers: to projects it owns or belongs to); create projects (a team's managers: owned by the team); request adjustments; set capture settings and the override mode; mint, rotate and revoke service keys. A project's owning team's managers, and a sub-team's parent team's managers, are its managers unless set otherwise. |
| Budget manager | a cost center (or globally) | Grant from the cost center to scopes; approve or reject requests that draw on it (new grants, permanent and one-time adjustments). |
| Admin | global | Everything, including operational overrides; break-glass. |

Separation: approving a request that you raised is refused unless you are
an admin and say so (recorded).

## 3. Rights and keys

A request's effective rights are an intersection, evaluated at request
time, never copied into the key:

```
effective = scope policy                 (models/aliases, dialects, routes, data class)
          ∩ principal's membership       (still a member? member-level constraints)
          ∩ key constraints              (what the key's creator narrowed it to)
```

So a key loses rights the moment its owner leaves the scope or the scope's
policy narrows; it can never be wider than its owner.

Key constraints, all optional:

- models or aliases (subset of the scope's), dialects/endpoints;
- spend cap per period and/or lifetime; rate limits (req/min, tokens/min,
  $/min) and max concurrent streams;
- expiry (personal keys must expire; the maximum lifetime is policy,
  e.g. 90 days); not-before;
- source binding: allowed CIDRs;
- `max_tokens` ceiling, allowed `reasoning` effort; capture and retention overrides (§3.1);
- metadata: name, purpose, tags (become telemetry attributes).

### 3.1 Content capture and retention

Decided in ADRs 0016 and 0035.

| Level | Set by | Settings |
|---|---|---|
| Scope policy (a `ScopePolicy` CRD) | platform admins | the ceiling: capture allowed or forbidden, the retention classes allowed, required redaction hooks |
| Top-level team | its managers | `capture`: off, metadata (Langfuse sees traces without content), content; its Langfuse project, whose retention is one of the classes (7, 30, 90 or 365 days, or 3, 5 or 7 years) |
| Sub-team or project | its managers | inherit the parent's settings and Langfuse project, or have its own project (with its own retention) and `capture` setting |
| Key or service | the key's owner; the service's owner | inherit, or turn capture **down** only (content to metadata, or off) |

Retention is the Langfuse project's own setting: a scope that needs a
different retention from its parent has its own project. Keys and services
cannot lengthen retention or turn capture on. The classes above one year
are available only in scopes whose policy allows them, since long-lived
content is the largest privacy and storage commitment golem makes.

The effective setting is computed per request from the snapshot. A switch
may be time-boxed (a job turns it off at expiry). Every change is an audit
event; every captured record names the project it went to and who enabled
capture. Content goes only to the scope's Langfuse project, through the
collector, which holds the credentials.

### 3.2 Minting

Personal keys are minted by users for themselves (self-service, API/CLI,
or `golemctl login` for coding agents, which mints a short-lived key after an
Entra device-code or browser flow). Service keys are minted by the service
owner. Keys are shown once; golem stores the hash and a display prefix.

The model is stored and enforced by golem-control's service layer
(control-plane.md §2); the gateway only evaluates the snapshot of it.

Services may also authenticate without keys, by Entra tokens (client
credentials or workload identity federation from their Kubernetes service
account): the token's `azp`/`appid` maps to the service.

## 4. Budgets

All money is in one configured currency, stored as int64 micro-units.
Budgets are per **period** (calendar month in a configured timezone); no
rollover unless configured.

### 4.1 Pools and grants

Every team and project has a **pool** per period. Money enters a pool only
by a **grant**:

| From | To | Granted by | Approval |
|---|---|---|---|
| Cost center | team | budget manager | none (the budget manager is the approver) |
| Cost center | project | budget manager, or requested by a manager | budget manager |
| Team | sub-team (a team whose parent it is) | the team's managers | none |
| Team | project (any project the team owns or is a member of) | the team's managers | none (confirmed) |
| Team or project | member user or service (a **cap**) | the scope's managers | none |

A grant is **recurring** (every period from a start period, until ended:
the "permanent" amount) or **one-time** (one period only). An
**adjustment** is a change to a recurring grant's amount from a period on
(permanent) or an extra one-time grant; managers request, budget managers
approve when the money comes from a cost center.

A pool's numbers for a period:

```
pool.total     = Σ grants in
pool.granted   = Σ grants out to sub-teams and projects   (team pools only)
pool.available = total − granted − spent            (spent: known spend, ADR 0031)
```

Caps on members do not move money; they are limits within the pool (a
key's spend cap is the same kind of object, on a key).
Their sum may exceed the pool (oversubscription, on, since few members
use all of theirs). A member without a cap draws from the pool up to the
scope's **default member cap** (possibly zero).

### 4.2 Enforcement

A request is charged to its key's scope. Before going upstream golem
checks **known spend** (the ledger's balance published in the snapshot,
plus the site's counter since) at each level, and fails with a 402 (not
429, which SDKs retry: ADR 0024) naming the first level already reached:

1. the key's own cap (if any);
2. the member's cap in the scope (or the default member cap);
3. the scope pool's `available`.

Budgets are soft and eventually consistent (ADR 0031): calls already in
flight can take spend past a limit: by about the principal's in-flight
limit times its largest declared call, plus what it spends within one
refresh lag; a scope's pool, shared by its members, by about the scope's
own in-flight limit times the largest call (ADR 0041 §7.6). These are
expected exposure while calls cost what they declare, not guarantees.
A scope may choose the **declared-worst-case check** (`declaredWorstCaseCheck`): the
request's declared worst case must also fit, and a request that declares
none (no `max_tokens`, server-side tools) is refused. That narrows the
overshoot by about one call; it is not a ceiling. A budget is an
admission and monitoring control: the provider's bill, reconciled, is the
record, and the provider account's spending limit the backstop (ADR 0044).

What happens at a limit depends on the scope's enforcement mode (PRD
§9.1): `observe` records the refusal it would have made, `warn` allows it
with a warning header and a notification, `enforce` refuses.

At completion the call's actual cost is added to known spend. Team pools are not charged for a sub-team's or a project's spend: the
grant already moved that money. Thresholds (e.g. 50/80/100%) at every level emit an
event and notify the pool's managers; 100% at level 2 or 3 rejects, or
downgrades to a configured cheaper alias.

### 4.3 Approvals (when a budget manager asks for them; M2 ships caps and a cost-center mapping: ADR 0034)

A request (new grant from a cost center, permanent or one-time
adjustment) has states `pending → approved | rejected | expired |
withdrawn`. Approved requests take effect from their stated period (a
one-time adjustment may take effect immediately in the current period).
Every transition is an audit event with actor, reason and diff.

### 4.4 Attribution

Each spend record carries: request id, time, principal (user or service),
key id, scope (team/project), the scope's funding sources at that time,
model, deployment, tokens by kind, cost, and the price version. For
chargeback, a project's spend is apportioned to its funding sources pro
rata by their grants in that period (configurable: in order). The ledger
is in ClickHouse (ADR 0015).

## 5. Examples

- Platform team gets $20k/month recurring from CC-4410 (budget manager).
  Its manager grants $5k/month to project "support-bot" and caps each
  engineer at $200/month. An engineer mints a personal key bound to
  the team for Claude Code, constrained to the `claude-*` aliases and
  $50/month.
- Project "support-bot" (primary team: platform; member team: support)
  asks for a one-time $3k from CC-7720 for a launch. A CC-7720 budget
  manager approves; the project's pool is $8k this month.
- Service `support-bot-api` (owned by the project) authenticates by Entra
  workload identity; its cap is $6k; its keys are for a vendor
  integration that cannot do Entra.

## Capture-access boundary (ADR 0041)

Per [ADR 0041](adr/0041-security-boundaries-and-incident-controls.md#2-capture-uses-pool-and-project-trust-boundaries-m2),
inheriting a Langfuse project requires review of its actual readers. A child
scope with narrower readers needs its own project. Scope membership does not
automatically confer access to a parent's capture project. A shared gateway
pool is a compromise boundary across all capture destinations it may use.
