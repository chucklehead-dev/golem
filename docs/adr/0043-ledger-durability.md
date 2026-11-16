# 0043: Ledger durability lives in the collector, not the gateway

Status: Accepted. Amends ADR 0015 (where the persistent queue sits). Its
numbering record and gap reconstruction (the two bullets so marked below)
were removed with the site counter ([0031](0031-soft-budgets.md)): rows
carry no number, and nothing is reconstructed. Built: the collector is a
native sidecar in the gateway's pod on loopback (deploy/charts/golem-gateway),
its config deploy/otel/collector.yaml, its `file_storage` queue on the
pod's volume; spend.sql's views take its rows (test/ledger).

## Context

A ledger row is an OTel log record the gateway sends to the collector,
which writes it to ClickHouse (ADR 0015). Rows can be lost in three ways:

- **A planned shutdown.** Until recently this was the routine loss: every
  deploy cut long streams with no accounting. It is fixed: the gateway
  drains, cuts what remains, waits for each call's accounting, writes
  rows still waiting for numbers, and shuts telemetry down last.
- **A collector outage.** The gateway's in-memory queue (65,536 rows,
  about 30 s at 2,000 calls a second) fills, and later rows are counted
  (`golem.ledger.dropped`) and lost.
- **A crash or node loss.** About a second of batched rows per pod, plus
  any rows waiting for their counter numbers during a Valkey outage.

A spool on the gateway (SQLite, or an append-only segment file) was
considered.

## Decision

- **No durable spool in the gateway.**
  - **Storage dies with the pod:** an `emptyDir` survives a container
    restart, not a deploy, an eviction or node loss, which are the common
    cases. Durability would need a persistent volume per gateway pod,
    making the gateway a StatefulSet against ADR 0031's stateless
    decision.
  - **Volume:** rows are about 5 MB/s per site at 2,000 calls a second
    (18 GB an hour before compression), so an hour's outage needs gigabytes
    per pod.
  - **SQLite is the wrong shape:** the data is a queue, not a database,
    and SQLite would add cgo or a large pure-Go port to the hot binary.
- **The persistent queue is a node-local collector's.** The collector runs
  as a DaemonSet, or in the gateway's pod, with a `file_storage`
  `sending_queue` on node-local disk. The gateway exports to it
  compressed: in the gateway's pod, at a literal loopback address
  (plaintext, the hop inside the pod's network namespace); as a
  DaemonSet, at the node's address, which is not loopback from the pod,
  over TLS (operations.md, "Telemetry transport"; golem#7). The gateway's queue then only bridges the local agent's
  restart, and counts what it cannot deliver (`golem.ledger.dropped`,
  `golem.ledger.failed`, alerted on).
- (Removed.) **A row does not wait for its number.** The row is written
  when its call ends. When the limiter numbers the event, it emits a small
  `(event_id, site, scope, period, counter_seq)` record, joined to the row
  in ClickHouse. A Valkey outage then holds no billing rows in gateway
  memory; today up to 100,000 wait there and a crash loses them.
- (Removed.) **A gap is reconstructed before it is declared lost.** The
  site counter keeps each event's ID and cost for five hours (ADR 0042),
  far longer than the 15-minute gap timeout.
  - Before declaring a gap lost, a site-local job (golem-control still
    reads no site's Valkey) writes a minimal `attempt` row from the
    counter: event, scope, period, cost, marked reconstructed.
  - With the principal and key added to the counter's events, member and
    key caps stay exact too. Only token-level attribution is lost, and
    reconciliation corrects the books.
- **What remains** is node loss with unacknowledged agent data plus a
  Valkey failover in the same window. That is rare, visible (gaps,
  reconciliation) and bounded by the soft-budget contract (ADR 0031).
  The gateway spool is reconsidered only if the drop and gap metrics show
  real loss after the above, or if finance requires hard ceilings
  (ADR 0031's reserved alternative).

## Consequences

The gateway stays stateless. Deployment gains a node-local collector with
a persistent queue (deploy/, M1). The ledger contract gains a numbering
record (contracts.md §3, when it is built). golem-control's gap job gains
a reconstruction step that a site-local reader of the counter feeds.

## Alternatives

- **SQLite in the gateway.** Rejected above.
- **An append-only segment file, spooled on overflow.** The right shape
  if a gateway spool is ever needed: framed, CRC-checked records,
  rotation, torn-tail recovery and replay. A few hundred lines, and a
  second persistence system to own. Deferred until the metrics call for
  it.
- **Direct ClickHouse inserts from gateways.** ADR 0015's rejected
  alternative: a second write path in the hot binary.
