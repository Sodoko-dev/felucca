#!/usr/bin/env bash
# FLU-6, FLU-7 lab gate (ingress) — runs ON the control-plane VM (see scripts/verify-ingress.sh).
#
# A deleted sandbox's hostname never reaches another sandbox (its node port is
# quarantined, the worker hands ports out round-robin), waking a sandbox does
# not send a request for its second service to its first, the gateway stops
# serving from a route table that is too old, a fork child's inherited
# hostname never answers with the parent's body.
#
#   ingress.sh [all | "I1 I2 ..."]
#
# Cases (the numbers of .omc/wave-1/GATES.md "verify-ingress", plus 8-13):
#   I1  a deleted sandbox's hostname, its node port given out again (two sandboxes on one worker)
#   I2  unexpose + expose of one guest port gets the same node port back
#   I3  the expose churn caps: 429 per sandbox (64) and per tenant (512), another tenant still exposes
#   I4  a second gateway ($GW_BIN) cut off from feluccad: serves for --max-stale 40, then 503; back within 10 s;
#       503, not 404, before its first fetch
#   I5  one sandbox, two services, asleep: both hostnames at once, 20 rounds, each answered by its own service
#   I6  a wake feluccad refuses (the worker holds the sandbox stopped): the state page in under 3 s
#   I7  `$GW_BIN --help` does not print the token
#   I8  a dynamic expose (<port>--<sandbox>) across a sleep: same service, same node port
#   I9  (INGRESS_LONG=1 only, 16 minutes) a dynamic expose of a sandbox asleep > 900 s is dropped, the
#       quarantine gauge grows by one, the next request re-creates it. Run it ALONE: entries of other cases
#       that leave quarantine during the 16 minutes would move the gauge too.
#   I10 $GW_BIN's flag checks (--refresh 0 / 61, --max-stale below 2 x refresh + 20 s refuse; 40/5 starts)
#   I11 fork of an exposed parent: the child's inherited hostname never answers with the parent's body
#   I12 felucca_node_ports_quarantined grows by one when an expose is removed
#   I13a/I13b/I13c  one worker unreachable (its agent SIGSTOPped by the wrapper between I13a and I13b, CONTed
#       between I13b and I13c): expose and a new dynamic port on the other worker still work, the stopped
#       worker reads `down` after 20 s, `ready` again after CONT.
#   END removes every ig- sandbox and the run's state.
# "all" runs I7 I10 I1 I2 I12 I11 I4 I3 I5 I8 I9 I6 in that order (I9 only with INGRESS_LONG=1).
# The wrapper calls the parts in turn; they share their state (sandbox ids)
# through /var/tmp/felucca-lab-ingress.
#
# Two sandboxes on one worker without relying on luck: the second is a FORK of
# the first (a fork child lives on its parent's worker). The first is placed on
# $INGRESS_NODE (default kata-lab-1; matched against the node's hostname) by
# filling: a create that lands elsewhere is kept until one lands there (the
# scheduler picks the ready node with the fewest VMs), then the fillers go.
#
# Needs: FELUCCA_AUTH_FILE (one line `Authorization: Bearer <admin token>`,
# readable only by the user running this). The token is never put on a command
# line: curls read the header file (-H @file); the second gateway gets it in its
# ENVIRONMENT (FELUCCA_TOKEN, exported by the shell, not passed as an argument).
# Optional: GW_BIN (default /usr/local/bin/felucca-gw), INGRESS_NODE, INGRESS_LONG,
# CP_API, GW_API.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTH="${FELUCCA_AUTH_FILE:-}"
[ -n "$AUTH" ] && [ -r "$AUTH" ] || { echo "ERROR: FELUCCA_AUTH_FILE is not a readable header file" >&2; exit 2; }
. "${HERE}/lib.sh"            # say ok bad skip eq summary, $H (= @$FELUCCA_AUTH_FILE)
H="@$AUTH"
CASES="${1:-all}"
CP="${CP_API:-http://127.0.0.1:8080}"; GW="${GW_API:-http://127.0.0.1:8088}"
GW_BIN="${GW_BIN:-/usr/local/bin/felucca-gw}"
NODE_WANT="${INGRESS_NODE:-kata-lab-1}"
DOM="$(sudo sed -n 's/^FELUCCA_GW_DOMAIN=//p' /etc/felucca/felucca-gw.env 2>/dev/null | tr -d '"'"'")"; DOM="${DOM:-sb.lab.test}"
W=/var/tmp/felucca-lab-ingress; mkdir -p "$W"; chmod 700 "$W"
IDS="$W/ids"; touch "$IDS"; KEYS="$W/keys"; touch "$KEYS"
RUN="$(date +%m%d%H%M%S)"
GW2_PORT=18088; FWD_PORT=18080; GW3_PORT=18089
# The guest service: answers "<label> <the address the connection came in on> <hostname>". The address
# tells a fork child (its own address) from its parent (the same memory, so the same label and hostname).
SRV_PY='import socket,sys
port=int(sys.argv[1]); label=sys.argv[2]
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",port)); s.listen(64)
while True:
    c,_=s.accept()
    try:
        c.settimeout(5); c.recv(65536); b=("%s %s %s\n" % (label, c.getsockname()[0], socket.gethostname())).encode()
        c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: "+str(len(b)).encode()+b"\r\n\r\n"+b)
    except Exception: pass
    finally: c.close()
'
# A TCP relay to feluccad, for the second gateway of I4: stopping it cuts that gateway off (no firewall rule).
FWD_PY='import socket,sys,threading
lp=int(sys.argv[1]); th,tp=sys.argv[2],int(sys.argv[3])
def pipe(a,b):
    try:
        while True:
            d=a.recv(65536)
            if not d: break
            b.sendall(d)
    except Exception: pass
    finally:
        for x in (a,b):
            try: x.shutdown(socket.SHUT_RDWR)
            except Exception: pass
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("127.0.0.1",lp)); s.listen(64)
while True:
    c,_=s.accept()
    try: u=socket.create_connection((th,tp),timeout=5); u.settimeout(None)
    except Exception: c.close(); continue
    threading.Thread(target=pipe,args=(c,u),daemon=True).start(); threading.Thread(target=pipe,args=(u,c),daemon=True).start()
'

