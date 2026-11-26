#!/usr/bin/env bash
# Reproduction of FLU-15 — runs ON a lab worker:
#   bash test/lab/run-on-worker.sh repro-flu15 [worker-vm]
#
# exec is dead after a pause and resume that has no snapshot in between, with
# or without an agent restart, while the guest itself runs (its Firecracker
# says Running and it answers ping). The first half pauses a guest behind the
# agent's back and freezes its Firecracker across an agent restart — the case
# in which the agent has to come back to a Firecracker that could not answer
# at start-up; the second half is a plain pause and resume through the API.
#
# Shows what happens, asserts nothing about the bug.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
gping() { local ip; ip="$(vm_ip "$1")"; ping -c2 -W1 "$ip" >/dev/null 2>&1 && echo yes || echo no; }
cleanup_prefix fz-
eq "create fz-1" "$(vm_create fz-1 512)" 201; wait_exec fz-1 150 || bad "fz-1 never became exec-ready"
P=$(vm_pid fz-1); D1="$(vm_dir fz-1)"
say "  before: vmm=$(vmm_state fz-1) ping=$(gping fz-1) exec=$(exec_ok fz-1 && echo works || echo FAILS)"
sudo curl -s -m 5 -o /dev/null --unix-socket "$D1/fc.sock" -X PATCH -H 'Content-Type: application/json' -d '{"state":"Paused"}' http://localhost/vm
sudo kill -STOP "$P"
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 2
say "  start-up: $(journal_since "$T0" | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g' | grep -o 'adopted=.*' | cut -c1-120)"
sudo kill -CONT "$P"
i=0; until journal_since "$T0" | grep -q 'answers now' || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
say "  settle line (after ${i}s): $(journal_since "$T0" | grep 'answers now' | tail -1 | sed 's/\x1b\[[0-9;]*m//g' | grep -o 'vm=.*' | cut -c1-140)"
for t in 1 5 15 30; do
  sleep "$([ $t = 1 ] && echo 1 || echo $((t>5 ? t-5 : 4)))"
  say "  +${t}s after it answered: vmm=$(vmm_state fz-1) ping=$(gping fz-1) exec=$(exec_ok fz-1 && echo works || echo FAILS)"
done
say "  a plain pause+resume through the agent API, no freeze, for comparison:"
eq "create fz-2" "$(vm_create fz-2 512)" 201; wait_exec fz-2 150 || bad "fz-2 never became exec-ready"
eq "pause fz-2" "$(vm_act fz-2 pause)" 200; sleep 12; eq "resume fz-2" "$(vm_act fz-2 resume)" 200; sleep 3
say "  fz-2 after 12 s paused: vmm=$(vmm_state fz-2) ping=$(gping fz-2) exec=$(exec_ok fz-2 && echo works || echo FAILS)"
eq "pause fz-2 again" "$(vm_act fz-2 pause)" 200; sleep 1; eq "resume fz-2" "$(vm_act fz-2 resume)" 200; sleep 3
say "  fz-2 after 1 s paused:  vmm=$(vmm_state fz-2) ping=$(gping fz-2) exec=$(exec_ok fz-2 && echo works || echo FAILS)"
cleanup_prefix fz-
summary
