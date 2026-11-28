#!/usr/bin/env bash
# Lab gate for FLU-8: the agent's isolation watch — runs ON a lab worker (see
# scripts/verify-isolation.sh, which runs it in parts and does the control
# plane VM's half of C3 and C10).
#
#   isolation.sh [all | "C1 C2 C3a C3b C4 ... C10a C10b END"]
#
# Three sandboxes stay up for the whole run: is-x and is-z of tenant is-tx and
# is-y of tenant is-ty. Guest is-x probes, each every 0.5 s with a 1 s limit
# and one line per attempt in /root/probe.log (started once through exec):
#   y     ping is-y            (other tenant: must stay blocked)
#   z     ping is-z            (same tenant: must work)
#   h9090 tcp to the worker's agent port on the guest bridge  (must stay blocked)
#   h22   tcp to the worker's ssh port on the guest bridge    (must stay blocked)
#   eg    ping an internet address, or the control plane when the lab has no egress (must work)
# Each case breaks the worker's Felucca rules on purpose, only in the ways the
# case names, on the tables `ip|bridge|netdev felucca` and `inet felucca_host`;
# each ends by checking that everything is back: the three tables as the agent
# builds them (stateless listing equal to the one taken in C1), the host table
# loaded, felucca0 administratively up, /healthz {"ok":true,"isolation":true}.
# C3a leaves the kernel refusing the repair (the bridge down) for the wrapper's
# curl from the control-plane VM; C3b puts it back. C3a also arms a detached
# undo that deletes the hookless chain after 300 s if C3b never ran; C10a/C10b
# likewise. C7 sets the watch to `report` with a drop-in and removes it again.
# END deletes everything is-* and compares the counts with the first part's.
#
# State shared between the parts lives in /var/tmp/felucca-lab-isolation.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"
CASES="${1:-all}"
want() { [ "$CASES" = all ] || case " $CASES " in *" $1 "*) return 0;; *) return 1;; esac; }
W=/var/tmp/felucca-lab-isolation; mkdir -p "$W"
CFG=/etc/felucca/felucca-agent.json
DROPIN_DIR=/etc/systemd/system/felucca-agent.service.d
DROPIN="$DROPIN_DIR/lab-test-isolation-report.conf"
GOOD='{"ok":true,"isolation":true}'
MARK='^counter packets [0-9]+ bytes [0-9]+ comment "felucca:[0-9a-f]{16}"$'

