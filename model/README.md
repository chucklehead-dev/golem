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
| [exposure.md](exposure.md) | For a scope shared by several principals on several gateways, with late ingestion, Valkey outages and cancelled calls, does the scope stay within a stated bound, and is the rest of the exposure recorded (ADR 0041 §6)? |

Each model is checked two ways (`make check`): random simulation of a
mid-sized instance, and an exhaustive search of a small one
(`<model>.small.yaml`, gotogether's `simulate -graph`, which `make check`
runs when gotogether is installed): every run up to a depth, each broken
design's shortest counterexample, and dead actions reported. The
exhaustive search is a proof for its instance and depth only.

Planned (testing.md §3): approvals and grants.
