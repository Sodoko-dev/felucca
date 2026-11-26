#!/usr/bin/env bash
# FLU-1 lab gate — runs ON a lab worker (see scripts/verify-restart.sh).
#
# Sandboxes must outlive the agent: restarting, killing, stopping or upgrading
# felucca-agent may not cost a tenant anything, and whatever the agent was in
# the middle of when it stopped must be put right by the next start.
#
# The script is rough with the worker on purpose (it kills the agent, damages
# records, breaks nft for the agent, starts a second agent by hand). Every
# sandbox it creates is named rs-* / rc-* and removed at the end; do not run it
# on a worker that holds anything you care about.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"

CFG=/etc/felucca/felucca-agent.json
DROPIN_DIR=/etc/systemd/system/felucca-agent.service.d
FAILNFT=/opt/felucca-lab-failnft
CP_URL="$(sudo sed -n 's/.*"control_plane"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" | head -1)"
CP_HOST="$(printf '%s' "$CP_URL" | sed -E 's#^[a-z]+://##; s#[:/].*$##')"
CP_PORT="$(printf '%s' "$CP_URL" | sed -nE 's#^[a-z]+://[^:/]+:([0-9]+).*#\1#p')"; CP_PORT="${CP_PORT:-8080}"
POOL_TARGET="$(sudo sed -n 's/.*"pool_size"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$CFG" | head -1)"; POOL_TARGET="${POOL_TARGET:-0}"

undo() {
  sudo rm -f "$DROPIN_DIR/zz-lab-test-nonft.conf"; sudo rm -rf "$FAILNFT"
  [ -f /var/tmp/felucca-lab-agent.json.saved ] && sudo cp -p /var/tmp/felucca-lab-agent.json.saved "$CFG" && sudo rm -f /var/tmp/felucca-lab-agent.json.saved
  sudo nft delete table inet felucca_lab_block 2>/dev/null
  sudo systemctl daemon-reload
}
trap 'undo' EXIT

