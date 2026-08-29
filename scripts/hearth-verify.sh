#!/usr/bin/env bash
# hearth verify (v4 P5.3) — run the Hearth conformance suite against ANY
# deployment. The suite is the contract (docs/API-V2.md in executable form);
# self-hosters run this against their own fleet as the trust artifact.
#
#   bash scripts/hearth-verify.sh https://api.example.com <admin-token>
#   AGENT_API=http://worker:9090 bash scripts/hearth-verify.sh ...   # + agent suite
#
# The admin token is REQUIRED and must be the one the deployment was started
# with: hearthd and hearth-agent both refuse to start on an empty, placeholder,
# or short token, so there is no unauthenticated deployment to verify. Pass it
# as $2, export HEARTH_TOKEN, or keep it in $HEARTH_TOKEN_FILE (default
# ~/.config/hearth/lab-token). A loopback lab running
# `hearthd --insecure-no-auth` is verified with HEARTH_INSECURE_NO_AUTH=1.
#
# Needs: bash, curl, jq. Exit 0 iff every check passes.
set -u

ENDPOINT="${1:-}"
if [ -z "$ENDPOINT" ]; then
  echo "usage: $0 <hearthd-endpoint> [admin-token]" >&2
  echo "  e.g. $0 https://api.example.com \"\$(cat ~/.config/hearth/lab-token)\"" >&2
  echo "  the token may also come from HEARTH_TOKEN or HEARTH_TOKEN_FILE" >&2
  exit 2
fi

CONF_DIR="$(cd "$(dirname "$0")/../test/conformance" && pwd)"

# One implementation of the credential rules, shared with the suite itself:
# resolve_token fills in the file fallback and refuses a value the deployment
# would refuse (empty, shipped placeholder, under 32 chars). Failing here — in
# the tool the operator actually invoked — beats reporting it as a wall of 401s.
. "$CONF_DIR/lib.sh"
HEARTH_TOKEN="${2:-${HEARTH_TOKEN:-}}"
if ! resolve_token; then
  exit 2
fi

# hearthd-only by default: the agent API is usually not reachable from
# outside the fleet. Set AGENT_API to include the agent suite.
SUITE="${SUITE:-hearthd}"
if [ -n "${AGENT_API:-}" ] && [ "$SUITE" = "hearthd" ]; then
  SUITE=all
fi

HEARTH_API="$ENDPOINT" HEARTH_TOKEN="$HEARTH_TOKEN" AGENT_API="${AGENT_API:-}" \
  SUITE="$SUITE" bash "$CONF_DIR/run.sh"
