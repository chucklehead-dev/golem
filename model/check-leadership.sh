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
# The actions each holding design may leave dead: those its switches turn off.
for v in "lease object, rollback none" "lease object, rollback doc" "lease object, rollback lead" "lease object, checkpoint attack, rollback doc" "lease object, checkpoint attack, rollback lead" "lease object, restore preserves" "lease object, lost replies" "hybrid, rollback none" "hybrid, rollback doc" "hybrid, rollback lead" "hybrid, checkpoint attack, rollback lead" "hybrid, checkpoint attack, rollback doc" "hybrid, lost replies"; do dead[$v]="takeDoc bump readLease rollbackDoc rollbackLead restore claimLost adopt publishLost forgeCheckpoint replayCheckpoint deleteCheckpoint"; done
for v in "lead object, rollback none" "lead object, rollback doc"; do dead[$v]="takeDoc claim claimLost adopt publishLost readLease rollbackDoc rollbackLead restore forgeCheckpoint replayCheckpoint deleteCheckpoint"; done
for v in "today (document)"; do dead[$v]="acquire claim claimLost adopt bump publishLost readLease rollbackDoc rollbackLead restore forgeCheckpoint replayCheckpoint deleteCheckpoint"; done
echo "== exhaustive: leadership.small.yaml, each design every run to its depth (holding designs 9 or 8, broken ones just past their counterexamples)"
matrix leadership
exit $fail
