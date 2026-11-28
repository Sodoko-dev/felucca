#!/usr/bin/env bash
# Lab gate for FLU-8: the agent's isolation watch puts back what is removed,
# takes the guest bridge down only when the kernel refuses the repair, and the
# control plane steers creates away from a worker that reports its isolation
# down. Run from the macOS host:
#
#   bash scripts/verify-isolation.sh [worker-vm] [cases]   # default worker: kata-lab-1
#
# <cases> is "all" (default) or a list of C1 C2 C3 C4 C5 C6 C7 C8 C9 C10 END
# (C3 and C10 each run as two parts, with the control-plane VM's half in
# between). The cases are in test/lab/isolation.sh, which runs ON the worker
# against the agent's own API; this wrapper ships it, runs each part DETACHED
# (a dropped terminal must not leave the worker with its bridge down: the part
# that holds a refusal also arms an undo after 300 s) and follows its output,
# and from the control-plane VM ($FELUCCA_LAB_CP, default infra-saas-lab) it
#   - fetches the exposed node port before, during (must fail at once) and
#     after C3's refusal;
#   - reads feluccad's journal for C10 (`node reports its isolation down`,
#     `in place again`).
#
# The lab token is read from $FELUCCA_TOKEN_FILE (default
# ~/.config/felucca/lab-token) and never put on a command line: it goes over
# standard input into a header file in a 0700 directory of the worker (mode
# 0600) that the gate's curls read with `-H @file`; the file is removed when
# this wrapper ends.
#
# Prints one `RESULT: n passed, m failed, k skipped` over all parts and exits
# non-zero on any failure. Takes about 25 minutes. Rough with the worker on
# purpose (it deletes and breaks the worker's Felucca nft tables, flushes its
# whole ruleset once, restarts the agent): do not point it at a worker that
# holds sandboxes you care about. Every sandbox it creates is named is-*.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORKER="${1:-${FELUCCA_LAB_WORKER:-kata-lab-1}}"
CASES="${2:-all}"
CPVM="${FELUCCA_LAB_CP:-infra-saas-lab}"
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
[ -s "$TOKEN_FILE" ] || { echo "ERROR: no lab token: write it to $TOKEN_FILE (or point FELUCCA_TOKEN_FILE at it)" >&2; exit 2; }
REMOTE=/tmp/felucca-lab
AUTHD=/tmp/felucca-lab-auth; AUTH="$AUTHD/auth.hdr"
P=0; F=0; S=0
LOCAL_OUT="$(mktemp -t felucca-isolation.XXXXXX)"
vsh() { limactl shell --workdir /tmp "$WORKER" -- "$@"; }
csh() { limactl shell --workdir /tmp "$CPVM" -- "$@"; }
pass() { P=$((P+1)); echo "  PASS: $*"; }
fail() { F=$((F+1)); echo "  FAIL: $*"; }
trap 'vsh rm -f "$AUTH" >/dev/null 2>&1; rm -f "$LOCAL_OUT"' EXIT

vsh mkdir -p "$REMOTE" || { echo "ERROR: cannot reach lab VM $WORKER" >&2; exit 2; }
vsh bash -c "umask 077; mkdir -p $AUTHD && chmod 700 $AUTHD && { printf 'Authorization: Bearer '; tr -d ' \t\r\n'; printf '\n'; } > $AUTH && chmod 600 $AUTH" < "$TOKEN_FILE" \
  || { echo "ERROR: could not put the auth header file into $WORKER" >&2; exit 2; }
ship() { for f in lib.sh isolation.sh; do limactl cp "$REPO/test/lab/$f" "$WORKER:$REMOTE/$f" >/dev/null || return 1; done; }
ship || { echo "ERROR: cannot copy the gate into $WORKER" >&2; exit 2; }
WADDR="$(vsh ip -4 -br addr show eth0 2>/dev/null | awk '{print $3}' | cut -d/ -f1 | tr -d '\r')"

