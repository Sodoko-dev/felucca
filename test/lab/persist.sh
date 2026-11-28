#!/usr/bin/env bash
# FLU-5 lab gate (with FLU-74, FLU-82, FLU-83) — runs ON the control-plane VM (see scripts/verify-persist.sh).
#
# A change is acknowledged only once it is saved. While the database cannot be
# written the control plane refuses changes (503 `store unavailable`, or
# `applied but not saved` when a worker already acted), keeps serving reads,
# says so on /healthz, and catches up by itself once the database can be
# written again. Nothing that was answered 2xx is lost by a restart.
#
#   persist.sh [all | "P1 P2 ..."]
#
# Cases (the numbers of .omc/wave-1/GATES.md, "verify-persist", plus 9-12):
#   P1 baseline            P2 write lock held        P3 lock released, catch-up
#   P4 restart: no 2xx lost P5 restart under the lock P6 usage across a lock window
#   P7 heartbeat value out of range (run it LAST: on a build without the range
#      check one such value makes every later save fail)
#   P8 full / read-only file system under the database (only with PERSIST_FULL=1)
#   P9 read-only database file at start  P10 stop under exec load
#   P11 two forks of one running parent  P12 /healthz is not counted
# "all" runs P1 P12 P2 P3 P4 P5 P6 P9 P10 P11 P8 P7 in that order. P3 and P4
# need P2 in the same run (they check what P2 did); alone they are skipped.
#
# It holds SQLite's write lock with test/lab/dblock.py (as the service's user),
# restarts and stops feluccad, and makes the database file read-only for a
# while. Back the database up before running it (scripts/verify-persist.sh
# does). Every sandbox it creates is named ps-* and removed at the end, also on
# a worker that the control plane forgot it on; tenants are named ps-<case>-<run>
# (feluccad has no tenant delete; their keys are revoked).
#
# Needs: FELUCCA_AUTH_FILE, a file readable only by the user running this
# script that holds one line `Authorization: Bearer <admin token>` (on the lab
# the workers use the same shared credential, which P7 relies on). The token is
# never put on a command line: every curl reads the header from a file
# (`-H @file`), the tenants' keys too (files in $W, mode 0600), because command
# lines show in process listings. Optional: CP_API, GW_API.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
AUTH="${FELUCCA_AUTH_FILE:-}"
[ -n "$AUTH" ] && [ -r "$AUTH" ] || { echo "ERROR: FELUCCA_AUTH_FILE is not a readable header file" >&2; exit 2; }
FELUCCA_TOKEN="(in $AUTH)"   # lib.sh insists on the variable; its $H is replaced below
# lib.sh is the workers' helper file; only its counters and check helpers
# (say ok bad skip eq summary, $H) are used here.
. "${HERE}/lib.sh"
H="@$AUTH"                   # curl -H @file: the header comes from the file
CASES="${1:-all}"
CP="${CP_API:-http://127.0.0.1:8080}"; GW="${GW_API:-http://127.0.0.1:8088}"
SVC=feluccad
SVC_USER="$(systemctl show "$SVC" -p User --value)"; SVC_USER="${SVC_USER:-root}"
CFG=/etc/felucca/feluccad.json
DB="$(sudo jq -r '.db_path // empty' "$CFG" 2>/dev/null)"; DB="${DB:-/var/lib/felucca/felucca.db}"
STATE="$(dirname "$DB")"
DOM="$(sudo sed -n 's/^FELUCCA_GW_DOMAIN=//p' /etc/felucca/felucca-gw.env 2>/dev/null | tr -d '"'"'")"
RUN="$(date +%m%d%H%M%S)"
W=/var/tmp/felucca-lab-persist; rm -rf "$W"; mkdir -p "$W"; chmod 755 "$W"
REL="$W/release"; LOCKPID=""
IDS="$W/ids"; : > "$IDS"          # every sandbox id this run was given, for the final cleanup
KEYS="$W/keys"; : > "$KEYS"       # key ids to revoke
declare -A EXP                    # what the last 2xx said: EXP[<id>.state], EXP[<id>.web]
SRV_PY='import socket,sys
port=int(sys.argv[1]); label=sys.argv[2]
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",port)); s.listen(64)
while True:
    c,_=s.accept()
    try:
        c.settimeout(5); c.recv(65536); b=(label+"\n").encode()
        c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\nContent-Length: "+str(len(b)).encode()+b"\r\n\r\n"+b)
    except Exception: pass
    finally: c.close()
'

want() { [ "$CASES" = all ] || case " $CASES " in *" $1 "*) return 0;; *) return 1;; esac; }
now() { date +%s.%N | cut -c1-14; }
lt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a+0<b+0)}'; }
# req <METHOD> <path> [body] [auth header file] — sets RC (code), RB (body, one line, cut), RBF (all of it), RT (seconds), RA (Retry-After)
req() {
  local m="$1" p="$2" b="${3:-}" ah="${4:-$AUTH}" hf out tail
  hf="$(mktemp "$W/h.XXXXXX")"
  if [ -n "$b" ]; then
    out="$(curl -s -m "${REQ_TIMEOUT:-60}" -X "$m" -H "@$ah" -H 'Content-Type: application/json' -d "$b" -D "$hf" -w '\n%{http_code} %{time_total}' "$CP$p")"
  else
    out="$(curl -s -m "${REQ_TIMEOUT:-60}" -X "$m" -H "@$ah" -D "$hf" -w '\n%{http_code} %{time_total}' "$CP$p")"
  fi
  tail="${out##*$'\n'}"; RB="${out%$'\n'*}"; [ "$tail" = "$out" ] && RB=""
  # RBF: the whole body (for parsing: a tenant's answer is longer than 300 characters); RB: its first 300, for the log
  RC="${tail%% *}"; RT="${tail#* }"; RBF="$(printf '%s' "$RB" | tr -d '\n')"; RB="$(printf '%s' "$RBF" | cut -c1-300)"
  RA="$(sed -n 's/^[Rr]etry-[Aa]fter:[[:space:]]*\([0-9]*\).*/\1/p' "$hf" | head -1)"; rm -f "$hf"
}
seen()  { printf '%s\n' "$1" >> "$IDS"; }
jf()    { printf '%s' "$1" | jq -r "$2" 2>/dev/null; }
healthz() { curl -s -m 3 -w ' %{http_code} %{time_total}' "$CP/healthz"; }   # -> "<body> <code> <secs>"
hz_code() { curl -s -m 3 -o /dev/null -w '%{http_code}' "$CP/healthz"; }
wait_hz() { # <code> <limit-s> -> seconds it took, or "never"
  local s; s="$(date +%s.%N)"
  while :; do [ "$(hz_code)" = "$1" ] && { awk -v a="$s" -v b="$(date +%s.%N)" 'BEGIN{printf "%.1f", b-a}'; return 0; }
    lt "$(awk -v a="$s" -v b="$(date +%s.%N)" 'BEGIN{print b-a}')" "$2" || { echo never; return 1; }; sleep 0.25; done; }
