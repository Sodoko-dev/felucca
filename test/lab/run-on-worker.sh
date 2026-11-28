#!/usr/bin/env bash
# Run one worker-side lab gate (test/lab/<gate>.sh) inside a lab worker VM,
# from the macOS host.
#
#   bash test/lab/run-on-worker.sh <gate> [worker-vm] [token] [gate args...]
#
# <gate> is the name of a script in this directory without .sh: restart, oom,
# firewall, break, reboot-pre, reboot-post, repro-flu15, repro-flu70.
#
# The worker VMs do not mount the repository, so the gate and its helper are
# copied into the VM first. The gate is started DETACHED in the VM with its
# output in a file there, and this script only follows that file: these gates
# restart services and take minutes, and a dropped terminal must not leave a
# worker half-way through one (the gates undo what they change when they
# exit, and they only get to exit if they keep running).
#
# Exit status: 0 when the gate ends with 0 failures, 1 otherwise, 2 on a setup
# problem. Environment passed through to the gate when set: INSTALLER_BUNDLE,
# PREV_AGENT_BIN.
set -u

GATE="${1:-}"
case "$GATE" in restart|oom|firewall|break|reboot-pre|reboot-post|repro-flu15|repro-flu70) ;; *) echo "usage: $0 <restart|oom|firewall|break|reboot-pre|reboot-post|repro-flu15|repro-flu70> [worker-vm] [token] [gate args...]" >&2; exit 2;; esac
WORKER="${2:-${FELUCCA_LAB_WORKER:-kata-lab-0}}"

# The lab token authenticates a root-privileged agent API, so it is never a
# literal in the repo: argument, then FELUCCA_TOKEN, then a file kept outside
# the tree (same order as scripts/verify-v2.sh).
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
TOKEN="${3:-${FELUCCA_TOKEN:-}}"
if [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ]; then TOKEN="$(tr -d " \t\r\n" < "$TOKEN_FILE")"; fi
if [ -z "$TOKEN" ]; then
  echo "ERROR: no lab token. Pass it as \$3, export FELUCCA_TOKEN, or write it to $TOKEN_FILE" >&2
  exit 2
fi
[ "$#" -ge 3 ] && shift 3 || shift "$#"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE=/tmp/felucca-lab
OUT="/var/tmp/felucca-lab-${GATE}.out"

limactl shell "$WORKER" -- mkdir -p "$REMOTE" || { echo "ERROR: cannot reach lab VM $WORKER" >&2; exit 2; }
limactl cp "$HERE/lib.sh" "$WORKER:$REMOTE/lib.sh" >/dev/null || exit 2
limactl cp "$HERE/$GATE.sh" "$WORKER:$REMOTE/$GATE.sh" >/dev/null || exit 2

ARGS=""; for a in "$@"; do ARGS="$ARGS $(printf '%q' "$a")"; done
# The token goes over standard input into a header file (0700 dir, 0600 file)
# that lib.sh hands to curl with -H @file; it is on no command line, here or in
# the VM (the detached shell's command line shows in process listings for the
# whole run). The detached shell removes the file when the gate ends.
AUTHD=/tmp/felucca-lab-auth; AUTH="$AUTHD/auth.hdr"
printf '%s' "$TOKEN" | limactl shell "$WORKER" -- bash -c "umask 077; mkdir -p $AUTHD && chmod 700 $AUTHD && { printf 'Authorization: Bearer '; tr -d ' \t\r\n'; printf '\n'; } > $AUTH && chmod 600 $AUTH" \
  || { echo "ERROR: could not put the auth header file into $WORKER" >&2; exit 2; }
TOKEN=""
ENVS="FELUCCA_AUTH_FILE=$AUTH"
[ -n "${INSTALLER_BUNDLE:-}" ] && ENVS="$ENVS INSTALLER_BUNDLE=$(printf '%q' "$INSTALLER_BUNDLE")"
[ -n "${PREV_AGENT_BIN:-}" ] && ENVS="$ENVS PREV_AGENT_BIN=$(printf '%q' "$PREV_AGENT_BIN")"

echo "== $GATE gate on $WORKER (log in the VM: $OUT) =="
limactl shell "$WORKER" -- bash -c ": > $OUT; ( $ENVS setsid nohup bash $REMOTE/$GATE.sh$ARGS >> $OUT 2>&1 < /dev/null; rc=\$?; rm -f $AUTH; echo \"GATE-EXIT \$rc\" >> $OUT ) >/dev/null 2>&1 &" || exit 2

seen=0
while :; do
  total="$(limactl shell "$WORKER" -- bash -c "wc -l < $OUT" 2>/dev/null | tr -d ' \r\n')"
  total="${total:-0}"
  if [ "$total" -gt "$seen" ] 2>/dev/null; then
    limactl shell "$WORKER" -- bash -c "sed -n '$((seen+1)),${total}p' $OUT" 2>/dev/null | grep -v '^GATE-EXIT '
    seen="$total"
  fi
  rc="$(limactl shell "$WORKER" -- bash -c "sed -n 's/^GATE-EXIT //p' $OUT | tail -1" 2>/dev/null | tr -d ' \r\n')"
  [ -n "$rc" ] && break
  sleep 5
done
[ "$rc" = 0 ]