metaless_dirs() { sudo sh -c "for d in $D/*/; do [ -e \"\$d\" ] || continue; [ -f \"\${d}meta.json\" ] || basename \"\$d\"; done" | tr '\n' ' ' | sed 's/ $//'; }
took_over()     { journal_since "$1" | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//'; }
ping_guest()    { local ip; ip="$(vm_ip "$1")"; [ -n "$ip" ] && ping -c2 -W1 "$ip" >/dev/null 2>&1; }
no_garbage()    { # <label> — nothing left that no record owns
  eq "$1: no Firecracker without a record" "$(orphans | wc -l | tr -d ' ')" 0
  eq "$1: no instance directory without a meta.json" "$(metaless_dirs)" ""
}

# ---- the zoo: one VM in every state a restart has to carry over --------------
ZOO="rs-run rs-claim rs-sw"
snapshot() {
  FC0="$(fcs)"; UP0="$(guest_uptime rs-run)"
  P_RUN=$(vm_pid rs-run); P_CLAIM=$(vm_pid rs-claim); P_SW=$(vm_pid rs-sw); P_PAUSE=$(vm_pid rs-pause)
  POOL="$(pooled_ids | head -1)"; POOL_PID=""; [ -n "$POOL" ] && POOL_PID="$(vm_pid "$POOL")"
}
survived() { # <label>
  eq "$1: every Firecracker kept its pid" "$(fcs)" "$FC0"
  eq "$1: rs-run running, same pid"     "$(vm_state rs-run) $(vm_pid rs-run)"     "running $P_RUN"
  eq "$1: rs-claim running, same pid"   "$(vm_state rs-claim) $(vm_pid rs-claim)" "running $P_CLAIM"
  eq "$1: rs-sw running, same pid"      "$(vm_state rs-sw) $(vm_pid rs-sw)"       "running $P_SW"
  eq "$1: rs-pause paused, same pid"    "$(vm_state rs-pause) $(vm_pid rs-pause)" "paused $P_PAUSE"
  eq "$1: rs-sleep sleeping"            "$(vm_state rs-sleep)" sleeping
  if [ -n "$POOL" ]; then eq "$1: pool VM still pooled, same pid" "$(vm_state "$POOL") $(vm_pid "$POOL")" "pooled $POOL_PID"; fi
  local up; up="$(guest_uptime rs-run)"
  if [ -n "$up" ] && [ -n "$UP0" ] && [ "$up" -ge "$UP0" ]; then ok "$1: rs-run guest uptime kept counting (${UP0}s -> ${up}s)"; else bad "$1: rs-run uptime ${UP0}s -> '${up}'"; fi
  eq "$1: rs-run kept its file" "$(guest rs-run 'cat /root/marker' | tr -d '\n')" lab-marker
  check "$1: exec on rs-claim" exec_ok rs-claim
  check "$1: exec on rs-sw" exec_ok rs-sw
  eq "$1: every Firecracker is an OOM victim (300)" "$(for p in $(pgrep -x firecracker); do cat /proc/$p/oom_score_adj; done | sort -u | tr '\n' ' ' | sed 's/ $//')" 300
  no_garbage "$1"
  UP0="$up"
}

say "== 0. the unit that is installed =="
preflight_unit
eq "KillMode" "$(systemctl show $UNIT -p KillMode --value)" process
eq "Restart"  "$(systemctl show $UNIT -p Restart --value)" always
eq "start limit disabled" "$(systemctl show $UNIT -p StartLimitIntervalUSec --value)" 0
eq "agent active" "$(systemctl is-active $UNIT)" active
say "  data dir ${DATA_DIR}, control plane ${CP_HOST}:${CP_PORT}, config pool ${POOL_TARGET}"
cleanup_prefix rs-; cleanup_prefix rc-

say "== 1. build the zoo =="
if [ "$POOL_TARGET" -gt 0 ]; then i=0; until [ -n "$(pooled_ids)" ] || [ $i -ge 30 ]; do sleep 2; i=$((i+1)); done; fi
eq "create rs-claim" "$(vm_create rs-claim 256)" 201
for v in rs-run rs-pause rs-sleep rs-sw; do eq "create $v" "$(vm_create $v 256)" 201; done
for v in rs-run rs-claim rs-pause rs-sleep rs-sw; do wait_exec $v 150 || bad "$v never became exec-ready"; done
guest rs-run 'echo lab-marker > /root/marker; sync' >/dev/null
eq "pause rs-pause" "$(vm_act rs-pause pause)" 200
eq "sleep rs-sleep" "$(vm_act rs-sleep sleep)" 200
eq "sleep rs-sw" "$(vm_act rs-sw sleep)" 200; eq "wake rs-sw" "$(vm_act rs-sw wake)" 200
wait_exec rs-sw 60 || bad "rs-sw not exec-ready after wake"
if [ "$POOL_TARGET" -gt 0 ]; then
  i=0; until [ -n "$(pooled_ids)" ] || [ $i -ge 40 ]; do sleep 2; i=$((i+1)); done
  [ -n "$(pooled_ids)" ] && ok "warm pool refilled" || bad "no pooled VM"
  case "$(basename "$(vm_dir rs-claim)")" in pool-*) ok "rs-claim was claimed from the pool (lives in $(basename "$(vm_dir rs-claim)"))";; *) say "  note: rs-claim was cold-booted";; esac
else
  skip "warm-pool adoption (pool_size is 0 on this worker)"
fi
snapshot
say "  firecrackers: $FC0"

say "== 2. systemctl restart =="
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 1
survived "restart"
say "  start-up: $(took_over "$T0")"

say "== 3. kill -9 (a crash) =="
NR0=$(systemctl show $UNIT -p NRestarts --value)
kill9_agent; sleep 1
survived "kill -9"
eq "kill -9: systemd restarted the agent once" "$(systemctl show $UNIT -p NRestarts --value)" "$((NR0+1))"

say "== 4. a plain SIGTERM to the agent (a 'clean' exit) =="
OLD=$(agent_pid); sudo kill -TERM "$OLD"
i=0; until [ "$(agent_pid)" != "$OLD" ] && [ "$(agent_pid)" != 0 ] || [ $i -ge 40 ]; do sleep 0.5; i=$((i+1)); done; wait_up
[ "$(agent_pid)" != "$OLD" ] && [ "$(systemctl is-active $UNIT)" = active ] && ok "SIGTERM: the agent came back (Restart=always)" || bad "SIGTERM: the agent stayed down"
survived "SIGTERM"

