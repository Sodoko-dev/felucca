#!/usr/bin/env bash
# Reproduction of FLU-70 (and the full-disk half of FLU-11) — runs ON a lab worker:
#   bash test/lab/run-on-worker.sh repro-flu70 [worker-vm]
#
# Before the fix, a sleep that failed part-way (the disk fills up) left the
# guest paused under a record that said running, and left snapshot files that
# looked complete; when that sandbox's Firecracker later died, the agent listed
# it as sleeping and a wake restored it from the half-written memory file.
#
# Since FLU-70 and FLU-4 (F2, S1) it prints: after the failed sleep the record
# says `running`, the Firecracker says `Running`, and for a guest that never
# slept no vmstate.bin or mem.bin is listed; the sandbox whose Firecracker is
# killed while the agent is stopped comes back `stopped`, and its wake answers
# 409 with "state":"stopped". test/lab/snapshot.sh (S5, S3) asserts the same.
#
# This script only shows what happens; it asserts nothing about the bug. It
# fills the data disk for a few seconds.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
trap 'sudo rm -f "$DATA_DIR/FILL"' EXIT
cleanup_prefix sf-
eq "create sf-1" "$(vm_create sf-1 256)" 201; wait_exec sf-1 150 || bad "sf-1 never became exec-ready"
D1="$(vm_dir sf-1)"
say "  before: files: $(sudo ls "$D1" | tr '\n' ' ')"
FREE_KB=$(df --output=avail "$DATA_DIR" | tail -1 | tr -d ' ')
sudo fallocate -l $(( (FREE_KB - 20000) * 1024 )) "$DATA_DIR/FILL" && say "  disk filled ($(df -h --output=avail "$DATA_DIR" | tail -1 | tr -d ' ') left)"
say "  sleep on a full disk: $(api -m 60 -X POST "$A/v1/vms/sf-1/sleep" | cut -c1-150)"
say "  after: record=$(vm_state sf-1) vmm=$(vmm_state sf-1) exec=$(exec_ok sf-1 && echo works || echo FAILS)"
say "  files: $(sudo sh -c "cd '$D1' && stat -c '%n=%s' *" | tr '\n' ' ')"
sudo rm -f "$DATA_DIR/FILL"
say "  the caller retries the sleep with the disk free again: $(api -m 60 -X POST "$A/v1/vms/sf-1/sleep" | cut -c1-120) -> record=$(vm_state sf-1)"
eq "wake sf-1" "$(vm_act sf-1 wake)" 200
wait_exec sf-1 60 && ok "sf-1 answers after sleep + wake" || bad "sf-1 does not answer"
# The other path: nobody retries, and the Firecracker dies later (power-off, host reboot, OOM).
# (Since FLU-4: that sandbox comes back `stopped` and its wake is refused with 409.)
eq "create sf-2" "$(vm_create sf-2 256)" 201; wait_exec sf-2 150 || bad "sf-2 never became exec-ready"
D2="$(vm_dir sf-2)"; P2="$(vm_pid sf-2)"
FREE_KB=$(df --output=avail "$DATA_DIR" | tail -1 | tr -d ' ')
sudo fallocate -l $(( (FREE_KB - 20000) * 1024 )) "$DATA_DIR/FILL"
api -m 60 -o /dev/null -X POST "$A/v1/vms/sf-2/sleep"
sudo rm -f "$DATA_DIR/FILL"
say "  sf-2 after a failed sleep, disk freed: record=$(vm_state sf-2) vmm=$(vmm_state sf-2) files: $(sudo sh -c "cd '$D2' && stat -c '%n=%s' *" | tr '\n' ' ')"
sudo systemctl stop $UNIT; sudo kill -9 "$P2"; sleep 1; sudo systemctl start $UNIT; wait_up; sleep 2
say "  its Firecracker died while the agent was down; after start-up the record says: $(vm_state sf-2)"
say "  wake of that record: $(api -m 60 -X POST "$A/v1/vms/sf-2/wake" | cut -c1-160) -> $(vm_state sf-2)"
cleanup_prefix sf-
summary