metric() { curl -s -m 5 -H "$H" "$CP/metrics" | awk -v n="$1" '$1==n {print $2; exit}'; }
svc_active() { systemctl is-active "$SVC"; }
# After a start, feluccad loads the nodes with their last heartbeat from the
# database: older than the liveness window, so they are `down` and a create
# answers 503 "no ready node" until each worker's next heartbeat (measured on
# the lab: down right after the start, ready 12 s later). Not the store's
# business: the cases wait for the workers before they judge a create.
wait_nodes() { # [limit-s]
  local i=0 s; s="$(curl -s -m 5 -H "$H" "$CP/api/v1/nodes" | jq -r '[(.nodes // .)[] | .status] | unique | join(",")')"
  while [ "$s" != ready ] && [ $i -lt $(( ${1:-30} * 2 )) ]; do sleep 0.5; i=$((i+1)); s="$(curl -s -m 5 -H "$H" "$CP/api/v1/nodes" | jq -r '[(.nodes // .)[] | .status] | unique | join(",")')"; done
  [ "$s" = ready ] || say "  (workers not all ready after ${1:-30} s: '$s')"
}
restart_cp() { sudo systemctl restart "$SVC"; local i=0; until curl -s -m 1 -o /dev/null "$CP/healthz" || [ $i -ge 60 ]; do sleep 0.5; i=$((i+1)); done; wait_nodes 30; }

# ---- the database (always as the service's user: see dblock.py) --------------
lock_on() { # [seconds] — hold the write lock until lock_off (or the limit)
  rm -f "$REL" "$W/lock.log"
  sudo -u "$SVC_USER" python3 "$HERE/dblock.py" "$DB" "${1:-300}" "$REL" > "$W/lock.log" 2>&1 &
  LOCKPID=$!
  local i=0; until grep -q LOCKED "$W/lock.log" 2>/dev/null || [ $i -ge 100 ]; do sleep 0.1; i=$((i+1)); done
  grep -q LOCKED "$W/lock.log" && say "  write lock held: $(grep LOCKED "$W/lock.log")" || { bad "could not take the write lock: $(tr '\n' '|' < "$W/lock.log")"; return 1; }
}
lock_off() { [ -n "$LOCKPID" ] || return 0; touch "$REL"; wait "$LOCKPID" 2>/dev/null; LOCKPID=""; rm -f "$REL"; say "  write lock released: $(grep RELEASED "$W/lock.log")"; }
# "<id> <state> <expose names>" per sandbox row, sorted — what the DATABASE says
db_rows() {
  sudo -u "$SVC_USER" python3 - "$DB" <<'EOF'
import json, sqlite3, sys
con = sqlite3.connect("file:%s?mode=ro" % sys.argv[1], uri=True, timeout=5)
for sid, st, ex in sorted(con.execute("SELECT id, state, exposes FROM sandboxes")):
    try: names = sorted(e.get("name", "") for e in (json.loads(ex) if ex else []))  # '' = none (the column's default)
    except Exception: names = ["?"]
    print(sid, st, ",".join(names) or "-")
EOF
}
db_has() { db_rows | awk -v i="$1" '$1==i{f=1} END{exit !f}'; }
# the same from the API (memory)
api_rows() { curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '(.sandboxes // .)[] | "\(.id) \(.state) \([(.exposes // [])[].name] | sort | join(",") | if .=="" then "-" else . end)"' | sort; }

# ---- the workers, through their own API (the lab's shared token) -------------
worker_addrs() { curl -s -m 10 -H "$H" "$CP/api/v1/nodes" | jq -r '(.nodes // .)[] | .addr' | sort -u; }
WORKERS="$(worker_addrs)"
worker_vms() { local a; for a in $WORKERS; do curl -s -m 8 -H "$H" "http://$a/v1/vms" | jq -r --arg a "$a" '.vms[] | "\(.id) \(.state) \($a)"'; done; }
on_worker() { worker_vms | awk -v i="$1" '$1==i{print $2"@"$3; f=1} END{exit !f}'; }
worker_ids() { worker_vms | awk '$1 !~ /^pool-/ {print $1}' | sort; }

# ---- sandboxes ----------------------------------------------------------------
mk() { # <name> [auth header file] -> sets SB (id or ""), and RC/RB
  req POST /api/v1/sandboxes "{\"name\":\"$1\",\"vcpus\":1,\"mem_mib\":256}" "${2:-$AUTH}"
  SB="$(jf "$RBF" '.id // empty')"; [ -n "$SB" ] && seen "$SB"
  [ "$RC" = 201 ] && [ -n "$SB" ] && { EXP[$SB.state]=running; EXP[$SB.web]=no; }
  [ "$RC" = 201 ] || say "  (create $1 answered $RC $RB in ${RT}s)"
  return 0
}
cp_exec() { # <id> <sh command> [auth header file] -> sets OUT (the command's stdout) and RC/RT
  req POST "/api/v1/sandboxes/$1/exec" "$(jq -cn --arg c "$2" '{cmd:["sh","-c",$c],timeout_ms:15000}')" "${3:-$AUTH}"
  OUT="$(jf "$RBF" '.stdout // empty' | tr -d '\n')"
}
ready() { # <id> [limit-s] — exec through the control plane works
  local i=0 t="${2:-240}" o
  while [ $i -lt "$t" ]; do o="$(curl -s -m 8 -H "$H" -X POST -d '{"cmd":["true"],"timeout_ms":3000}' "$CP/api/v1/sandboxes/$1/exec")"
    printf '%s' "$o" | grep -q '"ok":true' && return 0; sleep 2; i=$((i+2)); done; return 1; }
