#!/usr/bin/env bash
# Hearth conformance suite — the executable form of docs/API-V2.md.
# Implementation-agnostic: point it at any hearthd / hearth-agent (Zig, Go,
# Rust) over plain HTTP. Run from inside any lab VM or any host with curl+jq.
#
#   HEARTH_API=http://127.0.0.1:8080 HEARTH_TOKEN=hearth-lab-token \
#   AGENT_API=http://192.168.104.1:9090 SUITE=all \
#   bash test/conformance/run.sh
#
# SUITE=hearthd|agent|all (default all). RECORD=1 re-records goldens instead
# of comparing (use record.sh). Optional for local-only cases:
#   CANDIDATE_HEARTHD=/path/to/binary  (case 15, state adoption)
#   AGENT_DATA_DIR=/srv/ignis          (agent case 09, meta.json shape)
set -u

CONF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$CONF_DIR/lib.sh"

HEARTH_API="${HEARTH_API:-http://127.0.0.1:8080}"
HEARTH_TOKEN="${HEARTH_TOKEN:-}"
AGENT_API="${AGENT_API:-}"
SUITE="${SUITE:-all}"

# Cross-case state (set by create/fork cases, read by later ones); empty
# defaults keep `set -u` from aborting the suite when an early case fails.
CF_SB_ID=""
CF_CHILD_ID=""
CF_VM1=""
CF_VM2=""

run_suite() {
  # Deliberately awkward names: cases are sourced into this scope, so simple
  # loop variables in a case (`for s in ...`) would clobber ours.
  local __cf_suite="$1" __cf_case
  for __cf_case in "$CONF_DIR/cases/$__cf_suite"/*.sh; do
    say "== $__cf_suite/$(basename "$__cf_case" .sh) =="
    . "$__cf_case"
  done
}

if [ "$SUITE" = "hearthd" ] || [ "$SUITE" = "all" ]; then
  run_suite hearthd
fi
if [ "$SUITE" = "agent" ] || [ "$SUITE" = "all" ]; then
  if [ -n "$AGENT_API" ]; then
    run_suite agent
  else
    say "== agent suite: AGENT_API not set, skipping =="
  fi
fi

say ""
say "RESULT: $PASS passed, $FAIL failed, $SKIP skipped"
[ "$FAIL" = "0" ]