# "all" is every case but the I13 parts (they need the wrapper) and END.
want() { if [ "$CASES" = all ]; then case "$1" in I13*|END) return 1;; *) return 0;; esac; fi
  case " $CASES " in *" $1 "*) return 0;; *) return 1;; esac; }
now() { date +%s.%N | cut -c1-14; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN{printf "%.2f", b-a}'; }
lt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0<b+0)}'; }
# req <METHOD> <path> [body] [auth header file] -> RC (code), RB (body, one line, first 300), RBF (all), RT (s), RA (Retry-After)
req() {
  local m="$1" p="$2" b="${3:-}" ah="${4:-$AUTH}" hf out tail
  hf="$(mktemp "$W/h.XXXXXX")"
  if [ -n "$b" ]; then out="$(curl -s -m "${REQ_TIMEOUT:-60}" -X "$m" -H "@$ah" -H 'Content-Type: application/json' -d "$b" -D "$hf" -w '\n%{http_code} %{time_total}' "$CP$p")"
  else out="$(curl -s -m "${REQ_TIMEOUT:-60}" -X "$m" -H "@$ah" -D "$hf" -w '\n%{http_code} %{time_total}' "$CP$p")"; fi
  tail="${out##*$'\n'}"; RB="${out%$'\n'*}"; [ "$tail" = "$out" ] && RB=""
  RC="${tail%% *}"; RT="${tail#* }"; RBF="$(printf '%s' "$RB" | tr -d '\n')"; RB="$(printf '%s' "$RBF" | cut -c1-300)"
  RA="$(sed -n 's/^[Rr]etry-[Aa]fter:[[:space:]]*\([0-9]*\).*/\1/p' "$hf" | head -1)"; rm -f "$hf"
}
jf() { printf '%s' "$1" | jq -r "$2" 2>/dev/null; }
# gwq <host> [gateway base] [timeout] -> GC (code), GB (body, one line), GT (s), GA (Retry-After)
gwq() {
  local hf out tail; hf="$(mktemp "$W/g.XXXXXX")"
  out="$(curl -s -m "${3:-5}" -H "Host: $1" -D "$hf" -w '\n%{http_code} %{time_total}' "${2:-$GW}/")"
  tail="${out##*$'\n'}"; GB="${out%$'\n'*}"; [ "$tail" = "$out" ] && GB=""
  GC="${tail%% *}"; GT="${tail#* }"; GB="$(printf '%s' "$GB" | tr '\n' ' ' | sed 's/ *$//' | cut -c1-200)"
  GA="$(sed -n 's/^[Rr]etry-[Aa]fter:[[:space:]]*\([0-9]*\).*/\1/p' "$hf" | head -1)"; rm -f "$hf"
}
metric() { curl -s -m 5 -H "$H" "$CP/metrics" | awk -v n="$1" '$1==n {print $2; exit}'; }
nodes_json() { curl -s -m 10 -H "$H" "$CP/api/v1/nodes" | jq -c '(.nodes // .)'; }
node_status_of() { nodes_json | jq -r --arg h "$1" '.[] | select((.hostname // "") | endswith($h)) | .status'; }   # <hostname suffix>
node_host_of_id() { nodes_json | jq -r --arg i "$1" '.[] | select(.id==$i) | .hostname // .id'; }
wait_nodes() { local i=0 s; s="$(nodes_json | jq -r '[.[] | .status] | unique | join(",")')"
  while [ "$s" != ready ] && [ $i -lt $(( ${1:-30} * 2 )) ]; do sleep 0.5; i=$((i+1)); s="$(nodes_json | jq -r '[.[] | .status] | unique | join(",")')"; done
  [ "$s" = ready ] || say "  (workers not all ready after ${1:-30} s: '$s')"; }
sbj() { curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes/$1"; }
sb_state() { sbj "$1" | jq -r '.state // "absent"' 2>/dev/null; }
sb_ip() { sbj "$1" | jq -r '.ip // empty' 2>/dev/null; }
sb_node() { node_host_of_id "$(sbj "$1" | jq -r '.node_id // empty')"; }
routes() { curl -s -m 10 -H "$H" "$CP/api/v1/routes" | jq -c '(.routes // .)'; }
route_field() { routes | jq -r --arg h "$1" --arg f "$2" '.[] | select(.hostname==$h) | .[$f]'; }   # <label> <field>
seen() { printf '%s\n' "$1" >> "$IDS"; }
# mk <name> [extra json] [auth file] -> SB (id or ""), RC
mk() { req POST /api/v1/sandboxes "{\"name\":\"$1\",\"vcpus\":1,\"mem_mib\":256${2:+,$2}}" "${3:-$AUTH}"
  SB="$(jf "$RBF" '.id // empty')"; [ -n "$SB" ] && seen "$SB"; [ "$RC" = 201 ] || say "  (create $1 answered $RC $RB in ${RT}s)"; return 0; }
ready() { local i=0 t="${2:-240}" o
  while [ $i -lt "$t" ]; do o="$(curl -s -m 8 -H "${3:-$H}" -X POST -d '{"cmd":["true"],"timeout_ms":3000}' "$CP/api/v1/sandboxes/$1/exec")"
    printf '%s' "$o" | grep -q '"ok":true' && return 0; sleep 2; i=$((i+2)); done; return 1; }
cp_exec() { req POST "/api/v1/sandboxes/$1/exec" "$(jq -cn --arg c "$2" '{cmd:["sh","-c",$c],timeout_ms:15000}')"; OUT="$(jf "$RBF" '.stdout // empty' | tr -d '\n')"; }
# srv <id> <port> <label> — the guest service on <port>, checked from inside the guest
srv() { local b64; b64="$(printf '%s' "$SRV_PY" | base64 -w0)"
  cp_exec "$1" "echo $b64 | base64 -d > /root/srv.py; setsid nohup python3 /root/srv.py $2 $3 >/root/srv$2.log 2>&1 < /dev/null & sleep 1; python3 -c 'import urllib.request;print(urllib.request.urlopen(\"http://127.0.0.1:$2/\",timeout=3).read().decode().split()[0])'"
  [ "$OUT" = "$3" ] || say "  (service $3 on $1:$2 answered '$OUT' inside the guest: $RC $RB)"; }
expose() { req POST "/api/v1/sandboxes/$1/expose" "{\"name\":\"$2\",\"port\":$3}" "${4:-$AUTH}"; NP="$(jf "$RBF" '.node_port // empty')"; }   # <id> <name> <guest port> [auth]
unexpose() { req DELETE "/api/v1/sandboxes/$1/expose/$2" "" "${3:-$AUTH}"; }
# wait_gw <host> <first word of the body> [limit-s] — until the gateway answers 200 with that label
wait_gw() { local i=0; while [ $i -lt $(( ${3:-20} * 2 )) ]; do gwq "$1"; [ "$GC" = 200 ] && [ "${GB%% *}" = "$2" ] && return 0; sleep 0.5; i=$((i+1)); done; return 1; }
del() { req DELETE "/api/v1/sandboxes/$1"; }
fork() { REQ_TIMEOUT=120 req POST "/api/v1/sandboxes/$1/fork" "{\"name\":\"$2\"}" "${3:-$AUTH}"; SB="$(jf "$RBF" '.id // empty')"; [ -n "$SB" ] && seen "$SB"; }
# place <name> [extra json] [auth] — a sandbox on $NODE_WANT, by filling (see the header); sets SB
place() {
  local fill="" try=0 n
  while [ $try -lt 4 ]; do
    try=$((try+1)); mk "$1" "${2:-}" "${3:-$AUTH}"; [ "$RC" = 201 ] || { sleep 3; continue; }
    n="$(sb_node "$SB")"
    case "$n" in *"$NODE_WANT") break;; esac
    say "  (placement: $1 landed on $n, kept as a filler, creating another)"; fill="$fill $SB"; SB=""; sleep 6
  done
  for n in $fill; do del "$n" >/dev/null; done
  [ -n "$SB" ] || say "  (placement: no sandbox landed on $NODE_WANT in $try creates)"
}
worker_addrs() { nodes_json | jq -r '.[] | .addr' | sort -u; }
worker_ids() { local a; for a in $(worker_addrs); do curl -s -m 8 -H "$H" "http://$a/v1/vms" | jq -r '.vms[] | select(.id|startswith("pool-")|not) | .id'; done | sort; }
gw_journal_since() { sudo journalctl -u felucca-gw --since "$1" --no-pager -o cat; }