sb_state() { curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes/$1" | jq -r '.state // "absent"' 2>/dev/null; }
sb_web()   { curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes/$1" | jq -r 'if ([(.exposes // [])[] | select(.name=="web")] | length) > 0 then "yes" else "no" end' 2>/dev/null; }
# <what> <id> <attr> <value> — remember a change that was answered 2xx
acked() { EXP[$2.$3]="$4"; say "  2xx: $1 ($2 -> $3=$4)"; }
# a change answered "applied but not saved": memory has it, the database may not; P4 does not count it
applied() { EXP[$1.$2]="?"; }
serve() { # <id> — a web server on 8069 in the guest, exposed as "web"; sets HOST (the gateway host)
  local b64; b64="$(printf '%s' "$SRV_PY" | base64 -w0)"
  cp_exec "$1" "echo $b64 | base64 -d > /root/srv.py; setsid nohup python3 /root/srv.py 8069 ps-$1 >/root/srv.log 2>&1 < /dev/null & sleep 1"
  req POST "/api/v1/sandboxes/$1/expose" '{"name":"web","port":8069}'
  [ "$RC" = 201 ] && acked "expose web" "$1" web yes
  HOST="web--$1.$DOM"
}
gw_get() { curl -s -m 5 -H "Host: $1" -w ' %{http_code}' "$GW/" | tr -d '\n'; }   # -> "<body> <code>"
delete_all() { # every sandbox this run created: through the control plane, then on the workers directly
  local id a
  for id in $(sort -u "$IDS"); do curl -s -m 60 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/sandboxes/$id"; done
  for id in $(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '(.sandboxes // .)[] | select(.name|startswith("ps-")) | .id'); do
    curl -s -m 60 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/sandboxes/$id"; done
  sleep 2
  for id in $(sort -u "$IDS"); do
    for a in $WORKERS; do
      if curl -s -m 8 -H "$H" "http://$a/v1/vms" | jq -e --arg i "$id" '.vms[] | select(.id==$i)' >/dev/null 2>&1; then
        say "  cleanup: $id is still on the worker at $a although the control plane was told to delete it: removed there"
        curl -s -m 60 -o /dev/null -H "$H" -X DELETE "http://$a/v1/vms/$id"
      fi
    done
  done
}
# Between cases (after P4, which needs what P2/P3 made): every sandbox of the
# run removed, so that no more than three run at once on the lab.
prune() { delete_all; : > "$IDS"; wait_nodes 30; }
finish() {
  lock_off
  # whatever P9 changed: the database files writable again for the service
  for f in "$DB" "$DB-wal" "$DB-shm"; do sudo test -e "$f" && { sudo chown "$SVC_USER:" "$f"; sudo chmod 0600 "$f"; }; done
  [ "$(svc_active)" = active ] || sudo systemctl start "$SVC"
  delete_all
  local k; for k in $(cat "$KEYS"); do curl -s -m 10 -o /dev/null -H "$H" -X DELETE "$CP/api/v1/keys/$k"; done
  rm -f "$W"/tk-*.hdr
}
trap 'finish' EXIT

tenant() { # <name> -> sets TID, TKEY (the path of a 0600 header file with the tenant's key; the key is never printed)
  req POST /api/v1/tenants "{\"name\":\"$1\",\"max_sandboxes\":6,\"max_vcpus\":6,\"max_mem_mib\":4096,\"max_disk_gb\":60}"
  TID="$(jf "$RBF" '.tenant.id // .id // empty')"; TKEY=""
  local kid k; kid="$(jf "$RBF" '.key_id // empty')"; [ -n "$kid" ] && echo "$kid" >> "$KEYS"
  k="$(jf "$RBF" '.api_key // empty')"
  if [ -n "$k" ]; then TKEY="$W/tk-$1.hdr"; ( umask 077; printf 'Authorization: Bearer %s\n' "$k" > "$TKEY" ); fi
  k=""; RB="(tenant answer not shown: it holds a key)"; RBF="$RB"
  [ -n "$TID" ] && [ -n "$TKEY" ]
}

say "== 0. the control plane =="
say "  feluccad $(svc_active), pid $(systemctl show $SVC -p MainPID --value), user $SVC_USER, db $DB, binary $(sudo sha256sum /usr/local/bin/feluccad | cut -c1-16)"
say "  workers: $(echo $WORKERS)"
say "  sandboxes on the workers before: $(worker_vms | awk '{print $1}' | sort | tr '\n' ' ')"
[ "$(svc_active)" = active ] || { bad "feluccad is not active"; exit 1; }
[ -n "$WORKERS" ] || { bad "no worker in /api/v1/nodes"; exit 1; }
WORKERS0="$(worker_ids)"

# ==== P1 =========================================================================
if want P1; then
say "== P1. baseline: every change answers 2xx, /healthz {\"ok\":true}, the store is healthy =="
mk ps-base; eq "P1 create" "$RC" 201; B="$SB"
if [ -n "$B" ]; then
  ready "$B" 240 || bad "P1 ps-base never became exec-ready"
  req POST "/api/v1/sandboxes/$B/sleep"; eq "P1 sleep" "$RC" 200
  req POST "/api/v1/sandboxes/$B/wake";  eq "P1 wake" "$RC" 200
  ready "$B" 120 || bad "P1 ps-base not exec-ready after the wake"
  req POST "/api/v1/sandboxes/$B/expose" '{"name":"web","port":8069}'; eq "P1 expose" "$RC" 201
  req DELETE "/api/v1/sandboxes/$B/expose/web"; case "$RC" in 2??) ok "P1 unexpose ($RC)";; *) bad "P1 unexpose — got '$RC' $RB, want 2xx";; esac
  req DELETE "/api/v1/sandboxes/$B"; eq "P1 delete" "$RC" 204; [ "$RC" = 204 ] && EXP[$B.state]=absent
fi
eq "P1 /healthz (body, code)" "$(curl -s -m 3 -w ' %{http_code}' "$CP/healthz")" '{"ok":true} 200'
eq "P1 felucca_store_healthy" "$(metric felucca_store_healthy)" 1
fi

# ==== P12 ========================================================================
if want P12; then
say "== P12. /healthz is not counted in felucca_api_requests_total =="
# Other traffic counts too (heartbeats, the gateway's refresh and activity
# reports, a few a second at most) and the second /metrics read counts itself,
# so the 20 probes are sent within a fraction of a second between two reads,
# three times; the smallest growth is judged: 20 counted probes cannot hide in it.
MIN=""
for _ in 1 2 3; do
  a="$(metric felucca_api_requests_total)"; for _ in $(seq 1 20); do curl -s -m 2 -o /dev/null "$CP/healthz"; done; b="$(metric felucca_api_requests_total)"
  d=$(( ${b:-0} - ${a:-0} )); [ -z "$MIN" ] || [ "$d" -lt "$MIN" ] && MIN="$d"
