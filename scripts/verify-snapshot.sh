#!/usr/bin/env bash
# Lab gate for FLU-4, FLU-70 and FLU-72: snapshots, sleep and wake, fork.
# Run from the macOS host:
#
#   bash scripts/verify-snapshot.sh [worker-vm] [token] [cases]   # default worker: kata-lab-1
#   PREV_AGENT_BIN=/var/lib/felucca-lab/felucca-agent.prev bash scripts/verify-snapshot.sh
#
# <cases> is "all" (default) or a list such as "S1 S5 S10"; S10 is the power
# cut. The cases are in test/lab/snapshot.sh (S1-S9, S11-S13, on the worker)
# and test/lab/snapshot-pre.sh / snapshot-post.sh (S10). For S10 this script
# puts sandbox D to sleep through the worker's own shell and cuts the VM's
# power (`limactl stop -f`) the moment the sleep is acknowledged, starts the
# VM again in the background (`limactl start` can wait minutes for an event
# although the VM is up) and waits for the agent. S11 (upgrade from the
# previous agent binary and rollback) needs PREV_AGENT_BIN, a path ON the
# worker; without it S11 is skipped.
#
# Prints one final `RESULT: n passed, m failed, k skipped` over all parts and
# exits non-zero on any failure. Takes about 35 minutes. Rough with the worker
# on purpose (it stops the agent, fills the data disk, swaps the agent binary,
# cuts the VM's power): do not point it at a worker that holds sandboxes you
# care about.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORKER="${1:-${FELUCCA_LAB_WORKER:-kata-lab-1}}"
CASES="${3:-all}"
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
TOKEN="${2:-${FELUCCA_TOKEN:-}}"
if [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ]; then TOKEN="$(tr -d " \t\r\n" < "$TOKEN_FILE")"; fi
[ -n "$TOKEN" ] || { echo "ERROR: no lab token. Pass it as \$2, export FELUCCA_TOKEN, or write it to $TOKEN_FILE" >&2; exit 2; }
REMOTE=/tmp/felucca-lab
P=0; F=0; S=0
LOCAL_OUT="$(mktemp -t felucca-snapshot.XXXXXX)"; trap 'rm -f "$LOCAL_OUT"' EXIT
hostnow() { perl -MTime::HiRes=time -e 'printf "%.3f\n", time'; }
vsh() { limactl shell --workdir /tmp "$WORKER" -- "$@"; }
ship() { vsh mkdir -p "$REMOTE" && for f in lib.sh snapshot.sh snapshot-pre.sh snapshot-post.sh; do limactl cp "$REPO/test/lab/$f" "$WORKER:$REMOTE/$f" >/dev/null || return 1; done; }
ENVS="FELUCCA_TOKEN=$(printf '%q' "$TOKEN")"
[ -n "${PREV_AGENT_BIN:-}" ] && ENVS="$ENVS PREV_AGENT_BIN=$(printf '%q' "$PREV_AGENT_BIN")"
tally() { # add the RESULT line of the last part
  local l; l="$(grep -E '^RESULT: [0-9]+ passed, [0-9]+ failed, [0-9]+ skipped' "$LOCAL_OUT" | tail -1)"
  if [ -z "$l" ]; then F=$((F+1)); echo "  FAIL: $1 printed no RESULT line"; return; fi
  set -- $l; P=$((P+$2)); F=$((F+$4)); S=$((S+$6))
}
# <script> [args] — run one part DETACHED in the VM (a dropped terminal must not
# leave the worker half-way through it) and follow its output file.
part() {
  local name="$1"; shift
  local out="/var/tmp/felucca-lab-$name.out" args="" a seen=0 total rc
  for a in "$@"; do args="$args $(printf '%q' "$a")"; done
  ship || { echo "ERROR: cannot reach lab VM $WORKER" >&2; exit 2; }
  : > "$LOCAL_OUT"
  echo "== $name on $WORKER (log in the VM: $out) =="
  vsh bash -c ": > $out; ( $ENVS setsid nohup bash $REMOTE/$name.sh$args >> $out 2>&1 < /dev/null; echo \"GATE-EXIT \$?\" >> $out ) >/dev/null 2>&1 &" || exit 2
  while :; do
    total="$(vsh bash -c "wc -l < $out" 2>/dev/null | tr -d ' \r\n')"; total="${total:-0}"
    if [ "$total" -gt "$seen" ] 2>/dev/null; then
      vsh bash -c "sed -n '$((seen+1)),${total}p' $out" 2>/dev/null | grep -v '^GATE-EXIT ' | tee -a "$LOCAL_OUT"; seen="$total"
    fi
    rc="$(vsh bash -c "sed -n 's/^GATE-EXIT //p' $out | tail -1" 2>/dev/null | tr -d ' \r\n')"
    [ -n "$rc" ] && break
    sleep 5
  done
  tally "$name"
}

# S1-S9, S11-S13
WCASES="$CASES"
[ "$CASES" != all ] && WCASES="$(printf '%s\n' $CASES | grep -v '^S10$' | tr '\n' ' ' | sed 's/ $//')"
[ -n "$WCASES" ] && part snapshot "$WCASES"

if [ "$CASES" = all ] || case " $CASES " in *" S10 "*) true;; *) false;; esac; then
  part snapshot-pre
  echo "== S10. power cut right after D's sleep is acknowledged =="
  T_CALL="$(hostnow)"
  R="$(vsh bash -c "$ENVS bash $REMOTE/snapshot-pre.sh sleep-d" 2>/dev/null | grep '^D-SLEEP' | tr -d '\r')"
  T_BACK="$(hostnow)"
  limactl stop -f "$WORKER" >/dev/null 2>&1 &
  T_STOP="$(hostnow)"; wait
  set -- $R
  echo "  D's sleep answered '${2:-none}' at $3 (worker clock); host: call sent $T_CALL, answer back $T_BACK, limactl stop -f started $T_STOP"
  : > "$LOCAL_OUT"
  if [ "${2:-}" = 200 ]; then P=$((P+1)); echo "  PASS: D's sleep acknowledged (200)"; else F=$((F+1)); echo "  FAIL: D's sleep — got '${2:-none}', want 200"; fi
  GAP="$(awk -v a="${3:-0}" -v b="$T_STOP" 'BEGIN{printf "%.2f", b-a}')"
  if awk -v g="$GAP" 'BEGIN{exit !(g<2 && g>-1)}'; then P=$((P+1)); echo "  PASS: the power was cut ${GAP} s after the acknowledgement (< 2 s; worker clock against host clock)"
  else F=$((F+1)); echo "  FAIL: the power was cut ${GAP} s after the acknowledgement, want < 2 s"; fi
  T0="$(date +%s)"; ( limactl start "$WORKER" >/dev/null 2>&1 & ); i=0
  until [ "$(vsh sudo systemctl is-active felucca-agent 2>/dev/null | tr -d '\r\n')" = active ] || [ $i -ge 72 ]; do sleep 5; i=$((i+1)); done
  if [ $i -ge 72 ]; then F=$((F+1)); echo "  FAIL: felucca-agent on $WORKER is not active $(( $(date +%s) - T0 )) s after the VM was started"
  else echo "  felucca-agent on $WORKER is active again $(( $(date +%s) - T0 )) s after the VM was started"; fi
  part snapshot-post
fi

echo
echo "RESULT: ${P} passed, ${F} failed, ${S} skipped"
[ "$F" -eq 0 ]
