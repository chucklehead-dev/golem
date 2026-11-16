# golem: tools

Status: draft, third pass. Every tool here is a client of the control API
(architecture.md §3) and can do nothing the API does not allow the caller.

## 1. golemctl

**Built (M1):** `config validate`, `keys mint --dev`, `login` (Entra's
device code flow; tenant, client ID and API remembered), `token`, and over
the control API `scopes create` (an ID: a scope's policy is its
ScopePolicy, changed by a pull request, ADR 0050), `members add|remove`,
`groups bind|unbind`, `managers add`, `keys create` (owner defaults to the
caller; `--models`, `--expires 30d`) and `keys revoke`, each mutating one
with `--dry-run`; and the reads `whoami`, `scopes list|get` and `keys list
[--scope]` (a scope's members, managers and admins read it, and outsiders
are told it is not found; keys are seen by their owner, their scope's
managers and admins, never the key itself). Tokens are cached in a file
only their owner reads (`GOLEMCTL_HOME`, else the user's config directory)
and refreshed from the refresh token. There are no caps in M1 (ADR 0031).
The table below is the whole design; what M1 does not build is in
m1-plan.md (*Not building in M1*): caps, grants, spend, overrides,
explain, gateways, agent setup, the console.

One binary for every persona; commands are hidden from `--help` only when
the caller lacks every role they need. Sign-in is Entra (device code, or
browser with PKCE), tokens cached in the OS keychain. Output: table by
default, `-o json|yaml` for scripts. Every mutating command takes
`--dry-run` (the API's `validate_only`).

| Persona | Commands |
|---|---|
| Everyone | `login`, `whoami` (identity, scopes, roles), `models` (what I may use, by scope), `spend` (mine, by period/model) |
| User | `keys create --scope … [--models … --cap 50 --rate … --expires 30d …]`, `keys cap`, `keys list`, `keys constrain`, `keys rotate`, `keys revoke`, `capture on|off --key …` (own keys, within the scope's ceiling) |
| Coding agents | `agent setup claude-code|codex|cursor|copilot --scope …` writes the agent's config to point at golem; `token` prints a cached Entra access token for agents with a credential helper (Claude Code's `apiKeyHelper` and equivalents), with the scope sent as a header; agents that take only a static key get a minted personal key (12-hour default); either way no long-lived key sits on disk |
| Manager | `scopes get`, `members add|remove`, `projects create --team …`, `services create --scope …`, `services keys …`, `members cap <scope> <principal> 200`, `grants create --from team/… --to project/…`, `requests create` (adjustments), `capture set --scope|--service … [--retention 90d --override narrow]`, `spend --scope …` (a scope's budget mode, `observe`, `warn` or `enforce`, is its ScopePolicy's `budgetMode`, changed by a pull request: ADR 0050) |
| Budget manager | `requests list --pending`, `requests approve|reject`, `grants create --from cc/…`, `spend --cost-center …` |
| Admin / operator | `config get` (effective config per pool: CRDs plus overrides), `config validate dir/` (offline, the snapshot compiler; for CI, before a merge), `overrides create|list|expire` (incident levers, each with an expiry and a reason), `explain` (route, deployment, limits and rights for a sample request), `gateways list` (applied snapshot, its age, health), `gateways drain`, `audit list`, `state history|restore`, `bootstrap` (the first document, pools and scopes), `support-bundle` |

`golemctl explain --as user@corp --scope project/support-bot --model
claude-sonnet --tokens 20000` is the first tool for "why was I limited /
routed there".

## 2. golem-console (M2)

A small web UI for the tasks people do weekly, not for operating golem.

- Go, server-rendered (`html/template`, htmx), no SPA build; its own
  Deployment; signs users in with Entra (authorization code with PKCE) and
  calls the control API on their behalf with their token, so it has no
  rights of its own.
- Pages: my keys (mint with a constraint form, revoke, copy once), my
  spend; my teams and projects (members, services, caps, grants);
  caps and spend (approvals when they arrive, ADR 0034); cost
  center view; admin: gateways and versions, config (read-only, linking to
  the CRD or `golemctl` for edits), audit search.
- Charts link to Grafana panels (spend by member, model, day) filtered to
  the scope, rather than re-implementing dashboards.

## 3. Backstage plugin (M3, optional)

TypeScript, generated Connect client. An entity tab on Components and
Groups: a Component annotated `golem.dev/service: <id>` shows its service's
spend, budget and keys (rotate, revoke for owners); a Group mapped to a
golem team shows the team's pools and pending requests. Writes go through
the API with the user's Entra token (Backstage's Microsoft auth provider).

Building the console first and the plugin second keeps one reference UI
that tracks the API; if Backstage is the enterprise's portal, the order can
flip with no API change.

## 4. Grafana

Not a golem tool, but the place people look: dashboards in
`deploy/grafana` for spend (by cost center, team, project, member, model),
budget burn and forecast, limits hit, latency and TTFT by deployment,
failovers and ejections, and gateway versions. Alerts on burn rate and on
ledger drift (control-plane.md §5).

## 5. Standalone gateway

For a developer's own machine: `golem-gateway --standalone ./config/`
serves the CRD YAML in `./config/` with the developer's provider
credentials, and `--explain request.json` shows how a request would be
routed and limited (ADR 0023). `golemctl agent setup … --local` points an
agent at it.
