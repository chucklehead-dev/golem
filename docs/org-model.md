# golem: organisation, keys and budgets

Status: draft. The entitlement model behind PRD §5.3–§5.5; decided in
ADR 0008 (the model) and ADR 0009 (where it lives).

## 1. Entities

| Entity | Identity | Notes |
|---|---|---|
| **User** | Entra `oid` (+ `tid`) | Created on first sign-in. Display name and UPN from the token. |
| **Team** | golem id; optionally bound to Entra groups | Membership from the bound groups (synced from Graph) and/or managed in golem. |
| **Project** | golem id | Owned by exactly one **primary team**. Members: other teams (all their members), services, users. |
| **Service** | golem id; Entra app (client id) and/or SPIFFE ID; service keys | Owned by one team or one project. A non-human principal. May be added to other projects. |
| **Cost center** | external code (e.g. `CC-4410`) | Not managed by golem: a funding source referenced by budgets, with its budget managers. |
| **Key** | `glm_` + 32 random bytes, stored as SHA-256 | Belongs to a user or a service; bound to one **scope**. |
| **Scope** | a team or a project | Where spend is charged and rights come from. |

A team that is a member of a project contributes its members; it does not
fund the project unless it also gives the project a budget.

## 2. Roles

Roles are assigned in golem and audited; the Entra app role
`golem.admin` bootstraps administrators.

| Role | Held on | May |
|---|---|---|
| Member | team, project | Use the scope's models within their allocation; mint personal keys in the scope. |
| Team manager | team | Manage team membership (where not Entra-bound), create projects owned by the team, create team services, allocate the team's budget to members, services and projects; request adjustments. |
| Project manager | project | Add/remove member teams, services, users; create project services; allocate the project's budget to members and services; request adjustments. A project's primary team's managers are its project managers by default. |
| Budget manager | cost center (or globally) | Assign budgets to teams and projects from the cost center; approve or reject requests that draw on it (new budgets, permanent and one-time adjustments). |
| Service owner | service | Mint, rotate and revoke the service's keys (team or project managers by default). |
| Admin | global | Everything, plus operator config views; break-glass. |

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
- source binding: allowed SPIFFE IDs (from the mesh), CIDRs;
- `max_tokens` ceiling, allowed `reasoning` effort, no-content-capture flag;
- metadata: name, purpose, tags (become telemetry attributes).

Personal keys are minted by users for themselves (self-service, API/CLI,
or `golem login` for coding agents, which mints a short-lived key after an
Entra device-code or browser flow). Service keys are minted by the service
owner. Keys are shown once; golem stores the hash and a display prefix.

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
| Cost center | project | budget manager, or requested by a project/team manager | budget manager |
| Team | project (any project the team owns or is a member of) | team manager | none by default; policy may require a budget manager |
| Team or project | member user or service (an **allocation**) | team / project manager | none |

A grant is **recurring** (every period from a start period, until ended:
the "permanent" amount) or **one-time** (one period only). An
**adjustment** is a change to a recurring grant's amount from a period on
(permanent) or an extra one-time grant; managers request, budget managers
approve when the money comes from a cost center.

A pool's numbers for a period:

```
pool.total     = Σ grants in
pool.granted   = Σ grants out to projects            (team pools only)
pool.available = total − granted − spent − reserved
```

Allocations to members do not move money; they are caps within the pool.
Their sum may exceed the pool (oversubscription, default on, since few
members use all of theirs); a pool may forbid it. A member without an
allocation draws from the pool up to the scope's **default member cap**
(possibly zero).

### 4.2 Enforcement

A request is charged to its key's scope. Before going upstream golem
reserves the estimated cost at each level, atomically, and fails with a
429 naming the first level that would be exceeded:

1. the key's own cap (if any);
2. the member's allocation in the scope (or the default member cap);
3. the scope pool's `available`.

At completion the reservation becomes spend (corrected to actual cost).
Team pools are not charged for a project's spend: the grant already
moved that money. Thresholds (e.g. 50/80/100%) at every level emit an
event and notify the pool's managers; 100% at level 2 or 3 rejects, or
downgrades to a configured cheaper alias.

### 4.3 Approvals

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
is in ClickHouse (ADR 0010).

## 5. Examples

- Platform team gets $20k/month recurring from CC-4410 (budget manager).
  Its manager grants $5k/month to project "support-bot" and allocates
  $200/month to each engineer. An engineer mints a personal key bound to
  the team for Claude Code, constrained to the `claude-*` aliases and
  $50/month.
- Project "support-bot" (primary team: platform; member team: support)
  asks for a one-time $3k from CC-7720 for a launch. A CC-7720 budget
  manager approves; the project's pool is $8k this month.
- Service `support-bot-api` (owned by the project) authenticates by Entra
  workload identity; its allocation is $6k; its keys are for a vendor
  integration that cannot do Entra.
