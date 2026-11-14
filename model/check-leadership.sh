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
STATES=${STATES:-3000000}
D=${DEPTH:-10}
echo "== exhaustive: leadership.small.yaml, every run up to depth $D"
echo "-- the architect's design (lease object + fence and records in the document)"
for rb in none doc lead; do graph_holds leadership $D "ROLLBACK=\"$rb\"" takeDoc bump readLease rollbackDoc rollbackLead; done
graph_breaks leadership $D 'JOBS="none"' noStaleSync
graph_breaks leadership $D 'JOBS="readLease"' noStaleSync
graph_breaks leadership $D 'GUARD=false,ROLLBACK="doc"' floorNeverDrops
echo "-- the specialist's design (lease and records in a lead object + epoch copy in the document)"
for rb in none doc; do graph_holds leadership $D "DESIGN=\"object\",ROLLBACK=\"$rb\"" takeDoc claim readLease rollbackDoc rollbackLead; done
graph_breaks leadership $D 'DESIGN="object",ROLLBACK="lead"' epochsUnique
graph_breaks leadership $D 'DESIGN="object",JOBS_AFTER_CLAIM=false' noStaleSync
graph_breaks leadership $D 'DESIGN="object",GUARD=false,ROLLBACK="lead"' floorNeverDrops
echo "-- today (lease and records in the document)"
graph_holds leadership $D 'DESIGN="doc"' acquire claim bump readLease rollbackDoc rollbackLead
graph_breaks leadership $D 'DESIGN="doc",JOBS="none"' noStaleSync
graph_breaks leadership $D 'DESIGN="doc",GUARD=false,ROLLBACK="doc"' floorNeverDrops
graph_breaks leadership $D 'DESIGN="doc",ROLLBACK="doc"' epochsUnique
exit $fail