done
say "  growth of felucca_api_requests_total over 20 /healthz (smallest of 3): $MIN"
[ -n "$a" ] && [ "$MIN" -lt 10 ] && ok "P12 /healthz not counted (grew by $MIN, of which 1 is the /metrics read itself)" || bad "P12 /healthz not counted — got growth '$MIN' over 20 /healthz, want < 10"
fi

# ==== P2 / P3 ====================================================================
P2_DONE=no
if want P2; then
say "== P2. the database's write lock is held by another process =="
tenant "ps-a-$RUN" && TA="$TKEY" || { bad "P2 could not create tenant ps-a-$RUN: $RC"; TA=""; }
# Three sandboxes at most run at once (the lab's memory): ps-l-sl is also the
# expose target, and ps-l-zz (asleep) also the delete target.
mk ps-l-run; R="$SB"; mk ps-l-sl; SL="$SB"; EX="$SL"; mk ps-l-zz; ZZ="$SB"; DL="$ZZ"
for v in "$R" "$SL" "$ZZ"; do [ -n "$v" ] && { ready "$v" 240 || bad "P2 $v never became exec-ready"; }; done
serve "$R"
i=0; until [ "$(gw_get "$HOST" | awk '{print $NF}')" = 200 ] || [ $i -ge 40 ]; do sleep 0.5; i=$((i+1)); done
eq "P2 before the lock: the gateway proxies ps-l-run" "$(gw_get "$HOST")" "ps-$R 200"
req POST "/api/v1/sandboxes/$ZZ/sleep"; [ "$RC" = 200 ] && acked sleep "$ZZ" state sleeping; eq "P2 ps-l-zz asleep before the lock (for the wake below)" "$RC" 200
PF0="$(metric felucca_persist_failures_total)"
# -- window 1: the first change is a create
lock_on 300
V0="$(worker_ids)"
mk ps-l-new; T1="$RT"; NEW="$SB"
say "  first change under the lock, create ps-l-new: $RC $RB (${T1}s)"
[ "$RC" = 201 ] && { acked create "$NEW" state running; }
eq "P2 first change (create): answer" "$RC $(jf "$RBF" .error)" "503 store unavailable"
lt "$T1" 10 && ok "P2 first change answered within 10 s (${T1}s)" || bad "P2 first change answered within 10 s — got ${T1}s"
i=0; until [ "$(worker_ids)" = "$V0" ] || [ $i -ge 40 ]; do sleep 0.5; i=$((i+1)); done
eq "P2 the refused create left no VM on any worker (20 s limit)" "$(comm -13 <(echo "$V0") <(worker_ids) | tr '\n' ' ')" ""
# later changes: refused at once, and no worker is called
for op in "sleep $SL" "expose $EX" "wake $ZZ" "delete $DL"; do
  set -- $op
  case "$1" in
    sleep)  req POST "/api/v1/sandboxes/$2/sleep";;
    expose) req POST "/api/v1/sandboxes/$2/expose" '{"name":"web","port":8069}';;
    delete) req DELETE "/api/v1/sandboxes/$2";;
    wake)   req POST "/api/v1/sandboxes/$2/wake";;
  esac
  say "  later change under the lock, $1: $RC $RB (${RT}s, Retry-After '${RA}')"
  case "$RC" in 2??) case "$1" in sleep) acked sleep "$2" state sleeping;; wake) acked wake "$2" state running;; expose) acked expose "$2" web yes;; delete) acked delete "$2" state absent;; esac;; esac
  eq "P2 later $1: answer" "$RC $(jf "$RBF" .error) ${RA}" "503 store unavailable 5"
  lt "$RT" 1 && ok "P2 later $1: answered in under 1 s (${RT}s)" || bad "P2 later $1: answered in under 1 s — got ${RT}s"
done
eq "P2 the refused sleep reached no worker (ps-l-sl still running there)" "$(on_worker "$SL" | cut -d@ -f1)" running
eq "P2 the refused wake and delete reached no worker (ps-l-zz still asleep there)" "$(on_worker "$ZZ" | cut -d@ -f1)" sleeping
set -- $(healthz)
eq "P2 /healthz under the lock (body, code)" "$1 $2" '{"ok":false,"store":"failing"} 503'
lt "${3:-9}" 1 && ok "P2 /healthz answered in under 1 s (${3}s)" || bad "P2 /healthz answered in under 1 s — got ${3:-none}s"
PF1="$(metric felucca_persist_failures_total)"
[ -n "$PF1" ] && [ "${PF1:-0}" -gt "${PF0:-0}" ] 2>/dev/null && ok "P2 felucca_persist_failures_total grew (${PF0} -> ${PF1})" || bad "P2 felucca_persist_failures_total grew — got '${PF0}' -> '${PF1}'"
eq "P2 felucca_store_healthy under the lock" "$(metric felucca_store_healthy)" 0
# reads go on
if [ -n "$TA" ]; then req GET /api/v1/sandboxes "" "$TA"; eq "P2 tenant-key GET /api/v1/sandboxes" "$RC" 200; lt "$RT" 1 && ok "P2 tenant GET in under 1 s (${RT}s)" || bad "P2 tenant GET in under 1 s — got ${RT}s"; fi
req GET /api/v1/sandboxes; eq "P2 admin GET /api/v1/sandboxes" "$RC" 200; lt "$RT" 1 && ok "P2 admin GET in under 1 s (${RT}s)" || bad "P2 admin GET in under 1 s — got ${RT}s"
req GET /metrics; eq "P2 /metrics" "$RC" 200; lt "$RT" 1 && ok "P2 /metrics in under 1 s (${RT}s)" || bad "P2 /metrics in under 1 s — got ${RT}s"
req GET /api/v1/routes; eq "P2 route table" "$RC" 200; lt "$RT" 1 && ok "P2 route table in under 1 s (${RT}s)" || bad "P2 route table in under 1 s — got ${RT}s"
cp_exec "$R" 'echo exec-ok'; eq "P2 exec in ps-l-run" "$RC $OUT" "200 exec-ok"; lt "$RT" 1 && ok "P2 exec in under 1 s (${RT}s)" || bad "P2 exec in under 1 s — got ${RT}s"
eq "P2 the gateway still proxies ps-l-run" "$(gw_get "$HOST")" "ps-$R 200"
sleep 6   # a heartbeat or more from each worker
eq "P2 worker heartbeats are taken: every node ready" "$(curl -s -m 10 -H "$H" "$CP/api/v1/nodes" | jq -r '[(.nodes // .)[] | .status] | unique | join(",")')" ready
say "  memory under the lock: $(api_rows | grep -E "$(echo "$R|$SL|$EX|$DL|$ZZ|${NEW:-none}")" | tr '\n' ';')"
lock_off
P2_DONE=yes
fi

