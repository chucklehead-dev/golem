#!/usr/bin/env bash
# Checks the leadership model exhaustively (leadership.md §6): each design
# with its mechanisms holds under each rollback, and each mechanism is
# shown necessary by the run that breaks without it.
set -euo pipefail
cd "$(dirname "$0")"
QUINT=${QUINT:-quint}
"$QUINT" typecheck leadership.qnt
fail=0
if ! command -v "${GOTOGETHER:-gotogether}" >/dev/null; then
  echo "  skipped: gotogether is not installed"; exit 0
fi
. ./graph.sh
echo "== exhaustive: leadership.small.yaml, each design every run to its depth (holding designs 9 or 8, broken ones just past their counterexamples)"
matrix leadership
exit $fail