# ---- the shared fixture: S, an admin sandbox on $NODE_WANT with the service "S" on 8069 exposed as web
fix_get() { cat "$W/fix-$1" 2>/dev/null; }
fix_set() { printf '%s' "$2" > "$W/fix-$1"; }
need_S() {
  S="$(fix_get S)"
  if [ -n "$S" ] && [ "$(sb_state "$S")" = running ]; then SL="$(fix_get SL)"; SHOST="web--$S.$DOM"; return 0; fi
  say "  (fixture: creating S on $NODE_WANT)"
  place ig-s '"allow_dynamic_ports":true'; S="$SB"; [ -n "$S" ] || { bad "fixture S could not be created"; return 1; }
  ready "$S" 240 || { bad "fixture S never became exec-ready"; return 1; }
  srv "$S" 8069 S; expose "$S" web 8069; SL=S; fix_set S "$S"; fix_set SL S; SHOST="web--$S.$DOM"
  wait_gw "$SHOST" "$SL" 20 || bad "fixture S not served through the gateway (got $GC $GB)"
}
# ---- W, the sleeper of I5 I8 I9 I6: services one (8069), two (8070), dyn (8071); one and two exposed
need_W() {
  WS="$(fix_get W)"
  if [ -n "$WS" ] && [ "$(sb_state "$WS")" != absent ]; then return 0; fi
  mk ig-w '"allow_dynamic_ports":true'; WS="$SB"; [ -n "$WS" ] || { bad "fixture W could not be created ($RC $RB)"; return 1; }
  ready "$WS" 240 || { bad "fixture W never became exec-ready"; return 1; }
  srv "$WS" 8069 one; srv "$WS" 8070 two; srv "$WS" 8071 dyn
  expose "$WS" one 8069; expose "$WS" two 8070; fix_set W "$WS"
  wait_gw "one--$WS.$DOM" one 20 && wait_gw "two--$WS.$DOM" two 20 || bad "fixture W not served through the gateway (got $GC $GB)"
}
# sleep_through <id> — sleep through the API, wait until every route of it says sleeping, then one gateway refresh
sleep_through() {
  req POST "/api/v1/sandboxes/$1/sleep"; [ "$RC" = 200 ] || { say "  (sleep $1 answered $RC $RB)"; return 1; }
  local i=0; until [ "$(routes | jq -r --arg s "$1" '[.[] | select(.sandbox_id==$s) | .state] | unique | join(",")')" = sleeping ] || [ $i -ge 60 ]; do sleep 0.5; i=$((i+1)); done
  sleep 6
}

say "== 0. $(date '+%F %T') ingress gate, cases '$CASES' =="
say "  feluccad $(systemctl is-active feluccad) $(sudo sha256sum /usr/local/bin/feluccad | cut -c1-16); felucca-gw $(systemctl is-active felucca-gw) $(sudo sha256sum /usr/local/bin/felucca-gw | cut -c1-16); GW_BIN $GW_BIN $(sudo sha256sum "$GW_BIN" | cut -c1-16); domain $DOM; node wanted $NODE_WANT"
say "  nodes: $(nodes_json | jq -c '[.[] | "\(.hostname):\(.status):vms=\(.vm_count)"]')"
[ -e "$W/workers0" ] || { worker_ids > "$W/workers0"; say "  sandboxes on the workers at the start (pool VMs aside): '$(tr '\n' ' ' < "$W/workers0")'"; }
case " $CASES " in *" END "*|*" I13b "*|*" I13c "*) ;; *) wait_nodes 30;; esac

# ==== I7 ======================================================================
if want I7; then
say "== I7. \`felucca-gw --help\` does not print the token =="
MK="ig-marker-$RUN-$$"
N="$(env -i PATH=/usr/bin:/bin FELUCCA_TOKEN="$MK" "$GW_BIN" --help 2>&1 | grep -c -- "$MK")"
say "  lines of --help: $(env -i PATH=/usr/bin:/bin "$GW_BIN" --help 2>&1 | wc -l); the -token line: $(env -i PATH=/usr/bin:/bin FELUCCA_TOKEN="$MK" "$GW_BIN" --help 2>&1 | grep -A1 -- '-token' | tr -s ' \n' ' ' | sed "s/$MK/<MARKER>/g" | cut -c1-160)"
eq "I7 the marker token in --help (count)" "$N" 0
fi

