#!/usr/bin/env bash
# build-template.sh — build a Hearth template image via the public API.
#
# Boots a builder sandbox, streams a provision script into the guest, runs it
# (nohup + poll, so builds aren't bound by the 5-minute exec cap), stops the
# sandbox, and captures its rootfs as a new template
# (POST /api/v1/templates {from_sandbox}). The builder is deleted on success
# and kept for debugging on provision failure.
#
# Usage:
#   build-template.sh --name docker-base --provision deploy/templates/docker-base.sh \
#     [--api http://127.0.0.1:8080] [--token-env HEARTH_TOKEN] \
#     [--base TEMPLATE] [--vcpus N] [--mem-mib M] [--disk-gb G] [--pool-size P] \
#     [--build-timeout SECONDS]
#
# The admin token is read from the environment (default HEARTH_TOKEN) — never
# from argv, so it can't leak via process listings.
# Requires: curl, jq, base64.
set -euo pipefail

API=http://127.0.0.1:8080
TOKEN_ENV=HEARTH_TOKEN
NAME="" PROVISION="" BASE=""
VCPUS=1 MEM_MIB=1024 DISK_GB=4 POOL_SIZE=0
BUILD_TIMEOUT=1800

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api)           API="$2"; shift 2 ;;
    --token-env)     TOKEN_ENV="$2"; shift 2 ;;
    --name)          NAME="$2"; shift 2 ;;
    --provision)     PROVISION="$2"; shift 2 ;;
    --base)          BASE="$2"; shift 2 ;;
    --vcpus)         VCPUS="$2"; shift 2 ;;
    --mem-mib)       MEM_MIB="$2"; shift 2 ;;
    --disk-gb)       DISK_GB="$2"; shift 2 ;;
    --pool-size)     POOL_SIZE="$2"; shift 2 ;;
    --build-timeout) BUILD_TIMEOUT="$2"; shift 2 ;;
    -h|--help)       grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "ERROR: unknown argument: $1 (try --help)" >&2; exit 2 ;;
  esac
done

TOKEN="${!TOKEN_ENV:-}"
[ -n "$NAME" ]      || { echo "ERROR: --name required" >&2; exit 2; }
[ -n "$PROVISION" ] || { echo "ERROR: --provision required" >&2; exit 2; }
[ -f "$PROVISION" ] || { echo "ERROR: provision script not found: $PROVISION" >&2; exit 2; }
[ -n "$TOKEN" ]     || { echo "ERROR: \$$TOKEN_ENV is empty (admin token)" >&2; exit 2; }
for cmd in curl jq base64; do
  command -v "$cmd" >/dev/null || { echo "ERROR: required command not found: $cmd" >&2; exit 2; }
done

say() { printf '%s\n' "$*"; }