# ---- the control plane (only C10 talks to it) --------------------------------
cp_from_journal() { sudo journalctl -u "$UNIT" -b --no-pager -o cat 2>/dev/null | sed 's/\x1b\[[0-9;]*m//g' | sed -n 's/.*felucca-agent startup control_plane=\([^ ]*\).*/\1/p' | tail -1; }
cp_from_wg() { sudo test -f "${DATA_DIR%/}/wg.json" && sudo jq -r 'select(.server_overlay_ip and .api_port) | "http://\(.server_overlay_ip):\(.api_port)"' "${DATA_DIR%/}/wg.json" 2>/dev/null; }
cp_from_file() { sudo sed -n 's/.*"control_plane"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" | head -1; }
CP_URL=""; for src in journal wg file; do CP_URL="$(cp_from_$src)"; [ -n "$CP_URL" ] && break; done
CP_HOST="$(printf '%s' "$CP_URL" | sed -E 's#^[a-z]+://##; s#[:/].*$##')"

# ---- small helpers -------------------------------------------------------------
now() { date +%s.%N | cut -c1-14; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN{printf "%.1f", b-a}'; }
hz() { curl -s -m 2 "$A/healthz"; }
jsince() { sudo journalctl -u "$UNIT" --since "@${1%.*}" --no-pager -o cat 2>/dev/null | sed 's/\x1b\[[0-9;]*m//g'; }
nftq() { sudo nft "$@" 2>&1; }
first_rule() { # <family> <chain> -> the chain's first rule ("" when the chain is not there)
  sudo nft -t list chain "$1" felucca "$2" 2>/dev/null | awk '/^\t\t(type |policy )/{next} /^\t\t[^\t]/{r=$0; sub(/^\t\t/,"",r); print r; exit}'; }
has_marker() { first_rule "$1" "$2" | grep -Eq "$MARK"; }
bridge_up() { link_admin_up felucca0; }
switch_val() { cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null; }
host_table() { sudo nft list chain inet felucca_host input 2>/dev/null | grep -c 'iifname "felucca0" drop'; }
# waitt <limit-s> <command...> — poll every 0.25 s; WT = seconds until it held (or the limit)
waitt() { local lim="$1" t0; shift; t0="$(now)"; while :; do if "$@" >/dev/null 2>&1; then WT="$(since "$t0")"; return 0; fi
  if awk -v a="$t0" -v b="$(now)" -v l="$lim" 'BEGIN{exit !(b-a>=l)}'; then WT="$(since "$t0")"; return 1; fi; sleep 0.25; done; }
hz_true()  { [ "$(hz)" = "$GOOD" ]; }
hz_false() { hz | grep -q '"isolation":false'; }
# The three Felucca tables, stateless and terse, one line per chain, sorted: what C1 saw and every case must give back.
tables_fp() { local f; for f in ip bridge netdev; do sudo nft -s -t list table $f felucca 2>/dev/null | awk -v f=$f '
  /^\tchain /{c=$0; body=""; inb=1; next} inb && /^\t}/{print f" "c" |"body; inb=0; next} inb{body=body" "$0; next}
  /^\tset /{print f" "$0}'; done | tr -s '\t' ' ' | sort; }
# <id> <json body> -> "<code> <body>"
post() { local r; r="$(api -m 180 -w '\n%{http_code}' -X POST ${2:+-d "$2"} "$A$1")"; printf '%s %s' "$(printf '%s' "$r" | tail -n1)" "$(printf '%s' "$r" | sed '$d' | tr -d '\n' | cut -c1-300)"; }
mkvm() { post /v1/vms "{\"id\":\"$1\",\"name\":\"lab\",\"vcpus\":1,\"mem_mib\":256${2:+,\"tenant_id\":\"$2\"}}"; }
zoo_pids() { printf '%s %s %s' "$(vm_pid is-x)" "$(vm_pid is-y)" "$(vm_pid is-z)"; }
# A background sampler of felucca0's UP flag (and /healthz and the switch) every 0.25 s into <file>: "<t> up|down <switch> <isolation>"
sampler_on() { SAMPF="$1"; : > "$SAMPF"; ( while [ -e "$SAMPF" ]; do i="$(curl -s -m 1 "$A/healthz" | sed -n 's/.*"isolation":\([a-z]*\).*/\1/p')"
  printf '%s %s %s %s\n' "$(now)" "$(link_admin_up felucca0 && echo up || echo down)" "$(switch_val)" "${i:-none}" >> "$SAMPF.d"; sleep 0.25; done ) & SAMP=$!; }
sampler_off() { rm -f "$SAMPF"; wait "$SAMP" 2>/dev/null; mv -f "$SAMPF.d" "$SAMPF" 2>/dev/null; }
downs() { awk '$2=="down"' "$1" | wc -l | tr -d ' '; }

# ---- the probes in guest is-x --------------------------------------------------
PROBE_SH='#!/bin/bash
# probe.sh <y> <z> <host> <egress> — one line per attempt: "<epoch> <target> ok|no"
p() { local n="$1"; shift; while :; do if timeout 1 "$@" >/dev/null 2>&1; then r=ok; else r=no; fi; echo "$(date +%s.%N | cut -c1-14) $n $r" >> /root/probe.log; sleep 0.5; done; }
p y ping -c1 -W1 "$1" & p z ping -c1 -W1 "$2" &
p h9090 bash -c "echo > /dev/tcp/$3/9090" & p h22 bash -c "echo > /dev/tcp/$3/22" &
p eg ping -c1 -W1 "$4" &
wait'
goff() { # host clock minus guest is-x's clock, seconds
  local a b g; a="$(now)"; g="$(guest is-x 'date +%s.%N | cut -c1-14' | tr -d '\n')"; b="$(now)"
  awk -v a="$a" -v b="$b" -v g="$g" 'BEGIN{printf "%.3f", (a+b)/2-g}'; }
# plines <host t0> <host t1> -> the probe lines of that window, times converted to host time
plines() { local o; o="$(goff)"; guest is-x "awk -v a=$(awk -v t="$1" -v o="$o" 'BEGIN{printf "%.3f", t-o}') -v b=$(awk -v t="$2" -v o="$o" 'BEGIN{printf "%.3f", t-o}') '\$1>=a && \$1<=b' /root/probe.log" | awk -v o="$o" '{printf "%.3f %s %s\n", $1+o, $2, $3}'; }
# probes <label> <t break> <t repaired> <t end> — the leaked probes must all be before <t repaired>+1.5 s;
# after that, y/h9090/h22 all blocked and z/eg all through.
probes() {
  local l="$1" tb="$2" tr="$3" te="$4" f="$W/probes-$1.txt" n tgt
  plines "$(awk -v t="$tb" 'BEGIN{printf "%.3f", t-1}')" "$te" > "$f"
  for tgt in y h9090 h22; do
    n="$(awk -v x=$tgt '$2==x && $3=="ok"' "$f" | wc -l | tr -d ' ')"
    say "  $l: $tgt got through $n of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' ')$( [ "$n" -gt 0 ] && awk -v x=$tgt -v b="$tb" '$2==x && $3=="ok"{if(f=="")f=$1; l=$1} END{printf " (from +%.1f s to +%.1f s after the break)", f-b, l-b}' "$f")"
    eq "$l: $tgt blocked again after the repair (attempts through later than +1.5 s after it)" "$(awk -v x=$tgt -v r="$tr" '$2==x && $3=="ok" && $1>r+1.5' "$f" | wc -l | tr -d ' ')" 0
  done
  for tgt in z eg; do
    say "  $l: $tgt failed $(awk -v x=$tgt '$2==x && $3=="no"' "$f" | wc -l | tr -d ' ') of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' ')"
    eq "$l: $tgt works again (last attempt after the repair)" "$(awk -v x=$tgt -v r="$tr" '$2==x && $1>r+1.5{s=$3} END{print s}' "$f")" ok
  done
}
# blocked_all <label> <t0> <t1> — nothing at all got through in the window (bridge down)
blocked_all() { local f="$W/probes-$1.txt" tgt; plines "$2" "$3" > "$f"
  for tgt in y z h9090 h22 eg; do eq "$1: $tgt through while the bridge is down (of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' '))" "$(awk -v x=$tgt '$2==x && $3=="ok"' "$f" | wc -l | tr -d ' ')" 0; done; }

# ---- set-up and the end check of every case -------------------------------------
zoo() {
  if [ ! -f "$W/start-counts" ]; then printf '%s %s %s %s %s\n' "$(vm_count)" "$(dir_count)" "$(fc_count)" "$(tap_count)" "$(chain_count)" > "$W/start-counts"; fi
  if [ "$(vm_state is-x)" = running ] && [ "$(vm_state is-y)" = running ] && [ "$(vm_state is-z)" = running ] && [ -f "$W/zoo" ]; then . "$W/zoo"; return 0; fi
  cleanup_prefix is-
  local r; r="$(mkvm is-x is-tx)"; eq "set-up: create is-x (tenant is-tx)" "${r%% *}" 201
  r="$(mkvm is-y is-ty)"; eq "set-up: create is-y (tenant is-ty)" "${r%% *}" 201
  r="$(mkvm is-z is-tx)"; eq "set-up: create is-z (tenant is-tx)" "${r%% *}" 201
  local v; for v in is-x is-y is-z; do wait_exec $v 150 || bad "set-up: $v not exec-ready"; done
  YIP="$(vm_ip is-y)"; ZIP="$(vm_ip is-z)"; XIP="$(vm_ip is-x)"; XT="hth-$(vm_slot is-x)"; YT="hth-$(vm_slot is-y)"; ZT="hth-$(vm_slot is-z)"
  HIP="$(ip -4 -o addr show dev felucca0 | awk '{print $4}' | cut -d/ -f1)"
  EG=1.1.1.1; [ "$(guest is-x "ping -c2 -W2 $EG >/dev/null 2>&1 && echo yes")" = yes ] || EG="$CP_HOST"
  say "  set-up: is-x $XIP ($XT), is-y $YIP ($YT), is-z $ZIP ($ZT); host $HIP; egress target $EG"
  local b64; b64="$(printf '%s' "$PROBE_SH" | base64 -w0)"
  guest is-x "echo $b64 | base64 -d > /root/probe.sh; rm -f /root/probe.log; setsid nohup bash /root/probe.sh $YIP $ZIP $HIP $EG >/dev/null 2>&1 < /dev/null & echo started" >/dev/null
  local srv; srv="$(printf '%s' 'import socket
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",8069)); s.listen(64)
while True:
    c,_=s.accept()
    try:
        c.settimeout(5); c.recv(65536); c.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nis-x\n")
    except Exception: pass
    finally: c.close()
' | base64 -w0)"
  guest is-x "echo $srv | base64 -d > /root/srv.py; setsid nohup python3 /root/srv.py >/dev/null 2>&1 < /dev/null & echo ok" >/dev/null
  r="$(post /v1/vms/is-x/expose '{"guest_port":8069}')"; NP="$(printf '%s' "${r#* }" | jq -r '.node_port // empty' 2>/dev/null)"
  [ -n "$NP" ] && ok "set-up: is-x:8069 exposed on node port $NP" || bad "set-up: expose is-x:8069 — got '$r'"
  sleep 3
  eq "set-up: the probe loop writes" "$(guest is-x 'test -s /root/probe.log && echo yes')" yes
  printf 'YIP=%s\nZIP=%s\nXIP=%s\nXT=%s\nYT=%s\nZT=%s\nHIP=%s\nEG=%s\nNP=%s\n' "$YIP" "$ZIP" "$XIP" "$XT" "$YT" "$ZT" "$HIP" "$EG" "$NP" > "$W/zoo"
  tables_fp > "$W/tables-base"
}
# back <label> — everything is as the agent builds it
back() {
  waitt 15 hz_true; eq "$1: back: /healthz" "$(hz)" "$GOOD"
  eq "$1: back: felucca0 administratively up" "$(bridge_up && echo up || echo down)" up
  eq "$1: back: host table inet felucca_host loaded" "$(host_table)" 1
  local d; d="$(diff <(cat "$W/tables-base") <(tables_fp) | grep -c '^[<>]')"
  eq "$1: back: the three Felucca tables as the agent builds them (stateless listing as in C1; differing lines)" "$d" 0
  [ "$d" -gt 0 ] && diff <(cat "$W/tables-base") <(tables_fp) | head -8 | cut -c1-240 | sed 's/^/    /'
  eq "$1: back: the bridge-nf-call-iptables switch" "$(switch_val)" 1
}
# journal_names <label> <t> <chain...> — a drift line and a "repaired and confirmed" line for each chain
journal_names() { local l="$1" t="$2" c j n; shift 2; j="$(jsince "$t")"
  for c in "$@"; do
    n="$(printf '%s\n' "$j" | grep 'isolation watch: drift' | grep -Ec "chain=\"?$c\"?( |$)")"
    [ "$n" -ge 1 ] && ok "$l: journal: drift line naming $c ($n)" || bad "$l: journal: drift line naming $c — got 0 lines"
    n="$(printf '%s\n' "$j" | grep 'chain repaired and confirmed' | grep -Ec "chain=\"?$c\"?( |$)")"
    [ "$n" -ge 1 ] && ok "$l: journal: 'chain repaired and confirmed' naming $c ($n)" || bad "$l: journal: 'chain repaired and confirmed' naming $c — got 0 lines"
  done; }
# arm_undo <name> — if the next part never comes, delete the hookless input chain after 300 s (the agent rebuilds it)
arm_undo() { touch "$W/held-$1"; ( setsid nohup bash -c "sleep 300; [ -e $W/held-$1 ] && sudo nft delete chain ip felucca input && rm -f $W/held-$1" >/dev/null 2>&1 < /dev/null & ) ; }
refuse() { printf 'flush chain ip felucca input\ndelete chain ip felucca input\nadd chain ip felucca input\n' | sudo nft -f - 2>&1; }

# ---- C2: drift that is repaired ---------------------------------------------------
# c2 <label> <nft commands, one per line> <family:chain ...>
c2() {
  local l="$1" cmds="$2" chains="$3" t tr te p0 fc ok_all=1 fam c names=""
  say "-- $l: $(printf '%s' "$cmds" | tr '\n' ';')"
  p0="$(zoo_pids)"; sampler_on "$W/samp-$l"; sleep 1
  t="$(now)"; out="$(printf '%s\n' "$cmds" | sudo nft -f - 2>&1)"; [ -n "$out" ] && say "  nft: $out"
  rep() { local x; for x in $chains; do has_marker "${x%%:*}" "${x#*:}" || return 1; done; }
  if waitt 10 rep; then ok "$l: back with its marker within 10 s (${WT} s)"; else bad "$l: back with its marker within 10 s — got not back after ${WT} s"; fi
  tr="$(now)"
  for x in $chains; do names="$names ${x#*:}"; done
  sleep 5; te="$(now)"; sampler_off
  journal_names "$l" "$t" $names
  probes "$l" "$t" "$tr" "$te"
  eq "$l: the running guests kept their Firecracker pids" "$(zoo_pids)" "$p0"
  eq "$l: felucca0 never down (samples, of $(wc -l < "$W/samp-$l"))" "$(downs "$W/samp-$l")" 0
}
NETDEV_CHAINS() { local s; for s in $(vms | jq -r '.vms[].id' | while read -r id; do vm_slot "$id"; done | sort -n); do printf 'netdev:as_hth_%s ' "$s"; done; }

# =====================================================================================
say "== isolation gate on $(hostname) $(date '+%F %T'): agent $(sudo sha256sum /usr/local/bin/felucca-agent | cut -c1-16) pid $(agent_pid), cases: $CASES"
say "  load $(cut -d' ' -f1-3 /proc/loadavg); control plane ${CP_URL:-unknown}"
preflight_unit || { summary; exit 1; }
zoo

if want C1; then
  say "== C1. baseline (20 s of probes)"
  t="$(now)"; sleep 20; te="$(now)"
  f="$W/probes-C1.txt"; plines "$t" "$te" > "$f"
  for tgt in y h9090 h22; do eq "C1: $tgt through (of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' '))" "$(awk -v x=$tgt '$2==x && $3=="ok"' "$f" | wc -l | tr -d ' ')" 0; done
  for tgt in z eg; do eq "C1: $tgt failed (of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' '))" "$(awk -v x=$tgt '$2==x && $3=="no"' "$f" | wc -l | tr -d ' ')" 0; done
  eq "C1: /healthz" "$(hz)" "$GOOD"
  say "NODEPORT=$NP"
fi

if want C2; then
  say "== C2. drift that is repaired"
  c2 C2a "delete table ip felucca" "ip:postrouting ip:input ip:forward ip:ingress"; back C2a
  c2 C2b "delete table bridge felucca" "bridge:forward"; back C2b
  c2 C2c "delete table netdev felucca" "$(NETDEV_CHAINS)"; back C2c
  c2 C2d "flush chain ip felucca forward" "ip:forward"; back C2d
  c2 C2e "flush chain ip felucca input" "ip:input"; back C2e
  c2 C2f "$(printf 'delete table ip felucca\ndelete table bridge felucca\ndelete table netdev felucca')" "ip:postrouting ip:input ip:forward ip:ingress bridge:forward $(NETDEV_CHAINS)"; back C2f
  # flush ruleset: the host table goes too and only the firewall unit loads it again
  say "-- C2g: flush ruleset"
  p0="$(zoo_pids)"; sampler_on "$W/samp-C2g"; sleep 1; t="$(now)"; sudo nft flush ruleset
  rep() { local x; for x in ip:postrouting ip:input ip:forward ip:ingress bridge:forward $(NETDEV_CHAINS); do has_marker "${x%%:*}" "${x#*:}" || return 1; done; }
  if waitt 10 rep; then ok "C2g: the agent's chains back with their markers within 10 s (${WT} s)"; else bad "C2g: back within 10 s — got not back after ${WT} s"; fi
  tr="$(now)"; sleep 3
  eq "C2g: /healthz before the firewall reload (no host table)" "$(hz | sed -n 's/.*"isolation":\([a-z]*\).*/\1/p')" false
  eq "C2g: host table still missing before the reload" "$(host_table)" 0
  sudo systemctl reload felucca-firewall; tl="$(now)"
  if waitt 10 hz_true; then ok "C2g: /healthz true ${WT} s after the reload"; else bad "C2g: /healthz true within 10 s of the reload — got '$(hz)'"; fi
  sleep 4; te="$(now)"; sampler_off
  journal_names C2g "$t" postrouting input forward ingress
  probes C2g "$t" "$tr" "$te"
  say "  C2g: /healthz first said false $(awk -v a="$t" '$1>a && $4=="false"{printf "+%.1f s", $1-a; exit}' "$W/samp-C2g") after the flush"
  eq "C2g: no /healthz true while the host table was missing, later than one tick (5.5 s) after the flush" "$(awk -v a="$t" -v b="$tl" '$1>a+5.5 && $1<b && $4=="true"' "$W/samp-C2g" | wc -l | tr -d ' ')" 0
  eq "C2g: the running guests kept their Firecracker pids" "$(zoo_pids)" "$p0"
  eq "C2g: felucca0 never down (samples, of $(wc -l < "$W/samp-C2g"))" "$(downs "$W/samp-C2g")" 0
  back C2g
fi

if want C3a; then
  say "== C3a. a repair the kernel refuses (enforce)"
  r="$(mkvm is-s)"; eq "C3a: create is-s (to be asleep)" "${r%% *}" 201
  r="$(mkvm is-d)"; eq "C3a: create is-d (to be deleted)" "${r%% *}" 201
  r="$(mkvm is-q)"; eq "C3a: create is-q (to be put to sleep)" "${r%% *}" 201
  for v in is-s is-d is-q; do wait_exec $v 150 || bad "C3a: $v not exec-ready"; done
  eq "C3a: sleep is-s" "$(vm_act is-s sleep)" 200
  p0="$(zoo_pids)"; printf '%s\n' "$p0" > "$W/c3-pids"; sampler_on "$W/samp-C3a"; sleep 1
  t="$(now)"; printf '%s\n' "$t" > "$W/c3-t"; out="$(refuse)"; [ -n "$out" ] && say "  nft: $out"
  arm_undo c3
  if waitt 15 bash -c '! ip -o link show felucca0 | grep -Eq "<([^>]*,)?UP(,[^>]*)?>"'; then ok "C3a: felucca0 administratively down within 15 s (${WT} s)"; else bad "C3a: felucca0 down within 15 s — got still UP after ${WT} s"; fi
  td="$(now)"
  j="$(jsince "$t")"
  rl="$(printf '%s\n' "$j" | grep 'isolation watch: the repair was refused' | head -1)"
  [ -n "$rl" ] && ok "C3a: journal has the refusal: $(printf '%s' "$rl" | cut -c1-400)" || bad "C3a: journal: no 'the repair was refused' line"
  eq "C3a: the refusal carries nft's own line" "$(printf '%s' "$rl" | grep -c 'Error')" 1
  eq "C3a: journal: the bridge taken down" "$(printf '%s\n' "$j" | grep -c 'the guest bridge has been taken down' | awk '{print ($1>0)?"yes":"no"}')" yes
  waitt 10 hz_false; eq "C3a: /healthz" "$(hz)" '{"ok":true,"isolation":false}'
  sleep 6; blocked_all C3a "$(awk -v t="$td" 'BEGIN{printf "%.3f", t+1}')" "$(now)"
  r="$(mkvm is-n)"; eq "C3a: create answers 500 IsolationFailed" "$r" '500 {"error":"IsolationFailed"}'
  r="$(post /v1/vms/is-s/wake)"; eq "C3a: wake of sleeping is-s answers 500 IsolationFailed" "$r" '500 {"error":"IsolationFailed"}'
  eq "C3a: delete is-d still works" "$(vm_del is-d)" 204
  eq "C3a: sleep is-q still works" "$(vm_act is-q sleep)" 200
  eq "C3a: the running guests kept their Firecracker pids" "$(zoo_pids)" "$p0"
  for v in is-x is-y is-z; do
    if exec_ok $v; then ok "C3a: exec works in $v while the bridge is down (vsock)"; else bad "C3a: exec in $v while the bridge is down — got no answer"; fi
  done
  rg="$(ip route get "$YIP" 2>&1 | head -1)"; say "  ip route get $YIP: $rg"
  # an `unreachable` route makes `ip route get` fail with EHOSTUNREACH ("No route to host"); without it the
  # address would follow the default route out of the uplink
  eq "C3a: ip route get a guest address is answered by the unreachable route" "$(printf '%s' "$rg" | grep -Ec '^unreachable|No route to host')" 1
  eq "C3a: the unreachable route for the guest network is installed" "$(ip -4 route show type unreachable | grep -c "metric 4294967295")" 1
  eq "C3a: felucca0 still down (samples since the down, up)" "$(awk -v a="$td" '$1>a+0.5 && $2=="up"' "$W/samp-C3a.d" | wc -l | tr -d ' ')" 0
  say "C3HOLD node_port=$NP"
  # the sampler keeps running into C3b
  printf '%s\n' "$SAMPF" > "$W/c3-samp"
fi

if want C3b; then
  say "== C3b. the hookless chain deleted: the agent rebuilds it"
  SAMPF="$(cat "$W/c3-samp" 2>/dev/null)"; t3="$(cat "$W/c3-t" 2>/dev/null)"; p0="$(cat "$W/c3-pids" 2>/dev/null)"
  t="$(now)"; rm -f "$W/held-c3"; sudo nft delete chain ip felucca input
  c3back() { has_marker ip input && bridge_up && hz_true; }
  if waitt 15 c3back; then ok "C3b: input rebuilt, bridge up, /healthz true within 15 s (${WT} s)"; else bad "C3b: within 15 s — got marker $(has_marker ip input && echo yes || echo no) bridge $(bridge_up && echo up || echo down) healthz '$(hz)' after ${WT} s"; fi
  tr="$(now)"; sleep 6; te="$(now)"
  [ -n "$SAMPF" ] && { rm -f "$SAMPF"; sleep 0.5; mv -f "$SAMPF.d" "$SAMPF" 2>/dev/null; say "  felucca0 samples: $(downs "$SAMPF") down of $(wc -l < "$SAMPF")"; }
  j="$(jsince "$t")"; eq "C3b: journal: the bridge up again" "$(printf '%s\n' "$j" | grep -c 'the guest bridge is up again')" 1
  probes C3b "$t" "$tr" "$te"
  r="$(mkvm is-c3)"; eq "C3b: a create works again" "${r%% *}" 201
  eq "C3b: delete is-c3" "$(vm_del is-c3)" 204
  eq "C3b: the running guests kept their Firecracker pids" "$(zoo_pids)" "$p0"
  for v in is-s is-q; do vm_del $v >/dev/null; done
  back C3b
fi

if want C4; then
  say "== C4. one tap's chain"
  others=""; for s in $(vms | jq -r '.vms[].id' | while read -r id; do vm_slot "$id"; done | sort -n); do [ "hth-$s" = "$XT" ] || others="$others as_hth_$s"; done
  bef="$(for c in $others; do sudo nft -a -s list chain netdev felucca $c; done | md5sum)"
  p0="$(zoo_pids)"; sampler_on "$W/samp-C4"; sleep 1; t="$(now)"
  sudo nft delete chain netdev felucca "as_${XT//-/_}"
  if waitt 10 has_marker netdev "as_${XT//-/_}"; then ok "C4: as_${XT//-/_} back with its marker within 10 s (${WT} s)"; else bad "C4: as_${XT//-/_} back within 10 s — got not after ${WT} s"; fi
  tr="$(now)"; sleep 5; te="$(now)"; sampler_off
  journal_names C4 "$t" "as_${XT//-/_}"
  probes C4 "$t" "$tr" "$te"
  eq "C4: the other taps' chains untouched (rules and handles:$others)" "$(for c in $others; do sudo nft -a -s list chain netdev felucca $c; done | md5sum)" "$bef"
  eq "C4: is-x's tap still UP" "$(link_admin_up "$XT" && echo up || echo down)" up
  eq "C4: the running guests kept their Firecracker pids" "$(zoo_pids)" "$p0"
  back C4
fi

if want C5; then
  say "== C5. a switch: bridge-nf-call-iptables=0"
  sampler_on "$W/samp-C5"; sleep 1; t="$(now)"; sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0
  if waitt 10 bash -c '[ "$(cat /proc/sys/net/bridge/bridge-nf-call-iptables)" = 1 ]'; then ok "C5: the switch is 1 again within 10 s (${WT} s)"; else bad "C5: the switch back within 10 s — got '$(switch_val)' after ${WT} s"; fi
  sleep 3; sampler_off
  j="$(jsince "$t")"
  eq "C5: journal: drift on the switch" "$(printf '%s\n' "$j" | grep -c 'a switch this agent set to 1 is not')" 1
  eq "C5: journal: switch repaired and confirmed" "$(printf '%s\n' "$j" | grep -c 'switch repaired and confirmed')" 1
  say "  samples with the switch at 0: $(awk '$3=="0"' "$W/samp-C5" | wc -l | tr -d ' '), of them /healthz true: $(awk '$3=="0" && $4=="true"' "$W/samp-C5" | wc -l | tr -d ' ')"
  eq "C5: /healthz true while the switch read 0 for longer than one tick (5 s)" "$(awk -v a="$t" '$3=="0" && $4=="true" && $1>a+5.5' "$W/samp-C5" | wc -l | tr -d ' ')" 0
  eq "C5: felucca0 never down" "$(downs "$W/samp-C5")" 0
  back C5
fi

if want C6; then
  say "== C6. the host table deleted"
  sampler_on "$W/samp-C6"; sleep 1; t="$(now)"; sudo nft delete table inet felucca_host
  if waitt 10 hz_false; then ok "C6: /healthz false within 10 s (${WT} s)"; else bad "C6: /healthz false within 10 s — got '$(hz)'"; fi
  sleep 6; tm="$(now)"
  eq "C6: journal: the host fence warning" "$(jsince "$t" | grep -c 'the host fence (iifname "felucca0" drop, chain input of table inet')" 1
  f="$W/probes-C6.txt"; plines "$t" "$tm" > "$f"
  for tgt in h9090 h22 y; do eq "C6: $tgt through while the host table is missing (of $(awk -v x=$tgt '$2==x' "$f" | wc -l | tr -d ' '))" "$(awk -v x=$tgt '$2==x && $3=="ok"' "$f" | wc -l | tr -d ' ')" 0; done
  sudo systemctl reload felucca-firewall
  if waitt 10 hz_true; then ok "C6: /healthz true within 10 s of the reload (${WT} s)"; else bad "C6: /healthz true within 10 s of the reload — got '$(hz)'"; fi
  sampler_off
  eq "C6: felucca0 never down" "$(downs "$W/samp-C6")" 0
  back C6
fi

if want C7; then
  say "== C7. report mode (drop-in $DROPIN)"
  p0="$(zoo_pids)"
  printf '[Service]\nEnvironment=FELUCCA_ISOLATION_WATCH=report\n' | sudo tee "$DROPIN" >/dev/null; sudo systemctl daemon-reload
  t="$(now)"; restart_agent || bad "C7: the agent does not answer after the restart"; sleep 2
  j="$(jsince "$t")"
  eq "C7: journal: watch started in report mode, once" "$(printf '%s\n' "$j" | grep 'isolation watch started' | grep -c 'mode=Report')" 1
  eq "C7: no 'isolation watch NOT started'" "$(printf '%s\n' "$j" | grep -c 'isolation watch NOT started')" 0
  eq "C7: the running guests kept their Firecracker pids across the restart" "$(zoo_pids)" "$p0"
  waitt 15 hz_true; eq "C7: /healthz after the restart" "$(hz)" "$GOOD"
  sampler_on "$W/samp-C7"; sleep 1; t="$(now)"; out="$(refuse)"; [ -n "$out" ] && say "  nft: $out"
  arm_undo c7
  if waitt 15 hz_false; then ok "C7: /healthz false within 15 s (${WT} s)"; else bad "C7: /healthz false within 15 s — got '$(hz)'"; fi
  sleep 6
  eq "C7: journal: the 'left up' line" "$(jsince "$t" | grep -c 'the guest bridge is left up')" 1
  r="$(mkvm is-n7)"; eq "C7: create answers 500 IsolationFailed" "$r" '500 {"error":"IsolationFailed"}'
  eq "C7: felucca0 stays up (samples down, of $(wc -l < "$W/samp-C7.d"))" "$(downs "$W/samp-C7.d")" 0
  tb="$(now)"; rm -f "$W/held-c7"; sudo nft delete chain ip felucca input
  c7back() { has_marker ip input && hz_true; }
  if waitt 15 c7back; then ok "C7: input rebuilt and /healthz true within 15 s (${WT} s)"; else bad "C7: back within 15 s — got healthz '$(hz)'"; fi
  tr="$(now)"; sleep 5; te="$(now)"; sampler_off
  probes C7 "$tb" "$tr" "$te"
  eq "C7: felucca0 never down" "$(downs "$W/samp-C7")" 0
  sudo rm -f "$DROPIN"; sudo systemctl daemon-reload
  t="$(now)"; restart_agent || bad "C7: the agent does not answer after the second restart"; sleep 2
  j="$(jsince "$t")"
  eq "C7: drop-in removed: watch started in enforce mode, once" "$(printf '%s\n' "$j" | grep 'isolation watch started' | grep -c 'mode=Enforce')" 1
  eq "C7: the running guests kept their Firecracker pids across both restarts" "$(zoo_pids)" "$p0"
  eq "C7: no test drop-in left" "$(systemctl show "$UNIT" -p DropInPaths --value | grep -c lab-test)" 0
  back C7
fi

if want C8; then
  say "== C8. a dormant table"
  sampler_on "$W/samp-C8"; sleep 1; t="$(now)"; sudo nft add table ip felucca '{ flags dormant; }'
  say "  listed: $(sudo nft -t list table ip felucca | sed -n 2p)"
  undormant() { ! sudo nft list table ip felucca | grep -q 'flags dormant' && has_marker ip input; }
  if waitt 10 undormant; then ok "C8: the dormant flag gone within 10 s (${WT} s)"; else bad "C8: flag gone within 10 s — got '$(sudo nft list table ip felucca | grep -m1 flags)' after ${WT} s"; fi
  tr="$(now)"; sleep 5; te="$(now)"; sampler_off
  j="$(jsince "$t")"
  eq "C8: journal: drift lines (one or more)" "$(printf '%s\n' "$j" | grep -c 'isolation watch: drift' | awk '{print ($1>0)?"yes":"no"}')" yes
  eq "C8: journal: repair lines (one or more)" "$(printf '%s\n' "$j" | grep -c 'chain repaired and confirmed' | awk '{print ($1>0)?"yes":"no"}')" yes
  printf '%s\n' "$j" | grep -E 'drift|repaired|dormant' | cut -c1-260 | sed 's/^/    /'
  probes C8 "$t" "$tr" "$te"
  eq "C8: felucca0 never down" "$(downs "$W/samp-C8")" 0
  back C8
fi

if want C9; then
  say "== C9. no false alarm under churn: 60 x create, expose, unexpose, delete"
  c0="$(vm_count) $(dir_count) $(fc_count) $(tap_count) $(chain_count)"
  touch "$W/c9-on"; ( while [ -e "$W/c9-on" ]; do printf '%s %s\n' "$(now)" "$(hz)"; sleep 1; done ) > "$W/c9-hz" & HZP=$!
  t="$(now)"; nb=0
  for i in $(seq 1 60); do
    r="$(mkvm is-c$i)"; [ "${r%% *}" = 201 ] || { nb=$((nb+1)); say "  cycle $i create: $r"; }
    r="$(post /v1/vms/is-c$i/expose '{"guest_port":8069}')"; [ "${r%% *}" = 200 ] || { nb=$((nb+1)); say "  cycle $i expose: $r"; }
    r="$(api -m 30 -o /dev/null -w '%{http_code}' -X DELETE -d '{"guest_port":8069}' "$A/v1/vms/is-c$i/expose")"; [ "$r" = 200 ] || { nb=$((nb+1)); say "  cycle $i unexpose: $r"; }
    r="$(vm_del is-c$i)"; [ "$r" = 204 ] || { nb=$((nb+1)); say "  cycle $i delete: $r"; }
  done
  rm -f "$W/c9-on"; wait $HZP 2>/dev/null; sleep 3
  say "  60 cycles in $(since "$t") s"
  eq "C9: operations that did not answer 201/200/200/204" "$nb" 0
  j="$(jsince "$t")"
  eq "C9: journal: drift / the repair / taken down / this tick is skipped" "$(printf '%s\n' "$j" | grep -cE 'isolation watch: drift|the repair|taken down|this tick is skipped')" 0
  n="$(wc -l < "$W/c9-hz" | tr -d ' ')"
  [ "$n" -ge 10 ] && ok "C9: /healthz sampled every second ($n samples)" || bad "C9: /healthz samples — got $n, want one per second of the churn"
  eq "C9: /healthz samples not $GOOD (of $n)" "$(grep -vcF " $GOOD" "$W/c9-hz")" 0
  eq "C9: counts (records dirs fc taps chains) as before" "$(vm_count) $(dir_count) $(fc_count) $(tap_count) $(chain_count)" "$c0"
  back C9
fi

if want C10a; then
  say "== C10a. the control plane's side: hold kata-lab's refusal"
  if [ -z "$CP_URL" ]; then skip "C10a: no control plane address found"; else
  MYID="$(api -m 10 "$CP_URL/api/v1/nodes" | jq -r --arg h "$(hostname)" '(.nodes // .)[] | select(.hostname==$h) | .id' | head -1)"
  say "  this node: ${MYID:-unknown} at $CP_URL"
  gauge() { api -m 10 "$CP_URL/metrics" | sed -n 's/^felucca_nodes_unfenced \([0-9]*\)$/\1/p'; }
  eq "C10a: felucca_nodes_unfenced before" "$(gauge)" 0
  t="$(now)"; printf '%s\n' "$t" > "$W/c10-t"; out="$(refuse)"; [ -n "$out" ] && say "  nft: $out"
  arm_undo c10
  waitt 15 bash -c '! ip -o link show felucca0 | grep -Eq "<([^>]*,)?UP(,[^>]*)?>"' && ok "C10a: bridge down (${WT} s)" || bad "C10a: bridge not down in 15 s"
  g1() { [ "$(gauge)" = 1 ]; }
  if waitt 40 g1; then ok "C10a: felucca_nodes_unfenced 1 within 40 s (${WT} s)"; else bad "C10a: felucca_nodes_unfenced — got '$(gauge)', want 1"; fi
  say "  node record: $(api -m 10 "$CP_URL/api/v1/nodes" | jq -c --arg i "$MYID" '(.nodes // .)[] | select(.id==$i) | {status, isolation_ok}' 2>/dev/null)"
  for i in 1 2 3 4 5; do
    r="$(api -m 180 -w '\n%{http_code}' -X POST -d "{\"name\":\"is-cp$i\",\"vcpus\":1,\"mem_mib\":256}" "$CP_URL/api/v1/sandboxes")"
    c="$(printf '%s' "$r" | tail -n1)"; sid="$(printf '%s' "$r" | sed '$d' | jq -r '.id // empty' 2>/dev/null)"
    nd="$(api -m 10 "$CP_URL/api/v1/sandboxes/$sid" | jq -r '.node_id // .node // empty' 2>/dev/null)"
    nh="$(api -m 10 "$CP_URL/api/v1/nodes" | jq -r --arg i "$nd" '(.nodes // .)[] | select(.id==$i) | .hostname' 2>/dev/null)"
    eq "C10a: create is-cp$i through the control plane" "$c" 201
    eq "C10a: is-cp$i placed on" "$nh" lima-kata-lab-0
    [ -n "$sid" ] && printf '%s\n' "$sid" >> "$W/c10-ids"
  done
  for sid in $(cat "$W/c10-ids" 2>/dev/null); do api -m 60 -o /dev/null -X DELETE "$CP_URL/api/v1/sandboxes/$sid"; done; rm -f "$W/c10-ids"
  say "C10HOLD since=$t"
  fi
fi

if want C10b; then
  say "== C10b. repaired"
  rm -f "$W/held-c10"; sudo nft delete chain ip felucca input
  c10back() { has_marker ip input && bridge_up && hz_true; }
  waitt 15 c10back && ok "C10b: input rebuilt, bridge up, /healthz true (${WT} s)" || bad "C10b: not back in 15 s — healthz '$(hz)'"
  if [ -n "$CP_URL" ]; then
    gauge() { api -m 10 "$CP_URL/metrics" | sed -n 's/^felucca_nodes_unfenced \([0-9]*\)$/\1/p'; }
    g0() { [ "$(gauge)" = 0 ]; }
    if waitt 40 g0; then ok "C10b: felucca_nodes_unfenced 0 within 40 s (${WT} s)"; else bad "C10b: felucca_nodes_unfenced — got '$(gauge)', want 0"; fi
  fi
  say "C10BACK"
  back C10b
fi

if want END; then
  say "== END"
  guest is-x 'pkill -f probe.sh; pkill -f srv.py' >/dev/null 2>&1
  api -m 30 -o /dev/null -X DELETE -d '{"guest_port":8069}' "$A/v1/vms/is-x/expose"
  cleanup_prefix is-; sleep 3
  sudo rm -f "$DROPIN"; rm -f "$W"/held-*
  eq "END: counts (records dirs fc taps chains) as at the start" "$(vm_count) $(dir_count) $(fc_count) $(tap_count) $(chain_count)" "$(cat "$W/start-counts" 2>/dev/null)"
  eq "END: /healthz" "$(hz)" "$GOOD"
  eq "END: felucca0 up" "$(bridge_up && echo up || echo down)" up
  eq "END: no test drop-in" "$(systemctl show "$UNIT" -p DropInPaths --value | grep -c lab-test)" 0
  rm -rf "$W"
fi
summary
