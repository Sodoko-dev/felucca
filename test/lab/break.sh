#!/usr/bin/env bash
# Break scenarios for a worker — runs ON a lab worker (see scripts/verify-break.sh).
#
# Where test/lab/restart.sh asserts what a restart must not cost, this script
# goes looking for trouble around it: tenant rules across restarts, an agent
# killed every few seconds under load, many sandboxes, a full disk, every nft
# table deleted, the documented power-off, a binary rolled back under the new
# unit, a record that cannot be written, an agent that dies the instant a
# create is committed. Several sections print what they observed without
# judging it (lines starting "  =>" or "  note:"): that output is the evidence
# attached to FLU-11, FLU-28, FLU-61 and FLU-70.
#
#   break.sh [all | "B1 B5 ..."]
#
# Every sandbox it creates is named bk-*; it fills the data disk for a moment
# (B4) and swaps the agent binary for an older one and back (B7; skipped
# unless an older agent binary is on the worker at $PREV_AGENT_BIN, default
# /usr/local/bin/felucca-agent.pre-w0). Do not run it on a worker that holds
# anything you care about.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
PREV="${PREV_AGENT_BIN:-/usr/local/bin/felucca-agent.pre-w0}"
SECTIONS="${1:-all}"
want() { [ "$SECTIONS" = all ] || case " $SECTIONS " in *" $1 "*) return 0;; *) return 1;; esac; }
metaless_dirs() { sudo sh -c "for d in $D/*/; do [ -e \"\$d\" ] || continue; [ -f \"\${d}meta.json\" ] || basename \"\$d\"; done" | tr '\n' ' ' | sed 's/ $//'; }
took_over() { journal_since "$1" | grep 'reconcile: took over' | tail -1 | sed 's/\x1b\[[0-9;]*m//g; s/.*took over what the previous agent left *//'; }
no_garbage() { eq "$1: no Firecracker without a record" "$(orphans | wc -l | tr -d ' ')" 0; eq "$1: no directory without meta.json" "$(metaless_dirs)" ""; }
mk() { api -o /dev/null -w '%{http_code}' -m 180 -X POST -d "{\"id\":\"$1\",\"name\":\"bk\",\"vcpus\":1,\"mem_mib\":${2:-256}${3:+,\"tenant_id\":\"$3\"}}" "$A/v1/vms"; }
gping() { guest "$1" "ping -c2 -W1 $2 >/dev/null 2>&1 && echo yes || echo no" | tr -d '\n'; }
cleanup_prefix bk-

if want B1; then
say "== B1. tenant isolation, exposes and anti-spoof chains across a restart =="
eq "create bk-a (tenant t1)" "$(mk bk-a 256 t1)" 201; eq "create bk-b (tenant t2)" "$(mk bk-b 256 t2)" 201; eq "create bk-c (tenant t1)" "$(mk bk-c 256 t1)" 201
for v in bk-a bk-b bk-c; do wait_exec $v 150 || bad "$v not exec-ready"; done
IPB=$(vm_ip bk-b); IPC=$(vm_ip bk-c)
eq "before: bk-a -> bk-b (other tenant) blocked" "$(gping bk-a $IPB)" no
eq "before: bk-a -> bk-c (same tenant) allowed" "$(gping bk-a $IPC)" yes
NP=$(api -m 15 -X POST -d '{"guest_port":8000}' "$A/v1/vms/bk-a/expose" | jq -r '.node_port // empty')
[ -n "$NP" ] && ok "bk-a:8000 exposed on node port $NP" || bad "expose failed"
eq "before: ingress rule present" "$(sudo nft list chain ip felucca ingress 2>/dev/null | grep -c "dport $NP")" 1
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 2
eq "after restart: bk-a -> bk-b (other tenant) still blocked" "$(gping bk-a $IPB)" no
eq "after restart: bk-a -> bk-c (same tenant) still allowed" "$(gping bk-a $IPC)" yes
eq "after restart: ingress rule present" "$(sudo nft list chain ip felucca ingress 2>/dev/null | grep -c "dport $NP")" 1
eq "after restart: expose still on the record on disk" "$(meta_field bk-a '.exposes[0].node_port // empty')" "$NP"
eq "after restart: one anti-spoof chain per tap" "$(chain_count)" "$(tap_count)"
kill9_agent; sleep 2
eq "after kill -9: other tenant still blocked" "$(gping bk-a $IPB)" no
eq "after kill -9: ingress rule present" "$(sudo nft list chain ip felucca ingress 2>/dev/null | grep -c "dport $NP")" 1
cleanup_prefix bk-
fi

