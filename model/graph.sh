# Exhaustive checks of a model's small instance (<model>.small.yaml) with
# gotogether's state-graph search: every run up to a depth, not a sample.
# Sourced by the check scripts, which set fail=0 first.
GOTOGETHER=${GOTOGETHER:-gotogether}
STATES=${STATES:-300000}

# graph_run MODEL DEPTH [CONST]: the report, its exit status in $graph_rc.
graph_run() {
  graph_out=$("$GOTOGETHER" simulate -binding "$1.small.yaml" -graph -depth "$2" -states "$STATES" ${3:+-const "$3"} 2>&1)
  graph_rc=$?
}

# graph_holds MODEL DEPTH CONST [ALLOWED_DEAD...]: the instance holds every
# invariant in every run of up to DEPTH steps, and no action is dead except
# those a switch turns off.
graph_holds() {
  local m=$1 d=$2 c=$3; shift 3
  graph_run "$m" "$d" "$c"
  local dead; dead=$(grep -E '^  never' <<<"$graph_out" | sed -E 's/^  never +//; s/: .*//; s/,//g')
  local extra=""
  for a in $dead; do [[ " $* " == *" $a "* ]] || extra+=" $a"; done
  local scope; scope=$(grep -E '^  graph' <<<"$graph_out" | sed -E 's/^  graph +//; s/: .*//')
  if [[ $scope == *"state bound"* ]]; then
    echo "${c:-design}: the search hit its state bound before depth $d: lower the depth or raise STATES"; fail=1
  elif [[ $graph_rc == 0 && -z $extra ]]; then
    echo "${c:-design} holds in every run of up to $d steps ($(grep -m1 -oE '[0-9]+ states' <<<"$graph_out"))"
  else
    echo "${c:-design}: NOT held (exit $graph_rc${extra:+; dead:$extra})"; grep -E '^  (FAIL|never)' <<<"$graph_out" | cut -c1-200; fail=1
  fi
}

# graph_breaks MODEL DEPTH CONST INVARIANT: the broken design breaks it,
# found by the search (and so with a shortest counterexample).
graph_breaks() {
  graph_run "$1" "$2" "$3"
  if [[ $graph_rc == 1 ]] && grep -qE "^  FAIL +$4:" <<<"$graph_out"; then
    echo "$3 breaks $4 within $2 steps: $(grep -m1 -E '^    replay:' <<<"$graph_out" | sed -E 's/^ +replay: run r = //' | cut -c1-140)"
  else
    echo "$3: does not break $4 within $2 steps (exit $graph_rc)"; fail=1
  fi
}