hd() { # hd <method> <path> [json-body] [max-time] — authed API call
  # The Authorization header rides a curl config on stdin (--config -), never
  # argv: /proc/<pid>/cmdline is world-readable and builds run for minutes.
  # max-time defaults to 60s; the capture call passes its own (it streams the
  # whole rootfs synchronously).
  local method="$1" path="$2" body="${3:-}" mt="${4:-60}"
  if [ -n "$body" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | \
      curl -sS -m "$mt" --config - -X "$method" \
        -H 'Content-Type: application/json' -d "$body" "$API$path"
  else
    printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | \
      curl -sS -m "$mt" --config - -X "$method" "$API$path"
  fi
}

guest_exec() { # guest_exec <id> <command-string> [timeout-ms] — exec via /bin/sh -c
  local id="$1" cmd="$2" tmo="${3:-30000}"
  hd POST "/api/v1/sandboxes/$id/exec" \
    "$(jq -cn --arg c "$cmd" --argjson t "$tmo" '{cmd:["/bin/sh","-c",$c],timeout_ms:$t}')"
}

guest_exec_ok() { # like guest_exec but FAILS unless the command exited 0.
  # ("ok":true only means the exec transport worked — exit_code carries the
  # command's result; checking ok alone let an empty upload slip through.)
  local out
  out=$(guest_exec "$@")
  printf '%s' "$out" | jq -e '.ok == true and .exit_code == 0' >/dev/null || {
    echo "ERROR: guest command failed: $2 -> $out" >&2
    return 1
  }
  printf '%s' "$out"
}

# ---- 1) builder sandbox ----------------------------------------------------

CREATE_BODY=$(jq -cn --arg n "build-$NAME" --arg tpl "$BASE" \
  --argjson v "$VCPUS" --argjson m "$MEM_MIB" --argjson d "$DISK_GB" \
  '{name:$n, vcpus:$v, mem_mib:$m, disk_gb:$d} + (if $tpl != "" then {template:$tpl} else {} end)')
say "==> Creating builder sandbox (vcpus=$VCPUS mem=${MEM_MIB}MiB disk=${DISK_GB}GB base=${BASE:-ubuntu-base})"
RESP=$(hd POST /api/v1/sandboxes "$CREATE_BODY")
SB=$(printf '%s' "$RESP" | jq -r '.id // empty')
[ -n "$SB" ] || { echo "ERROR: create failed: $RESP" >&2; exit 1; }
say "    builder: $SB"

# ---- 2) wait for the guest agent -------------------------------------------

say "==> Waiting for guest agent"
ready=0
for _ in $(seq 1 60); do
  if guest_exec "$SB" true 2000 2>/dev/null | grep -q '"ok":true'; then ready=1; break; fi
  sleep 2
done
[ "$ready" = 1 ] || { echo "ERROR: guest agent never came up in $SB" >&2; exit 1; }

# ---- 3) upload provision script (base64 chunks survive JSON + shell) -------

say "==> Uploading provision script ($(wc -c < "$PROVISION") bytes)"
guest_exec_ok "$SB" 'rm -f /tmp/p.b64 /tmp/provision.sh /tmp/provision.rc /tmp/provision.log; true' >/dev/null
# Process substitution (not a pipe): the loop must run in this shell so a
# failed chunk aborts the whole build. `|| [ -n "$chunk" ]` keeps the FINAL
# chunk: fold's last line has no trailing newline, and a bare `read` returns
# false on it — silently uploading nothing for small scripts.
while IFS= read -r chunk || [ -n "$chunk" ]; do
  [ -n "$chunk" ] || continue
  guest_exec_ok "$SB" "printf '%s' '$chunk' >> /tmp/p.b64" >/dev/null || exit 1
done < <(base64 < "$PROVISION" | tr -d '\n' | fold -w 3000)
# Decode and verify the EXACT byte count survived the trip.
WANT_BYTES=$(wc -c < "$PROVISION" | tr -d ' ')
R=$(guest_exec_ok "$SB" 'base64 -d /tmp/p.b64 > /tmp/provision.sh && wc -c < /tmp/provision.sh') || exit 1
GOT_BYTES=$(printf '%s' "$R" | jq -r .stdout | tr -d '[:space:]')
[ "$GOT_BYTES" = "$WANT_BYTES" ] || { echo "ERROR: provision size mismatch in guest: got $GOT_BYTES want $WANT_BYTES" >&2; exit 1; }

# ---- 4) run it (nohup + poll — not bound by the per-exec timeout cap) ------

say "==> Running provision script (timeout ${BUILD_TIMEOUT}s)"
# `sh /tmp/provision.sh`, never direct exec: guest /tmp is mounted noexec.
guest_exec "$SB" 'nohup sh -c "sh /tmp/provision.sh > /tmp/provision.log 2>&1; echo \$? > /tmp/provision.rc" >/dev/null 2>&1 &' >/dev/null

rc=""
waited=0
while [ "$waited" -lt "$BUILD_TIMEOUT" ]; do
  sleep 5; waited=$((waited+5))
  OUT=$(guest_exec "$SB" 'cat /tmp/provision.rc 2>/dev/null || echo RUNNING' 5000 || true)
  body=$(printf '%s' "$OUT" | jq -r '.stdout // empty' 2>/dev/null || true)
  case "$body" in
    RUNNING*|"") : ;;
    *) rc=$(printf '%s' "$body" | tr -d '[:space:]'); break ;;
  esac
done

if [ "$rc" != "0" ]; then
  echo "ERROR: provision rc=${rc:-timeout}; last log lines:" >&2
  guest_exec "$SB" 'tail -50 /tmp/provision.log 2>/dev/null' 10000 | jq -r '.stdout // empty' >&2 || true
  echo "Builder kept for debugging: $SB" >&2
  exit 1
fi
say "    provision OK; log tail:"
guest_exec "$SB" 'tail -5 /tmp/provision.log' | jq -r '.stdout // empty' | sed 's/^/    /'

# ---- 5) clean transient files, stop, capture --------------------------------

guest_exec "$SB" 'rm -f /tmp/p.b64 /tmp/provision.sh /tmp/provision.rc /tmp/provision.log; sync' >/dev/null
say "==> Stopping builder"
hd POST "/api/v1/sandboxes/$SB/stop" >/dev/null
for _ in $(seq 1 30); do
  STATE=$(hd GET "/api/v1/sandboxes/$SB" | jq -r .state)
  [ "$STATE" = "stopped" ] && break
  sleep 1
done
[ "$STATE" = "stopped" ] || { echo "ERROR: builder never stopped (state=$STATE)" >&2; exit 1; }

say "==> Capturing rootfs as template '$NAME'"
TPL_BODY=$(jq -cn --arg n "$NAME" --arg sb "$SB" \
  --argjson v "$VCPUS" --argjson m "$MEM_MIB" --argjson d "$DISK_GB" --argjson p "$POOL_SIZE" \
  '{name:$n, from_sandbox:$sb, vcpus:$v, mem_mib:$m, disk_gb:$d, pool_size:$p}')
# Capture streams the whole rootfs synchronously — budget generously.
TPL=$(hd POST /api/v1/templates "$TPL_BODY" 1800)
printf '%s' "$TPL" | jq -e '.id' >/dev/null || { echo "ERROR: capture failed: $TPL" >&2; exit 1; }

# ---- 6) delete the builder ---------------------------------------------------

say "==> Deleting builder"
hd DELETE "/api/v1/sandboxes/$SB" >/dev/null || true

say "==> Done"
printf '%s\n' "$TPL" | jq .