say "== 5. agent stopped for 8 s =="
sudo systemctl stop $UNIT; sleep 8
eq "stopped: unit inactive" "$(systemctl is-active $UNIT)" inactive
eq "stopped: every Firecracker still running" "$(fcs)" "$FC0"
if ping_guest rs-run; then ok "stopped: guest still answers on its network"; else bad "stopped: guest unreachable while the agent is down"; fi
sudo systemctl start $UNIT; wait_up; sleep 1
survived "stop/start"

say "== 6. six restarts in a row (the default start limit is 5 in 10 s) =="
for _ in 1 2 3 4 5 6; do sudo systemctl restart $UNIT; done
wait_up; sleep 1
eq "still active" "$(systemctl is-active $UNIT)" active
[ "$(systemctl show $UNIT -p Result --value)" != start-limit-hit ] && ok "not start-limit-hit" || bad "start-limit-hit"
survived "six restarts"

say "== 7. the lifecycle still works on adopted VMs =="
eq "resume rs-pause" "$(vm_act rs-pause resume)" 200
eq "wake rs-sleep" "$(vm_act rs-sleep wake)" 200
wait_exec rs-sleep 60 && ok "exec on the woken rs-sleep" || bad "rs-sleep not exec-ready after wake"
for i in 1 2 3; do
  eq "adopted rs-sw: sleep ($i)" "$(vm_act rs-sw sleep)" 200
  eq "adopted rs-sw: wake straight after ($i)" "$(vm_act rs-sw wake)" 200
done
wait_exec rs-sw 60 && ok "exec on rs-sw after three sleep/wake rounds" || bad "rs-sw not exec-ready"
eq "fork rs-run -> rs-kid" "$(vm_fork rs-run rs-kid)" 201
wait_exec rs-kid 60 && ok "exec on the fork child" || bad "fork child not exec-ready"
eq "stop rs-claim" "$(vm_act rs-claim stop)" 200
alive "$P_CLAIM" && bad "rs-claim's Firecracker still alive after stop" || ok "rs-claim's Firecracker is gone"
eq "start rs-claim" "$(vm_act rs-claim start)" 200
wait_exec rs-claim 120 && ok "exec after stop+start" || bad "rs-claim not exec-ready after start"
eq "delete rs-kid" "$(vm_del rs-kid)" 204
cleanup_prefix rs-pause; cleanup_prefix rs-sleep

say "== 8. agent restarted in the middle of an operation =="
for delay in 0.3 1.5 3; do
  ( vm_create rs-mid 512 >/dev/null 2>&1 & ); sleep "$delay"
  restart_agent; sleep 3
  st="$(vm_state rs-mid)"
  case "$st" in
    "")      ok "create cut at ${delay}s: no record (the caller got an error)";;
    running) if wait_exec rs-mid 120; then ok "create cut at ${delay}s: completed, exec works"; else bad "create cut at ${delay}s: listed running but dead"; fi;;
    *)       bad "create cut at ${delay}s: left in state '$st'";;
  esac
  no_garbage "create cut at ${delay}s"
  vm_del rs-mid >/dev/null
done

eq "create rs-big (1024 MiB: its snapshot takes a while to write)" "$(vm_create rs-big 1024)" 201
wait_exec rs-big 150 || bad "rs-big never became exec-ready"
not_frozen() { # <label> <id> — a VM listed running must answer
  local st; st="$(vm_state "$2")"
  case "$st" in
    running)  if wait_exec "$2" 45; then ok "$1: $2 is running and answers"; else bad "$1: $2 is listed running and does not answer (frozen)"; fi;;
    sleeping) eq "$1: $2 went to sleep; wake" "$(vm_act "$2" wake)" 200; wait_exec "$2" 60 && ok "$1: $2 answers after wake" || bad "$1: $2 does not answer after wake";;
    *)        bad "$1: $2 left in state '$st'";;
  esac
}
T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_act rs-big sleep >/dev/null 2>&1 & ); sleep 0.5; restart_agent; sleep 2
say "  sleep cut short; start-up: $(took_over "$T0")"
not_frozen "sleep cut short" rs-big
no_garbage "sleep cut short"

