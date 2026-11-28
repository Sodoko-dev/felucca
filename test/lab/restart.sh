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
  sudo rm -f /var/tmp/felucca-lab-meta.* /tmp/felucca-lab-rc.out
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
# A stop is on disk at once, and it is still a stop after a restart — even with
# an old snapshot lying in the directory (rs-sw slept and woke above). It used
# to come back as "sleeping", which `start` refuses.
# (FLU-4, S7/S8: a wake removes vmstate.bin and a stop removes the snapshot
# files, so since then the "note:" branch below is the expected one.)
eq "stop rs-sw" "$(vm_act rs-sw stop)" 200
eq "stop rs-sw: the record on disk says so" "$(meta_field rs-sw '"\(.state) \(.pid)"')" "stopped null"
sudo test -f "$(vm_dir rs-sw)/vmstate.bin" && ok "rs-sw has an old snapshot in its directory" || say "  note: no snapshot file in rs-sw's directory"
restart_agent; sleep 1
eq "restart: rs-sw is still stopped" "$(vm_state rs-sw)" stopped
eq "start rs-sw" "$(vm_act rs-sw start)" 200
wait_exec rs-sw 120 && ok "exec after stop, restart, start" || bad "rs-sw not exec-ready after start"
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

# A guest whose snapshot takes a while to write (mem.bin is mem_mib bytes).
# When that is longer than the two seconds start-up waits for a Firecracker to
# answer, the agent has to come back to it later, or the guest stays paused
# under a record that says running. How long the write takes depends on the
# worker's disk, so both outcomes are accepted below and the frozen-Firecracker
# case further down makes the slow one certain. LAB_BIG_MIB=8192 on a worker
# with the memory to spare shows it with a real snapshot.
BIG="${LAB_BIG_MIB:-2048}"
eq "create rs-big (${BIG} MiB: its snapshot takes seconds to write)" "$(vm_create rs-big "$BIG")" 201
wait_exec rs-big 150 || bad "rs-big never became exec-ready"
# <label> <id> — a VM listed running must really be running. Its Firecracker is
# the witness, and the guest answering on its network. exec is tried first but
# is not required: exec stays dead after any pause and resume that had no
# snapshot in between, with or without a restart (FLU-15).
not_frozen() {
  local st i; st="$(vm_state "$2")"
  case "$st" in
    running)
      i=0; until [ "$(vmm_state "$2")" = Running ] || [ $i -ge 120 ]; do sleep 1; i=$((i+1)); done
      if [ "$(vmm_state "$2")" != Running ]; then bad "$1: $2 is listed running and its Firecracker says '$(vmm_state "$2")' (frozen)"
      elif wait_exec "$2" 20; then ok "$1: $2 is running and answers"
      elif ping_guest "$2"; then ok "$1: $2 is running (Firecracker says Running, the guest answers ping; exec does not: FLU-15)"
      else bad "$1: $2 is listed running, its Firecracker says Running, and the guest answers neither exec nor ping"; fi;;
    sleeping) eq "$1: $2 went to sleep; wake" "$(vm_act "$2" wake)" 200; wait_exec "$2" 60 && ok "$1: $2 answers after wake" || bad "$1: $2 does not answer after wake";;
    *)        bad "$1: $2 left in state '$st'";;
  esac
}
asked_again() { # <label> <since> — a Firecracker that was silent at start-up was settled later
  local to; to="$(took_over "$2")"
  case "$to" in
    *unreachable=0*) say "  note: $1: the Firecracker answered at once (its snapshot was already written)";;
    *unreachable=*)
      ok "$1: start-up did not wait for a Firecracker that was busy ($(printf '%s' "$to" | grep -o 'unreachable=[0-9]*'))"
      local i=0; until journal_since "$2" | grep -q 'answers now' || [ $i -ge 120 ]; do sleep 1; i=$((i+1)); done
      if journal_since "$2" | grep -q 'answers now'; then ok "$1: it was asked again and settled (after ${i}s): $(journal_since "$2" | grep 'answers now' | tail -1 | sed 's/\x1b\[[0-9;]*m//g' | grep -o 'vmm=.*' | cut -c1-70)"; else bad "$1: a Firecracker that did not answer at start-up was never asked again"; fi;;
    *) bad "$1: no start-up summary in the journal";;
  esac
}
T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_act rs-big sleep >/dev/null 2>&1 & ); sleep 0.5; restart_agent; sleep 2
say "  sleep cut short; start-up: $(took_over "$T0")"
asked_again "sleep cut short" "$T0"
not_frozen "sleep cut short" rs-big
no_garbage "sleep cut short"

