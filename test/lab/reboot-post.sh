#!/usr/bin/env bash
# After a worker reboot — runs ON a lab worker (see scripts/verify-reboot.sh).
#
# What came back, and what the probe started by test/lab/reboot-pre.sh saw
# while the host went down. The probe's log is read straight out of the
# guest's root filesystem (debugfs), before that guest is started again.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
took_over() { sudo journalctl -u "$UNIT" -b --no-pager -o cat | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//'; }
say "== after the reboot =="
wait_up 90
eq "firewall unit" "$(systemctl is-active felucca-firewall)" active
eq "agent unit" "$(systemctl is-active $UNIT)" active
sudo nft list chain inet felucca_host input 2>/dev/null | grep -q 'iifname "felucca0" drop' && ok "guest fence loaded" || bad "no guest fence"
say "  start-up: $(took_over)"
say "  records: $(vms | jq -c '[.vms[]|"\(.id):\(.state)"]')"
eq "rb-run (was running, has no snapshot) is stopped" "$(vm_state rb-run)" stopped
eq "rb-peer is stopped" "$(vm_state rb-peer)" stopped
eq "rb-sleep is still asleep" "$(vm_state rb-sleep)" sleeping
eq "no Firecracker without a record" "$(orphans | wc -l | tr -d ' ')" 0
POOLT="$(sudo sed -n 's/.*"pool_size"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' /etc/felucca/felucca-agent.json | head -1)"; POOLT="${POOLT:-0}"
i=0; until [ "$(pooled_ids | wc -l | tr -d ' ')" -ge "$POOLT" ] || [ $i -ge 40 ]; do sleep 2; i=$((i+1)); done
eq "pool back at its target, no dead pool record left" "$(pooled_ids | wc -l | tr -d ' ') $(vms | jq '[.vms[]|select(.id|startswith("pool-"))|select(.state!="pooled")]|length')" "$POOLT 0"

say "== the guest's own log of the shutdown (its last samples) =="
DIR="$(vm_dir rb-run)"
PROBE="$(sudo debugfs -R "cat /root/probe.log" "$DIR/rootfs.ext4" 2>/dev/null)"
printf '%s\n' "$PROBE" | tail -n 8
[ -n "$PROBE" ] && ok "the probe's log survived ($(printf '%s\n' "$PROBE" | wc -l | tr -d ' ') samples)" || bad "no probe log on the guest's disk"
eq "samples in which the guest reached the other guest" "$(printf '%s\n' "$PROBE" | grep -c 'peer=YES')" 0
eq "samples in which the guest reached the host's agent port" "$(printf '%s\n' "$PROBE" | grep -c 'hostapi=YES')" 0

say "== what was running cold-starts, what slept wakes =="
eq "start rb-run" "$(vm_act rb-run start)" 200
wait_exec rb-run 150 && ok "rb-run answers" || bad "rb-run does not answer"
eq "rb-run kept the file written before the reboot" "$(guest rb-run 'cat /root/marker' | tr -d '\n')" before-reboot
eq "wake rb-sleep" "$(vm_act rb-sleep wake)" 200
wait_exec rb-sleep 90 && ok "rb-sleep answers after the wake" || bad "rb-sleep does not answer"
cleanup_prefix rb-
summary
