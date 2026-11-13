# 0044: Budgets are admission and monitoring controls, not spending ceilings

Status: Accepted. Amends [0031](0031-soft-budgets.md) (the overshoot bound
and "strict with `L = 1` is a hard ceiling"), [0038](0038-accounting-refinements.md)
("the bound shown to budget owners is a known number") and
[0012](0012-limits-and-accounting.md). Renames the strict check.

## Context

ADR 0031 made budgets soft, but kept a stated overshoot bound (`L × W`
under the soft check, `(L − 1) × W` under the strict one) and a strict
option that, with one call in flight, was "a hard ceiling on one site".
Both rest on `W`: that no call costs more than the worst case golem
computes from the request. The accounting model checks the arithmetic
given that assumption (`model/accounting.md` §9: the formula "is what makes
`WORST` a real upper bound"); the real world does not grant it.

The reviews of #3 and #4 found the assumption failing one way after
another, and each fix found the next: token-dense output against a
bytes-per-token average; output in fields golem does not know; server-side
tools and their billing units; images, audio and files, whose input tokens
a request's bytes do not bound (a URL is short); a provider still working
after a stream is cut, whose output golem never sees; outcomes no response
reports; a price list behind the provider's. Each can be patched, and the
next provider feature, modality or billing category reopens it. A mode
named strict, whose guarantee depends on every provider behaving as
modelled, is the dangerous outcome: it promises what nothing enforces.

Finance's actual boundary already exists elsewhere: the provider account.
Golem's job is to keep expected exposure small and to know, after the
fact, who spent what.

## Decision

**What a budget is.** Golem budgets are admission and monitoring controls,
not guaranteed spending ceilings. They reduce expected exposure through
concurrency, rate, request and output limits. Provider-native quotas and
billing controls remain the authoritative financial boundary. Missing or
ambiguous usage is estimated and later reconciled; no estimate is
guaranteed to cover every provider feature or failure mode.

**What golem enforces**, exactly, and claims nothing beyond:

- authentication and authorization: keys, Entra tokens, scope membership
  (ADR 0026, 0041);
- concurrency: principal and scope in-flight slots, renewed while a call
  runs (ADR 0031, 0038, 0041 §7.6);
- rate limits;
- request size: the body limit, a scope's `maxInputBytes`;
- declared output ceilings: a scope's `maxOutputTokens` over `max_tokens`
  × `n`, and a missing `max_tokens` refused under one (ADR 0038);
- request features: restricted mode, storage forbidden, server tools
  refused where the scope says (ADR 0027, 0037, 0041 §3);
- egress: upstream destinations checked at each dial, telemetry over TLS
  (ADR 0041);
- call lifetime: first-byte, idle and maximum durations;
- refusal once known spend reaches a budget, on every site within the
  refresh lag (ADR 0031, 0042).

**Estimates are conservative, not guaranteed.** A call whose usage is
missing or ambiguous is charged by rules chosen to err high (every byte
received a token, at most `max_tokens` × `n`; every request byte a token;
an uncertain call its worst case). They are recorded as estimates
(`estimated_fraction`) and can still be low: a provider's tail after a
cut, a modality that bytes do not bound, a unit golem does not price.

**Reconciliation is authoritative.** The provider's billing is the record;
the monthly reconciliation (ADR 0038) books the difference as adjustments,
attributed where the provider's data allows. Budgets, reports and alerts
are corrected by it, not the other way round.

**Provider-account spending limits are required.** Every provider account
golem routes to has a spending limit or budget alarm set at the provider
(Anthropic's workspace spend limits, OpenAI's project budgets, AWS Budgets
actions for Bedrock), sized to what finance accepts losing. This is a
deployment requirement (operations.md), not a check golem can make, and it
is the financial backstop.

**The worst-case check, renamed and described by what it closes on.** The
scope option `strict` becomes `worstCaseCheck`. It is a fail-closed
admission mode. It refuses a call when:

1. known spend plus the request's declared worst case (every input byte a
   token at the full input rate, plus `max_tokens` × `n` at the output
   rate, no cache discount) exceeds the budget;
2. the request sets no `max_tokens`, so it declares no worst case;
3. the request asks for server-side tools or MCP servers, whose cost the
   request does not declare;
4. the worst case cannot be priced without overflow.

It does not cover input tokens that bytes do not bound (images, audio,
files, fetched URLs), units billed outside tokens, a provider's tail past
a cut, prices out of date, spend lost in a crash, or spend at other sites
within the refresh lag. It narrows exposure by about one call per
principal; it is not a ceiling.

**The overshoot formulas** of ADR 0031 and the accounting and exposure
models stay as statements of the design's expected exposure when every
call costs at most its declared worst case. They are not shown to budget
owners as guarantees. A budget owner is told: "spending stops within about
a minute of reaching the limit; calls in flight may go past it; the
provider's bill, reconciled monthly, is the record".

## Consequences

- `ScopePolicy.strict` is `worstCaseCheck` (v1alpha1, unreleased: no
  conversion). The snapshot, the limiter and its Valkey script follow.
  Refusal messages name the check and what it closes on.
- The accounting model's `STRICT` switch is `WORST_CHECK`, its instances
  `worstCheckDesign` and `worstCheckSingle`, and `hardCeiling` is
  `withinDeclaredWorst`: the property holds of the model, in which `WORST`
  bounds every call; the model says so.
- The PRD, org model, gateway, contracts and security documents drop
  "hard ceiling" and "strict accounting"; the operations guide lists the
  provider spending limits as a production requirement.
- The fixes of #3 and #4 stay: fail-closed parsing, overflow protection,
  untrusted usage treated as missing, conservative estimates, uncertain
  outcomes charged. They reduce error; none is a proof of containment.
- Future review findings of the form "an estimate can be low when …" are
  judged by their expected cost against reconciliation and the provider's
  limit, not as violations of a ceiling golem does not claim.

## Alternatives

- **Keep strict and close each gap as found.** An open-ended proof
  obligation against every provider's features, modalities and billing.
- **Drop the worst-case check.** It still refuses calls that would
  plainly pass a budget and calls that declare no bound, cheaply; kept,
  under a name that says what it does.
- **Hard ceilings through reservations and a journal** (the design ADR
  0031 superseded). The same assumption about `W`, at far greater cost.