# ==== I10 =====================================================================
if want I10; then
say "== I10. $GW_BIN flag checks (run by hand on 127.0.0.1:$GW3_PORT, never the unit) =="
# gw3 <flags...> -> G3RC (exit status, or 'running' when it is still up after 3 s), G3OUT (its output, one line)
gw3() {
  local o="$W/gw3.out" p rc; : > "$o"
  ( export FELUCCA_TOKEN=ig-flag-check; exec "$GW_BIN" --listen "127.0.0.1:$GW3_PORT" --feluccad http://127.0.0.1:1 --domain "$DOM" "$@" ) > "$o" 2>&1 &
  p=$!; local i=0; while kill -0 "$p" 2>/dev/null && [ $i -lt 30 ]; do sleep 0.1; i=$((i+1)); done
  if kill -0 "$p" 2>/dev/null; then G3RC=running; kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; else wait "$p"; G3RC=$?; fi
  G3OUT="$(tr '\n' ' ' < "$o" | cut -c1-240)"
}
for f in "--refresh 0" "--refresh 61" "--max-stale 5s --refresh 5s" "--max-stale 5s --refresh 5"; do
  # shellcheck disable=SC2086
  gw3 $f; say "  $f: exit '$G3RC', output: $G3OUT"
  if [ "$G3RC" != running ] && [ "$G3RC" != 0 ] && [ -n "$G3OUT" ]; then ok "I10 $f refuses to start, with a message"; else bad "I10 $f refuses to start — got exit '$G3RC' output '$G3OUT'"; fi
done
gw3 --max-stale 40 --refresh 5; say "  --max-stale 40 --refresh 5: '$G3RC', output: $G3OUT"
eq "I10 --max-stale 40 --refresh 5 starts" "$G3RC" running
fi

# ==== I1 ======================================================================
if want I1; then
say "== I1. a deleted sandbox's hostname, its node port given out again on the same worker =="
place ig-a '"allow_dynamic_ports":true'; A="$SB"
if [ -z "$A" ]; then bad "I1 could not place A on $NODE_WANT"; else
  ready "$A" 240 || bad "I1 A never became exec-ready"
  fork "$A" ig-b; B="$SB"; say "  B = fork of A: $RC (${RT}s) $B"
  eq "I1 the fork answers 201" "$RC" 201
  NA="$(sb_node "$A")"; NB="$(sb_node "$B")"; say "  A=$A on $NA, B=$B on $NB"
  eq "I1 A and B on one worker" "$NB" "$NA"
  ready "$B" 120 || bad "I1 B never became exec-ready"
  srv "$A" 8069 A; srv "$B" 8069 B
  expose "$A" web 8069; PA="$NP"; say "  A expose: $RC $RB"
  AH="web--$A.$DOM"
  wait_gw "$AH" A 20 && ok "I1 A answers through the gateway ($GB)" || bad "I1 A answers through the gateway — got $GC $GB"
  POLL="$W/i1-poll"; : > "$POLL"; T0="$(now)"
  ( e=$(awk -v t="$T0" 'BEGIN{printf "%.3f", t+60}'); while lt "$(now)" "$e"; do s="$(since "$T0")"; gwq "$AH" "$GW" 2; printf '%s %s %s %s\n' "$s" "$GC" "$GT" "${GB%% *}" >> "$POLL"; sleep 0.05; done ) &
  PP=$!
  sleep 1
  del "$A"; TD="$(since "$T0")"; say "  +${TD}s delete A: $RC (${RT}s)"
  sleep 0.2
  expose "$B" web 8069; PB="$NP"; say "  +$(since "$T0")s B expose: $RC $RB"
  wait "$PP"
  say "  poll of A's hostname every 50 ms for 60 s (offset, code, first word of the body): count, first, last"
  awk '{k=$2" "$4; if(!(k in c)) f[k]=$1; c[k]++; l[k]=$1} END{for(k in c) printf "    %-40s n=%d first=%s last=%s\n", k, c[k], f[k], l[k]}' "$POLL" | sort -t= -k3 -n
  NBB="$(awk '$2==200 && $4=="B"' "$POLL" | wc -l | tr -d ' ')"
  eq "I1 A's hostname answered 200 with B's body (of $(wc -l < "$POLL" | tr -d ' ') requests)" "$NBB" 0
  [ -n "$PA" ] && [ -n "$PB" ] && [ "$PA" != "$PB" ] && ok "I1 B's node port differs from A's ($PB vs $PA)" || bad "I1 B's node port differs from A's — got '$PB', want not '$PA'"
  wait_gw "web--$B.$DOM" B 20 && ok "I1 B answers through the gateway" || bad "I1 B answers through the gateway — got $GC $GB"
  # A's old port: in no answer of 50 new exposes (fresh guest ports, each removed again: at most 16 exposes per sandbox)
  HIT=0; GOT=""; for i in $(seq 1 50); do expose "$B" "n$i" $((9100+i)); GOT="$GOT $NP"; [ "$NP" = "$PA" ] && HIT=$((HIT+1)); [ "$RC" = 201 ] || say "  (expose n$i: $RC $RB)"; unexpose "$B" "n$i"; done
  say "  the 50 new exposes' node ports:$GOT"
  eq "I1 A's old port $PA in the answers of 50 new exposes" "$HIT" 0
  # B is the shared fixture S from here on
  fix_set S "$B"; fix_set SL B
fi
fi

# ==== I2 ======================================================================
if want I2; then
say "== I2. unexpose and expose again of one guest port gets the same node port =="
if need_S; then
  P0="$(jf "$(sbj "$S")" '.exposes[] | select(.name=="web") | .node_port')"
  unexpose "$S" web; say "  unexpose web: $RC"
  expose "$S" web 8069; say "  expose web again: $RC $RB"
  eq "I2 the same node port back" "$NP" "$P0"
  wait_gw "$SHOST" "$SL" 20 && ok "I2 served again through the gateway" || bad "I2 served again — got $GC $GB"
fi
fi

# ==== I12 =====================================================================
if want I12; then
say "== I12. felucca_node_ports_quarantined grows by one when an expose is removed =="
if need_S; then
  for try in 1 2; do
    expose "$S" "q$try" $((9200+try)); Q="$NP"; G0="$(metric felucca_node_ports_quarantined)"
    unexpose "$S" "q$try"; G1="$(metric felucca_node_ports_quarantined)"
    say "  try $try: expose q$try -> node port $Q; gauge $G0 -> $G1 after the unexpose ($RC)"
    [ -n "$G0" ] && [ -n "$G1" ] && [ $((G1-G0)) = 1 ] && break
  done
  eq "I12 growth of the gauge" "$(( ${G1:-0} - ${G0:-0} ))$([ -z "$G1" ] && echo ' (no gauge)')" 1
fi
fi

# ==== I11 =====================================================================
if want I11; then
say "== I11. fork of an exposed parent: the child's inherited hostname never answers with the parent's body =="
if need_S; then
  PIP="$(sb_ip "$S")"; wait_gw "$SHOST" "$SL" 20 || bad "I11 the parent is not served ($GC $GB)"
  PNP="$(route_field "web--$S" node_port)"; say "  parent $S ip $PIP node port $PNP serves '$GB'"
  # the route table while the fork runs: every row of another sandbox named web (the child's id is not known yet)
  : > "$W/i11-rt"; rm -f "$W/i11-stop"; TF="$(now)"
  ( while [ ! -e "$W/i11-stop" ]; do routes | jq -r --arg s "$S" --arg t "$(since "$TF")" '.[] | select(.name=="web" and .sandbox_id!=$s) | "\($t) \(.sandbox_id) \(.state) \(.node_port)"' >> "$W/i11-rt"; sleep 0.05; done ) &
  RTP=$!
  fork "$S" ig-k; K="$SB"; KIP="$(jf "$RBF" '.ip // empty')"; T201="$(since "$TF")"; say "  fork: $RC (${RT}s, answered at +$T201 s) child $K ip '$KIP'"
  sleep 1; touch "$W/i11-stop"; wait "$RTP"
  eq "I11 the fork answers 201" "$RC" 201
  say "  the child's rows in the route table while the fork ran (offset state node_port: count, first, last):"
  awk -v k="$K" '$2==k {key=$3" "$4; if(!(key in c)) f[key]=$1; c[key]++; l[key]=$1} END{for(x in c) printf "    %-24s n=%d first=+%s last=+%s\n", x, c[x], f[x], l[x]}' "$W/i11-rt" | sort -t+ -k2 -n
  eq "I11 rows of the child that say running on the parent's node port $PNP" "$(awk -v k="$K" -v p="$PNP" '$2==k && $3=="running" && $4==p' "$W/i11-rt" | wc -l | tr -d ' ')" 0
  NBEF="$(awk -v k="$K" -v t="$T201" '$2==k && $1+0 < t+0' "$W/i11-rt" | wc -l | tr -d ' ')"
  if [ "$NBEF" -gt 0 ]; then eq "I11 the child's rows seen before the 201 (of $NBEF) that are not 'creating'" "$(awk -v k="$K" -v t="$T201" '$2==k && $1+0 < t+0 && $3!="creating"' "$W/i11-rt" | wc -l | tr -d ' ')" 0
  else say "  (no row of the child was seen before the 201)"; fi
  KH="web--$K.$DOM"; : > "$W/i11"; T0="$(now)"
  for i in $(seq 1 20); do gwq "$KH" "$GW" 1; printf '%s %s %s\n' "$(since "$T0")" "$GC" "$GB" >> "$W/i11"; sleep 0.08; done
  [ -n "$KIP" ] || KIP="$(sb_ip "$K")"
  say "  child ip $KIP; the 20 answers in $(since "$T0") s:"; sed 's/^/    /' "$W/i11"
  NPAR="$(awk -v p="$PIP" '$2==200 && $4==p' "$W/i11" | wc -l | tr -d ' ')"
  # 404 "no such service": the gateway has not fetched a table with the child's row yet (it refreshes every 5 s)
  NOTH="$(awk -v k="$KIP" '!(($2==200 && $4==k) || $2==503 || ($2==404 && $3=="{\"error\":\"no"))' "$W/i11" | wc -l | tr -d ' ')"
  say "  answers: child's body $(awk -v k="$KIP" '$2==200 && $4==k' "$W/i11" | wc -l | tr -d ' '), 503 $(awk '$2==503' "$W/i11" | wc -l | tr -d ' '), 404 no such service $(awk '$2==404' "$W/i11" | wc -l | tr -d ' ')"
  eq "I11 answers with the parent's body (address $PIP)" "$NPAR" 0
  eq "I11 answers that are neither the child's body, a 503, nor the gateway's 404 before its next refresh" "$NOTH" 0
  i=0; until { gwq "$KH"; [ "$GC" = 200 ] && [ "$(echo "$GB" | awk '{print $2}')" = "$KIP" ]; } || lt 5 "$(since "$T0")"; do sleep 0.2; done
  [ "$GC" = 200 ] && [ "$(echo "$GB" | awk '{print $2}')" = "$KIP" ] && ok "I11 the child's body within 5 s ($(since "$T0") s: $GB)" || bad "I11 the child's body within 5 s — got $GC '$GB' after $(since "$T0") s"
  del "$K" >/dev/null