T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_fork rs-big rs-bigkid >/dev/null 2>&1 & ); sleep 0.5; restart_agent; sleep 2
say "  fork cut short; start-up: $(took_over "$T0")"
not_frozen "fork cut short (parent)" rs-big
case "$(vm_state rs-bigkid)" in
  "")      ok "fork cut short: no child record";;
  running) wait_exec rs-bigkid 60 && ok "fork cut short: the child completed and answers" || bad "fork cut short: child listed running but dead";;
  *)       bad "fork cut short: child left in state '$(vm_state rs-bigkid)'";;
esac
no_garbage "fork cut short"
vm_del rs-bigkid >/dev/null

eq "sleep rs-big (to completion)" "$(vm_act rs-big sleep)" 200
T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_act rs-big wake >/dev/null 2>&1 & ); sleep 0.3; restart_agent; sleep 2
say "  wake cut short; start-up: $(took_over "$T0")"
not_frozen "wake cut short" rs-big
no_garbage "wake cut short"
vm_del rs-big >/dev/null

say "== 9. an adopted Firecracker dies =="
eq "create rs-die" "$(vm_create rs-die 256)" 201; wait_exec rs-die 150 || bad "rs-die never became exec-ready"
restart_agent
P=$(vm_pid rs-die); sudo kill -9 "$P"
i=0; until [ "$(vm_state rs-die)" = error ] || [ $i -ge 15 ]; do sleep 1; i=$((i+1)); done
eq "its record goes to error (after ${i}s; the sweep runs every 5 s)" "$(vm_state rs-die)" error
vm_del rs-die >/dev/null

say "== 10. damaged and missing records =="
eq "create rs-dmg" "$(vm_create rs-dmg 256)" 201; eq "create rs-orph" "$(vm_create rs-orph 256)" 201
wait_exec rs-dmg 150 || bad "rs-dmg never became exec-ready"; wait_exec rs-orph 150 || bad "rs-orph never became exec-ready"
P_DMG=$(vm_pid rs-dmg); P_ORPH=$(vm_pid rs-orph); DIR_DMG="$(vm_dir rs-dmg)"; DIR_ORPH="$(vm_dir rs-orph)"
sudo systemctl stop $UNIT
sudo cp -p "$DIR_DMG/meta.json" /var/tmp/felucca-lab-meta.saved
sudo truncate -s 0 "$DIR_DMG/meta.json"            # what a crash mid-write used to leave
sudo mv "$DIR_ORPH/meta.json" /var/tmp/felucca-lab-meta.orph   # a create that never finished
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 2
say "  start-up: $(took_over "$T0")"
alive "$P_DMG" && ok "empty meta.json: its Firecracker was left running" || bad "empty meta.json: its Firecracker was killed"
eq "empty meta.json: no record" "$(vm_state rs-dmg)" ""
journal_since "$T0" | grep -q 'meta.json does not load' && ok "empty meta.json: reported in the journal" || bad "empty meta.json: not reported"
alive "$P_ORPH" && bad "no meta.json: its Firecracker is still running" || ok "no meta.json: its Firecracker was killed"
sudo test -e "$DIR_ORPH" && bad "no meta.json: its directory is still there" || ok "no meta.json: its directory was removed"
eq "kept the VMs that were fine" "$(vm_state rs-run) $(vm_pid rs-run)" "running $P_RUN"
sudo cp -p /var/tmp/felucca-lab-meta.saved "$DIR_DMG/meta.json"
restart_agent; sleep 1
eq "meta.json repaired: adopted again, same Firecracker" "$(vm_state rs-dmg) $(vm_pid rs-dmg)" "running $P_DMG"
check "meta.json repaired: exec works" exec_ok rs-dmg
vm_del rs-dmg >/dev/null; sudo rm -f /var/tmp/felucca-lab-meta.saved /var/tmp/felucca-lab-meta.orph
snapshot_small() { FCS="$(fcs)"; }
snapshot_small

