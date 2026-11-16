# Models

Literate Quint specifications of golem's protocols: each `.md` is the
model and its explanation, read top to bottom; `make` extracts the Quint
(`lmt`) and checks it. The generated `.qnt` files are committed so they can
be run without lmt; `make verify-tangled` checks they match.

| Model | Question it answers |
|---|---|
| [publication.md](publication.md) | After a revocation is acknowledged, how long can any gateway keep serving the key, and what must hold for that bound? |
| [attempts.md](attempts.md) | When a call is retried or fails over before it commits, does each caller get one answer, is every attempt charged once, does the budget bound hold, do retries stay within the slots, the deployments' ceilings and the budgets, and does an ejected deployment see only its probe? |
| [exposure.md](exposure.md) | For a scope shared by several principals on several gateways, with late ingestion, Valkey outages and cancelled calls, does the scope stay within a stated bound, and is the rest of the exposure recorded (ADR 0041 §6)? |

`make check` type-checks each model and runs a random simulation of a
mid-sized instance: the design holding every property, every scenario
reached, and each broken design caught. Simulation samples behaviours; it
is not a proof. The Go code is checked against publication, exposure and
attempts by connect (`TestPublicationModel`, `TestExposureModel`,
`TestAttemptsModel`: the model's traces drive the code).

Each model's invariants (its `safe` val's conjuncts) are listed in its
`<model>.invariants.yaml`, which exists only to name them for `gotogether
invariants`. `make invariants` (`./invariants.sh`) writes their tables in
the [orientation page](../docs/walkthroughs/README.md#each-models-invariants):
what each means, the code that enforces it (`//quint:enforces`) and the
tests that check it (`//quint:checks`). `make check-invariants` is CI's
check.

## Retired models

Removed in the commit after dc20b0c (`git show dc20b0c:model/<name>.md`
reads them); they are no longer checked or maintained.

- **accounting.md**: soft budgets (ADR 0031): the overshoot bound, no
  double count, the books matching the provider's bill after a crash. Its
  ground is covered by exposure.md, which extends it.
- **freshness.md**: config freshness with two sources (the state document
  and the config CRDs; ADR 0041 §5, §7.2). It modelled a CRD watch that
  M1 does not build.
- **leadership.md**: compared placements of the lease and the fence and
  chose ADR 0049's fence placement, which ADR 0053 keeps with one
  replica and no lease. Its counterexamples that still apply are Go
  tests: control/publish's `TestRegressedOrTamperedRefused`, control/state's `TestClaim` and
  `TestFenceInvariant`.

Planned (testing.md §4): approvals and grants, when grants are built (M2).