if want B2; then
say "== B2. agent killed every 3 s while sandboxes are being created =="
rm -f /tmp/felucca-lab-bk-create.out
( for i in $(seq 1 12); do c=$(mk bk-s$i 256); echo "$i $c" >> /tmp/felucca-lab-bk-create.out; sleep 0.5; done ) &
CRE=$!
# MainPID is 0 between two starts, and `kill -9 0` would be this script's own process group.
for _ in 1 2 3 4 5 6; do sleep 3; AP="$(agent_pid)"; [ "${AP:-0}" -gt 1 ] && sudo kill -9 "$AP" 2>/dev/null; done
wait $CRE; wait_up; sleep 8
OKS=$(grep -c ' 201$' /tmp/felucca-lab-bk-create.out); say "  creates: $(sort -n /tmp/felucca-lab-bk-create.out | awk '{printf "%s ", $2}')"
DEAD=0
for i in $(sort -n /tmp/felucca-lab-bk-create.out | awk '$2==201{print $1}'); do
  if [ "$(vm_state bk-s$i)" = running ] && wait_exec bk-s$i 150; then :; else DEAD=$((DEAD+1)); say "  bk-s$i: 201 but state=$(vm_state bk-s$i)"; fi
done
[ "$OKS" -gt 0 ] && ok "$OKS creates answered 201 between the kills" || say "  note: no create got through"
eq "sandboxes that answered 201 and are not running and answering" "$DEAD" 0
no_garbage "kill storm"
say "  records now: $(vms | jq -c '[.vms[]|select(.id|startswith("bk-s"))|.state]|group_by(.)|map({(.[0]):length})|add')"
cleanup_prefix bk-; rm -f /tmp/felucca-lab-bk-create.out
fi

if want B3; then
say "== B3. 15 sandboxes: how long a restart takes, and a double restart =="
for i in $(seq 1 15); do mk bk-m$i 256 >/dev/null & [ $((i % 5)) -eq 0 ] && wait; done; wait
for i in $(seq 1 15); do wait_exec bk-m$i 240 || bad "bk-m$i not exec-ready"; done
FC0="$(fcs)"; N0=$(fc_count)
T0=$(date '+%Y-%m-%d %H:%M:%S'); S=$(date +%s.%N); sudo systemctl restart $UNIT; wait_up; E=$(date +%s.%N)
say "  restart -> /healthz with $N0 Firecrackers: $(echo "$E - $S" | bc | cut -c1-5) s; start-up: $(took_over "$T0")"
eq "15 sandboxes: every Firecracker kept its pid" "$(fcs)" "$FC0"
for _ in 1 2 3 4 5; do ( sudo systemctl restart $UNIT & sudo systemctl restart $UNIT; wait ); done; wait_up; sleep 3
eq "five double restarts: every Firecracker kept its pid" "$(fcs)" "$FC0"
BADX=0; for i in $(seq 1 15); do exec_ok bk-m$i || BADX=$((BADX+1)); done
eq "sandboxes that no longer answer" "$BADX" 0
no_garbage "double restarts"
fi

if want B4; then
say "== B4. disk full =="
[ -n "$(vm_state bk-m1)" ] || { mk bk-m1 256 >/dev/null; wait_exec bk-m1 150; }
FREE_KB=$(df --output=avail "$DATA_DIR" | tail -1 | tr -d ' ')
sudo fallocate -l $(( (FREE_KB - 20000) * 1024 )) "$DATA_DIR/FILL" && ok "filled $DATA_DIR ($(df -h --output=avail "$DATA_DIR" | tail -1 | tr -d ' ') left)" || bad "could not fill the disk"
P1=$(vm_pid bk-m1)
R=$(api -m 60 -X POST "$A/v1/vms/bk-m1/sleep" | cut -c1-120); say "  sleep with no disk space: $R"
say "  after the failed sleep: record=$(vm_state bk-m1) pid_alive=$(alive "$P1" && echo yes || echo no) vmm=$(sudo curl -s -m3 --unix-socket "$(vm_dir bk-m1)/fc.sock" http://localhost/ | jq -r .state) exec=$(exec_ok bk-m1 && echo works || echo FAILS)"
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 2
say "  agent restarted on a full disk: active=$(systemctl is-active $UNIT); start-up: $(took_over "$T0")"
say "  after that restart: record=$(vm_state bk-m1) vmm=$(sudo curl -s -m3 --unix-socket "$(vm_dir bk-m1)/fc.sock" http://localhost/ | jq -r .state) exec=$(exec_ok bk-m1 && echo works || echo FAILS)"
eq "full disk: every other sandbox still listed running" "$(vms | jq '[.vms[]|select(.id|startswith("bk-m"))|select(.state!="running")]|length')" "$([ "$(vm_state bk-m1)" = running ] && echo 0 || echo 1)"
sudo rm -f "$DATA_DIR/FILL"
restart_agent; sleep 2
say "  disk freed, restarted: bk-m1 record=$(vm_state bk-m1) exec=$(exec_ok bk-m1 && echo works || echo FAILS)"
no_garbage "disk full"
fi

