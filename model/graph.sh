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

# The actions a design that breaks nothing may leave never enabled (a
# switch of the small instance turns them off): dead[variant], else
# matrix's own arguments. (gotogether#144 proposes a dead: list per
# variant, which would move these into the binding.)
declare -A dead=()

# matrix MODEL [ALLOWED_DEAD...]: every variant holds what it lists and
# breaks what it lists, each search complete to its depth; a variant that
# breaks nothing leaves no action dead but those allowed.
matrix() {
  local m=$1 rc=0 json; shift
  json=$("$GOTOGETHER" simulate -matrix -graph -json -binding "$m.small.yaml" -memory "$MEMORY" -process-memory "$PROCESS_MEMORY" -write-matrix "$m.md" 2>/dev/null) || rc=$?
  if [[ -z $json ]]; then
    echo "$m: the matrix did not run (exit $rc): run gotogether simulate -matrix -graph -binding $m.small.yaml to see why"; fail=1; return
  fi
  jq -r .table <<<"$json"
  local cuts
  cuts=$(jq -r '.variants[] | .report.graph as $g | select($g.cut == "states" or $g.cut == "memory")
    | "\(.name): the search'"'"'s \($g.cut) bound stopped it at depth \($g.completeDepth) of \($g.bound.depth): raise its states, or MEMORY"' <<<"$json")
  [[ -z $cuts ]] || { echo "$cuts"; fail=1; }
  # A failing check the table has no row for (a deadlock): named here.
  jq -r '.variants[] | .name as $v | .report.checks[] | select(.verdict == "fail" and .kind != "invariant")
    | "\($v): \(.name) fails"' <<<"$json"
  local v missing a allowed extra
  while IFS=$'\t' read -r v missing; do
    allowed=${dead[$v]-$*}
    extra=""
    for a in $missing; do [[ " $allowed " == *" $a "* ]] || extra+=" $a"; done
    [[ -z $extra ]] || { echo "$v: actions never enabled:$extra"; fail=1; }
  done < <(jq -r '[.rows[].cells[] | select(.expect == "allows") | .variant] as $broken
    | .variants[] | select(.name as $n | $broken | index($n) | not)
    | [.name, (.report.graph.stepActions.missing // [] | join(" "))] | @tsv' <<<"$json")
  [[ $rc == 0 ]] || { echo "$m: NOT as designed (exit $rc): see the FAIL cells"; fail=1; }
}
