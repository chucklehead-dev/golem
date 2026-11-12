#!/usr/bin/env bash
# Checks the accounting model; see exposure.md §9 and check.sh.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
BACKEND=${QUINT_BACKEND:-typescript}
SAMPLES=${SAMPLES:-5000}
STEPS=${STEPS:-25}
SEED=${SEED:-1}
SPEC=exposure.qnt
run() { set +o pipefail; "$QUINT" run --backend="$BACKEND" --max-steps="$STEPS" --max-samples="$SAMPLES" $( [[ $SEED == random ]] || echo --seed="$SEED" ) "$@" "$SPEC" 2>&1 | grep -E "^\[(ok|violation)\]" | head -1 || true; }
passes() { "$QUINT" test --backend="$BACKEND" --main="$1" --match="^$2\$" "$SPEC" >/dev/null 2>&1; }
fail=0

"$QUINT" typecheck "$SPEC"

echo "== the design holds (simulation of $SAMPLES runs, not a proof)"
r=$(run --main=exposureDesign --invariant=safe); echo "exposureDesign safe: $r"
[[ $r == "[ok]"* ]] || fail=1

echo "== the design can reach every scenario"
for v in overshoots waiverExceedsLimit cancelledBilledMore handoffInFlight; do
  r=$(run --main=exposureDesign --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached by simulation"; fail=1; }
done

echo "== each broken design is caught"
# resetHandoff breaks withinBound only after several snapshots in a row, too rare for simulation; resetTest is its witness.
for pair in noScopeLimit:limitsHonoured resetHandoff:noLostIncrements sumHandoff:knownNeverOver releaseOnCancel:limitsHonoured noReacquire:limitsHonoured; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m breaks $i: $r"
  [[ $r == "[violation]"* ]] || { echo "  not caught by simulation"; fail=1; }
done

echo "== the stories behave as exposure.md §9 says"
expect() { # instance story pass|fail
  if passes "$1" "$2"; then got=pass; else got=fail; fi
  echo "$2 in $1: $got"; [[ $got == "$3" ]] || { echo "  expected $3"; fail=1; }
}
expect noScopeLimit scopeExposureTest pass
expect exposureDesign scopeExposureTest fail
expect resetHandoff resetTest pass
expect exposureDesign resetTest fail
expect sumHandoff sumTest pass
expect exposureDesign sumTest fail
expect releaseOnCancel cancelResendTest pass
expect exposureDesign cancelResendTest fail
expect noReacquire forgottenSlotTest pass
expect exposureDesign forgottenSlotTest fail
expect exposureDesign waiverTest pass

exit $fail
