#!/usr/bin/env bash
# Checks the attempt loop model; see attempts.md §2 and check.sh.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
BACKEND=${QUINT_BACKEND:-typescript}
SAMPLES=${SAMPLES:-2000}
STEPS=${STEPS:-25}
SEED=${SEED:-1}
SPEC=attempts.qnt
run() { set +o pipefail; "$QUINT" run --backend="$BACKEND" --max-steps="$STEPS" --max-samples="$SAMPLES" $( [[ $SEED == random ]] || echo --seed="$SEED" ) "$@" "$SPEC" 2>&1 | grep -E "^\[(ok|violation)\]" | head -1 || true; }
passes() { "$QUINT" test --backend="$BACKEND" --main="$1" --match="^$2\$" "$SPEC" >/dev/null 2>&1; }
fail=0

"$QUINT" typecheck "$SPEC"

echo "== the design holds (simulation of $SAMPLES runs, not a proof)"
r=$(run --main=attemptsDesign --invariant=safe); echo "attemptsDesign safe: $r"
[[ $r == "[ok]"* ]] || fail=1

echo "== the design can reach every scenario"
for v in failsOver uncertainEndsCall ejects overshoots; do
  r=$(run --main=attemptsDesign --invariant="not($v)"); echo "$v: $r"
  [[ $r == "[violation]"* ]] || { echo "  scenario not reached"; fail=1; }
done

echo "== the stories behave as attempts.md §8 says"
expect() { # instance story pass|fail
  if passes "$1" "$2"; then got=pass; else got=fail; fi
  echo "$2 in $1: $got"; [[ $got == "$3" ]] || { echo "  expected $3"; fail=1; }
}
expect attemptsDesign failoverTest pass
expect attemptsDesign uncertainTest pass
expect attemptsDesign ejectionTest pass
expect retryUncertain uncertainRetryTest pass
expect attemptsDesign uncertainRetryTest fail
expect retryCut cutRetryTest pass
expect attemptsDesign cutRetryTest fail
expect sharedEvent sharedEventTest pass
expect attemptsDesign sharedEventTest fail

exit $fail
