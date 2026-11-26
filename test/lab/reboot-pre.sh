#!/usr/bin/env bash
# Before a worker reboot — runs ON a lab worker (see scripts/verify-reboot.sh).
#
# Leaves one running sandbox with a file on its disk, one sleeping sandbox and
# a second running one, and starts a probe INSIDE the first guest that logs to
# the guest's own disk, about every 0.7 s until the guest dies:
#   peer=    can it reach the other guest (neither has a tenant, so it must not)
#   hostapi= can it open the host's agent port through the bridge gateway
# test/lab/reboot-post.sh reads that log back after the reboot.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
cleanup_prefix rb-
eq "create rb-run" "$(vm_create rb-run 256)" 201
eq "create rb-sleep" "$(vm_create rb-sleep 256)" 201
eq "create rb-peer" "$(vm_create rb-peer 256)" 201
for v in rb-run rb-sleep rb-peer; do wait_exec $v 150 || bad "$v never became exec-ready"; done
guest rb-run 'echo before-reboot > /root/marker; sync' >/dev/null
eq "sleep rb-sleep" "$(vm_act rb-sleep sleep)" 200
PEER="$(vm_ip rb-peer)"; GW="$(ip -4 -o addr show felucca0 | awk '{print $4}' | cut -d/ -f1)"
# bash, not sh: the guest's sh is dash, which has no /dev/tcp.
guest rb-run "rm -f /root/probe.log; nohup bash -c 'while :; do a=no; b=no; timeout 0.3 ping -c1 $PEER >/dev/null 2>&1 && a=YES; timeout 0.3 bash -c \"echo > /dev/tcp/$GW/9090\" 2>/dev/null && b=YES; echo \"\$(date +%s.%N | cut -c1-14) peer=\$a hostapi=\$b\" >> /root/probe.log; sync; sleep 0.1; done' >/dev/null 2>&1 &" >/dev/null
sleep 4
LAST="$(guest rb-run 'tail -n 1 /root/probe.log' | tr -d '\n')"
case "$LAST" in *"peer=no hostapi=no"*) ok "the probe runs, and the fences hold now: $LAST";; *) bad "probe not running or a fence is open: '$LAST'";; esac
# The probe must be able to say YES at all.
eq "control: the guest reaches the bridge gateway by ping" "$(guest rb-run "ping -c1 -W1 $GW >/dev/null 2>&1 && echo YES || echo no" | tr -d '\n')" YES
eq "control: from the host itself the agent port is open" "$(timeout 1 bash -c "echo > /dev/tcp/127.0.0.1/9090" 2>/dev/null && echo YES || echo no)" YES
say "  pool: $(pooled_ids | tr '\n' ' ') firecrackers: $(fcs)"
summary
