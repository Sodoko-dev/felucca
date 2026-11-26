#!/usr/bin/env bash
# Break scenarios for a worker (FLU-1 / ADR-0011). Run from the macOS host:
#
#   bash scripts/verify-break.sh [worker-vm] [token] [sections]   # default worker: kata-lab-0
#
# Looks for trouble around agent restarts: tenant rules and exposes across a
# restart and a kill -9, an agent killed every 3 s while sandboxes are being
# created, fifteen sandboxes and double restarts, a full disk, every Felucca
# nft table deleted, the documented power-off, a binary rolled back under the
# new unit, a record that cannot be written, an agent that dies the instant a
# create is committed. The cases are in test/lab/break.sh (sections B1-B9; pass
# e.g. "B4 B8" as the third argument to run some of them). About 12 minutes.
#
# The rollback section (B7) needs an agent binary from before ADR-0011 on the
# worker: at /usr/local/bin/felucca-agent.pre-w0, or wherever PREV_AGENT_BIN
# points. Without one it is skipped and the result says so.
#
# Rough with the worker on purpose. Do not point it at a worker that holds
# sandboxes you care about.
exec bash "$(cd "$(dirname "$0")/.." && pwd)/test/lab/run-on-worker.sh" break "${1:-}" "${2:-}" "${3:-all}"
