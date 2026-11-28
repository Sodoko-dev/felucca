#!/usr/bin/env bash
# Lab gate for FLU-6 and FLU-7 (ingress): a deleted sandbox's hostname never
# reaches another sandbox, waking a sandbox does not send a request for its
# second service to its first, the gateway stops serving from a route table
# that is too old. Run from the macOS host:
#
#   bash scripts/verify-ingress.sh [control-plane-vm] [cases]   # default VM: infra-saas-lab
#   GW_BIN=/path/in/the/vm/felucca-gw bash scripts/verify-ingress.sh   # the gateway binary of I4, I7, I10
#   INGRESS_LONG=1 bash scripts/verify-ingress.sh "" I9                # also I9 (sleeps 16 minutes; run it alone)
#
# <cases> is "all" (default) or a list of I1 ... I13 (see test/lab/ingress.sh
# for what each one checks). The cases run inside the control-plane VM, against
# feluccad, the gateway and both workers; this wrapper ships them, runs each
# part DETACHED and follows its output. I13 needs the other worker unreachable:
# between its parts this wrapper SIGSTOPs that worker's agent
# (`systemctl kill -s STOP felucca-agent`, via limactl) and CONTs it again; a
# detached CONT after 150 s is armed on that worker first, so a dropped
# terminal cannot leave it stopped. The part END (always run) removes every
# ig- sandbox.
#
# Environment: GW_BIN (a path inside the control-plane VM; default the
# installed /usr/local/bin/felucca-gw), INGRESS_LONG, INGRESS_NODE (the worker
# the same-worker cases use, matched against the node's hostname; default
# kata-lab-1), INGRESS_STOP_VM (the Lima VM of the OTHER worker, stopped in I13;
# default kata-lab-0).
#
# The lab token is read from $FELUCCA_TOKEN_FILE (default
# ~/.config/felucca/lab-token) and never put on a command line: it goes over
# standard input into a header file in a 0700 directory of the VM (mode 0600)
# that the gate's curls read with `-H @file`; the file is removed when this
# wrapper ends.
#
# Prints one `RESULT: n passed, m failed, k skipped` over all parts and exits
# non-zero on any failure. Takes about 20 minutes (I9 adds 16). Every sandbox it
# creates is named ig-*; at most four run at once.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
VM="${1:-${FELUCCA_LAB_CP:-infra-saas-lab}}"; [ -n "$VM" ] || VM=infra-saas-lab
CASES="${2:-all}"
STOP_VM="${INGRESS_STOP_VM:-kata-lab-0}"
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
[ -s "$TOKEN_FILE" ] || { echo "ERROR: no lab token: write it to $TOKEN_FILE (or point FELUCCA_TOKEN_FILE at it)" >&2; exit 2; }
REMOTE=/tmp/felucca-lab
AUTHD=/tmp/felucca-lab-auth; AUTH="$AUTHD/auth.hdr"
P=0; F=0; S=0
LOCAL_OUT="$(mktemp -t felucca-ingress.XXXXXX)"
vsh() { limactl shell --workdir /tmp "$VM" -- "$@"; }
ssh_stop() { limactl shell --workdir /tmp "$STOP_VM" -- "$@"; }
cont_other() { ssh_stop sudo systemctl kill -s CONT felucca-agent >/dev/null 2>&1; }
STOPPED=no
trap '[ "$STOPPED" = yes ] && cont_other; vsh rm -f "$AUTH" >/dev/null 2>&1; rm -f "$LOCAL_OUT" "$LOCAL_OUT.last"' EXIT

vsh mkdir -p "$REMOTE" || { echo "ERROR: cannot reach lab VM $VM" >&2; exit 2; }
vsh bash -c "umask 077; mkdir -p $AUTHD && chmod 700 $AUTHD && { printf 'Authorization: Bearer '; tr -d ' \t\r\n'; printf '\n'; } > $AUTH && chmod 600 $AUTH" < "$TOKEN_FILE" \
  || { echo "ERROR: could not put the auth header file into $VM" >&2; exit 2; }
for f in lib.sh ingress.sh; do limactl cp "$REPO/test/lab/$f" "$VM:$REMOTE/$f" >/dev/null || { echo "ERROR: cannot copy the gate into $VM" >&2; exit 2; }; done
vsh chmod 755 "$REMOTE" && vsh chmod 644 "$REMOTE/lib.sh" "$REMOTE/ingress.sh"
ENVS="FELUCCA_AUTH_FILE=$AUTH"
for v in GW_BIN INGRESS_LONG INGRESS_NODE; do eval "x=\${$v:-}"; [ -n "$x" ] && ENVS="$ENVS $v=$(printf '%q' "$x")"; done

tally() {
  local l; l="$(grep -E '^RESULT: [0-9]+ passed, [0-9]+ failed, [0-9]+ skipped' "$LOCAL_OUT" | tail -1)"
  if [ -z "$l" ]; then F=$((F+1)); echo "  FAIL: part '$1' printed no RESULT line"; return; fi
  set -- $l; P=$((P+$2)); F=$((F+$4)); S=$((S+$6))
}
# part <cases> — run test/lab/ingress.sh <cases> detached in the VM and follow it
part() {
  local out=/var/tmp/felucca-lab-ingress.out seen=0 total rc
  : > "$LOCAL_OUT"
  echo "== ingress.sh '$1' on $VM (log in the VM: $out) =="
  vsh bash -c ": > $out; ( $ENVS setsid nohup bash $REMOTE/ingress.sh $(printf '%q' "$1") >> $out 2>&1 < /dev/null; echo \"GATE-EXIT \$?\" >> $out ) >/dev/null 2>&1 &" || exit 2
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
  tally "$1"
}
has() { [ "$CASES" = all ] || case " $CASES " in *" $1 "*) return 0;; *) return 1;; esac; }

G=""; for c in I7 I10 I1 I2 I12 I11 I4 I3 I5 I8 I9 I6; do has $c && G="$G $c"; done
[ -n "$G" ] && part "${G# }"
if has I13; then
  part I13a
  echo "== $(date '+%T') $STOP_VM: SIGSTOP felucca-agent (a CONT is armed there for +150 s) =="
  ssh_stop bash -c "setsid nohup bash -c 'sleep 150; sudo systemctl kill -s CONT felucca-agent' >/dev/null 2>&1 < /dev/null &"
  ssh_stop sudo systemctl kill -s STOP felucca-agent && STOPPED=yes || echo "  (could not stop the agent on $STOP_VM)"
  part I13b
  cont_other && STOPPED=no; echo "== $(date '+%T') $STOP_VM: SIGCONT felucca-agent =="
  part I13c
fi
part END

echo
echo "RESULT: ${P} passed, ${F} failed, ${S} skipped"
[ "$F" -eq 0 ]
