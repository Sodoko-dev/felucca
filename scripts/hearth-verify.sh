#!/usr/bin/env bash
# hearth verify (v4 P5.3) — run the Hearth conformance suite against ANY
# deployment. The suite is the contract (docs/API-V2.md in executable form);
# self-hosters run this against their own fleet as the trust artifact.
#
#   bash scripts/hearth-verify.sh https://api.example.com <admin-token>
#   AGENT_API=http://worker:9090 bash scripts/hearth-verify.sh ...   # + agent suite
#
# Needs: bash, curl, jq. Exit 0 iff every check passes.
set -u

ENDPOINT="${1:-}"
TOKEN="${2:-${HEARTH_TOKEN:-}}"
if [ -z "$ENDPOINT" ]; then
  echo "usage: $0 <hearthd-endpoint> [admin-token]" >&2
  echo "  e.g. $0 http://127.0.0.1:8080 hearth-lab-token" >&2
  exit 2
fi

CONF_DIR="$(cd "$(dirname "$0")/../test/conformance" && pwd)"

# hearthd-only by default: the agent API is usually not reachable from
# outside the fleet. Set AGENT_API to include the agent suite.
SUITE="${SUITE:-hearthd}"
if [ -n "${AGENT_API:-}" ] && [ "$SUITE" = "hearthd" ]; then
  SUITE=all
fi

HEARTH_API="$ENDPOINT" HEARTH_TOKEN="$TOKEN" AGENT_API="${AGENT_API:-}" \
  SUITE="$SUITE" bash "$CONF_DIR/run.sh"
