#!/usr/bin/env bash
# Run the conformance suite against the Lima lab from the macOS host.
#   bash scripts/conformance-lab.sh [token] [record]
# The suite itself executes inside the control-plane VM (the repo is mounted
# there); agent suite targets kata-lab-0 over the user-v2 network.
set -u

TOKEN="${1:-hearth-lab-token}"
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
  HEARTH_API=http://127.0.0.1:8080 \
  HEARTH_TOKEN="$TOKEN" \
  AGENT_API="http://$AGENT_ADDR" \
  SUITE=all \
  bash "$REPO_DIR_HOST/test/conformance/$ENTRY"
