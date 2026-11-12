#!/usr/bin/env bash
# Runs every fuzz target briefly (FUZZTIME, default 10s): enough to catch a
# regression on the corpus and a little new ground, not a fuzzing campaign.
set -euo pipefail
cd "$(dirname "$0")/.."
FUZZTIME=${FUZZTIME:-10s}
for pkg in $(go list ./...); do
  for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
    echo "== $pkg $target"
    go test -run='^$' -fuzz="^${target}\$" -fuzztime="$FUZZTIME" "$pkg"
  done
done
