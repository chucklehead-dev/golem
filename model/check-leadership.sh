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
STATES=${STATES:-3000000} # set before graph.sh, whose default is smaller
. ./graph.sh
# The breaking runs search one or two steps past their counterexample's
# length, not to a common depth: a breadth-first search to depth 10 stores
# about 7 GiB of graph, a whole CI runner's memory, to find a two-step run.
H=${HOLD_DEPTH:-9} # the designs holding: depth 10 needs over 4 GiB of stored graph
echo "== exhaustive: leadership.small.yaml, the designs to depth $H, each broken one just past its counterexample"
A=${ATTACK_DEPTH:-8} # with the checkpoint attacker, whose runs are longer
echo "-- the architect's design (lease object + fence and records in the document)"
for rb in none doc lead; do graph_holds leadership $H "ROLLBACK=\"$rb\"" takeDoc bump readLease rollbackDoc rollbackLead restore claimLost adopt publishLost forgeCheckpoint replayCheckpoint deleteCheckpoint; done
for c in 'CP_ATTACK=true,ROLLBACK="doc"' 'CP_ATTACK=true,ROLLBACK="lead"' 'RESTORE="preserve"' 'LOST_REPLY=true'; do
  graph_holds leadership $A "$c" takeDoc bump readLease rollbackDoc rollbackLead restore claimLost adopt publishLost forgeCheckpoint replayCheckpoint deleteCheckpoint
done
graph_breaks leadership 8 'JOBS="none"' noStaleSync
graph_breaks leadership 9 'JOBS="readLease"' noStaleSync
graph_breaks leadership 6 'GUARD=false,ROLLBACK="doc"' floorNeverDrops
graph_breaks leadership 4 'CP_ATTACK=true,VERIFY=false' noInflation
graph_breaks leadership $A 'ROLLBACK="both"' epochsUnique
echo "-- the specialist's first design (lease and records in a lead object + epoch copy in the document)"
for rb in none doc; do graph_holds leadership $H "DESIGN=\"object\",ROLLBACK=\"$rb\"" takeDoc claim claimLost adopt publishLost readLease rollbackDoc rollbackLead restore forgeCheckpoint replayCheckpoint deleteCheckpoint; done
graph_breaks leadership 5 'DESIGN="object",ROLLBACK="lead"' epochsUnique
graph_breaks leadership 8 'DESIGN="object",JOBS_AFTER_CLAIM=false' noStaleSync
echo "-- the hybrid (records in the lead object, belief after the claim in the document)"
for rb in none doc lead; do graph_holds leadership $H "DESIGN=\"hybrid\",ROLLBACK=\"$rb\"" takeDoc bump readLease rollbackDoc rollbackLead restore claimLost adopt publishLost forgeCheckpoint replayCheckpoint deleteCheckpoint; done
for c in 'DESIGN="hybrid",CP_ATTACK=true,ROLLBACK="lead"' 'DESIGN="hybrid",CP_ATTACK=true,ROLLBACK="doc"' 'DESIGN="hybrid",LOST_REPLY=true'; do
  graph_holds leadership $A "$c" takeDoc bump readLease rollbackDoc rollbackLead restore claimLost adopt publishLost forgeCheckpoint replayCheckpoint deleteCheckpoint
done
graph_breaks leadership 8 'DESIGN="hybrid",JOBS="none"' noStaleSync
graph_breaks leadership 6 'DESIGN="hybrid",GUARD=false,ROLLBACK="lead"' floorNeverDrops
graph_breaks leadership 4 'DESIGN="hybrid",CP_ATTACK=true,VERIFY=false' noInflation
graph_breaks leadership $A 'DESIGN="hybrid",ROLLBACK="both"' epochsUnique
echo "-- today (lease and records in the document)"
graph_holds leadership $H 'DESIGN="doc"' acquire claim claimLost adopt bump publishLost readLease rollbackDoc rollbackLead restore forgeCheckpoint replayCheckpoint deleteCheckpoint
graph_breaks leadership 8 'DESIGN="doc",JOBS="none"' noStaleSync
graph_breaks leadership 6 'DESIGN="doc",GUARD=false,ROLLBACK="doc"' floorNeverDrops
graph_breaks leadership 5 'DESIGN="doc",ROLLBACK="doc"' epochsUnique
exit $fail