fi
fi

# ==== I4 ======================================================================
if want I4; then
say "== I4. a second gateway ($GW_BIN) on 127.0.0.1:$GW2_PORT, --max-stale 40 --refresh 5, reaching feluccad through a relay on :$FWD_PORT =="
if need_S; then
  G2="http://127.0.0.1:$GW2_PORT"; FWDPID=""; GW2PID=""
  fwd_on()  { python3 -c "$FWD_PY" "$FWD_PORT" 127.0.0.1 "${CP##*:}" > "$W/fwd.log" 2>&1 & FWDPID=$!; sleep 0.3; }
  fwd_off() { [ -n "$FWDPID" ] && { kill "$FWDPID" 2>/dev/null; wait "$FWDPID" 2>/dev/null; }; FWDPID=""; }
  gw2_on() { # [extra flags]
    ( export FELUCCA_TOKEN; FELUCCA_TOKEN="$(sed -n 's/^Authorization: Bearer //p' "$AUTH")"
      exec "$GW_BIN" --listen "127.0.0.1:$GW2_PORT" --feluccad "http://127.0.0.1:$FWD_PORT" --domain "$DOM" --refresh 5 "$@" ) > "$W/gw2.log" 2>&1 &
    GW2PID=$!; sleep 1; kill -0 "$GW2PID" 2>/dev/null; }
  OLDGW=no
  gw2_on --max-stale 40 || { say "  the gateway did not start with --max-stale: $(tr '\n' ' ' < "$W/gw2.log" | cut -c1-200); started without it"; OLDGW=yes; gw2_on; }
  say "  gateway pid $GW2PID up: $(kill -0 "$GW2PID" 2>/dev/null && echo yes || echo no)"
  gwq "$SHOST" "$G2"; say "  before its first fetch (relay down): $GC $GB (Retry-After '$GA')"
  eq "I4 before the first successful fetch: answer" "$GC $GB" '503 {"error":"route table unavailable"}'
  RZ="$(curl -s -m 3 -w ' %{http_code}' "$G2/readyz" | tr '\n' ' ')"; say "  /readyz before the first fetch: $RZ"
  eq "I4 /readyz before the first fetch (code)" "${RZ##* }" 503
  fwd_on; T1="$(now)"
  i=0; until { gwq "$SHOST" "$G2"; [ "$GC" = 200 ]; } || [ $i -ge 30 ]; do sleep 0.5; i=$((i+1)); done
  say "  relay up: serves '$GC $GB' after $(since "$T1") s"
  eq "I4 serves once it can fetch" "$GC ${GB%% *}" "200 $SL"
  sleep 6   # a refresh or more through the relay
  fwd_off; TC="$(now)"; say "  $(date '+%T') relay stopped: the gateway is cut off from feluccad"
  : > "$W/i4"; LAST200=""; FIRST503=""
  while lt "$(since "$TC")" 62; do
    gwq "$SHOST" "$G2" 2; s="$(since "$TC")"; printf '%s %s %s %s\n' "$s" "$GC" "${GA:--}" "$GB" >> "$W/i4"
    [ "$GC" = 200 ] && LAST200="$s"; [ "$GC" = 503 ] && [ -z "$FIRST503" ] && { FIRST503="$s"; F503B="$GB"; F503A="$GA"; RZC="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$G2/readyz")"; HZC="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$G2/healthz")"; }
    [ -n "$FIRST503" ] && lt "$(awk -v a="$FIRST503" 'BEGIN{printf "%.2f", a+5}')" "$s" && break
    sleep 1
  done
  say "  answers after the cut (offset code Retry-After body), first, around the change, last:"
  head -2 "$W/i4" | sed 's/^/    /'; awk -v f="${FIRST503:-999}" '$1>f-4 && $1<f+3' "$W/i4" | sed 's/^/    /'; tail -1 "$W/i4" | sed 's/^/    /'
  say "  last 200 at +${LAST200:-never} s, first 503 at +${FIRST503:-never} s"
  [ -n "$LAST200" ] && lt 34 "$LAST200" && ok "I4 serves from its table while it is young (last 200 at +$LAST200 s)" || bad "I4 serves from its table for about 40 s — got last 200 at +${LAST200:-never} s"
  [ -n "$FIRST503" ] && lt "$FIRST503" 47 && ok "I4 503 once the table is older than 40 s (first at +$FIRST503 s)" || bad "I4 503 within 47 s of the cut — got first 503 at '${FIRST503:-never}', last answer $(tail -1 "$W/i4" | cut -c1-120)"
  eq "I4 the 503: body, Retry-After" "${F503B:-} ${F503A:-}" '{"error":"route table unavailable"} 5'
  eq "I4 /readyz while the table is old (code)" "${RZC:-}" 503
  eq "I4 /healthz while the table is old (code)" "${HZC:-}" 200
  fwd_on; T2="$(now)"
  i=0; until { gwq "$SHOST" "$G2" 2; [ "$GC" = 200 ]; } || lt 15 "$(since "$T2")"; do sleep 0.5; done
  TB="$(since "$T2")"; say "  relay up again: '$GC $GB' after $TB s"
  [ "$GC" = 200 ] && lt "$TB" 10 && ok "I4 serves again within 10 s ($TB s)" || bad "I4 serves again within 10 s — got '$GC' after $TB s"
  RZ="$(curl -s -m 3 -o /dev/null -w '%{http_code}' "$G2/readyz")"; eq "I4 /readyz once fresh again (code)" "$RZ" 200
  kill "$GW2PID" 2>/dev/null; wait "$GW2PID" 2>/dev/null; fwd_off
  say "  the second gateway's log (WARN/ERROR, first 4): $(grep -E 'WARN|ERROR' "$W/gw2.log" | head -4 | cut -c1-200 | tr '\n' '|')"
