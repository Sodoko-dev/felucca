#!/usr/bin/env bash
# Hearth conformance suite — the executable form of docs/API-V2.md.
# Implementation-agnostic: point it at any hearthd / hearth-agent (Zig, Go,
# Rust) over plain HTTP. Run from inside any lab VM or any host with curl+jq.
#
#   HEARTH_API=http://127.0.0.1:8080 \
#   HEARTH_TOKEN="$(cat ~/.config/hearth/lab-token)" \
#   AGENT_API=http://192.168.104.1:9090 SUITE=all \
#   bash test/conformance/run.sh
#
# The token is REQUIRED and must be the one the target was started with: both
# binaries refuse to start on an empty, placeholder, or short token, so there
# is no unauthenticated target to fall back to (API-V2 §6). With no
# HEARTH_TOKEN the suite reads $HEARTH_TOKEN_FILE (default
# ~/.config/hearth/lab-token) and aborts if that is empty too. The one
# tokenless path is HEARTH_INSECURE_NO_AUTH=1, against a hearthd started with
# --insecure-no-auth: it skips the credential cases, and the tenant-scoping
# cases will fail there because open mode has no tenants to scope. Only a
# token-authenticated target can pass the whole contract.
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

# Abort before the first request rather than reporting dozens of 401s that all
# mean "the suite had no usable credential".
if ! resolve_token; then
  exit 2
fi

# Cross-case state (set by create/fork cases, read by later ones); empty
# defaults keep `set -u` from aborting the suite when an early case fails.
#
# SC2034 ("appears unused") is a false positive here and cannot be otherwise:
# the readers are the case fragments, which run_suite discovers by glob and
# sources into this scope at runtime, so no static analysis can see the use.
# Do NOT "fix" this by exporting them — they are deliberately shell-local, and
# exporting would leak sandbox ids into every curl/jq child the suite spawns.
# shellcheck disable=SC2034
CF_SB_ID=""
# shellcheck disable=SC2034
CF_CHILD_ID=""
# shellcheck disable=SC2034
CF_VM1=""
# shellcheck disable=SC2034
CF_VM2=""

run_suite() {
  # Deliberately awkward names: cases are sourced into this scope, so simple
  # loop variables in a case (`for s in ...`) would clobber ours.
  local __cf_suite="$1" __cf_case
  for __cf_case in "$CONF_DIR/cases/$__cf_suite"/*.sh; do
    say "== $__cf_suite/$(basename "$__cf_case" .sh) =="
    # The case set is whatever is on disk under cases/<suite>/, by design — a
    # new case is a new file, with nothing to register here. That makes the
    # source path non-constant and SC1090 unavoidable; each fragment carries
    # its own `# shellcheck shell=bash` so it is still checked on its own.
    # shellcheck disable=SC1090
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
