# 0019: Background jobs are idempotent scans by the leader; Temporal later

Status: Accepted (v1 baseline). From archive 0020, 0032. Amended by [0045](0045-leadership-in-the-state-document.md) (leadership is a lease in the state document).

## Decision

- golem-control's leader (a Kubernetes Lease) runs every job as an
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