say "== 11. data_dir spelled differently (trailing slash) =="
sudo cp -p "$CFG" /var/tmp/felucca-lab-agent.json.saved
sudo sed -i "s#\"data_dir\"\\([[:space:]]*\\):\\([[:space:]]*\\)\"${DATA_DIR%/}\"#\"data_dir\"\\1:\\2\"${DATA_DIR%/}/\"#" "$CFG"
sudo grep -q "\"${DATA_DIR%/}/\"" "$CFG" && ok "config now says ${DATA_DIR%/}/" || bad "could not edit data_dir in $CFG"
restart_agent; sleep 1
eq "respelled: every Firecracker kept its pid" "$(fcs)" "$FCS"
eq "respelled: rs-run still adopted" "$(vm_state rs-run) $(vm_pid rs-run)" "running $P_RUN"
check "respelled: exec works" exec_ok rs-run
sudo cp -p /var/tmp/felucca-lab-agent.json.saved "$CFG"; sudo rm -f /var/tmp/felucca-lab-agent.json.saved
restart_agent; sleep 1
eq "config restored: rs-run still adopted" "$(vm_state rs-run) $(vm_pid rs-run)" "running $P_RUN"

say "== 12. nft fails for the agent at start-up =="
sudo mkdir -p "$FAILNFT" "$DROPIN_DIR"; printf '#!/bin/sh\nexit 1\n' | sudo tee "$FAILNFT/nft" >/dev/null; sudo chmod 0755 "$FAILNFT/nft"
printf '[Service]\nEnvironment=PATH=%s:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n' "$FAILNFT" | sudo tee "$DROPIN_DIR/zz-lab-test-nonft.conf" >/dev/null
sudo systemctl daemon-reload
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl restart $UNIT; sleep 8
case "$(systemctl is-active $UNIT)" in active) bad "agent is serving although it could not load its rules";; *) ok "agent refuses to serve ($(systemctl is-active $UNIT))";; esac
eq "every Firecracker still running" "$(fcs)" "$FCS"
journal_since "$T0" | grep -q 'refusing to serve tenants' && ok "journal: refusing to serve tenants" || bad "journal: no refusal"
journal_since "$T0" | grep -q 'guest bridge has been taken down' && ok "journal: the guest bridge was taken down" || bad "journal: bridge not taken down"
ip -br link show felucca0 | grep -qw DOWN && ok "felucca0 is down" || bad "felucca0 is still up: $(ip -br link show felucca0 | tr -s ' ')"
if ping_guest rs-run; then bad "guest still reachable on a node that could not fence it"; else ok "guests are off the network while the agent cannot fence them"; fi
sudo rm -f "$DROPIN_DIR/zz-lab-test-nonft.conf"; sudo rm -rf "$FAILNFT"; sudo systemctl daemon-reload
sudo systemctl restart $UNIT; wait_up; sleep 2
eq "nft back: agent active" "$(systemctl is-active $UNIT)" active
eq "nft back: every Firecracker kept its pid" "$(fcs)" "$FCS"
i=0; until ping_guest rs-run || [ $i -ge 10 ]; do sleep 1; i=$((i+1)); done
if ping_guest rs-run; then ok "nft back: guest network works again (no cold boot needed)"; else bad "nft back: guest network did not come back"; fi
check "nft back: exec works" exec_ok rs-run
eq "nft back: rs-run still adopted" "$(vm_state rs-run) $(vm_pid rs-run)" "running $P_RUN"

say "== 13. a second agent started by hand on the same data dir =="
AP=$(agent_pid)
OUT="$(sudo timeout 25 /usr/local/bin/felucca-agent --config "$CFG" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tail -60)"
printf '%s' "$OUT" | grep -q 'could not take the data-dir lock' && ok "the second agent does not sweep (no data-dir lock)" || bad "the second agent did not report the lock: $(printf '%s' "$OUT" | tail -3 | tr '\n' '|' | cut -c1-200)"
eq "the running agent is untouched" "$(agent_pid) $(systemctl is-active $UNIT)" "$AP active"
eq "every Firecracker kept its pid" "$(fcs)" "$FCS"
check "exec still works" exec_ok rs-run
sleep 6   # let the first agent's pool loop settle anything the visitor started
restart_agent; sleep 2; no_garbage "after the second agent"; snapshot_small