if want B5; then
say "== B5. the agent's and the host's nft tables are deleted while sandboxes run =="
[ -n "$(vm_state bk-m1)" ] || { mk bk-m1 256 >/dev/null; wait_exec bk-m1 150; }
FC0="$(fcs)"
for t in "inet felucca_host" "ip felucca" "bridge felucca" "netdev felucca"; do sudo nft delete table $t 2>/dev/null; done
say "  tables gone; agent still $(systemctl is-active $UNIT) (nothing re-checks at runtime: FLU-8)"
sudo systemctl restart $UNIT
i=0; until [ "$(systemctl is-active $UNIT)" = active ] && wait_up 2 || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
eq "tables deleted, agent restarted: it is back by itself (the unit reloads the host firewall)" "$(systemctl is-active $UNIT)" active
sudo nft list chain inet felucca_host input 2>/dev/null | grep -q 'iifname "felucca0" drop' && ok "the host fence is loaded again" || bad "no host fence after the restart"
eq "sandboxes kept their pids" "$(fcs)" "$FC0"
link_admin_up felucca0 && ok "felucca0 is up" || bad "felucca0 is down"
for t in "ip felucca" "bridge felucca" "netdev felucca"; do sudo nft list table $t >/dev/null 2>&1 && ok "agent rebuilt table $t" || bad "table $t missing"; done
eq "one anti-spoof chain per tap again" "$(chain_count)" "$(tap_count)"
check "exec works" exec_ok bk-m1
fi

if want B6; then
say "== B6. the documented power-off, then start =="
N0=$(fc_count); POOLED0=$(pooled_ids | wc -l | tr -d ' ')
sudo systemctl stop $UNIT; sudo systemctl kill $UNIT; sleep 3
eq "stop + kill: no Firecracker left (were $N0)" "$(fc_count)" 0
T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl start $UNIT; wait_up; sleep 3
say "  start-up: $(took_over "$T0")"
say "  records: $(vms | jq -c '[.vms[].state]|group_by(.)|map({(.[0]):length})|add')  (dead pool VMs before: $POOLED0)"
no_garbage "power-off"
eq "a powered-off sandbox cold-starts" "$(vm_act bk-m2 start)" 200
wait_exec bk-m2 150 && ok "and answers" || bad "bk-m2 does not answer after start"
fi

if want B7; then
say "== B7. binary rolled back under the new unit, then forward again =="
if [ ! -x "$PREV" ]; then skip "binary rolled back under the new unit (no previous agent binary at $PREV)"; else
  cleanup_prefix bk-; sleep 8
  sudo cp -p /usr/local/bin/felucca-agent /usr/local/bin/felucca-agent.break-keep
  i=0; until [ -n "$(pooled_ids)" ] || [ $i -ge 30 ]; do sleep 2; i=$((i+1)); done
  P0=$(vm_pid "$(pooled_ids | head -1)"); say "  pool VM pid $P0; firecrackers: $(fcs)"
  sudo install -m0755 "$PREV" /usr/local/bin/felucca-agent
  sudo systemctl restart $UNIT; wait_up; sleep 10
  say "  OLD binary under the new unit: firecrackers: $(fcs); orphans: $(orphans | tr '\n' ' '); records: $(vms | jq -c '[.vms[]|"\(.state):\(.pid)"]')"
  alive "$P0" && [ -n "$(orphans)" ] && ok "as documented: the old agent orphans the surviving pool VM (pid $P0 runs, no record owns it)" || say "  note: no orphan observed"
  sudo install -m0755 /usr/local/bin/felucca-agent.break-keep /usr/local/bin/felucca-agent
  T0=$(date '+%Y-%m-%d %H:%M:%S'); sudo systemctl restart $UNIT; wait_up; sleep 12
  say "  NEW binary again: start-up: $(took_over "$T0")"
  say "  firecrackers: $(fcs); records: $(vms | jq -c '[.vms[]|"\(.state):\(.pid)"]')"
  no_garbage "rolled forward"
  eq "pool is back at its target" "$(pooled_ids | wc -l | tr -d ' ')" "$(sudo sed -n 's/.*"pool_size"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' /etc/felucca/felucca-agent.json | head -1)"
  sudo rm -f /usr/local/bin/felucca-agent.break-keep
