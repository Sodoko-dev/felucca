#!/usr/bin/env bash
# Snapshot gate, case 10, before the power cut — runs ON a lab worker (see scripts/verify-snapshot.sh).
#
#   snapshot-pre.sh          sandboxes A, B, C and D's guest
#   snapshot-pre.sh sleep-d  only D's sleep; prints "D-SLEEP <code> <epoch>" — the host
#                            cuts the power (limactl stop -f) the moment this returns
#
#   sn-ra (A): slept and woke, runs           -> after the cut: stopped (S1)
#   sn-rb (B): put to sleep, acknowledged     -> sleeping, wakes with its memory
#   sn-rc (C): runs, never slept              -> stopped
#   sn-rd (D): sleep acknowledged less than two seconds before the cut -> sleeping, wakes
# What test/lab/snapshot-post.sh needs (blob sums, the counts) is kept in a file
# that is synced to disk before the cut.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
STATE=/var/lib/felucca-lab/sn-reboot.state

if [ "${1:-}" = sleep-d ]; then
  C="$(vm_act sn-rd sleep)"; echo "D-SLEEP $C $(date +%s.%N)"
  exit 0
fi

cleanup_prefix sn-r
DC0=$(dir_count); FC0=$(fc_count)
for v in sn-ra sn-rb sn-rc sn-rd; do eq "create $v" "$(vm_create $v 256)" 201; done
for v in sn-ra sn-rb sn-rc sn-rd; do wait_exec $v 150 && ok "$v answers exec" || bad "$v never became exec-ready"; done
for v in sn-ra sn-rc; do eq "$v writes its disk" "$(guest $v "echo disk-$v > /root/diskmark; sync; cat /root/diskmark" | tr -d '\n')" "disk-$v"; done
eq "A: sleep sn-ra" "$(vm_act sn-ra sleep)" 200; eq "A: wake sn-ra" "$(vm_act sn-ra wake)" 200
wait_exec sn-ra 60 && ok "A: sn-ra answers after the wake" || bad "A: sn-ra does not answer after the wake"
SB=$(blob_make sn-rb); SD=$(blob_make sn-rd)
[ -n "$SB" ] && [ -n "$SD" ] && ok "memory witnesses in B and D" || bad "no memory witness"
eq "B: sleep sn-rb" "$(vm_act sn-rb sleep)" 200
sudo mkdir -p /var/lib/felucca-lab
printf 'DC0 %s\nFC0 %s\nB %s\nD %s\n' "$DC0" "$FC0" "$SB" "$SD" | sudo tee "$STATE" >/dev/null; sudo sync
say "  records: $(vms | jq -c '[.vms[]|select(.id|startswith("sn-r"))|"\(.id):\(.state)"]'); firecrackers: $(fcs)"
say "  counts before the sandboxes: dirs $DC0, Firecrackers $FC0"
summary
