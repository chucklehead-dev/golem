# 0020: Durable workflows behind a seam; Temporal for the long-running, human-in-the-loop ones

Status: Proposed

## Context

Temporal runs elsewhere in the platform but is not yet a core,
ubiquitous service or a common competency. Some of golem's background work
is long-running, waits on people, calls external systems that fail, and
must not be done twice: Temporal's strengths. Other work is a tight loop
where Temporal's latency and operations would only cost.

## Decision

Fit, by piece:

| Piece | Fit | Why |
|---|---|---|
| Adjustment and grant approvals: wait for a budget manager, remind, escalate, expire, apply | **Strong** | Waits days on people; timers; exactly-once effect |
| Service key rotation: mint new, notify owners, overlap period, revoke old | **Strong** | Multi-step with waits; compensation if the owner objects |
| Provider batch jobs (M3): submit, poll for hours, fetch results, charge | **Strong** | Hours-long, external, must charge exactly once |
| Scope onboarding: project, Langfuse project, Teams webhook check, initial grant request | Good | A saga across external systems |
| Capture retention deletion | Good | Scheduled, batched, external API with retries |
| Store migration to multi-region | Good, one-off | Pause, copy, verify, switch, resume |
| Period rollover, ledger reconciliation, Entra sync | Neutral | Scheduled idempotent batches; a loop in golem-control's jobs is enough |
| Lease rebalancing (every 30 s), distribution, anything on the request path | **No** | Latency-critical, high frequency |
| Operator reconciliation | **No** | Kubernetes' level-triggered model is the right one there |

- golem-control's jobs defines its jobs behind a `workflow` seam: steps are
  activities that call the control plane's services (never the store
  directly, so layering holds), and waits are timers or signals.
- M1 implements the seam with a small durable job table in the store (a
  state machine per job, leased by worker replicas). It covers approvals'
  needs at M1's volume.
- Approvals, key rotation and batch jobs move to Temporal when the
  platform's Temporal is production-grade, without changing the
  services they call. The rest stays in golem-control's jobs.

## Consequences

- No hard dependency on Temporal for M1; the workflows that need it most
  are written so the move is mechanical.
- Two implementations of the seam for a while; the job table stays for
  the simple jobs.

## Alternatives

- Temporal from day one for all jobs: best durability, but makes an
  unfamiliar service a dependency of golem's first release.
- Never: the approval and batch flows would rebuild timers, retries and
  history by hand.