fi
fi

if want B8; then
say "== B8. the record cannot be written (immutable meta.json) =="
cleanup_prefix bk-w
eq "create bk-w" "$(mk bk-w 256)" 201; wait_exec bk-w 150 || bad "bk-w not exec-ready"
eq "sleep bk-w" "$(vm_act bk-w sleep)" 200
DW="$(vm_dir bk-w)"; sudo chattr +i "$DW/meta.json"
C1=$(vm_act bk-w wake); C2=$(vm_act bk-w wake)
say "  wake with an immutable record: first $C1, second $C2 (state $(vm_state bk-w), on disk $(sudo jq -r .state "$DW/meta.json"))"
PW=$(vm_pid bk-w)
# (a) the agent is restarted while the record still cannot be written
T0=$(date '+%Y-%m-%d %H:%M:%S'); restart_agent; sleep 2
say "  restart with the stale record on disk: $(took_over "$T0")"
alive "$PW" && ok "the awake guest was not killed by the restart" || bad "the awake guest was killed (record said sleeping)"
eq "it is adopted as running, same Firecracker" "$(vm_state bk-w) $(vm_pid bk-w)" "running $PW"
# (b) the record becomes writable again: the agent catches up on its own
sudo chattr -i "$DW/meta.json"
i=0; until [ "$(sudo jq -r .state "$DW/meta.json")" = running ] || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
eq "writable again: the record on disk caught up by itself (after ${i}s)" "$(sudo jq -r '"\(.state) \(.pid)"' "$DW/meta.json")" "running $PW"
journal_since "$T0" | grep -q 'meta.json written' && ok "journal: the owed write is reported" || bad "journal: no report of the late write"
wait_exec bk-w 60 && ok "bk-w answers" || bad "bk-w does not answer"
# the same for a cold start
eq "stop bk-w" "$(vm_act bk-w stop)" 200
sudo chattr +i "$DW/meta.json"
C1=$(vm_act bk-w start); C2=$(vm_act bk-w start); PW=$(vm_pid bk-w)
say "  start with an immutable record: first $C1, second $C2 (on disk $(sudo jq -r .state "$DW/meta.json"))"
sudo chattr -i "$DW/meta.json"
i=0; until [ "$(sudo jq -r .state "$DW/meta.json")" = running ] || [ $i -ge 20 ]; do sleep 1; i=$((i+1)); done
eq "start: the record on disk caught up (after ${i}s)" "$(sudo jq -r '"\(.state) \(.pid)"' "$DW/meta.json")" "running $PW"
restart_agent; sleep 2
eq "start: still running after a restart, same Firecracker" "$(vm_state bk-w) $(vm_pid bk-w)" "running $PW"
no_garbage "immutable record"
cleanup_prefix bk-w
fi

if want B9; then
say "== B9. the agent dies the instant a create is committed (evidence for the control-plane issue) =="
cleanup_prefix bk-c
( sudo sh -c "i=0; until [ -f $D/bk-c1/meta.json ] || [ \$i -ge 3000000 ]; do i=\$((i+1)); done; systemctl kill -s KILL --kill-whom=main $UNIT" ) &
C=$(mk bk-c1 384); wait; wait_up; sleep 3
say "  create answered: '$C'; after the restart bk-c1 is: $(vm_state bk-c1) pid $(vm_pid bk-c1)"
[ "$C" != 201 ] && [ "$(vm_state bk-c1)" = running ] && say "  => a caller that was told the create failed has a running VM on the node (the control plane records such a sandbox as 'error')"
no_garbage "create committed, never answered"
cleanup_prefix bk-c
fi

cleanup_prefix bk-
summary
