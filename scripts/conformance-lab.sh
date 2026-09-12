#!/usr/bin/env bash
# Run the conformance suite against the Lima lab from the macOS host.
#   bash scripts/conformance-lab.sh [token] [record]
# The suite itself executes inside the control-plane VM (the repo is mounted
# there); agent suite targets kata-lab-0 over the user-v2 network.
set -u

# The lab token authenticates a root-privileged agent API, so it is never a
# literal in the repo: argument, then FELUCCA_TOKEN, then a file kept outside
# the tree. Whatever the lab stack was started with has to match.
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
TOKEN="${1:-${FELUCCA_TOKEN:-}}"
if [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ]; then
  TOKEN="$(tr -d " \t\r\n" < "$TOKEN_FILE")"
fi
if [ -z "$TOKEN" ]; then
  echo "ERROR: no lab token." >&2
  echo "  Pass it as \$1, export FELUCCA_TOKEN, or write it to $TOKEN_FILE" >&2
  echo "  (generate one with: openssl rand -hex 32)" >&2
  exit 1
fi
MODE="${2:-run}"
CP_VM=infra-saas-lab
# Agent-suite target: resolve kata-lab-0's CURRENT user-v2 address at run
# time — Lima's DHCP reassigns these across long stops (the workers' .1/.4
# swapped once), so a hardcoded default silently retargets the suite.
if [ -z "${AGENT_ADDR:-}" ]; then
  k0_ip=$(limactl shell kata-lab-0 -- bash -c 'ip -4 -br addr | grep 192.168.104 | awk "{print \$3}" | cut -d/ -f1' 2>/dev/null | tr -d '\r\n')
  AGENT_ADDR="${k0_ip:-192.168.104.1}:9090"
fi

REPO_DIR_HOST="$(cd "$(dirname "$0")/.." && pwd)"
# Lima mounts the macOS home at the same path inside the VM.
ENTRY=run.sh
[ "$MODE" = "record" ] && ENTRY=record.sh

limactl shell $CP_VM -- env \
  FELUCCA_API=http://127.0.0.1:8080 \
  FELUCCA_TOKEN="$TOKEN" \
  AGENT_API="http://$AGENT_ADDR" \
  SUITE=all \
  bash "$REPO_DIR_HOST/test/conformance/$ENTRY"