T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_fork rs-big rs-bigkid >/dev/null 2>&1 & ); sleep 0.5; restart_agent; sleep 2
say "  fork cut short; start-up: $(took_over "$T0")"
asked_again "fork cut short" "$T0"
not_frozen "fork cut short (parent)" rs-big
case "$(vm_state rs-bigkid)" in
  "")      ok "fork cut short: no child record";;
  running) wait_exec rs-bigkid 60 && ok "fork cut short: the child completed and answers" || bad "fork cut short: child listed running but dead";;
  *)       bad "fork cut short: child left in state '$(vm_state rs-bigkid)'";;
esac
no_garbage "fork cut short"
vm_del rs-bigkid >/dev/null

# The same thing made certain, whatever the disk's speed: the guest is paused
# behind the agent's back (what a sleep or fork cut short leaves) and its
# Firecracker is frozen with SIGSTOP across the restart, so it cannot answer.
P_BIG=$(vm_pid rs-big); DIR_BIG="$(vm_dir rs-big)"
sudo curl -s -m 5 -o /dev/null --unix-socket "$DIR_BIG/fc.sock" -X PATCH -H 'Content-Type: application/json' -d '{"state":"Paused"}' http://localhost/vm
sudo kill -STOP "$P_BIG"
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 2
TO="$(took_over "$T0")"; say "  frozen Firecracker; start-up: $TO"
case "$TO" in *unreachable=1*) ok "frozen Firecracker: start-up went on without its answer (unreachable=1)";; *) bad "frozen Firecracker: expected unreachable=1 in the start-up summary";; esac
eq "frozen Firecracker: still listed running, same pid" "$(vm_state rs-big) $(vm_pid rs-big)" "running $P_BIG"
sudo kill -CONT "$P_BIG"
i=0; until journal_since "$T0" | grep -q 'answers now' || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
journal_since "$T0" | grep -q 'answers now' && ok "frozen Firecracker: asked again once it could answer (after ${i}s)" || bad "frozen Firecracker: never asked again"
eq "frozen Firecracker: its guest was resumed (the Firecracker says so)" "$(vmm_state rs-big)" Running
if ping_guest rs-big; then ok "frozen Firecracker: the guest answers on its network again"; else bad "frozen Firecracker: the guest does not answer ping"; fi
say "  exec after that pause and resume: $(exec_ok rs-big && echo works || echo 'fails (FLU-15: exec is dead after a pause and resume, restart or not)')"

eq "sleep rs-big (to completion)" "$(vm_act rs-big sleep)" 200
T0=$(date '+%Y-%m-%d %H:%M:%S'); ( vm_act rs-big wake >/dev/null 2>&1 & ); sleep 0.3; restart_agent; sleep 2
say "  wake cut short; start-up: $(took_over "$T0")"
not_frozen "wake cut short" rs-big
no_garbage "wake cut short"

# The caller hangs up in the middle of a sleep (feluccad does after 30 s; here
# after one). The sleep must still run to its end: cut off where the request
# was dropped, it left the guest paused, or dead, under a record saying running.
P_BIG=$(vm_pid rs-big)
api -o /dev/null -m 1 -X POST "$A/v1/vms/rs-big/sleep"
i=0; until [ "$(vm_state rs-big)" = sleeping ] || [ $i -ge 90 ]; do sleep 1; i=$((i+1)); done
eq "abandoned sleep request: the sleep still completed (after ${i}s)" "$(vm_state rs-big)" sleeping
i=0; until ! alive "$P_BIG" || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
alive "$P_BIG" && bad "abandoned sleep request: the Firecracker is still there" || ok "abandoned sleep request: the Firecracker is gone"
eq "wake rs-big" "$(vm_act rs-big wake)" 200
wait_exec rs-big 90 && ok "rs-big answers after the wake" || bad "rs-big does not answer after the wake"

