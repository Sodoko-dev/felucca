#!/usr/bin/env bash
# FLU-2 lab gate — runs ON a lab worker (see scripts/verify-oom.sh).
#
# The kernel OOM-kills one Firecracker of the unit. Pass: that one sandbox
# goes to `error`; the agent, the unit and every other sandbox are untouched.
# Against a unit without OOMPolicy=continue the same run shows the outage this
# gate exists for: the unit is stopped and every sandbox on the worker dies.
#
# How the worker "runs out of memory": the agent unit's own cgroup gets a
# MemoryMax a little above the guest memory it holds now (`systemctl
# set-property --runtime`, lifted again at the end), and then the big guest
# fills its RAM. The kernel has to OOM-kill inside that cgroup, exactly as it
# would on a host that is out of memory, and systemd applies the unit's
# OOMPolicy exactly as it would then — without taking the lab VM, or the Mac
# it runs on, down with it.
#
# The limit is set from the cgroup's ANONYMOUS memory, not from
# memory.current: most of what a freshly started worker "uses" is page cache
# from the rootfs copies, which the kernel just reclaims.
#
# What this run cannot tell: whether oom_score_adj decided WHICH process the
# kernel took. The big guest's Firecracker is also the largest process in the
# limited cgroup, so it would be chosen with or without the 300. The run does
# show that the score is in force at the moment of the kill (the kernel prints
# it), and section 1 that every Firecracker carries it and the agent does not.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"

BIG_MIB="${BIG_MIB:-2048}"           # the big sandbox: its guest will try to touch all of this
HEADROOM_MIB="${HEADROOM_MIB:-400}"  # room left above the guest memory in use now

lift_limit() { sudo systemctl set-property --runtime "$UNIT" MemoryMax=infinity >/dev/null 2>&1; }
trap 'lift_limit' EXIT

say "== 0. unit =="
say "  $(systemctl show $UNIT -p OOMPolicy -p KillMode -p ManagedOOMPreference -p Restart | tr '\n' ' ')"
preflight_unit
cleanup_prefix oom-
lift_limit

say "== 1. two small bystanders and one big sandbox =="
eq "create oom-by1 (256 MiB)" "$(vm_create oom-by1 256)" 201
eq "create oom-by2 (256 MiB)" "$(vm_create oom-by2 256)" 201
eq "create oom-hog (${BIG_MIB} MiB)" "$(vm_create oom-hog "$BIG_MIB")" 201
for v in oom-by1 oom-by2 oom-hog; do wait_exec $v 150 || bad "$v never became exec-ready"; done
eq "pause oom-by2 (a paused bystander must survive too)" "$(vm_act oom-by2 pause)" 200
AP0=$(agent_pid); NR0=$(systemctl show $UNIT -p NRestarts --value)
P_BY1=$(vm_pid oom-by1); P_BY2=$(vm_pid oom-by2); P_HOG=$(vm_pid oom-hog); UP0=$(guest_uptime oom-by1)
say "  agent pid $AP0 (NRestarts $NR0); oom-by1 pid $P_BY1 (uptime ${UP0}s); oom-by2 pid $P_BY2; oom-hog pid $P_HOG"
ADJ_FC="$(for p in $(pgrep -x firecracker); do cat /proc/$p/oom_score_adj; done | sort -u | tr '\n' ' ')"
ADJ_AGENT="$(cat /proc/$AP0/oom_score_adj)"
say "  oom_score_adj: firecrackers [${ADJ_FC% }], agent ${ADJ_AGENT}"
if [ "$(systemctl show $UNIT -p OOMPolicy --value)" = continue ]; then
  eq "every Firecracker is a preferred OOM victim" "${ADJ_FC% }" 300
  eq "the agent is not" "$ADJ_AGENT" 0
fi

