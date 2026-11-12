#!/usr/bin/env bash
# Checks the freshness model; see freshness.md §8 and check.sh.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
BACKEND=${QUINT_BACKEND:-typescript}
SAMPLES=${SAMPLES:-5000}
STEPS=${STEPS:-30}
SEED=${SEED:-1}
SPEC=freshness.qnt
run() { set +o pipefail; "$QUINT" run --backend="$BACKEND" --max-steps="$STEPS" --max-samples="$SAMPLES" $( [[ $SEED == random ]] || echo --seed="$SEED" ) "$@" "$SPEC" 2>&1 | grep -E "^\[(ok|violation)\]" | head -1 || true; }
passes() { "$QUINT" test --backend="$BACKEND" --main="$1" --match="^$2\$" "$SPEC" >/dev/null 2>&1; }
fail=0

"$QUINT" typecheck "$SPEC"

echo "== the design holds (simulation of $SAMPLES runs, not a proof)"
r=$(run --main=freshDesign --invariant=safe); echo "freshDesign safe: $r"
[[ $r == "[ok]"* ]] || fail=1

echo "== the design can reach every scenario"
# compositeGapReached needs five steps in one order, too rare for random simulation; compositeGapTest is its witness below.
for v in configRestrictionApplied regrantedRightServed staleLeaderPublished restoreHappened; do
  r=$(run --main=freshDesign --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached by simulation"; fail=1; }
done

echo "== each broken design is caught"
for pair in staleCache:restrictionsBounded anchorState:restrictionsBounded fenceStateOnly:noConfigRollback naiveRestore:restoreNeverWidens; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m breaks $i: $r"
  [[ $r == "[violation]"* ]] || { echo "  not caught by simulation"; fail=1; }
done

echo "== the stories behave as freshness.md §8 says"
expect() { # instance story pass|fail
  if passes "$1" "$2"; then got=pass; else got=fail; fi
  echo "$2 in $1: $got"; [[ $got == "$3" ]] || { echo "  expected $3"; fail=1; }
}
expect staleCache staleCacheTest pass
expect freshDesign staleCacheTest fail
expect anchorState anchorTest pass
expect freshDesign anchorTest fail
expect fenceStateOnly configRollbackTest pass
expect freshDesign configRollbackTest fail
expect naiveRestore restoreWidensTest pass
expect freshDesign restoreWidensTest fail
expect freshDesign compositeGapTest pass

exit $fail