# A wake that worked and was never written down. The real window is a few
# microseconds wide (guest resumed, record not yet replaced), so this puts the
# same thing on disk: a record that says "sleeping, no pid" next to a
# Firecracker whose guest is running. Start-up used to kill that guest and keep
# the record — pointing at a snapshot the guest's disk had already outrun.
P_BIG=$(vm_pid rs-big); UP_BIG="$(guest_uptime rs-big)"; DIR_BIG="$(vm_dir rs-big)"
sudo systemctl stop $UNIT
sudo cp -p "$DIR_BIG/meta.json" /var/tmp/felucca-lab-meta.big
sudo sh -c "jq -c '.state=\"sleeping\"|.pid=null' /var/tmp/felucca-lab-meta.big > '$DIR_BIG/meta.json'"
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 2
say "  unrecorded wake; start-up: $(took_over "$T0")"
alive "$P_BIG" && ok "unrecorded wake: the running guest was not killed" || bad "unrecorded wake: a guest that was awake was killed"
eq "unrecorded wake: adopted as running, same Firecracker" "$(vm_state rs-big) $(vm_pid rs-big)" "running $P_BIG"
eq "unrecorded wake: the record on disk now says so" "$(meta_field rs-big '"\(.state) \(.pid)"')" "running $P_BIG"
up="$(guest_uptime rs-big)"
if [ -n "$up" ] && [ -n "$UP_BIG" ] && [ "$up" -ge "$UP_BIG" ]; then ok "unrecorded wake: guest uptime kept counting (${UP_BIG}s -> ${up}s)"; else bad "unrecorded wake: uptime ${UP_BIG}s -> '${up}'"; fi
journal_since "$T0" | grep -q 'adopted as running' && ok "unrecorded wake: reported in the journal" || bad "unrecorded wake: not reported"
no_garbage "unrecorded wake"

# The other half: a Firecracker whose guest is NOT running next to a record
# that says asleep. The VMM is removed. The forged record has no
# snapshot-valid marker and rs-big's vmstate.bin went with its last wake
# (FLU-4, S3/S7/S9: a `sleeping` record whose snapshot files are not both there
# becomes `stopped`), so the VM comes back stopped, not asleep.
eq "pause rs-big" "$(vm_act rs-big pause)" 200
sudo systemctl stop $UNIT
sudo cp -p "$DIR_BIG/meta.json" /var/tmp/felucca-lab-meta.big
sudo sh -c "jq -c '.state=\"sleeping\"|.pid=null' /var/tmp/felucca-lab-meta.big > '$DIR_BIG/meta.json'"
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 2
say "  paused Firecracker beside a sleeping record; start-up: $(took_over "$T0")"
i=0; until ! alive "$P_BIG" || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
alive "$P_BIG" && bad "a Firecracker whose guest never ran again was left beside a sleeping record" || ok "a Firecracker whose guest is not running is removed"
eq "and the VM is stopped (its record had no complete snapshot behind it: FLU-4 S9)" "$(vm_state rs-big) $(vm_pid rs-big)" "stopped "
no_garbage "paused Firecracker beside a sleeping record"
sudo rm -f /var/tmp/felucca-lab-meta.big
vm_del rs-big >/dev/null

