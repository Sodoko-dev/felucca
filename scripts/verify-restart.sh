#!/usr/bin/env bash
# Lab gate for FLU-1 / ADR-0011: sandboxes outlive the agent.
# Run from the macOS host:
#
#   bash scripts/verify-restart.sh [worker-vm] [token]      # default worker: kata-lab-0
#
# Restarts, kills, stops and mis-configures felucca-agent on a lab worker and
# checks that every sandbox keeps its Firecracker, its state, its uptime and
# its network, and that whatever the agent was in the middle of is put right
# by the next start. The cases are in test/lab/restart.sh (it runs inside the
# worker VM, which is where systemd and /proc are); this wrapper ships it
# there. Takes about 20 minutes.
#
# It is rough with the worker on purpose. Do not point it at a worker that
# holds sandboxes you care about.
exec bash "$(cd "$(dirname "$0")/.." && pwd)/test/lab/run-on-worker.sh" restart "${1:-}" "${2:-}"
