# Exhaustive checks of a model's small instance (<model>.small.yaml) with
# gotogether's state-graph search: every run up to a depth, not a sample.
# Sourced by the check scripts, which set fail=0 first.
#
# matrix MODEL is the check: the binding's simulate.variants are the
# designs, each with what it must hold and what it must break, and
# `simulate -matrix` searches each and writes the table into MODEL.md
# (CI fails if the committed table differs). graph_holds and graph_breaks,
# one search per call, remain for leadership, whose variants need depths
# of their own (gotogether#144: a variant's search.depth is not applied yet).
GOTOGETHER=${GOTOGETHER:-gotogether}
STATES=${STATES:-300000}
# MiB for the stored graph (gotogether stops at 256 by default): an
# estimate of the graph alone.
MEMORY=${MEMORY:-6000}
# MiB for the whole process (-process-memory), set from the machine's
# memory: a search past it ends with a bounded result (cut: memory), not
# a killed runner. 5500 leaves a 7 GB runner room for quint and the rest.
PROCESS_MEMORY=${PROCESS_MEMORY:-5500}
# The matrix runs its variants in one process, and the process bound reads
# what the runtime has mapped: memory an earlier variant freed but the
# runtime kept would cut a later one (gotogether#144). A soft limit below
# the bound makes the runtime return it.
export GOMEMLIMIT=${GOMEMLIMIT:-$((PROCESS_MEMORY * 9 / 10))MiB}

# matrix MODEL [ALLOWED_DEAD...]: every variant holds what it lists and
# breaks what it lists (exactly: an unlisted break fails), each search
# complete to its depth; a variant that breaks nothing leaves no action
# dead except those named (a switch of the small instance turns them off).
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
  local v dead a extra
  while IFS=$'\t' read -r v dead; do
    extra=""
    for a in $dead; do [[ " $* " == *" $a "* ]] || extra+=" $a"; done
    [[ -z $extra ]] || { echo "$v: actions never enabled:$extra"; fail=1; }
  done < <(jq -r '[.rows[].cells[] | select(.expect == "allows") | .variant] as $broken
    | .variants[] | select(.name as $n | $broken | index($n) | not)
    | [.name, (.report.graph.stepActions.missing // [] | join(" "))] | @tsv' <<<"$json")
  [[ $rc == 0 ]] || { echo "$m: NOT as designed (exit $rc): see the FAIL cells"; fail=1; }
}

# graph_run MODEL DEPTH [CONST]: the report's JSON in $graph_json, the exit
# status in $graph_rc (0 pass, 1 fail, 2 incomplete).
graph_run() {
  # The scripts run under set -e: keep a failing search from ending them.
  graph_rc=0
  graph_json=$("$GOTOGETHER" simulate -json -binding "$1.small.yaml" -graph -depth "$2" -states "$STATES" -memory "$MEMORY" -process-memory "$PROCESS_MEMORY" ${3:+-const "$3"} 2>/dev/null) || graph_rc=$?
}

# graph_field JQ: a field of the last report.
graph_field() { jq -r "$1" <<<"$graph_json"; }

# graph_holds MODEL DEPTH CONST [ALLOWED_DEAD...]: the instance holds every
# invariant in every run of up to DEPTH steps, and no action is dead except
# those a switch turns off.
graph_holds() {
  local m=$1 d=$2 c=$3; shift 3
  graph_run "$m" "$d" "$c"
  local cut extra="" a
  cut=$(graph_field '.graph.cut // ""')
  for a in $(graph_field '.graph.stepActions.missing[]?'); do
    [[ " $* " == *" $a "* ]] || extra+=" $a"
  done
  if [[ $cut == states || $cut == memory ]]; then
    echo "${c:-design}: the search's $cut bound stopped it before depth $d (complete to depth $(graph_field .graph.completeDepth)): lower the depth, or raise STATES or MEMORY"; fail=1
  elif [[ $graph_rc == 0 && -z $extra ]]; then
    echo "${c:-design} holds in every run of up to $d steps ($(graph_field .graph.states) states)"
  else
    echo "${c:-design}: NOT held (exit $graph_rc${extra:+; dead:$extra})"
    graph_field '.checks[] | select(.verdict == "fail") | "  FAIL " + .name' | cut -c1-200; fail=1
  fi
}

# graph_breaks MODEL DEPTH CONST INVARIANT: the broken design breaks it,
# found by the search (and so with a shortest counterexample).
graph_breaks() {
  graph_run "$1" "$2" "$3"
  local failed
  failed=$(jq -r --arg inv "$4" '.checks[] | select(.name == $inv and .verdict == "fail") | .name' <<<"$graph_json" || true)
  if [[ $graph_rc == 1 && -n $failed ]]; then
    # The shortest counterexample's replay, init.then(...).then(...).
    local r; r=$(jq -r --arg inv "$4" '.checks[] | select(.name == $inv) | .counterexample.replay // ""' <<<"$graph_json")
    r=${r#*run r = }; r=${r#init.}; r=${r#all \{*\}.}
    echo "$3 breaks $4 within $2 steps: $(sed -E 's/^.*\}\)\.then/then/' <<<"$r" | cut -c1-150)"
  else
    echo "$3: does not break $4 within $2 steps (exit $graph_rc)"; fail=1
  fi
}