if want P3; then
say "== P3. lock released: the control plane catches up by itself =="
if [ "$P2_DONE" != yes ]; then skip "P3 (needs P2 in the same run)"; else
  # no request at all (the gateway's and the workers' own traffic goes on)
  T="$(wait_hz 200 10)"; [ "$T" != never ] && ok "P3 /healthz 200 again without a request or a restart (after ${T}s)" || bad "P3 /healthz 200 again within 10 s — got '$(hz_code)'"
  eq "P3 felucca_store_healthy" "$(metric felucca_store_healthy)" 1
  sleep 1
  eq "P3 the database holds what memory holds (id state exposes)" "$(db_rows | tr '\n' ';')" "$(api_rows | tr '\n' ';')"
  # -- windows 2-4: the first change acts on a worker: applied but not saved
  for op in "expose $EX" "sleep $SL" "delete $DL"; do
    set -- $op
    [ "$(hz_code)" = 200 ] || { T="$(wait_hz 200 15)"; }
    lock_on 300
    case "$1" in
      sleep)  req POST "/api/v1/sandboxes/$2/sleep";;
      expose) req POST "/api/v1/sandboxes/$2/expose" '{"name":"web","port":8069}';;
      delete) req DELETE "/api/v1/sandboxes/$2";;
    esac
    say "  first change under a new lock, $1: $RC $RB (${RT}s, Retry-After '${RA}')"
    case "$RC" in
      2??) case "$1" in sleep) acked sleep "$2" state sleeping;; expose) acked expose "$2" web yes;; delete) acked delete "$2" state absent;; esac;;
      503) case "$1" in sleep|delete) applied "$2" state;; expose) applied "$2" web;; esac;;
    esac
    eq "P3 first $1 under the lock: answer" "$RC $(jf "$RBF" .error) $(jf "$RBF" .applied) ${RA}" "503 applied but not saved true 5"
    lt "$RT" 10 && ok "P3 first $1: answered within 10 s (${RT}s)" || bad "P3 first $1: answered within 10 s — got ${RT}s"
    case "$1" in
      sleep)  eq "P3 the worker did sleep ps-l-sl" "$(on_worker "$2" | cut -d@ -f1)" sleeping;;
      delete) eq "P3 the worker did delete ps-l-zz" "$(on_worker "$2" | cut -d@ -f1)" "";;
      expose) eq "P3 memory has the expose of ps-l-ex" "$(sb_web "$2")" yes;;
    esac
    lock_off
    T="$(wait_hz 200 10)"; [ "$T" != never ] && ok "P3 after the $1 window: healthy again by itself (after ${T}s)" || bad "P3 after the $1 window: healthy again within 10 s — got '$(hz_code)'"
    sleep 1
    eq "P3 after the $1 window: the database holds what memory holds" "$(db_rows | tr '\n' ';')" "$(api_rows | tr '\n' ';')"
  done
fi
fi

# ==== P4 =========================================================================
if want P4; then
say "== P4. nothing answered 2xx is lost by a restart =="
if [ "$P2_DONE" != yes ]; then skip "P4 (needs P2 in the same run)"; else
  restart_cp; T="$(wait_hz 200 15)"
  LOST=0; N=0
  for k in "${!EXP[@]}"; do
    id="${k%.*}"; attr="${k##*.}"; w="${EXP[$k]}"; [ "$w" = "?" ] && continue
    N=$((N+1))
    case "$attr" in state) g="$(sb_state "$id")";; web) [ "$(sb_state "$id")" = absent ] && continue; g="$(sb_web "$id")";; esac
    if [ "$g" != "$w" ]; then LOST=$((LOST+1)); say "  lost: $id $attr — got '$g', want '$w' (answered 2xx before the restart)"; fi
  done
  eq "P4 changes answered 2xx and lost by the restart (of $N)" "$LOST" 0
  # the other direction: a sandbox the control plane does not know, running on a worker
  ORPH=""; for id in $(sort -u "$IDS"); do [ "$(sb_state "$id")" = absent ] && on_worker "$id" >/dev/null && ORPH="$ORPH $id"; done
  eq "P4 sandboxes running on a worker that the control plane does not know" "$ORPH" ""
fi
fi

# ==== P5 =========================================================================
if want P5; then
say "== P5. feluccad restarted WHILE the lock is held =="
prune
[ "$(hz_code)" = 200 ] || wait_hz 200 15 >/dev/null
lock_on 300
restart_cp
eq "P5 feluccad came up under the lock" "$(svc_active)" active
T="$(wait_hz 503 10)"; [ "$T" != never ] && ok "P5 /healthz 503 (after ${T}s)" || bad "P5 /healthz 503 within 10 s — got '$(healthz)'"
req GET /api/v1/sandboxes; eq "P5 reads work (GET /api/v1/sandboxes)" "$RC" 200
req GET /api/v1/routes; eq "P5 the route table works" "$RC" 200
mk ps-r5; say "  create under the lock after the restart: $RC $RB (${RT}s)"; N5="$SB"
eq "P5 a change is refused" "$RC $(jf "$RBF" .error)" "503 store unavailable"
[ "$RC" = 201 ] && acked create "$N5" state running
lock_off
T="$(wait_hz 200 10)"; [ "$T" != never ] && ok "P5 recovers by itself after the release (after ${T}s)" || bad "P5 recovers by itself within 10 s — got '$(hz_code)'"
mk ps-r5b; eq "P5 a change after the recovery" "$RC" 201; N5B="$SB"
i=0; until db_has "$N5B" || [ $i -ge 20 ]; do sleep 0.5; i=$((i+1)); done
db_has "$N5B" && ok "P5 and it is in the database" || bad "P5 the create after the recovery is not in the database (10 s limit)"
fi

