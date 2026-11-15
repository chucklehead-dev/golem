# Exhaustive checks of a model's small instance (<model>.small.yaml) with
# gotogether's design matrix (simulate -matrix -graph): every run of each
# design to its depth, not a sample. Sourced by the check scripts, which
# set fail=0 first.
#
# The binding's simulate.variants are the designs, each with what it must
# hold and what it must break (exactly: an unlisted break fails), and its
# own search bound where it needs one. `matrix MODEL` searches each and
# writes the table into MODEL.md (CI fails if the committed table
# differs).

GOTOGETHER=${GOTOGETHER:-gotogether}
# MiB for the stored graph (gotogether stops at 256 by default): an
# estimate of the graph alone.
MEMORY=${MEMORY:-6000}
# MiB for the whole process (-process-memory), set from the machine's
# memory: a search past it ends with a bounded result (cut: memory), not a
# killed runner. 5500 leaves a 7 GB runner room for quint and the rest.
PROCESS_MEMORY=${PROCESS_MEMORY:-5500}


# matrix MODEL: every variant holds what it lists, breaks what it lists,
# and leaves dead exactly the actions it lists (simulate.dead: a design
# that breaks nothing names those its switches turn off), each search
# complete to its depth. A failing check outside the table (a deadlock, a
# dead action) is named under it.
matrix() {
  local m=$1 rc=0 json
  json=$("$GOTOGETHER" simulate -matrix -graph -json -binding "$m.small.yaml" -memory "$MEMORY" -process-memory "$PROCESS_MEMORY" -write-matrix "$m.md" 2>/dev/null) || rc=$?
  if [[ -z $json ]]; then
    echo "$m: the matrix did not run (exit $rc): run gotogether simulate -matrix -graph -binding $m.small.yaml to see why"; fail=1; return
  fi
  jq -r .table <<<"$json"
  local cuts
  cuts=$(jq -r '.variants[] | .report.graph as $g | select($g.cut == "states" or $g.cut == "memory")
    | "\(.name): the search'"'"'s \($g.cut) bound stopped it at depth \($g.completeDepth) of \($g.bound.depth): raise its states, or MEMORY"' <<<"$json")
  [[ -z $cuts ]] || { echo "$cuts"; fail=1; }
  [[ $rc == 0 ]] || { echo "$m: NOT as designed (exit $rc): see FAIL above (cells, or the checks outside the table)"; fail=1; }
}
