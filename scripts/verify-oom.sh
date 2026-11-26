#!/usr/bin/env bash
# Lab gate for FLU-2 / ADR-0011: one OOM kill takes one sandbox.
# Run from the macOS host:
#
#   bash scripts/verify-oom.sh [worker-vm] [token]      # default worker: kata-lab-0
#
# Makes the kernel OOM-kill one Firecracker inside the agent unit's cgroup and
# checks that only that sandbox is lost: the agent keeps its pid, the unit
# stays active, the other sandboxes keep their Firecracker and their uptime.
# Against a unit without OOMPolicy=continue the same run fails in the way the
# audit found: the unit is stopped and every sandbox on the worker dies.
#
# The cases are in test/lab/oom.sh (it runs inside the worker VM). The memory
# limit it sets is temporary (`systemctl set-property --runtime`) and is
# lifted again when the gate exits. Takes about 3 minutes.
exec bash "$(cd "$(dirname "$0")/.." && pwd)/test/lab/run-on-worker.sh" oom "${1:-}" "${2:-}"
