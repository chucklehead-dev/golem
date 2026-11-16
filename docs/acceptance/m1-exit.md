# Acceptance: M1 exit

The record that decides M1's exit (m1-plan.md, *The exit, made
measurable*). A template until filled: each section says what goes in it
and where its evidence lives. Fill a section when its evidence exists;
link, do not paste, raw exports. Who filled it and when goes on each
section's last line.

## 0. Context

- Team, scopes and their ScopePolicies (links to the config repository at
  the commit published):
- Installation: pools, site, golem commit and chart versions:
- Provider accounts golem routes to, each with its spending limit and who
  set it (golem#7):

## 1. Baseline before golem

A month of the team's provider usage before the move, by account × model.

| Account | Provider | Model | Month | Requests | Input tokens | Output tokens | Cost | Source (CUR query, usage API export) |
|---|---|---|---|---|---|---|---|---|
| | | | | | | | | |

## 2. Endpoint inventory

The team's apps' and agents' endpoints, from a week of their provider logs
(CloudTrail and Bedrock invocation logs, the OpenAI usage API, the
Anthropic console), each either served by golem or deliberately not.

| Caller (app or agent) | Provider endpoint | Calls in the week | Served by golem as | Or: why not, and what replaces it |
|---|---|---|---|---|
| | | | | |

Source of the week's logs (queries, time range):

## 3. Exit 1: all traffic through golem for two weeks

- The two weeks (dates, UTC):
- Usage on the team's direct accounts and roles outside golem's dedicated
  ones over the two weeks (expected zero), with the query:
- Bypass attempts denied (the Bedrock SCP's CloudTrail denials, blocked
  egress), with the query:
- Signed off by:

## 4. Exit 2: the ledger within 3–5% of provider usage

The reconciliation query over `golem.spend_dedup` against CUR and the
OpenAI usage API (m1-plan.md exit 2's rule: the two weeks within 5%, every
day within 10%; rows by `started_at` in UTC).

- The query (linked, at a commit):
- Daily previews:

| UTC day | Account | Model | Ledger cost | Provider cost | Difference | Estimated share | Uncertain share | Notes |
|---|---|---|---|---|---|---|---|---|
| | | | | | | | | |

- Two-week totals and difference:
- Causes of each day over 5%:
- Signed off by:

## 5. Exit 3: outage drills

Each run under synthetic load at the team's peak rate. Passes with no
caller-visible failures beyond those in flight (cut streams allowed and
counted), TTFT p95 added under 2 s, every attempt on the ledger. Full
write-ups in m1-drills.md.

| Drill | Date | Load | Failures beyond in-flight | Cut streams | TTFT p95 added | Attempts on the ledger | Result | Record |
|---|---|---|---|---|---|---|---|---|
| A Bedrock region to another region or inference profile | | | | | | | | |
| A local box to its fallback pool | | | | | | | | |

## 6. Exit 4: budgets from observe to enforce

- Two weeks of observe (dates); every `over_budget` row explained:
  the query, and each explanation:
- Budgets sized from that history (the ScopePolicy PR):
- The rollback drill (a PR back to `observe`): date, time to take effect
  at the gateways:
- The switch to `enforce` (the PR, the published snapshot's sequence and
  commit):
- The first refusal in enforce: the `golem.refusals` count for
  `budget.pool_exhausted`, and the refused request's ID with its span:
- Signed off by the platform team's lead:

## 7. Release gate

- golem#7's evidence closed (link):
- The commit lifting `validate`:
