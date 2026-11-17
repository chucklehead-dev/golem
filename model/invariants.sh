#!/usr/bin/env bash
# Writes each model's two tables into docs/walkthroughs/README.md, between
# the marker lines named for the model: its invariants (the conjuncts of its
# safe val, each with what it means, the code that enforces it and the tests
# that check it, from //quint:enforces and //quint:checks) and its actions
# (the connect driver that runs each). With -check it writes nothing and
# fails when a table is stale (CI). Either way it fails when an invariant
# has no enforcing code or no test, or an annotation or driver names
# something the model lacks.
#
# Needs quint, and the gotogether CLI at the version go.mod requires:
#   go install "github.com/chucklehead-dev/gotogether/cmd/gotogether@$(go list -m -f '{{.Version}}' github.com/chucklehead-dev/gotogether)"
set -euo pipefail
cd "$(dirname "$0")/.."
case ${1:-} in
  "") mode=-write ;;
  -check) mode=-check ;;
  *) echo "usage: model/invariants.sh [-check]" >&2; exit 2 ;;
esac
GOTOGETHER=${GOTOGETHER:-gotogether}

# Each model, what it requires, then the packages it scans. Annotations
# name their model (//quint:enforces attempts.withinBound), so a package
# may carry several models'; a connect driver whose Config names another
# model's spec is left out of this model's actions.
models=(
  "publication enforced,checked,driven ./gateway/snapshot ./gateway/proxy ./control/..."
  "exposure enforced,checked,driven ./gateway/limit"
  "attempts enforced,checked,driven ./gateway/proxy ./gateway/route ./gateway/telemetry"
)

fail=0
for entry in "${models[@]}"; do
  read -r m require dirs <<<"$entry"
  # The .md is the source and lmt writes the .qnt (make tangle), so the
  # spec is the .qnt and the document plain Markdown (-literate=false).
  # shellcheck disable=SC2086 # dirs is a list of patterns
  "$GOTOGETHER" invariants -spec "model/$m.qnt" -literate=false -invariant safe \
    -links name -require "$require" "$mode" docs/walkthroughs/README.md $dirs || fail=1
done
exit $fail
