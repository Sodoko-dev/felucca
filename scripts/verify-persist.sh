#!/usr/bin/env bash
# Lab gate for FLU-5 (with FLU-74, FLU-82, FLU-83): a change is acknowledged
# only when it is saved. Run from the macOS host:
#
#   bash scripts/verify-persist.sh [control-plane-vm] [cases]   # default VM: infra-saas-lab
#   PERSIST_FULL=1 bash scripts/verify-persist.sh                # also case P8 (remounts the state dir)
#
# The lab token is read from $FELUCCA_TOKEN_FILE (default
# ~/.config/felucca/lab-token) and never put on a command line, here or in the
# VM (command lines show in process listings): it goes over standard input into
# a header file `Authorization: Bearer ...` in a 0700 directory of the VM
# (mode 0600), which every curl of the gate reads with `-H @file`, and which is
# removed when this wrapper ends.
#
# <cases> is "all" (default) or a list such as "P1 P2 P3". The cases are in
# test/lab/persist.sh, which runs inside the control-plane VM (systemd, the
# database and the service's user are there); this wrapper backs the database
# up first, ships the scripts and runs them DETACHED (a dropped terminal must
# not leave the write lock held or the database file read-only), following
# the output file. Every sandbox it creates is named ps-*.
#
# The backup is a consistent copy taken with SQLite's backup API as the
# service's user, plus a plain copy of the state directory, in
# /var/tmp/felucca-lab-persist-backup-<time> inside the VM; the path is
# printed. Restore (feluccad stopped): copy felucca.db back over the state
# directory's, remove felucca.db-wal and felucca.db-shm, chown to the
# service's user, start.
#
# It is rough with the control plane on purpose (it holds the database's write
# lock, restarts and stops feluccad, makes the database file read-only, sends a
# heartbeat with an out-of-range value). Takes about 25 minutes.
set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
VM="${1:-${FELUCCA_LAB_CP:-infra-saas-lab}}"
CASES="${2:-all}"
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
[ -s "$TOKEN_FILE" ] || { echo "ERROR: no lab token: write it to $TOKEN_FILE (or point FELUCCA_TOKEN_FILE at it)" >&2; exit 2; }
REMOTE=/tmp/felucca-lab
AUTHD=/tmp/felucca-lab-auth; AUTH="$AUTHD/auth.hdr"
OUT=/var/tmp/felucca-lab-persist.out
vsh() { limactl shell --workdir /tmp "$VM" -- "$@"; }

vsh mkdir -p "$REMOTE" || { echo "ERROR: cannot reach lab VM $VM" >&2; exit 2; }
# Removed by the detached gate once it has ended; by this trap only when the gate was never started.
LAUNCHED=no
trap '[ "$LAUNCHED" = yes ] || vsh rm -f "$AUTH" >/dev/null 2>&1' EXIT
vsh bash -c "umask 077; mkdir -p $AUTHD && chmod 700 $AUTHD && { printf 'Authorization: Bearer '; tr -d ' \t\r\n'; printf '\n'; } > $AUTH && chmod 600 $AUTH" < "$TOKEN_FILE" \
  || { echo "ERROR: could not put the auth header file into $VM" >&2; exit 2; }
for f in lib.sh persist.sh dblock.py; do limactl cp "$REPO/test/lab/$f" "$VM:$REMOTE/$f" >/dev/null || exit 2; done
vsh chmod 755 "$REMOTE" && vsh chmod 644 "$REMOTE/lib.sh" "$REMOTE/persist.sh" "$REMOTE/dblock.py"

BK="/var/tmp/felucca-lab-persist-backup-$(date +%Y%m%d-%H%M%S)"
vsh sudo bash -c "set -e; u=\$(systemctl show feluccad -p User --value); db=\$(jq -r '.db_path // \"/var/lib/felucca/felucca.db\"' /etc/felucca/feluccad.json)
  mkdir -p $BK; chown \"\$u:\" $BK; chmod 700 $BK
  sudo -u \"\$u\" python3 -c 'import sqlite3,sys; s=sqlite3.connect(\"file:%s?mode=ro\" % sys.argv[1], uri=True); d=sqlite3.connect(sys.argv[2]); s.backup(d); d.close(); s.close()' \"\$db\" $BK/felucca.db
  cp -a \"\$(dirname \"\$db\")\" $BK/state-dir" || { echo "ERROR: could not back the database up" >&2; exit 2; }
echo "== database backed up in $VM:$BK (felucca.db: SQLite backup API; state-dir: plain copy) =="

ENVS="FELUCCA_AUTH_FILE=$AUTH"
[ -n "${PERSIST_FULL:-}" ] && ENVS="$ENVS PERSIST_FULL=$(printf '%q' "$PERSIST_FULL")"
echo "== persist gate on $VM (log in the VM: $OUT) =="
LAUNCHED=yes
vsh bash -c ": > $OUT; ( $ENVS setsid nohup bash $REMOTE/persist.sh $(printf '%q' "$CASES") >> $OUT 2>&1 < /dev/null; rc=\$?; rm -f $AUTH; echo \"GATE-EXIT \$rc\" >> $OUT ) >/dev/null 2>&1 &" || exit 2
seen=0
while :; do
  total="$(vsh bash -c "wc -l < $OUT" 2>/dev/null | tr -d ' \r\n')"; total="${total:-0}"
  if [ "$total" -gt "$seen" ] 2>/dev/null; then
    vsh bash -c "sed -n '$((seen+1)),${total}p' $OUT" 2>/dev/null | grep -v '^GATE-EXIT '; seen="$total"
  fi
  rc="$(vsh bash -c "sed -n 's/^GATE-EXIT //p' $OUT | tail -1" 2>/dev/null | tr -d ' \r\n')"
  [ -n "$rc" ] && break
  sleep 5
done
echo "(database backup: $VM:$BK)"
[ "$rc" = 0 ]
