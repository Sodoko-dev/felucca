#!/usr/bin/env bash
# Snapshot gate, case 10, after the power cut — runs ON a lab worker (see scripts/verify-snapshot.sh).
# A sandbox that was running when the worker lost power is stopped, whether or
# not it had slept before (S1); a sleep that was acknowledged is a sleep (F3).
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
STATE=/var/lib/felucca-lab/sn-reboot.state
trap 'cleanup_prefix sn-r; sudo rm -f "$STATE"' EXIT
wait_up 90
val() { sudo sed -n "s/^$1 //p" "$STATE" 2>/dev/null; }
DC0="$(val DC0)"; FC0="$(val FC0)"; SB="$(val B)"; SD="$(val D)"
[ -n "$SB" ] && [ -n "$SD" ] && ok "the state file of snapshot-pre.sh survived" || bad "no state file from snapshot-pre.sh at $STATE"
say "  boot: $(uptime -s); records: $(vms | jq -c '[.vms[]|select(.id|startswith("sn-r"))|"\(.id):\(.state)"]')"
say "  start-up: $(sudo journalctl -u "$UNIT" -b --no-pager -o cat | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//')"
for v in sn-ra sn-rc; do
  eq "$v (was running) API state" "$(vm_state $v)" stopped
  eq "$v record on disk" "$(meta_snap $v)" "stopped false"
done
for v in sn-rb sn-rd; do
  eq "$v (sleep acknowledged) API state" "$(vm_state $v)" sleeping
  eq "$v record on disk" "$(meta_snap $v)" "sleeping true"
  eq "$v snapshot files" "$(snap_files "$(vm_dir $v)")" "vmstate=yes mem=yes tmp=no"
done
for v in sn-ra sn-rc; do refused_c="$(vm_act_body $v wake)"; eq "wake $v is refused: code and state" "${refused_c%% *} $(printf '%s' "${refused_c#* }" | jq -r '.state // empty' 2>/dev/null)" "409 stopped"; done
eq "wake sn-rb" "$(vm_act sn-rb wake)" 200; eq "wake sn-rd" "$(vm_act sn-rd wake)" 200
for v in sn-rb sn-rd; do wait_exec $v 60 && ok "$v answers after the wake" || bad "$v does not answer after the wake"; done
s="$(blob_sum sn-rb)"; eq "sn-rb memory intact" "${s:0:16}" "${SB:0:16}"
s="$(blob_sum sn-rd)"; eq "sn-rd memory intact (slept just before the power cut)" "${s:0:16}" "${SD:0:16}"
for v in sn-ra sn-rc; do
  eq "start $v" "$(vm_act $v start)" 200
  wait_exec $v 150 && eq "$v kept its disk" "$(guest $v 'cat /root/diskmark' | tr -d '\n')" "disk-$v" || bad "$v does not answer after the start"
done
cleanup_prefix sn-r
i=0; until { [ "$(fc_count)" -eq "${FC0:-0}" ] && [ "$(dir_count)" -eq "${DC0:-0}" ]; } || [ $i -ge 60 ]; do sleep 1; i=$((i+1)); done
eq "instance directories as before snapshot-pre.sh" "$(dir_count)" "${DC0:-?}"
eq "Firecrackers as before snapshot-pre.sh" "$(fc_count)" "${FC0:-?}"
summary