fi
fi

# ==== I3 ======================================================================
if want I3; then
say "== I3. expose churn caps: per sandbox (64) and per tenant on one worker (512); another tenant meanwhile =="
if need_S; then
  req POST /api/v1/tenants "{\"name\":\"ig-c-$RUN\",\"max_sandboxes\":4,\"max_vcpus\":4,\"max_mem_mib\":2048,\"max_disk_gb\":40}"
  TID="$(jf "$RBF" '.tenant.id // .id // empty')"; KID="$(jf "$RBF" '.key_id // empty')"; [ -n "$KID" ] && echo "$KID" >> "$KEYS"
  TK="$W/tk-c.hdr"; ( umask 077; printf 'Authorization: Bearer %s\n' "$(jf "$RBF" '.api_key // empty')" > "$TK" ); RBF=""; RB=""
  if [ -z "$TID" ]; then bad "I3 could not create the tenant"; else
    place ig-c0 "" "$TK"; T0S="$SB"
    if [ -z "$T0S" ]; then bad "I3 the tenant's sandbox could not be placed on $NODE_WANT"; else
      ready "$T0S" 240 "@$TK" || bad "I3 the tenant's sandbox never became exec-ready"
      say "  tenant $TID, sandbox $T0S on $(sb_node "$T0S"); fixture S $S on $(sb_node "$S")"
      # churn <id> <max cycles> -> CY (cycles that got 201), then the refusal in RC/RB/RA
      churn() { CY=0; local i; for i in $(seq 1 "$2"); do expose "$1" c $((10000+CHURN_N)) "$TK"; CHURN_N=$((CHURN_N+1))
          [ "$RC" = 201 ] || return 0; CY=$((CY+1)); unexpose "$1" c "$TK"; done; RC=none; }
      CHURN_N=0; G0="$(metric felucca_node_ports_quarantined)"; TC0="$(now)"
      churn "$T0S" 80
      say "  sandbox $T0S: $CY cycles, then: $RC $RB (Retry-After '$RA')"
      eq "I3 per sandbox: cycles before the refusal" "$CY" 64
      eq "I3 per sandbox: the refusal" "$RC $(jf "$RBF" .error)" "429 expose churn limit reached"
      [ -n "$RA" ] && [ "$RA" -ge 1 ] && [ "$RA" -le 900 ] && ok "I3 Retry-After in 1..900 ($RA)" || bad "I3 Retry-After in 1..900 — got '$RA'"
      if [ "$RC" = 429 ]; then
        TOT=$CY; k=0; LASTRC=""; LASTE=""
        while [ $k -lt 8 ]; do
          k=$((k+1)); fork "$T0S" "ig-cf$k" "$TK"; C="$SB"; [ "$RC" = 201 ] || { bad "I3 fork $k: $RC $RB"; break; }
          churn "$C" 70; TOT=$((TOT+CY)); say "  fork $k ($C): $CY cycles, then $RC $(jf "$RBF" .error) (Retry-After '$RA'); tenant total $TOT"
          LASTRC="$RC"; LASTE="$(jf "$RBF" .error)"
          del "$C" >/dev/null
          [ "$TOT" -ge 512 ] && [ "$CY" -lt 64 ] && break
        done
        say "  $TOT ports into quarantine in $(since "$TC0") s; gauge $G0 -> $(metric felucca_node_ports_quarantined)"
        eq "I3 per tenant: ports in quarantine when the tenant is refused" "$TOT" 512
        eq "I3 per tenant: the refusal" "$LASTRC $LASTE" "429 expose churn limit reached"
        fork "$T0S" ig-cfx "$TK"; C="$SB"
        if [ -n "$C" ]; then expose "$C" c 11999 "$TK"; say "  a fresh sandbox of the tenant: $RC $RB (Retry-After '$RA')"; eq "I3 per tenant: a fresh sandbox of the tenant is refused" "$RC" 429; del "$C" >/dev/null; fi
        expose "$S" other 9400; say "  another tenant (admin, fixture S) on the same worker meanwhile: $RC $RB"
        eq "I3 another tenant still exposes on that worker" "$RC" 201
        gwq "other--$S.$DOM"; unexpose "$S" other
      else
        bad "I3 no refusal after $CY cycles: the per-tenant part is not run"
      fi
      del "$T0S" >/dev/null
    fi
  fi
  [ -n "${KID:-}" ] && curl -s -m 10 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/keys/$KID"; rm -f "$TK"
