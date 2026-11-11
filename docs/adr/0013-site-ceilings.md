# 0013: Fixed site allocations per period

Status: Superseded by [ADR 0031](0031-soft-budgets.md): no site allocations; every site checks the same known spend.

## Decision

Each site enforces a fixed allocation per pool, cap and period. Issued
allocations cannot be lowered or reclaimed in that period. The sum of issued
allocations cannot exceed available funding. Additional allocations use only
unallocated funds; funding reductions and grants out cannot consume already
allocated funds. A disconnected site's last allocation remains allocated.
Key/member caps follow the same rule. One site may receive the full pool.

Ledger spend supports monitoring, never reclamation. Dynamic transfers need
acknowledged reductions or expiring fenced leases, modeled against actual
applied snapshots and outstanding liability before activation. No dynamic
rebalance job ships initially. Period rollover isolates old-period liability
until settlement. Unused allocations can remain stranded.

## Rationale

The original baseline asserted that ledger lag affected utilization only.
A disconnected site can keep its old ceiling after a controller reduces it
and reallocates the difference. Controller-side ceiling sums therefore do
not establish safety. Fixed allocations avoid this double allocation.
