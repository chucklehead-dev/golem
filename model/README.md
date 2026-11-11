# Models

Literate Quint specifications of golem's protocols: each `.md` is the
model and its explanation, read top to bottom; `make` extracts the Quint
(`lmt`) and checks it. The generated `.qnt` files are committed so they can
be run without lmt; `make verify-tangled` checks they match.

| Model | Question it answers |
|---|---|
| [publication.md](publication.md) | After a revocation is acknowledged, how long can any gateway keep serving the key, and what must hold for that bound? |

Planned (testing.md §3): the accounting journal, site allocations, and
approvals and grants.