fi
fi

# ==== I5 ======================================================================
if want I5; then
say "== I5. one sandbox, two services (one 8069, two 8070), asleep: both hostnames at the same moment, 20 rounds =="
if need_W; then
  H1="one--$WS.$DOM"; H2="two--$WS.$DOM"; BAD=0; SAME=0; SLOW=""
  for r in $(seq 1 20); do
    sleep_through "$WS" || { bad "I5 round $r: the sleep failed"; break; }
    rm -f "$W/go" "$W/i5-1" "$W/i5-2"
    ( until [ -e "$W/go" ]; do sleep 0.005; done; curl -s -m 60 -H "Host: $H1" -w ' %{http_code} %{time_total}' "$GW/" | tr '\n' ' ' > "$W/i5-1" ) &
    ( until [ -e "$W/go" ]; do sleep 0.005; done; curl -s -m 60 -H "Host: $H2" -w ' %{http_code} %{time_total}' "$GW/" | tr '\n' ' ' > "$W/i5-2" ) &
    sleep 0.3; touch "$W/go"; wait
    b1="$(cut -d' ' -f1 "$W/i5-1")"; b2="$(cut -d' ' -f1 "$W/i5-2")"
    say "  round $r: one-- '$(cut -c1-80 "$W/i5-1")' | two-- '$(cut -c1-80 "$W/i5-2")'"
    [ "$b1" = one ] && [ "$b2" = two ] || BAD=$((BAD+1)); [ "$b1" = "$b2" ] && SAME=$((SAME+1))
  done
  eq "I5 rounds in which a hostname was not answered by its own service (of 20)" "$BAD" 0
  say "  rounds with the same body for both: $SAME"
fi
fi

# ==== I8 ======================================================================
if want I8; then
say "== I8. a dynamic expose (8071--<sandbox>) across a sleep =="
if need_W; then
  DH="8071--$WS.$DOM"
  gwq "$DH" "$GW" 20; say "  first request: $GC '$GB' in ${GT}s"
  eq "I8 the dynamic port answers (code, label)" "$GC ${GB%% *}" "200 dyn"
  N1="$(route_field "8071--$WS" node_port)"; say "  route table: 8071--$WS node port '$N1'"
  sleep_through "$WS"
  say "  asleep: route 8071--$WS state '$(route_field "8071--$WS" state)' node port '$(route_field "8071--$WS" node_port)'"
  eq "I8 the dynamic expose is kept while asleep" "$(route_field "8071--$WS" node_port)" "$N1"
  gwq "$DH" "$GW" 30; say "  request while asleep (the gateway wakes it): $GC '$GB' in ${GT}s"
  eq "I8 answered by the same service after the wake" "$GC ${GB%% *}" "200 dyn"
  eq "I8 the same node port in the route table" "$(route_field "8071--$WS" node_port)" "$N1"
fi
fi

# ==== I9 ======================================================================
if want I9; then
say "== I9. (INGRESS_LONG=1) a dynamic expose of a sandbox asleep for more than 900 s is dropped =="
if [ "${INGRESS_LONG:-0}" != 1 ]; then skip "I9 (INGRESS_LONG is not 1: it sleeps 16 minutes)"
elif need_W; then
  DH="8071--$WS.$DOM"; gwq "$DH" "$GW" 20; N1="$(route_field "8071--$WS" node_port)"; say "  dynamic expose: $GC '$GB', node port $N1"
  G0="$(metric felucca_node_ports_quarantined)"; sleep_through "$WS"; T9="$(now)"; say "  $(date '+%T') asleep; gauge $G0"
  while lt "$(since "$T9")" 960; do sleep 30; done
  say "  after $(since "$T9") s asleep: route '$(route_field "8071--$WS" node_port)', gauge $(metric felucca_node_ports_quarantined)"
  eq "I9 the dynamic expose is gone from the route table" "$(route_field "8071--$WS" node_port)" ""
  eq "I9 the quarantine gauge grew by one" "$(( $(metric felucca_node_ports_quarantined) - G0 ))" 1
  eq "I9 one and two are still in the route table" "$(routes | jq -r --arg s "$WS" '[.[] | select(.sandbox_id==$s) | .name] | sort | join(",")')" "one,two"
  gwq "$DH" "$GW" 30; say "  next request: $GC '$GB'"; eq "I9 the next request re-creates it" "$GC ${GB%% *}" "200 dyn"
