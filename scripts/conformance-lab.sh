#!/usr/bin/env bash
# Run the conformance suite against the Lima lab from the macOS host.
#   bash scripts/conformance-lab.sh [token] [record]
# The suite itself executes inside the control-plane VM (the repo is mounted
# there); agent suite targets kata-lab-0 over the user-v2 network.
set -u

TOKEN="${1:-hearth-lab-token}"
MODE="${2:-run}"
CP_VM=infra-saas-lab
AGENT_ADDR="${AGENT_ADDR:-192.168.104.1:9090}"

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
