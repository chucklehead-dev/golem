#!/usr/bin/env bash
# Checks the accounting model; see accounting.md §2 and check.sh.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
BACKEND=${QUINT_BACKEND:-typescript}
SAMPLES=${SAMPLES:-5000}
STEPS=${STEPS:-25}
SEED=${SEED:-1}
SPEC=accounting.qnt
run() { set +o pipefail; "$QUINT" run --backend="$BACKEND" --max-steps="$STEPS" --max-samples="$SAMPLES" $( [[ $SEED == random ]] || echo --seed="$SEED" ) "$@" "$SPEC" 2>&1 | grep -E "^\[(ok|violation)\]" | head -1 || true; }
fail=0

"$QUINT" typecheck "$SPEC"

echo "== the design holds (simulation of $SAMPLES runs, not a proof)"
r=$(run --main=accountingDesign --invariant=safe); echo "design safe: $r"
[[ $r == "[ok]"* ]] || fail=1

echo "== the design can reach every scenario"
for v in settledInLedger resolvedAfterLostResponse uncertainAfterCrash duplicateAbsorbed; do
  r=$(run --main=accountingDesign --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached"; fail=1; }
done

echo "== each broken design is caught"
for pair in noIntentFirst:noLostLiability dropUncertain:noLostLiability trimOnSend:noLostLiability \
            rawSum:noDoubleCount fragileMedium:noLostLiability; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m breaks $i: $r"
  [[ $r == "[violation]"* ]] || { echo "  not caught"; fail=1; }
done

echo "== each bug's story passes in its broken design and fails in the correct one"
for pair in noIntentFirst:crashAfterDispatchTest dropUncertain:timeoutErasesTest trimOnSend:failedExportTest \
            rawSum:duplicateCountedTest fragileMedium:lostVolumeTest; do
  m=${pair%%:*}; t=${pair##*:}
  if "$QUINT" test --backend="$BACKEND" --main="$m" --match="^$t\$" "$SPEC" >/dev/null 2>&1; then echo "$t in $m: passes"; else echo "$t in $m: FAILS (should pass)"; fail=1; fi
  if "$QUINT" test --backend="$BACKEND" --main=accountingDesign --match="^$t\$" "$SPEC" >/dev/null 2>&1; then echo "$t in design: PASSES (should fail)"; fail=1; else echo "$t in design: impossible, as it should be"; fi
done

exit $fail
