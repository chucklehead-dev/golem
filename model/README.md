# Models

Literate Quint specifications of golem's protocols: each `.md` is the
model and its explanation, read top to bottom; `make` extracts the Quint
(`lmt`) and checks it. The generated `.qnt` files are committed so they can
be run without lmt; `make verify-tangled` checks they match.

| Model | Question it answers |
|---|---|
| [publication.md](publication.md) | After a revocation is acknowledged, how long can any gateway keep serving the key, and what must hold for that bound? |
| [freshness.md](freshness.md) | With two sources (the state document and the config CRDs), stale watch caches, Git rollbacks, leader changes and restores, is every restriction from either source honoured within one lease (ADR 0041 §5, §7.2)? |
| [accounting.md](accounting.md) | With soft budgets (ADR 0031), is the overshoot within the stated bound, is anything counted twice, and do the books match the provider's bill after a crash? |
| [attempts.md](attempts.md) | When a call is retried or fails over before it commits, does each caller get one answer, is every attempt charged once, does the budget bound hold, do retries stay within the slots, quotas and budgets, and does an ejected deployment see only its probe? |
| [exposure.md](exposure.md) | For a scope shared by several principals on several gateways, with late ingestion, Valkey outages and cancelled calls, does the scope stay within a stated bound, and is the rest of the exposure recorded (ADR 0041 §6)? |

Each model is checked two ways (`make check`): random simulation of a
mid-sized instance, and an exhaustive search of a small one
(`<model>.small.yaml`, gotogether's `simulate -graph`, which `make check`
runs when gotogether is installed): every run up to a depth, each broken
design's shortest counterexample, and dead actions reported. The
exhaustive search is a proof for its instance and depth only.

The small instance's designs are its binding's `simulate.variants`: each
with the mechanism it turns off, what it must hold and exactly what it
breaks. `simulate -matrix` searches every one and writes the table, the
properties by the designs, into the model's document (its "The small
instance" section); CI fails if a committed table is not the model's.
Each variant may bound its own search (leadership's broken designs stop a
step or two past their counterexamples).

Planned (testing.md §3): approvals and grants.
