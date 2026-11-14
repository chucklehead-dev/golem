# Exhaustive checks of a model's small instance (<model>.small.yaml) with
# gotogether's state-graph search: every run up to a depth, not a sample.
# Sourced by the check scripts, which set fail=0 first.
GOTOGETHER=${GOTOGETHER:-gotogether}
STATES=${STATES:-300000}
MEMORY=${MEMORY:-6000} # MiB for the stored graph (gotogether stops at 256 by default); above the machine's RAM the search is killed

# graph_run MODEL DEPTH [CONST]: the report, its exit status in $graph_rc.
graph_run() {
  # The scripts run under set -e: keep a failing search from ending them.
  graph_rc=0
  graph_out=$("$GOTOGETHER" simulate -binding "$1.small.yaml" -graph -depth "$2" -states "$STATES" -memory "$MEMORY" ${3:+-const "$3"} 2>&1) || graph_rc=$?
}

# graph_holds MODEL DEPTH CONST [ALLOWED_DEAD...]: the instance holds every
# invariant in every run of up to DEPTH steps, and no action is dead except
# those a switch turns off.
graph_holds() {
  local m=$1 d=$2 c=$3; shift 3
  graph_run "$m" "$d" "$c"
  local dead; dead=$( { grep -E '^  never' <<<"$graph_out" || true; } | sed -E 's/^  never +//; s/: .*//; s/,//g')
  local extra=""
  for a in $dead; do [[ " $* " == *" $a "* ]] || extra+=" $a"; done
  local scope; scope=$( { grep -E '^  graph' <<<"$graph_out" || true; } | sed -E 's/^  graph +//; s/: .*//')
  if [[ $scope == *"state bound"* || $scope == *"memory bound"* || $graph_out == *"memory bound cut"* ]]; then
    local done; done=$( { grep -oE 'complete to depth [0-9]+' <<<"$graph_out" || true; } | head -1)
    echo "${c:-design}: the search's bound stopped it before depth $d (${done:-no depth complete}): lower the depth, or raise STATES or MEMORY"; fail=1
  elif [[ $graph_rc == 0 && -z $extra ]]; then
    echo "${c:-design} holds in every run of up to $d steps ($( { grep -E '^  graph' <<<"$graph_out" || true; } | { grep -oE '[0-9]+ states' || true; } | head -1))"
  else
    echo "${c:-design}: NOT held (exit $graph_rc${extra:+; dead:$extra})"; { grep -E '^  (FAIL|never)' <<<"$graph_out" || true; } | cut -c1-200; fail=1
  fi
}

# graph_breaks MODEL DEPTH CONST INVARIANT: the broken design breaks it,
# found by the search (and so with a shortest counterexample).
graph_breaks() {
  graph_run "$1" "$2" "$3"
  if [[ $graph_rc == 1 ]] && { grep -qE "^  FAIL +$4:" <<<"$graph_out" || false; }; then
    # the replay of this invariant's counterexample, from init's first step on
    local r; r=$(awk -v inv="$4" '$1=="FAIL" && $2==inv":" {f=1} f && /^    replay:/ {print; exit}' <<<"$graph_out" || true)
    r=${r#*run r = }; r=${r#init.}; r=${r#all \{*\}.}
    echo "$3 breaks $4 within $2 steps: $(sed -E 's/^.*\}\)\.then/then/' <<<"$r" | cut -c1-150)"
  else
    echo "$3: does not break $4 within $2 steps (exit $graph_rc)"; fail=1
  fi
}