say "== 14. template pools across a restart =="
SHAPE='"image":"ubuntu-base","image_sha256":"","vcpus":1,"mem_mib":320,"disk_gb":0'
pool320() { vms | jq '[.vms[]|select(.state=="pooled" and .mem_mib==320)]|length'; }
pool320_pids() { vms | jq -r '.vms[]|select(.state=="pooled" and .mem_mib==320)|.pid' | sort -n | tr '\n' ' ' | sed 's/ $//'; }
eq "PUT /v1/pools (3 x 320 MiB)" "$(set_pools "[{${SHAPE},\"count\":3}]")" 200
i=0; until [ "$(pool320)" -ge 3 ] || [ $i -ge 60 ]; do sleep 2; i=$((i+1)); done
eq "pool built" "$(pool320)" 3
PP0="$(pool320_pids)"
# Keep the control plane from re-pushing its (empty) specs: the new agent must
# then not drain what it adopted, because it has not been told anything yet.
sudo nft add table inet felucca_lab_block
sudo nft add chain inet felucca_lab_block out '{ type filter hook output priority 0 ; }'
sudo nft add rule inet felucca_lab_block out ip daddr "$CP_HOST" tcp dport "$CP_PORT" reject
restart_agent; sleep 14   # two refill ticks and more
eq "specs not received yet: the pool was not drained" "$(pool320)" 3
eq "specs not received yet: same Firecrackers" "$(pool320_pids)" "$PP0"
for k in 1 2; do eq "create rs-t$k right after the restart" "$(vm_create rs-t$k 320)" 201; done
case "$(basename "$(vm_dir rs-t1)")" in pool-*) ok "rs-t1 was claimed from the adopted pool";; *) bad "rs-t1 was not claimed from the pool";; esac
sudo nft delete table inet felucca_lab_block
i=0; until [ "$(pool320)" -eq 0 ] || [ $i -ge 45 ]; do sleep 1; i=$((i+1)); done
eq "control plane reachable again: its (empty) specs drain the rest (after ${i}s)" "$(pool320)" 0
for k in 1 2; do
  sudo test -f "$(vm_dir rs-t$k)/meta.json" && ok "rs-t$k kept its directory through the drain" || bad "rs-t$k lost its directory"
  wait_exec rs-t$k 120 && ok "rs-t$k answers" || bad "rs-t$k does not answer"
done
cleanup_prefix rs-t

say "== 15. pool teardown while creates of that shape arrive (FLU-60) =="
eq "PUT /v1/pools (8 x 320 MiB)" "$(set_pools "[{${SHAPE},\"count\":8}]")" 200
i=0; until [ "$(pool320)" -ge 8 ] || [ $i -ge 90 ]; do sleep 2; i=$((i+1)); done
eq "pool built" "$(pool320)" 8
set_pools '[]' >/dev/null
j=0; until [ "$(pool320)" -lt 8 ] || [ $j -ge 600 ]; do j=$((j+1)); done   # the drain has started
LEFT="$(pool320)"
for k in 1 2 3 4 5 6; do ( c=$(vm_create rc-$k 320); echo "$k $c" >> /tmp/felucca-lab-rc.out ) & done; wait
say "  drain under way ($LEFT of 8 pool VMs left) when 6 creates were sent"
CLAIMED=0; BROKEN=0
for k in 1 2 3 4 5 6; do
  [ "$(grep "^$k " /tmp/felucca-lab-rc.out | cut -d' ' -f2)" = 201 ] || continue
  dir="$(vm_dir rc-$k)"; case "$(basename "${dir:-x}")" in pool-*) CLAIMED=$((CLAIMED+1));; esac
  if [ -n "$dir" ] && sudo test -f "$dir/meta.json" && sudo test -S "$dir/fc.sock" && wait_exec rc-$k 150; then :; else BROKEN=$((BROKEN+1)); say "  rc-$k: state=$(vm_state rc-$k) dir=${dir:-MISSING}"; fi
done
rm -f /tmp/felucca-lab-rc.out
[ "$CLAIMED" -gt 0 ] && ok "$CLAIMED creates were claimed from the pool while it was being torn down" || skip "no create overlapped the drain this time"
eq "sandboxes that came back 201 and are not intact" "$BROKEN" 0
cleanup_prefix rc-
i=0; until [ "$(pool320)" -eq 0 ] || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done

