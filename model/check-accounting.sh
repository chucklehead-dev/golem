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
passes() { "$QUINT" test --backend="$BACKEND" --main="$1" --match="^$2\$" "$SPEC" >/dev/null 2>&1; }
fail=0

"$QUINT" typecheck "$SPEC"

echo "== the designs hold (simulation of $SAMPLES runs, not a proof)"
for pair in softDesign:safe worstCheckDesign:safe worstCheckSingle:withinDeclaredWorst; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m $i: $r"
  [[ $r == "[ok]"* ]] || fail=1
done

echo "== the design can reach every scenario"
for v in overshoots refusesWhenSpent crashThenBalanced duplicateAbsorbed; do
  r=$(run --main=softDesign --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached"; fail=1; }
done

echo "== each broken design is caught"
# noLimit's and noSlotRenewal's counterexamples (every call admitted before any completes; slots expiring mid-call) are too
# rare for random simulation; their stories, unlimitedTest and slotExpiryTest, are the witnesses below.
for pair in rawSum:noDoubleCount noReconcile:booksMatch; do
  m=${pair%%:*}; i=${pair##*:}
  r=$(run --main="$m" --invariant="$i"); echo "$m breaks $i: $r"
  [[ $r == "[violation]"* ]] || { echo "  not caught"; fail=1; }
done

echo "== the stories behave as accounting.md §10 says"
expect() { # instance story pass|fail
  if passes "$1" "$2"; then got=pass; else got=fail; fi
  echo "$2 in $1: $got"; [[ $got == "$3" ]] || { echo "  expected $3"; fail=1; }
}
expect softDesign siblingsOvershootTest pass
expect worstCheckDesign siblingsOvershootTest fail
expect noLimit unlimitedTest pass
expect softDesign unlimitedTest fail
expect softDesign crashReconciledTest pass
expect noReconcile crashReconciledTest fail
expect rawSum duplicateCountedTest pass
expect softDesign duplicateCountedTest fail
expect noSlotRenewal slotExpiryTest pass
expect softDesign slotExpiryTest fail


echo "== exhaustive: accounting.small.yaml, each design every run up to a depth (not a sample)"
if command -v "${GOTOGETHER:-gotogether}" >/dev/null; then
  . ./graph.sh
  matrix accounting
else
  echo "  skipped: gotogether is not installed (go install github.com/chucklehead-dev/gotogether/cmd/gotogether@latest)"
fi

exit $fail