tally() {
  local l; l="$(grep -E '^RESULT: [0-9]+ passed, [0-9]+ failed, [0-9]+ skipped' "$LOCAL_OUT" | tail -1)"
  if [ -z "$l" ]; then F=$((F+1)); echo "  FAIL: part '$1' printed no RESULT line"; return; fi
  set -- $l; P=$((P+$2)); F=$((F+$4)); S=$((S+$6))
}
# part <cases> — run test/lab/isolation.sh <cases> detached in the worker and follow it
part() {
  local out=/var/tmp/felucca-lab-isolation.out seen=0 total rc
  : > "$LOCAL_OUT"
  echo "== isolation.sh '$1' on $WORKER (log in the VM: $out) =="
  vsh bash -c ": > $out; ( FELUCCA_AUTH_FILE=$AUTH setsid nohup bash $REMOTE/isolation.sh $(printf '%q' "$1") >> $out 2>&1 < /dev/null; echo \"GATE-EXIT \$?\" >> $out ) >/dev/null 2>&1 &" || exit 2
  while :; do
    total="$(vsh bash -c "wc -l < $out" 2>/dev/null | tr -d ' \r\n')"; total="${total:-0}"
    if [ "$total" -gt "$seen" ] 2>/dev/null; then
      vsh bash -c "sed -n '$((seen+1)),${total}p' $out" 2>/dev/null | grep -v '^GATE-EXIT ' | tee -a "$LOCAL_OUT"; seen="$total"
    fi
    rc="$(vsh bash -c "sed -n 's/^GATE-EXIT //p' $out | tail -1" 2>/dev/null | tr -d ' \r\n')"
    if [ -n "$rc" ]; then
      # the part may have ended between the line count and this look: read what came after the last count
      vsh bash -c "sed -n '$((seen+1)),\$p' $out" 2>/dev/null | grep -v '^GATE-EXIT ' | tee -a "$LOCAL_OUT"
      break
    fi
    sleep 5
  done
  cp "$LOCAL_OUT" "$LOCAL_OUT.last"
  tally "$1"
}
has() { [ "$CASES" = all ] || case " $CASES " in *" $1 "*) return 0;; *) return 1;; esac; }
NP=""
# fetch <label> <want: ok|fail> — the exposed node port from the control-plane VM
fetch() {
  local r code t
  [ -n "$NP" ] || { S=$((S+1)); echo "  SKIP: $1: no node port known (C1 prints it)"; return; }
  r="$(csh curl -s -m 3 -o /dev/null -w '%{http_code} %{time_total}' "http://$WADDR:$NP/" 2>/dev/null | tr -d '\r')"; code="${r%% *}"; t="${r#* }"
  echo "  $1: curl -m 3 http://$WADDR:$NP/ from $CPVM: code $code in ${t} s"
  if [ "$2" = ok ]; then [ "$code" = 200 ] && pass "$1: node port answers 200" || fail "$1: node port — got '$code', want 200"
  else
    [ "$code" = 000 ] && pass "$1: node port fails" || fail "$1: node port — got '$code', want no answer (000)"
    awk -v t="$t" 'BEGIN{exit !(t<1.0)}' && pass "$1: it fails at once (${t} s < 1 s)" || fail "$1: it fails at once — got ${t} s, want < 1 s"
  fi
}
grab() { sed -n "s/.*$1=\\([^ ]*\\).*/\\1/p" "$LOCAL_OUT.last" | tail -1 | tr -d '\r'; }

G=""; for c in C1 C2; do has $c && G="$G $c"; done
[ -n "$G" ] && { part "${G# }"; NP="$(grab NODEPORT)"; }
if has C3; then
  [ -z "$NP" ] && { part C1; NP="$(grab NODEPORT)"; }
  fetch "C3 before" ok
  part C3a
  fetch "C3 refusal held (bridge down)" fail
  part C3b
  fetch "C3 after the repair" ok
fi
G=""; for c in C4 C5 C6 C7 C8 C9; do has $c && G="$G $c"; done
[ -n "$G" ] && part "${G# }"
if has C10; then
  part C10a
  T10="$(grab since)"; T10="${T10%.*}"
  if [ -n "$T10" ]; then
    n="$(csh sudo journalctl -u feluccad --since "@$T10" --no-pager -o cat 2>/dev/null | grep -c 'node reports its isolation down')"
    [ "${n:-0}" -ge 1 ] && pass "C10: feluccad journal: 'node reports its isolation down' ($n)" || fail "C10: feluccad journal: 'node reports its isolation down' — got 0 lines"
    csh sudo journalctl -u feluccad --since "@$T10" --no-pager -o cat 2>/dev/null | grep -E 'isolation|WARN' | cut -c1-300 | sed 's/^/    /'
  else S=$((S+1)); echo "  SKIP: C10: C10a printed no start time"; fi
  part C10b
  if [ -n "$T10" ]; then
    n="$(csh sudo journalctl -u feluccad --since "@$T10" --no-pager -o cat 2>/dev/null | grep -c 'node reports its isolation in place again')"
    [ "${n:-0}" -ge 1 ] && pass "C10: feluccad journal: 'in place again' ($n)" || fail "C10: feluccad journal: 'in place again' — got 0 lines"
  fi
fi
has END && part END
rm -f "$LOCAL_OUT.last"

echo
echo "RESULT: ${P} passed, ${F} failed, ${S} skipped"
[ "$F" -eq 0 ]