say "== 16. ten restarts: nothing accumulates =="
sleep 6; restart_agent; sleep 6
count() { echo "fc=$(fc_count) taps=$(tap_count) records=$(vm_count) dirs=$(dir_count) chains=$(chain_count)"; }
C0="$(count)"
for _ in 1 2 3 4 5 6 7 8 9 10; do restart_agent; sleep 2; done
sleep 6
eq "counts after ten restarts" "$(count)" "$C0"
no_garbage "ten restarts"
[ "$(systemctl show $UNIT -p Result --value)" != start-limit-hit ] && ok "never start-limit-hit" || bad "start-limit-hit"

say "== 17. through the control plane =="
CP="http://${CP_HOST}:${CP_PORT}"; ME="$(hostname)"
MYNODE="$(api -m 10 "$CP/api/v1/nodes" | jq -r --arg h "$ME" '.nodes[]? // .[]? | select(.hostname==$h) | .id' 2>/dev/null | head -1)"
if [ -z "$MYNODE" ]; then
  skip "control-plane cases (this worker is not in $CP/api/v1/nodes as $ME)"
else
  # feluccad places a sandbox on the least-loaded node and offers no way to
  # name one. Sandboxes that land elsewhere are kept until the end: they are
  # what tips the next placement this way (the load it sees is one heartbeat,
  # 5 s, behind).
  SB=""; ELSEWHERE=""
  for _ in 1 2 3 4 5 6; do
    r="$(api -m 120 -X POST -d '{"name":"lab-restart","namespace":"lab","vcpus":1,"mem_mib":256}' "$CP/api/v1/sandboxes")"
    id="$(printf '%s' "$r" | jq -r '.id // empty')"; node="$(printf '%s' "$r" | jq -r '.node_id // empty')"
    [ -n "$id" ] || { sleep 3; continue; }
    if [ "$node" = "$MYNODE" ]; then SB="$id"; break; fi
    ELSEWHERE="$ELSEWHERE $id"; sleep 7
  done
  if [ -z "$SB" ]; then
    skip "control-plane cases (no sandbox was placed on this worker in 6 tries)"
  else
    cp_exec() { api -m 20 -X POST -d '{"cmd":["sh","-c","cut -d. -f1 /proc/uptime"],"timeout_ms":5000}' "$CP/api/v1/sandboxes/$SB/exec" | jq -r '.stdout // empty' | tr -d '\n'; }
    i=0; until [ -n "$(cp_exec)" ] || [ $i -ge 90 ]; do sleep 1; i=$((i+1)); done
    U0="$(cp_exec)"; [ -n "$U0" ] && ok "sandbox $SB placed here; exec through the control plane works" || bad "exec through the control plane never worked"
    restart_agent
    i=0; until [ "$(api -m 10 "$CP/api/v1/nodes" | jq -r --arg n "$MYNODE" '.nodes[]? // .[]? | select(.id==$n) | .status' | head -1)" = ready ] || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
    eq "node is ready again in the control plane (after ${i}s)" "$(api -m 10 "$CP/api/v1/nodes" | jq -r --arg n "$MYNODE" '.nodes[]? // .[]? | select(.id==$n) | .status' | head -1)" ready
    eq "the control plane still lists it running" "$(api -m 10 "$CP/api/v1/sandboxes/$SB" | jq -r '.state // empty')" running
    U1="$(cp_exec)"
    if [ -n "$U1" ] && [ -n "$U0" ] && [ "$U1" -ge "$U0" ]; then ok "exec through the control plane after the restart; guest uptime ${U0}s -> ${U1}s"; else bad "exec through the control plane after the restart: uptime ${U0}s -> '${U1}'"; fi
    api -m 30 -o /dev/null -X DELETE "$CP/api/v1/sandboxes/$SB"
  fi
  for id in $ELSEWHERE; do api -m 30 -o /dev/null -X DELETE "$CP/api/v1/sandboxes/$id"; done
fi

cleanup_prefix rs-; cleanup_prefix rc-
summary