say "== 9. an adopted Firecracker dies =="
eq "create rs-die" "$(vm_create rs-die 256)" 201; wait_exec rs-die 150 || bad "rs-die never became exec-ready"
restart_agent
P=$(vm_pid rs-die); sudo kill -9 "$P"
i=0; until [ "$(vm_state rs-die)" = error ] || [ $i -ge 15 ]; do sleep 1; i=$((i+1)); done
eq "its record goes to error (after ${i}s; the sweep runs every 5 s)" "$(vm_state rs-die)" error
vm_del rs-die >/dev/null
# A pool VM is different: nobody owns it, so an `error` record of one would
# keep its slot, its tap and its rootfs copy for ever.
if [ "$POOL_TARGET" -gt 0 ] && [ -n "$(pooled_ids | head -1)" ]; then
  PV="$(pooled_ids | head -1)"; PVP="$(vm_pid "$PV")"; PVD="$(vm_dir "$PV")"
  sudo kill -9 "$PVP"
  i=0; until [ -z "$(vm_state "$PV")" ] || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
  eq "dead pool VM: its record is removed, not left in error (after ${i}s)" "$(vm_state "$PV")" ""
  sudo test -e "$PVD" && bad "dead pool VM: its directory is still there" || ok "dead pool VM: its directory was removed"
  sleep 12   # two refill ticks and more
  eq "dead pool VM: no replacement straight away (its pool rests)" "$(pooled_ids | wc -l | tr -d ' ')" 0
  restart_agent
  i=0; until [ -n "$(pooled_ids)" ] || [ $i -ge 40 ]; do sleep 2; i=$((i+1)); done
  [ -n "$(pooled_ids)" ] && ok "dead pool VM: the pool is refilled after an agent restart" || bad "dead pool VM: pool not refilled after the restart"
else
  skip "a pool VM that dies (no pooled VM on this worker)"
fi

