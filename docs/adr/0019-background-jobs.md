# 0019: Background jobs are idempotent scans by the leader; Temporal later

Status: Accepted (v1 baseline). Amended by [0053](0053-one-golem-control-replica-for-m1.md) (one replica, its writes fenced). Built in M1: the publication and renewal loop, known spend, audit shipping and the Entra group sync (control-plane.md §5); the other jobs below apply from M2 (roadmap.md).

## Decision

- golem-control (its one replica, ADR 0053) runs every job as an
  idempotent scan: Entra group sync (also revoking disabled users' keys),
  period rollover, ceiling rebalance, audit shipping, pointer refresh,
  expiry of keys, requests and registrations, reminders, threshold
  notifications, ledger and provider reconciliation, capture expiry and
  retention deletion (M2), Copilot usage ingestion (M2).
- No job table and no workflow framework in M1: approvals are state
  (requests with a state and expiry); notifications carry idempotency keys
  recorded in the state, so a scan repeated after a leader change does not
  send twice.
- **Temporal**, when the platform's Temporal is production-grade, takes the
  long-running, human-in-the-loop work: approvals with escalation, service
  key rotation with an overlap, provider batch jobs (M3).
- Notifications: email through Graph `sendMail` from one shared mailbox (or
  an SMTP relay), and Teams channel messages through a Workflows webhook per
  scope.

## Alternatives

Temporal from day one (an unfamiliar dependency in the first release); a
durable job table (state machinery the scans do not need).