say "== 2. leave the unit ${HEADROOM_MIB} MiB of room, then let the big guest fill its ${BIG_MIB} MiB =="
CG="/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/$AP0/cgroup)"
ANON=$(awk '$1=="anon"{print $2}' "$CG/memory.stat"); LIMIT=$((ANON + HEADROOM_MIB*1024*1024))
OOM0=$(awk '$1=="oom_kill"{print $2}' "$CG/memory.events")
say "  cgroup $CG: $(( $(cat "$CG/memory.current")/1048576 )) MiB in use, of which guest/anonymous $((ANON/1048576)) MiB; oom_kill so far $OOM0; MemoryMax -> $((LIMIT/1048576)) MiB"
T0=$(date '+%Y-%m-%d %H:%M:%S')
sudo systemctl set-property --runtime "$UNIT" MemoryMax="$LIMIT"
# `tail /dev/zero` buffers without bound: the guest touches memory until something gives.
api -m 15 -o /dev/null -X POST -d '{"cmd":["sh","-c","nohup tail /dev/zero >/dev/null 2>&1 &"],"timeout_ms":5000}' "$A/v1/vms/oom-hog/exec"
# Watch the kernel log, not memory.events: when the unit is stopped and
# restarted (which is what the bug does) the cgroup is recreated and its
# counter starts again from zero.
killed() { sudo journalctl -k --since "$T0" --no-pager -o cat | grep -q 'Killed process'; }
i=0; until killed || [ $i -ge 120 ]; do sleep 1; i=$((i+1)); done
if killed; then ok "the kernel OOM-killed inside the unit's cgroup after ${i}s"; else bad "no OOM kill within ${i}s — nothing was tested"; fi
sleep 15   # systemd applies OOMPolicy; the agent's reaper (or a restarted agent) settles the records
lift_limit
say "  kernel: $(sudo journalctl -k --since "$T0" --no-pager -o cat | grep -E 'Killed process' | sed -E 's/.*(Killed process [0-9]+ \([^)]*\)).*oom_score_adj:(-?[0-9]+).*/\1 oom_score_adj=\2/' | head -3 | tr '\n' ';')"
say "  systemd: $(journal_since "$T0" | grep -E 'killed by the OOM killer|Failed with result|Scheduled restart|Stopped|Started' | cut -c1-110 | head -5 | tr '\n' ';')"

say "== 3. who paid =="
VICTIM=$(sudo journalctl -k --since "$T0" --no-pager -o cat | grep -E 'Killed process' | sed -E 's/.*Killed process ([0-9]+) .*/\1/' | head -1)
eq "the process the kernel took is the big sandbox's Firecracker" "$VICTIM" "$P_HOG"
VADJ=$(sudo journalctl -k --since "$T0" --no-pager -o cat | grep -E 'Killed process' | sed -nE 's/.*oom_score_adj:(-?[0-9]+).*/\1/p' | head -1)
if [ "$(systemctl show $UNIT -p OOMPolicy --value)" = continue ]; then eq "the kernel saw it as a preferred victim (oom_score_adj in the kill line)" "$VADJ" 300; fi
eq "unit still active" "$(systemctl is-active $UNIT)" active
eq "agent was not restarted (main pid)" "$(agent_pid)" "$AP0"
eq "NRestarts unchanged" "$(systemctl show $UNIT -p NRestarts --value)" "$NR0"
alive "$P_BY1" && ok "bystander oom-by1: Firecracker $P_BY1 still alive" || bad "bystander oom-by1: Firecracker $P_BY1 is gone"
alive "$P_BY2" && ok "paused bystander oom-by2: Firecracker $P_BY2 still alive" || bad "paused bystander oom-by2: Firecracker $P_BY2 is gone"
eq "oom-by1 state and pid" "$(vm_state oom-by1) $(vm_pid oom-by1)" "running $P_BY1"
eq "oom-by2 state and pid" "$(vm_state oom-by2) $(vm_pid oom-by2)" "paused $P_BY2"
UP1=$(guest_uptime oom-by1)
[ -n "$UP1" ] && [ "$UP1" -gt "$UP0" ] && ok "oom-by1 guest uptime kept counting (${UP0}s -> ${UP1}s)" || bad "oom-by1 uptime ${UP0}s -> '${UP1}' (guest restarted or unreachable)"
eq "the big sandbox is reported as error" "$(vm_state oom-hog)" error
alive "$P_HOG" && bad "its Firecracker is still alive" || ok "its Firecracker is gone"
journal_since "$T0" | grep -q 'firecracker exited unexpectedly' && ok "agent logged the unexpected exit" || bad "agent did not log the exit"
eq "it can be cold-started again" "$(vm_act oom-hog start)" 200

cleanup_prefix oom-
summary