say "== 10. damaged and missing records =="
eq "create rs-dmg" "$(vm_create rs-dmg 256)" 201; eq "create rs-orph" "$(vm_create rs-orph 256)" 201; eq "create rs-new" "$(vm_create rs-new 256)" 201
wait_exec rs-dmg 150 || bad "rs-dmg never became exec-ready"; wait_exec rs-orph 150 || bad "rs-orph never became exec-ready"; wait_exec rs-new 150 || bad "rs-new never became exec-ready"
P_DMG=$(vm_pid rs-dmg); P_ORPH=$(vm_pid rs-orph); DIR_DMG="$(vm_dir rs-dmg)"; DIR_ORPH="$(vm_dir rs-orph)"
P_NEW=$(vm_pid rs-new); DIR_NEW="$(vm_dir rs-new)"; SLOT_NEW="$(vm_slot rs-new)"
sudo systemctl stop $UNIT
sudo cp -p "$DIR_DMG/meta.json" /var/tmp/felucca-lab-meta.saved
sudo truncate -s 0 "$DIR_DMG/meta.json"            # what a crash mid-write used to leave
sudo mv "$DIR_ORPH/meta.json" /var/tmp/felucca-lab-meta.orph   # a create that never finished
# What an agent rolled back from a newer version finds: valid JSON, a state it does not know.
sudo cp -p "$DIR_NEW/meta.json" /var/tmp/felucca-lab-meta.new
sudo sh -c "jq -c '.state=\"hibernating\"' /var/tmp/felucca-lab-meta.new > '$DIR_NEW/meta.json'"
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 2
say "  start-up: $(took_over "$T0")"
alive "$P_NEW" && ok "unknown state in meta.json: its Firecracker was left running" || bad "unknown state in meta.json: its Firecracker was killed"
eq "unknown state in meta.json: no record" "$(vm_state rs-new)" ""
# Its tap is in use by a running guest: the next VM must get another slot.
eq "create rs-after while rs-new runs unmanaged" "$(vm_create rs-after 256)" 201
[ -n "$SLOT_NEW" ] && [ "$(vm_slot rs-after)" != "$SLOT_NEW" ] && ok "rs-after got slot $(vm_slot rs-after), not the unmanaged VM's slot $SLOT_NEW" || bad "rs-after was given slot '$(vm_slot rs-after)' (the unmanaged VM holds $SLOT_NEW)"
wait_exec rs-after 150 && ok "rs-after answers" || bad "rs-after does not answer"
alive "$P_NEW" && ok "the unmanaged VM is still running" || bad "the unmanaged VM died when rs-after was created"
# The control plane still knows rs-new and may delete it. That has to take the
# Firecracker with it: a VMM whose directory is gone is one no start can find.
eq "delete rs-new (no record, a Firecracker in its directory)" "$(vm_del rs-new)" 204
i=0; until ! alive "$P_NEW" || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
alive "$P_NEW" && bad "delete removed the directory and left the Firecracker running" || ok "delete killed the Firecracker no record held"
sudo test -e "$DIR_NEW" && bad "delete left the directory" || ok "delete removed the directory"
vm_del rs-after >/dev/null; sudo rm -f /var/tmp/felucca-lab-meta.new
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
link_admin_up felucca0 && bad "felucca0 is still up: $(ip -br link show felucca0 | tr -s ' ')" || ok "felucca0 is down"
if ping_guest rs-run; then bad "guest still reachable on a node that could not fence it"; else ok "guests are off the network while the agent cannot fence them"; fi
# The unit retries every 5 s. No attempt may put the guests back on the bridge
# before it knows the fences hold: each one used to raise the bridge first.
T1=$(date '+%Y-%m-%d %H:%M:%S'); UPS=0
for _ in $(seq 1 75); do link_admin_up felucca0 && UPS=$((UPS+1)); sleep 0.2; done
TRIES="$(journal_since "$T1" | grep -c 'refusing to serve tenants')"
[ "$TRIES" -ge 2 ] && ok "the agent tried $TRIES more times in 15 s" || bad "expected at least 2 more start attempts in 15 s, saw $TRIES"
eq "felucca0 was up in none of 75 samples taken across those attempts" "$UPS" 0
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
# Felucca's four tables, not the whole ruleset: a worker that also runs other
# things (the lab workers run k3s) has iptables-nft rules with packet counters
# in it, which never hash the same twice.
# (`nft -s`: stateless, without counter values. Since FLU-8 every chain the
# agent owns starts with a `counter comment "felucca:<hash>"` marker rule whose
# counts move with traffic, so a listing with counters changes between two
# looks although no rule changed.)
rules() { for t in "inet felucca_host" "ip felucca" "bridge felucca" "netdev felucca"; do sudo nft -s list table $t 2>/dev/null; done | sha256sum | cut -c1-16; }
states() { vms | jq -c '[.vms[]|{id,state,pid}]|sort_by(.id)'; }
R0="$(rules)"; S0="$(states)"; TAPS0="$(ip -o link | grep 'hth-' | sed 's/^[0-9]*: //' | cut -d' ' -f1,2,3 | sort | sha256sum | cut -c1-16)"
T0=$(date +%s)
OUT="$(sudo timeout 25 /usr/local/bin/felucca-agent --config "$CFG" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tail -60)"
TOOK=$(( $(date +%s) - T0 ))
printf '%s' "$OUT" | grep -q 'another felucca-agent holds' && ok "the second agent says who holds the data dir and exits" || bad "the second agent did not report the lock: $(printf '%s' "$OUT" | tail -3 | tr '\n' '|' | cut -c1-200)"
[ "$TOOK" -le 5 ] && ok "it exited at once (${TOOK}s), before doing anything" || bad "the second agent ran for ${TOOK}s"
printf '%s' "$OUT" | grep -q 'reconcile: took over' && bad "the second agent ran start-up against the live agent's sandboxes" || ok "it never ran start-up"
eq "the running agent is untouched" "$(agent_pid) $(systemctl is-active $UNIT)" "$AP active"
eq "every Firecracker kept its pid" "$(fcs)" "$FCS"
eq "no record changed state" "$(states)" "$S0"
eq "Felucca's nft tables are byte-for-byte what they were" "$(rules)" "$R0"
eq "no tap was touched" "$(ip -o link | grep 'hth-' | sed 's/^[0-9]*: //' | cut -d' ' -f1,2,3 | sort | sha256sum | cut -c1-16)" "$TAPS0"
check "exec still works" exec_ok rs-run
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
# The case only says something when a create really lands on a pool VM that
# is being drained, so it is repeated until one does (three rounds at most).
CLAIMED=0; BROKEN=0; ROUND=0
while [ "$CLAIMED" -eq 0 ] && [ "$ROUND" -lt 3 ]; do
  ROUND=$((ROUND+1))
  eq "round $ROUND: PUT /v1/pools (8 x 320 MiB)" "$(set_pools "[{${SHAPE},\"count\":8}]")" 200
  i=0; until [ "$(pool320)" -ge 8 ] || [ $i -ge 90 ]; do sleep 2; i=$((i+1)); done
  eq "round $ROUND: pool built" "$(pool320)" 8
  set_pools '[]' >/dev/null
  j=0; until [ "$(pool320)" -lt 8 ] || [ $j -ge 600 ]; do j=$((j+1)); done   # the drain has started
  LEFT="$(pool320)"
  rm -f /tmp/felucca-lab-rc.out
  for k in 1 2 3 4 5 6; do ( c=$(vm_create rc-$k 320); echo "$k $c" >> /tmp/felucca-lab-rc.out ) & done; wait
  say "  round $ROUND: drain under way ($LEFT of 8 pool VMs left) when 6 creates were sent"
  for k in 1 2 3 4 5 6; do
    [ "$(grep "^$k " /tmp/felucca-lab-rc.out | cut -d' ' -f2)" = 201 ] || continue
    dir="$(vm_dir rc-$k)"; case "$(basename "${dir:-x}")" in pool-*) CLAIMED=$((CLAIMED+1));; esac
    if [ -n "$dir" ] && sudo test -f "$dir/meta.json" && sudo test -S "$dir/fc.sock" && wait_exec rc-$k 150; then :; else BROKEN=$((BROKEN+1)); say "  rc-$k: state=$(vm_state rc-$k) dir=${dir:-MISSING}"; fi
  done
  rm -f /tmp/felucca-lab-rc.out
  cleanup_prefix rc-
  i=0; until [ "$(pool320)" -eq 0 ] || [ $i -ge 30 ]; do sleep 1; i=$((i+1)); done