# ==== P6 =========================================================================
if want P6; then
say "== P6. usage across a lock window: the hours are the intervals that really ran =="
prune
if ! tenant "ps-u-$RUN"; then bad "P6 could not create tenant ps-u-$RUN ($RC)"; else
  TU="$TKEY"; TUID="$TID"; T6="$(date +%s)"; sleep 1
  # intervals: "<id> <start> <end>" of acknowledged running time
  : > "$W/iv"
  mk ps-u1 "$TU"; U1="$SB"; [ "$RC" = 201 ] && C1="$(date +%s)"; eq "P6 create ps-u1" "$RC" 201
  ready "$U1" 240 || bad "P6 ps-u1 never became exec-ready"
  [ "$(hz_code)" = 200 ] || wait_hz 200 15 >/dev/null
  lock_on 300
  mk ps-u2 "$TU"; U2="$SB"; say "  create ps-u2 under the lock: $RC $RB"; C2=""; [ "$RC" = 201 ] && C2="$(date +%s)"
  req POST "/api/v1/sandboxes/$U1/sleep" "" "$TU"; say "  sleep ps-u1 under the lock: $RC $RB"; S1=""; [ "$RC" = 200 ] && S1="$(date +%s)"
  sleep 20
  WK=""; if [ -n "$S1" ]; then req POST "/api/v1/sandboxes/$U1/wake" "" "$TU"; say "  wake ps-u1 under the lock: $RC $RB"; [ "$RC" = 200 ] && WK="$(date +%s)"; fi
  lock_off
  wait_hz 200 10 >/dev/null; sleep 5
  req DELETE "/api/v1/sandboxes/$U1" "" "$TU"; D1="$(date +%s)"; eq "P6 delete ps-u1" "$RC" 204
  if [ -n "$C2" ]; then req DELETE "/api/v1/sandboxes/$U2" "" "$TU"; D2="$(date +%s)"; echo "$U2 $C2 $D2" >> "$W/iv"; fi
  if [ -n "$S1" ] && [ -n "$WK" ]; then echo "$U1 $C1 $S1" >> "$W/iv"; echo "$U1 $WK $D1" >> "$W/iv"
  elif [ -n "$S1" ]; then echo "$U1 $C1 $S1" >> "$W/iv"
  else echo "$U1 $C1 $D1" >> "$W/iv"; fi
  WANT="$(awk '{s+=$3-$2} END{print s+0}' "$W/iv")"; NB="$(wc -l < "$W/iv")"; TOL=$(( 4 * NB + 2 ))
  sleep 2
  req GET "/api/v1/tenants/$TUID/usage?from=$((T6-5))&to=$(( $(date +%s) + 60 ))"
  GOT="$(jf "$RBF" '.sandbox_hours * 3600 | floor')"
  say "  acknowledged running intervals: $(tr '\n' ';' < "$W/iv") = ${WANT}s; usage: $RB"
  if [ -n "$GOT" ] && [ "$(( GOT > WANT ? GOT - WANT : WANT - GOT ))" -le "$TOL" ]; then ok "P6 usage hours match what ran (${GOT}s, want ${WANT}s ± ${TOL}s)"; else bad "P6 usage hours match what ran — got '${GOT}'s, want ${WANT}s ± ${TOL}s"; fi
  sleep 10
  req GET "/api/v1/tenants/$TUID/usage?from=$((T6-5))&to=$(( $(date +%s) + 60 ))"
  eq "P6 no interval left open (sandbox seconds 10 s later)" "$(jf "$RBF" '.sandbox_hours * 3600 | floor')" "$GOT"
fi
fi

# ==== P9 =========================================================================
if want P9; then
say "== P9. the database FILE is read-only for the service when feluccad starts =="
prune
# What makes SQLite open read-only here: the main file. When open(O_RDWR) of
# felucca.db fails with EACCES, SQLite's unix VFS opens it O_RDONLY and the
# connection is read-only: reads work, every write fails with SQLITE_READONLY
# ("attempt to write a readonly database"). The -wal and -shm files do not
# decide it, but they need the same care: a read-only connection of a WAL
# database whose -wal/-shm are missing (feluccad removes them on a clean stop)
# CREATES them, with the main file's mode — 0400 here — and a -wal that is left
# read-only keeps every write failing after the main file is writable again
# (measured with python's sqlite3 on this VM: -wal 0400 fails writes, -shm 0400
# alone does not). So all three are made read-only, and all three writable
# again.
# The mode bits alone are not enough: feluccad chmods the main file to 0600
# when it opens the store (internal/store/sqlite.go, "owner-only"), and as the
# file's owner it may. Measured on the lab (l6b, first run): 0400 before the
# start, 0600 right after it. So the files are given to root, group the
# service's group, mode 0640: the service reads them and can neither write
# nor chmod them. finish() gives them back (owner, 0600) whatever happens.
SVC_GROUP="$(id -gn "$SVC_USER" 2>/dev/null)"
sudo systemctl stop "$SVC"
say "  after the stop: $(sudo ls -l "$STATE" | awk '/felucca.db/ {print $1, $3, $4, $NF}' | tr '\n' ';')"
for f in "$DB" "$DB-wal" "$DB-shm"; do sudo test -e "$f" && { sudo chown "root:$SVC_GROUP" "$f"; sudo chmod 0640 "$f"; }; done
sudo systemctl start "$SVC"; i=0; until curl -s -m 1 -o /dev/null "$CP/healthz" || [ $i -ge 40 ]; do sleep 0.25; i=$((i+1)); done
eq "P9 feluccad starts on a read-only database file" "$(svc_active)" active
T="$(wait_hz 503 10)"; [ "$T" != never ] && ok "P9 /healthz 503 (after ${T}s)" || bad "P9 /healthz 503 within 10 s — got '$(healthz)'"
say "  files now: $(sudo ls -l "$STATE" | awk '/felucca.db/ {print $1, $3, $4, $NF}' | tr '\n' ';')"
if sudo -u "$SVC_USER" test -w "$DB"; then bad "P9 the database file is writable for the service after the start: the case tests nothing"; else ok "P9 the database file is still read-only for the service after the start"; fi
req GET /api/v1/sandboxes; eq "P9 reads work" "$RC" 200
req GET /api/v1/routes; eq "P9 the route table works" "$RC" 200
mk ps-ro1; say "  create on the read-only database: $RC $RB (${RT}s)"
eq "P9 a create is refused" "$RC $(jf "$RBF" .error)" "503 store unavailable"
for f in "$DB" "$DB-wal" "$DB-shm"; do sudo test -e "$f" && { sudo chown "$SVC_USER:" "$f"; sudo chmod 0600 "$f"; }; done
T="$(wait_hz 200 15)"; [ "$T" != never ] && ok "P9 writable again: /healthz 200 without a restart (after ${T}s)" || bad "P9 writable again: /healthz 200 within 15 s — got '$(healthz)'"
wait_nodes 30
mk ps-ro2; eq "P9 writable again: a create" "$RC" 201; RO2="$SB"
i=0; until db_has "$RO2" || [ $i -ge 20 ]; do sleep 0.5; i=$((i+1)); done
db_has "$RO2" && ok "P9 and it is saved" || bad "P9 writable again: the create is not in the database (10 s limit; the connection stayed read-only)"
SAVED=no; db_has "$RO2" && SAVED=yes
delete_all; : > "$IDS"
if [ "$SAVED" = no ]; then say "  (feluccad restarted so that the rest of the run has a writable database)"; restart_cp; wait_hz 200 15 >/dev/null; fi
fi

