# Exhaustive checks of a model's small instance (<model>.small.yaml) with
# gotogether's state-graph search: every run up to a depth, not a sample.
# Sourced by the check scripts, which set fail=0 first. Results are read
# from the report's JSON (gotogether.report/2: simulate -json), with jq.
GOTOGETHER=${GOTOGETHER:-gotogether}
STATES=${STATES:-300000}
# MiB for the stored graph (gotogether stops at 256 by default). It bounds
# the graph alone, not the process: a search at 6000 has peaked at 7.1 GiB
# resident, so keep it well under the machine's memory.
MEMORY=${MEMORY:-6000}

# graph_run MODEL DEPTH [CONST]: the report's JSON in $graph_json, the exit
# status in $graph_rc (0 pass, 1 fail, 2 incomplete).
graph_run() {
  # The scripts run under set -e: keep a failing search from ending them.
  graph_rc=0
  graph_json=$("$GOTOGETHER" simulate -json -binding "$1.small.yaml" -graph -depth "$2" -states "$STATES" -memory "$MEMORY" ${3:+-const "$3"} 2>/dev/null) || graph_rc=$?
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
    # The counterexample's replay is in the report's prose notes only (no
    # JSON field carries it): the line after this invariant's FAIL.
    local r; r=$(graph_field '.notes[]?' | awk -v inv="$4" '$1=="FAIL" && $2==inv":" {f=1} f && /^    replay:/ {print; exit}' || true)
    r=${r#*run r = }; r=${r#init.}; r=${r#all \{*\}.}
    echo "$3 breaks $4 within $2 steps: $(sed -E 's/^.*\}\)\.then/then/' <<<"$r" | cut -c1-150)"
  else
    echo "$3: does not break $4 within $2 steps (exit $graph_rc)"; fail=1
  fi
}