fi
fi

# ==== I6 ======================================================================
if want I6; then
say "== I6. a wake feluccad refuses: the worker holds the sandbox stopped, feluccad says sleeping =="
if need_W; then
  [ "$(sb_state "$WS")" = sleeping ] || sleep_through "$WS"
  WA="$(nodes_json | jq -r --arg i "$(sbj "$WS" | jq -r .node_id)" '.[] | select(.id==$i) | .addr')"
  r="$(curl -s -m 60 -H "$H" -X POST -w ' %{http_code}' "http://$WA/v1/vms/$WS/stop")"; say "  stop through the agent's API ($WA): $r"
  say "  the worker: $(curl -s -m 8 -H "$H" "http://$WA/v1/vms" | jq -r --arg i "$WS" '.vms[] | select(.id==$i) | .state'); feluccad: $(sb_state "$WS")"
  sleep 6
  gwq "one--$WS.$DOM" "$GW" 30; say "  request through the gateway: $GC '$GB' in ${GT}s"
  lt "$GT" 3 && ok "I6 answered in under 3 s (${GT}s)" || bad "I6 answered in under 3 s — got ${GT}s"
  eq "I6 the state page" "$GC $GB" '503 {"error":"sandbox is stopped"}'
  eq "I6 feluccad's record then says" "$(sb_state "$WS")" stopped
  gwq "two--$WS.$DOM" "$GW" 30; say "  a second request at once: $GC '$GB' in ${GT}s"
  del "$WS" >/dev/null; rm -f "$W/fix-W"
fi
fi

# ==== I13 (three parts; the wrapper stops and continues the other worker's agent in between) ====
if want I13a; then
say "== I13a. one worker unreachable: the fixture on $NODE_WANT =="
if need_S; then
  srv "$S" 8072 S72; say "  S $S on $(sb_node "$S"); service on 8072: '$OUT'"
  OTHER="$(nodes_json | jq -r --arg h "$NODE_WANT" '.[] | select((.hostname // "") | endswith($h) | not) | .hostname' | head -1)"
  fix_set OTHER "$OTHER"; say "  the worker to stop: $OTHER ($(node_status_of "$OTHER"))"
  eq "I13 S is on $NODE_WANT" "$(case "$(sb_node "$S")" in *"$NODE_WANT") echo yes;; *) echo no;; esac)" yes
fi
fi
if want I13b; then
say "== I13b. the other worker's agent is stopped (SIGSTOP) =="
S="$(fix_get S)"; OTHER="$(fix_get OTHER)"; TS="$(now)"
say "  $(date '+%T') $OTHER reads '$(node_status_of "$OTHER")'"
expose "$S" u13 9301; say "  expose of S (on $NODE_WANT): $RC $RB in ${RT}s"
eq "I13 the expose answers 201" "$RC" 201
lt "$RT" 2 && ok "I13 the expose answers in under 2 s (${RT}s)" || bad "I13 the expose answers in under 2 s — got ${RT}s"
gwq "8072--$S.$DOM" "$GW" 20; say "  first request to a new dynamic port of S: $GC '$GB' in ${GT}s"
eq "I13 the new dynamic port is served" "$GC ${GB%% *}" "200 S72"
while lt "$(since "$TS")" 20; do sleep 0.5; done
ST="$(node_status_of "$OTHER")"; say "  +$(since "$TS") s: $OTHER reads '$ST'"
i=0; while [ "$ST" != down ] && [ $i -lt 40 ]; do sleep 0.5; i=$((i+1)); ST="$(node_status_of "$OTHER")"; done
[ $i -gt 0 ] && say "  ($OTHER read '$ST' at +$(since "$TS") s)"
eq "I13 the stopped worker reads down after 20 s" "$ST" down
unexpose "$S" u13 >/dev/null
fi
if want I13c; then
say "== I13c. the other worker's agent continued =="
OTHER="$(fix_get OTHER)"; TS="$(now)"; i=0; ST="$(node_status_of "$OTHER")"
while [ "$ST" != ready ] && [ $i -lt 60 ]; do sleep 0.5; i=$((i+1)); ST="$(node_status_of "$OTHER")"; done
say "  $OTHER reads '$ST' after $(since "$TS") s"
eq "I13 the worker reads ready again" "$ST" ready
fi

# ==== END =====================================================================
if want END; then
say "== END. every ig- sandbox removed =="
for id in $(sort -u "$IDS"); do curl -s -m 60 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/sandboxes/$id"; done
for id in $(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '(.sandboxes // .)[] | select(.name|startswith("ig-")) | .id'); do curl -s -m 60 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/sandboxes/$id"; done
sleep 2
for id in $(sort -u "$IDS"); do for a in $(worker_addrs); do
  if curl -s -m 8 -H "$H" "http://$a/v1/vms" | jq -e --arg i "$id" '.vms[] | select(.id==$i)' >/dev/null 2>&1; then
    say "  cleanup: $id is still on the worker at $a although the control plane was told to delete it: removed there"; curl -s -m 60 -o /dev/null -H "$H" -X DELETE "http://$a/v1/vms/$id"; fi
done; done
for k in $(cat "$KEYS"); do curl -s -m 10 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/keys/$k"; done
sleep 1
eq "END sandboxes on the workers are what they were (pool VMs aside)" "$(worker_ids | tr '\n' ' ')" "$(tr '\n' ' ' < "$W/workers0")"
eq "END no ig- sandbox in the control plane" "$(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '[(.sandboxes // .)[] | select(.name|startswith("ig-")) | .id] | join(" ")')" ""
say "  gauge felucca_node_ports_quarantined $(metric felucca_node_ports_quarantined); /healthz $(curl -s -m 3 "$CP/healthz"); gw /readyz $(curl -s -m 3 -w ' %{http_code}' "$GW/readyz" | tr '\n' ' ')"
rm -rf "$W"
fi
summary