# ==== P10 ========================================================================
if want P10; then
say "== P10. systemctl stop under exec load: the answered exec rows are written =="
prune
if ! tenant "ps-x-$RUN"; then bad "P10 could not create tenant ps-x-$RUN ($RC)"; else
  TX="$TKEY"; TXID="$TID"
  mk ps-x1 "$TX"; X1="$SB"; eq "P10 create ps-x1" "$RC" 201
  ready "$X1" 240 || bad "P10 ps-x1 never became exec-ready"
  sleep 2; T10="$(date +%s)"; sleep 1; : > "$W/ex"
  for L in 1 2 3; do ( e=$(( $(date +%s) + 14 )); while [ "$(date +%s)" -lt "$e" ]; do
      c="$(curl -s -m 10 -o /dev/null -w '%{http_code}' -H "@$TX" -X POST -d '{"cmd":["true"],"timeout_ms":3000}' "$CP/api/v1/sandboxes/$X1/exec")"
      echo "$L $c $(date +%s.%N)" >> "$W/ex"; done ) & done
  sleep 5; TS="$(date '+%Y-%m-%d %H:%M:%S')"; S0="$(date +%s.%N)"
  sudo systemctl stop "$SVC"; S1="$(date +%s.%N)"; wait
  STOP="$(awk -v a="$S0" -v b="$S1" 'BEGIN{printf "%.1f", b-a}')"
  lt "$STOP" 15 && ok "P10 feluccad stopped within 15 s (${STOP}s)" || bad "P10 feluccad stopped within 15 s — got ${STOP}s"
  sudo systemctl start "$SVC"; wait_hz 200 20 >/dev/null
  N2XX="$(awk '$2 ~ /^2/' "$W/ex" | wc -l | tr -d ' ')"; NALL="$(wc -l < "$W/ex" | tr -d ' ')"
  say "  exec answers: $(awk '{print $2}' "$W/ex" | sort | uniq -c | awk '{printf "%sx%s ", $1, $2}')(all before the stop that are 2xx: $N2XX)"
  # Counted: every exec answered 2xx (they were all answered before the process
  # exited). Usage counts one exec row per exec it served, answered or not, so
  # it may be higher, never lower.
  sleep 1; req GET "/api/v1/tenants/$TXID/usage?from=$T10&to=$(( $(date +%s) + 60 ))"; GOT="$(jf "$RBF" '.execs // empty')"
  [ -n "$GOT" ] && [ "$GOT" -ge "$N2XX" ] && ok "P10 usage counts every exec answered 2xx before the stop (execs $GOT >= $N2XX)" || bad "P10 usage counts every exec answered 2xx — got execs '$GOT', want >= $N2XX ($RB)"
  # The ticket: the stop's journal lines begin "stopping:" (the wording after it is not part of the contract).
  L="$(sudo journalctl -u "$SVC" --since "$TS" --no-pager -o cat | grep -c 'msg="stopping:')"
  say "  stop lines: $(sudo journalctl -u "$SVC" --since "$TS" --no-pager -o cat | grep -E 'msg="stop' | cut -c1-200 | tr '\n' '|')"
  [ "$L" -ge 1 ] && ok "P10 the journal has a 'stopping:' line" || bad "P10 the journal has a 'stopping:' line — got $L lines"
  req DELETE "/api/v1/sandboxes/$X1"
fi
fi

# ==== P11 ========================================================================
if want P11; then
say "== P11. two forks of one running parent at the same moment =="
prune
mk ps-fp; FP="$SB"; eq "P11 create the parent" "$RC" 201
ready "$FP" 240 || bad "P11 the parent never became exec-ready"
rm -f "$W/go"
for c in a b; do ( until [ -e "$W/go" ]; do sleep 0.005; done; REQ_TIMEOUT=120 req POST "/api/v1/sandboxes/$FP/fork" "{\"name\":\"ps-fk-$c\"}"; printf '%s %s %s\n' "$RC" "${RA:--}" "$RBF" > "$W/fork-$c" ) & done
sleep 0.3; touch "$W/go"; wait
for c in a b; do say "  fork $c: $(cat "$W/fork-$c")"; jf "$(cut -d' ' -f3- "$W/fork-$c")" '.id // empty' | while read -r k; do [ -n "$k" ] && seen "$k"; done; done
CODES="$(cat "$W"/fork-a "$W"/fork-b | awk '{print $1}' | sort | tr '\n' ' ' | sed 's/ $//')"
eq "P11 one 201 and one 409" "$CODES" "201 409"
REF="$(grep -h '^409 ' "$W"/fork-a "$W"/fork-b | head -1)"
eq "P11 the 409: body and Retry-After" "$(printf '%s' "$REF" | awk '{print $2}') $(jf "$(printf '%s' "$REF" | cut -d' ' -f3-)" .error)" "2 busy"
sleep 3
for k in $(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '(.sandboxes // .)[] | select(.name|startswith("ps-fk-")) | .id'); do seen "$k"; done
eq "P11 records of the two children afterwards (state)" "$(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '[(.sandboxes // .)[] | select(.name|startswith("ps-fk-")) | .state] | sort | join(" ")')" running
fi

# ==== P8 =========================================================================
if want P8; then
say "== P8. full and read-only file system under the database (PERSIST_FULL=1) =="
if [ "${PERSIST_FULL:-0}" != 1 ]; then
  skip "P8 full file system (PERSIST_FULL is not 1: it mounts a small file system over $STATE)"
  skip "P8 read-only file system (PERSIST_FULL is not 1: it remounts $STATE read-only)"
