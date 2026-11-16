#!/usr/bin/env bash
# Checks the publication model: the design holds, it can reach every
# scenario, and each broken design is caught. See publication.md §2.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
BACKEND=${QUINT_BACKEND:-typescript}
SAMPLES=${SAMPLES:-5000}
SEED=${SEED:-1}  # a fixed seed keeps CI deterministic; vary it (SEED=random) for wider searches
STEPS=${STEPS:-30}
run() { set +o pipefail; "$QUINT" run --backend="$BACKEND" --max-steps="$STEPS" --max-samples="$SAMPLES" $( [[ $SEED == random ]] || echo --seed="$SEED" ) "$@" publication.qnt 2>&1 | grep -E "^\[(ok|violation)\]" | head -1 || true; }
fail=0

"$QUINT" typecheck publication.qnt

echo "== the design holds (simulation of $SAMPLES runs, not a proof)"
r=$(run --main=design --invariant=safe); echo "design safe: $r"
[[ $r == "[ok]"* ]] || fail=1

echo "== the design can reach every scenario (each negation must be violated)"
for v in revocationReachesGateway partitionedAndExpired restoredWithRevocations; do
  r=$(run --main=design --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached: the model may be too strict"; fail=1; }
done

echo "== each broken design is caught"
for pair in noFencing:publicationsOrdered noFencing:noRollback \
            leaseFromSigning:revocationBounded statusFloor:noRollbackBelowCheckpoint \
            naiveRestore:revocationsSurviveRestore naiveRestore:revocationBounded \
            noExpiry:noExpiredServe noExpiry:revocationBounded; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m breaks $i: $r"
  [[ $r == "[violation]"* ]] || { echo "  not caught: the property or the mutant is wrong"; fail=1; }
done

echo "== each bug's story passes in its broken design and fails in the correct one"
for pair in leaseFromSigning:staleViewTest noFencing:pausedLeaderTest naiveRestore:restoreTest; do
  m=${pair%%:*}; t=${pair##*:}
  if "$QUINT" test --backend="$BACKEND" --main="$m" --match="^$t\$" publication.qnt >/dev/null 2>&1; then echo "$t in $m: passes"; else echo "$t in $m: FAILS (should pass)"; fail=1; fi
  if "$QUINT" test --backend="$BACKEND" --main=design --match="^$t\$" publication.qnt >/dev/null 2>&1; then echo "$t in design: PASSES (should fail)"; fail=1; else echo "$t in design: impossible, as it should be"; fi
done

exit $fail