done
[ "$CLAIMED" -gt 0 ] && ok "$CLAIMED creates were claimed from the pool while it was being torn down (round $ROUND)" || bad "no create overlapped the drain in $ROUND rounds: the case was not exercised"
eq "sandboxes that came back 201 and are not intact" "$BROKEN" 0

say "== 16. ten restarts: nothing accumulates =="
sleep 6; restart_agent; sleep 6
count() { echo "fc=$(fc_count) taps=$(tap_count) records=$(vm_count) dirs=$(dir_count) chains=$(chain_count)"; }
C0="$(count)"
for _ in 1 2 3 4 5 6 7 8 9 10; do restart_agent; sleep 2; done
sleep 6
eq "counts after ten restarts" "$(count)" "$C0"
no_garbage "ten restarts"
[ "$(systemctl show $UNIT -p Result --value)" != start-limit-hit ] && ok "never start-limit-hit" || bad "start-limit-hit"

say "== 16b. powering off every sandbox on the node (DEPLOYMENT.md) =="
eq "create rs-off" "$(vm_create rs-off 256)" 201; wait_exec rs-off 150 || bad "rs-off never became exec-ready"
sudo systemctl stop $UNIT
[ "$(fc_count)" -gt 0 ] && ok "stop alone leaves $(fc_count) Firecrackers running" || bad "no Firecracker left after stop"
sudo systemctl kill $UNIT
i=0; until [ "$(fc_count)" -eq 0 ] || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
eq "stop, then kill: no Firecracker left (after ${i}s)" "$(fc_count)" 0
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 2
say "  start-up: $(took_over "$T0")"
eq "after the power-off rs-off is stopped" "$(vm_state rs-off)" stopped
eq "start rs-off" "$(vm_act rs-off start)" 200
wait_exec rs-off 150 && ok "rs-off answers after a cold start" || bad "rs-off does not answer after the cold start"
no_garbage "power-off"
vm_del rs-off >/dev/null

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