else
  # (a) read-only file system: a read-only bind mount of the state directory on
  # itself, made while feluccad is stopped so that it starts on it.
  sudo systemctl stop "$SVC"
  sudo mount --bind "$STATE" "$STATE" && sudo mount -o remount,ro,bind "$STATE"
  sudo systemctl start "$SVC"; i=0; until curl -s -m 1 -o /dev/null "$CP/healthz" || [ $i -ge 40 ]; do sleep 0.25; i=$((i+1)); done
  eq "P8 ro: feluccad starts" "$(svc_active)" active
  T="$(wait_hz 503 10)"; [ "$T" != never ] && ok "P8 ro: /healthz 503 (after ${T}s)" || bad "P8 ro: /healthz 503 within 10 s — got '$(healthz)'"
  req GET /api/v1/sandboxes; eq "P8 ro: reads work" "$RC" 200
  mk ps-fs1; eq "P8 ro: a create is refused" "$RC $(jf "$RBF" .error)" "503 store unavailable"
  sudo umount "$STATE"
  T="$(wait_hz 200 15)"; [ "$T" != never ] && ok "P8 ro: writable again, recovered without a restart (after ${T}s)" || bad "P8 ro: recovered within 15 s — got '$(healthz)'"
  [ "$(hz_code)" = 200 ] || restart_cp
  # (b) full file system: the state directory's files on a 64 MiB file system
  # that is then filled. Copied over while feluccad is stopped, and back after.
  IMG=/var/tmp/felucca-lab-persist.img; MNT="$STATE"
  sudo systemctl stop "$SVC"
  sudo rm -rf /var/tmp/felucca-lab-persist.keep; sudo cp -a "$STATE" /var/tmp/felucca-lab-persist.keep
  sudo truncate -s 64M "$IMG"; sudo mkfs.ext4 -q -m 0 "$IMG"
  sudo mount -o loop "$IMG" "$MNT"
  for f in felucca.db felucca.db-wal felucca.db-shm wg.key; do sudo test -e "/var/tmp/felucca-lab-persist.keep/$f" && sudo cp -a "/var/tmp/felucca-lab-persist.keep/$f" "$MNT/"; done
  sudo chown "$SVC_USER:" "$MNT"; sudo chmod 750 "$MNT"
  sudo systemctl start "$SVC"; wait_hz 200 20 >/dev/null
  sudo fallocate -l $(( ($(df --output=avail "$MNT" | tail -1) - 8) * 1024 )) "$MNT/FILL" 2>/dev/null
  sudo dd if=/dev/zero of="$MNT/FILL2" bs=4k 2>/dev/null
  say "  filled: $(df -h --output=avail "$MNT" | tail -1 | tr -d ' ') left"
  mk ps-fs2; say "  create on a full file system: $RC $RB"
  eq "P8 full: a create is refused" "$RC $(jf "$RBF" .error)" "503 store unavailable"
  T="$(wait_hz 503 5)"; [ "$T" != never ] && ok "P8 full: /healthz 503" || bad "P8 full: /healthz 503 — got '$(healthz)'"
  sudo rm -f "$MNT/FILL" "$MNT/FILL2"
  T="$(wait_hz 200 15)"; [ "$T" != never ] && ok "P8 full: space again, recovered without a restart (after ${T}s)" || bad "P8 full: recovered within 15 s — got '$(healthz)'"
  sudo systemctl stop "$SVC"
  for f in felucca.db felucca.db-wal felucca.db-shm; do sudo rm -f "/var/tmp/felucca-lab-persist.keep/$f"; sudo test -e "$MNT/$f" && sudo cp -a "$MNT/$f" "/var/tmp/felucca-lab-persist.keep/"; done
  sudo umount "$MNT"
  for f in felucca.db felucca.db-wal felucca.db-shm; do sudo rm -f "$STATE/$f"; sudo test -e "/var/tmp/felucca-lab-persist.keep/$f" && sudo cp -a "/var/tmp/felucca-lab-persist.keep/$f" "$STATE/"; done
  sudo rm -f "$IMG"; sudo rm -rf /var/tmp/felucca-lab-persist.keep
  sudo systemctl start "$SVC"; wait_hz 200 20 >/dev/null
fi
fi

# ==== P7 (last) ==================================================================
if want P7; then
say "== P7. a heartbeat with mem_free_mib above 2^40 (the worker's own credential) =="
prune
NODE="$(curl -s -m 10 -H "$H" "$CP/api/v1/nodes" | jq -r '(.nodes // .)[] | select(.status=="ready") | .id' | head -1)"
NODEJ="$(curl -s -m 10 -H "$H" "$CP/api/v1/nodes" | jq -c --arg n "$NODE" '(.nodes // .)[] | select(.id==$n)')"
T7="$(date '+%Y-%m-%d %H:%M:%S')"
req POST /api/v1/agents/heartbeat "{\"id\":\"$NODE\",\"mem_free_mib\":9223372036854775808,\"vm_count\":$(jf "$NODEJ" '.vm_count // 0'),\"pool_size\":$(jf "$NODEJ" '.pool_size // 0')}"
say "  heartbeat for $NODE: $RC $RB"
eq "P7 the heartbeat is refused" "$RC $RB" '400 {"error":"mem_free_mib out of range"}'
mk ps-hb; eq "P7 a create afterwards" "$RC" 201; HB="$SB"
i=0; until db_has "$HB" || [ $i -ge 20 ]; do sleep 0.5; i=$((i+1)); done
db_has "$HB" && ok "P7 and it is saved" || bad "P7 the create after the heartbeat is not in the database (10 s limit)"
say "  persist failures in the journal since the heartbeat: $(sudo journalctl -u "$SVC" --since "$T7" --no-pager -o cat | grep -ci 'persist failed')"
say "  last one: $(sudo journalctl -u "$SVC" --since "$T7" --no-pager -o cat | grep -i 'persist failed' | tail -1 | cut -c1-240)"
skip "P7 registration with mem_total_mib above 2^40 (not sent: on a build without the range check it rewrites a live node's memory total until its agent registers again)"
fi

say "== end =="
finish; trap - EXIT
sleep 2
eq "end: sandboxes on the workers are what they were (pool VMs aside)" "$(worker_ids | tr '\n' ' ')" "$(printf '%s' "$WORKERS0" | sed '/^$/d' | tr '\n' ' ')"
eq "end: no ps- sandbox in the control plane" "$(curl -s -m 10 -H "$H" "$CP/api/v1/sandboxes" | jq -r '[(.sandboxes // .)[] | select(.name|startswith("ps-")) | .id] | join(" ")')" ""
say "  /healthz at the end: $(healthz)"
summary
