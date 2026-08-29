#!/usr/bin/env bash
# scripts/bench.sh — Hearth v4 P6.3 performance benchmark
#
# Usage:
#   bash scripts/bench.sh <hearthd-endpoint> [admin-token]
#   e.g. bash scripts/bench.sh http://127.0.0.1:8080 "$(cat ~/.config/hearth/lab-token)"
#
# The admin token is REQUIRED and must be the one hearthd was started with:
# every operation below is a /api/v1 call, and hearthd refuses to start without
# a real token, so there is no unauthenticated endpoint to benchmark. Pass it
# as $2, export HEARTH_TOKEN, or keep it in $HEARTH_TOKEN_FILE (default
# ~/.config/hearth/lab-token) — the same contract as the other lab scripts.
#
# Env knobs:
#   N                 — iterations per operation (default: 10)
#   BENCH_SHAPE_VCPUS — vCPUs for the pool / "claim" shape (default: 1)
#   BENCH_SHAPE_MEM   — mem_mib for the pool / "claim" shape (default: 256)
#
# Dependencies: bash >=4, curl, jq.  No bc; awk handles all arithmetic.
set -u

# ── Args ─────────────────────────────────────────────────────────────────────
ENDPOINT="${1:-}"
TOKEN="${2:-${HEARTH_TOKEN:-}}"
TOKEN_FILE="${HEARTH_TOKEN_FILE:-${HOME:-}/.config/hearth/lab-token}"
if [ -z "$ENDPOINT" ]; then
  printf 'usage: %s <hearthd-endpoint> [admin-token]\n' "$0" >&2
  printf '  e.g. %s http://127.0.0.1:8080 "$(cat ~/.config/hearth/lab-token)"\n' "$0" >&2
  printf '  the token may also come from HEARTH_TOKEN or HEARTH_TOKEN_FILE\n' >&2
  exit 2
fi
if [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ]; then
  TOKEN="$(tr -d ' \t\r\n' < "$TOKEN_FILE")"
fi
# Fail here rather than at the preflight: an empty token authorizes nobody now
# (it is not "auth off"), so a tokenless run would just be a 401 wearing a
# benchmark's clothes. HEARTH_INSECURE_NO_AUTH=1 is the explicit opt-out, for a
# hearthd started with --insecure-no-auth.
if [ -z "$TOKEN" ] && [ "${HEARTH_INSECURE_NO_AUTH:-0}" != "1" ]; then
  printf 'error: no admin token.\n' >&2
  printf '  Pass it as the second argument, export HEARTH_TOKEN, or write it to %s\n' "$TOKEN_FILE" >&2
  printf '  (generate one with: openssl rand -hex 32).\n' >&2
  printf '  It must be the token hearthd was STARTED with — hearthd refuses to start\n' >&2
  printf '  without a real one. For a loopback lab on --insecure-no-auth, re-run with\n' >&2
  printf '  HEARTH_INSECURE_NO_AUTH=1.\n' >&2
  exit 2
fi
case "$(printf '%s' "$TOKEN" | tr '[:upper:]' '[:lower:]')" in
  *replace_with*|hearth-lab-token)
    printf 'error: that token is a shipped placeholder — it is published in this repo, and\n' >&2
    printf '       hearthd refuses to start with it, so nothing is listening on it.\n' >&2
    printf '  Generate a real one with: openssl rand -hex 32\n' >&2
    exit 2 ;;
esac
ENDPOINT="${ENDPOINT%/}"    # strip trailing slash

N="${N:-10}"
BENCH_SHAPE_VCPUS="${BENCH_SHAPE_VCPUS:-1}"
BENCH_SHAPE_MEM="${BENCH_SHAPE_MEM:-256}"

# Cold-boot shape — deliberately mismatches the agent's default warm-pool spec
# (1 vCPU / 256 MiB) so that a pool claim is impossible and the VM must cold-boot.
# ASSUMPTION: the pool is configured for the default shape (1/256).  If your
# deployment uses a different pool spec, set BENCH_SHAPE_MEM to match the pool
# and change COLD_MEM to any value that does NOT match so cold-boot is guaranteed.
COLD_VCPUS=1
COLD_MEM=320

RUN="$$"    # PID-scoped; every sandbox name is bench-${RUN}-<op>-<seq>
GIT_REV=$(git -C "$(dirname "$0")/.." rev-parse --short HEAD 2>/dev/null || true)

# ── Nanosecond clock ──────────────────────────────────────────────────────────
# date +%s%N is GNU / Linux; macOS needs gdate (brew install coreutils) or perl.
_probe_ns=$(date +%s%N 2>/dev/null || true)
if printf '%s' "$_probe_ns" | grep -qE '^[0-9]{15,}$'; then
  _date_ns() { date +%s%N; }
elif command -v gdate >/dev/null 2>&1; then
  _date_ns() { gdate +%s%N; }
elif command -v perl >/dev/null 2>&1; then
  _date_ns() { perl -MTime::HiRes=time -e 'printf "%d\n", time()*1_000_000_000'; }
else
  printf 'error: nanosecond clock unavailable (need GNU date, gdate, or perl)\n' >&2
  exit 1
fi

# ── Sandbox tracking & cleanup ────────────────────────────────────────────────
# All sandboxes created by this run; trap deletes any still alive on exit.
CREATED_IDS=()

register_id() { CREATED_IDS+=("$1"); }

_del_one() {   # _del_one <id>  — best-effort delete, never aborts
  local id="$1"
  local args=(-s -m 15 -X DELETE)
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  curl "${args[@]}" "${ENDPOINT}/api/v1/sandboxes/${id}" >/dev/null 2>&1 || true
}

_remove_tracked() {   # _remove_tracked <id>  — drop id from CREATED_IDS
  local id="$1" new=()
  local x
  for x in "${CREATED_IDS[@]+"${CREATED_IDS[@]}"}"; do
    [ "$x" = "$id" ] || new+=("$x")
  done
  CREATED_IDS=("${new[@]+"${new[@]}"}")
}

_sweep() {    # EXIT trap — delete every surviving bench sandbox
  local id
  for id in "${CREATED_IDS[@]+"${CREATED_IDS[@]}"}"; do
    _del_one "$id"
  done
}
trap _sweep EXIT

# ── HTTP helper ───────────────────────────────────────────────────────────────
HD_STATUS="" HD_BODY=""

hd_req() {   # hd_req <method> <path> [body]  — sets HD_STATUS, HD_BODY
  local method="$1" path="$2" body="${3:-}"
  local tmp args
  tmp=$(mktemp)
  args=(-s -m 60 -X "$method" -o "$tmp" -w '%{http_code}')
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  HD_STATUS=$(curl "${args[@]}" "${ENDPOINT}${path}")
  HD_BODY=$(cat "$tmp")
  rm -f "$tmp"
}

# ── Guest-ready polling ───────────────────────────────────────────────────────
# Poll exec(["true"]) until {"ok":true} — never sleep a mid-boot guest.
# Two contract constraints at once: hearth-guest must be listening before exec /
# sleep / fork are exercised (a snapshot of a mid-boot guest is poisoned).
wait_guest_ready() {   # wait_guest_ready <id> [timeout_s=90]
  local id="$1" timeout="${2:-90}"
  local i=0 args
  args=(-s -m 5 -X POST -H "Content-Type: application/json"
        -d '{"cmd":["true"],"timeout_ms":2000}')
  [ -n "$TOKEN" ] && args+=(-H "Authorization: Bearer $TOKEN")
  while [ "$i" -lt "$timeout" ]; do
    if curl "${args[@]}" "${ENDPOINT}/api/v1/sandboxes/${id}/exec" 2>/dev/null \
        | jq -e '.ok == true' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1; i=$((i+1))
  done
  return 1
}

# ── SSE first-frame helper ───────────────────────────────────────────────────
# Read stdin until the first "data:" SSE line, then print a nanosecond timestamp.
# Defined as a named function so it can be the last stage of a pipeline inside
# $() without triggering bash's case-inside-command-substitution parse bug.
_first_sse_ts() {
  local _ln
  while IFS= read -r _ln; do
    case "$_ln" in
      data:*) _date_ns; return ;;
    esac
  done
}

# ── Statistics ────────────────────────────────────────────────────────────────
# Percentile: idx = int(p * n / 100 + 0.999999), clamped to [1, n].
# Inputs are sorted via sort -n; all arithmetic in awk (no bc).
TABLE_ROWS=""

pstats() {   # pstats <op_label> <space_sep_ms_samples> <fail_count>
  local op="$1" raw="$2" fails="${3:-0}"
  if [ -z "${raw// /}" ]; then
    printf '  %-30s  no samples  (fails=%s)\n' "$op" "$fails"
    TABLE_ROWS="${TABLE_ROWS}| \`${op}\` | — | — | — | — | 0 | ${fails} |\n"
    return
  fi
  local result
  result=$(printf '%s\n' $raw | grep -E '^[0-9]+$' | sort -n | awk -v fails="$fails" '
    # awk functions must be defined at top level, never inside a rule block
    # (gawk rejects a nested definition with a syntax error).
    function pct(p,   idx) {
      idx = int(p * n / 100 + 0.999999)
      if (idx < 1) idx = 1
      if (idx > n) idx = n
      return a[idx]
    }
    { a[NR] = $1 }
    END {
      if (NR == 0) { printf "0 0 0 0 0 %s\n", fails; exit }
      n = NR
      printf "%d %d %d %d %d %s\n", pct(50), pct(95), a[1], a[n], n, fails
    }
  ')
  local p50 p95 mn mx nn ff
  read -r p50 p95 mn mx nn ff <<< "$result"
  printf '  %-30s  p50=%6dms  p95=%6dms  min=%6dms  max=%6dms  n=%s  fails=%s\n' \
    "$op" "$p50" "$p95" "$mn" "$mx" "$nn" "$ff"
  TABLE_ROWS="${TABLE_ROWS}| \`${op}\` | ${p50} | ${p95} | ${mn} | ${mx} | ${nn} | ${ff} |\n"
}

# ── Preflight ─────────────────────────────────────────────────────────────────
printf '==> Preflight  %s\n' "$ENDPOINT"

_hs=$(curl -s -m 10 -o /dev/null -w '%{http_code}' "${ENDPOINT}/healthz")
if [ "$_hs" != "200" ]; then
  printf 'error: GET /healthz -> %s  (endpoint unreachable or wrong URL)\n' "$_hs" >&2
  exit 1
fi
printf '    /healthz 200 OK\n'

hd_req GET /api/v1/nodes
if [ "$HD_STATUS" != "200" ]; then
  printf 'error: GET /api/v1/nodes -> %s  (bad token or wrong endpoint)\n' "$HD_STATUS" >&2
  case "$HD_STATUS" in
    401) printf '  401: the token is not the one hearthd was started with.\n' >&2 ;;
    429) printf '  429: hearthd is throttling this source after repeated failed credentials;\n' >&2
         printf '       wait for the Retry-After window and re-run with the right token.\n' >&2 ;;
  esac
  exit 1
fi
printf '    GET /api/v1/nodes 200 OK\n'

# Sum pool_size across all nodes so the reader knows if create-claim hit a warm pool.
TOTAL_POOL=$(printf '%s' "$HD_BODY" | jq '[.nodes[].pool_size // 0] | add // 0' 2>/dev/null || printf '0')
if [ "${TOTAL_POOL:-0}" -gt 0 ] 2>/dev/null; then
  POOL_NOTE="pool_size=${TOTAL_POOL} across nodes — create-claim likely hits warm pool"
else
  POOL_NOTE="pool_size=0 — no warm pool detected; create-claim will cold-boot"
fi
printf '    %s\n\n' "$POOL_NOTE"

# ── Run header ────────────────────────────────────────────────────────────────
printf '### Hearth benchmark  %s  N=%s  git=%s\n\n' \
  "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$N" "${GIT_REV:-unknown}"

# =============================================================================
# Op 1: create-cold
# POST /api/v1/sandboxes with a shape that cannot match the pool (mem_mib=320).
# Measures: POST sent → 201 received.  Guest-ready polling is untimed.
# =============================================================================
printf '==> [1/6] create-cold  (vcpus=%s mem_mib=%s — off-pool shape, forces cold-boot)\n' \
  "$COLD_VCPUS" "$COLD_MEM"
S1="" F1=0
for (( _i=1; _i<=N; _i++ )); do
  _name="bench-${RUN}-cold-${_i}"
  _body="{\"name\":\"${_name}\",\"vcpus\":${COLD_VCPUS},\"mem_mib\":${COLD_MEM}}"
  _t0=$(_date_ns)
  hd_req POST /api/v1/sandboxes "$_body"
  _t1=$(_date_ns)
  if [ "$HD_STATUS" != "201" ]; then
    printf '    [%2d/%d] FAIL  status=%s\n' "$_i" "$N" "$HD_STATUS"
    F1=$((F1+1)); continue
  fi
  _id=$(printf '%s' "$HD_BODY" | jq -r '.id // empty')
  if [ -z "$_id" ]; then
    printf '    [%2d/%d] FAIL  no id in response\n' "$_i" "$N"
    F1=$((F1+1)); continue
  fi
  register_id "$_id"
  _ms=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  S1="${S1}${_ms} "
  printf '    [%2d/%d] %dms  id=%s\n' "$_i" "$N" "$_ms" "$_id"
  # Wait until guest-ready before deleting — deleting mid-boot is safe but wastes the
  # cold-boot work; more importantly this mirrors real-world usage (first exec after create).
  wait_guest_ready "$_id" 90 || true
  _del_one "$_id"
  _remove_tracked "$_id"
done
pstats "create-cold" "$S1" "$F1"
printf '\n'

# =============================================================================
# Op 2: create-claim
# Same POST but with the default pool shape (1 vCPU / 256 MiB).  When the agent
# has a warm pool the claim path is taken, avoiding a cold FC boot.
# Note: whether a pool was actually warm at bench start is printed in the header.
# =============================================================================
printf '==> [2/6] create-claim  (vcpus=%s mem_mib=%s — default pool shape)\n' \
  "$BENCH_SHAPE_VCPUS" "$BENCH_SHAPE_MEM"
printf '    # %s\n' "$POOL_NOTE"
S2="" F2=0
for (( _i=1; _i<=N; _i++ )); do
  _name="bench-${RUN}-claim-${_i}"
  _body="{\"name\":\"${_name}\",\"vcpus\":${BENCH_SHAPE_VCPUS},\"mem_mib\":${BENCH_SHAPE_MEM}}"
  _t0=$(_date_ns)
  hd_req POST /api/v1/sandboxes "$_body"
  _t1=$(_date_ns)
  if [ "$HD_STATUS" != "201" ]; then
    printf '    [%2d/%d] FAIL  status=%s\n' "$_i" "$N" "$HD_STATUS"
    F2=$((F2+1)); continue
  fi
  _id=$(printf '%s' "$HD_BODY" | jq -r '.id // empty')
  if [ -z "$_id" ]; then
    printf '    [%2d/%d] FAIL  no id\n' "$_i" "$N"
    F2=$((F2+1)); continue
  fi
  register_id "$_id"
  _ms=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  S2="${S2}${_ms} "
  printf '    [%2d/%d] %dms  id=%s\n' "$_i" "$N" "$_ms" "$_id"
  wait_guest_ready "$_id" 90 || true
  _del_one "$_id"
  _remove_tracked "$_id"
done
pstats "create-claim" "$S2" "$F2"
printf '\n'

# =============================================================================
# Long-lived sandbox shared by ops 3–6
# =============================================================================
printf '==> Creating long-lived sandbox for exec / stream / wake / fork ops...\n'
_ll_name="bench-${RUN}-ll"
hd_req POST /api/v1/sandboxes \
  "{\"name\":\"${_ll_name}\",\"vcpus\":${BENCH_SHAPE_VCPUS},\"mem_mib\":${BENCH_SHAPE_MEM}}"
if [ "$HD_STATUS" != "201" ]; then
  printf 'error: could not create long-lived sandbox (status=%s)\n%s\n' \
    "$HD_STATUS" "$HD_BODY" >&2
  exit 1
fi
LL_ID=$(printf '%s' "$HD_BODY" | jq -r '.id')
register_id "$LL_ID"
printf '    id=%s — polling exec([true]) until guest-ready...\n' "$LL_ID"
if ! wait_guest_ready "$LL_ID" 120; then
  printf 'error: guest agent never answered on %s after 120s\n' "$LL_ID" >&2
  exit 1
fi
printf '    guest ready\n\n'

# =============================================================================
# Op 3: exec-buffered
# POST /sandboxes/{id}/exec {"cmd":["true"]} on the long-lived sandbox.
# Measures: POST sent → 200 received (full buffered response).
# =============================================================================
printf '==> [3/6] exec-buffered\n'
S3="" F3=0
for (( _i=1; _i<=N; _i++ )); do
  _t0=$(_date_ns)
  hd_req POST "/api/v1/sandboxes/${LL_ID}/exec" '{"cmd":["true"],"timeout_ms":5000}'
  _t1=$(_date_ns)
  if [ "$HD_STATUS" != "200" ]; then
    printf '    [%2d/%d] FAIL  status=%s\n' "$_i" "$N" "$HD_STATUS"
    F3=$((F3+1)); continue
  fi
  if ! printf '%s' "$HD_BODY" | jq -e '.ok == true' >/dev/null 2>&1; then
    printf '    [%2d/%d] FAIL  ok!=true  body=%s\n' "$_i" "$N" "$HD_BODY"
    F3=$((F3+1)); continue
  fi
  _ms=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  S3="${S3}${_ms} "
  printf '    [%2d/%d] %dms\n' "$_i" "$N" "$_ms"
done
pstats "exec-buffered" "$S3" "$F3"
printf '\n'

# =============================================================================
# Op 4: exec-stream-first-frame
# POST /sandboxes/{id}/exec?stream=1 with cmd ["/bin/sh","-c","echo x; sleep 1"].
# Measures: request-start → first SSE "data:" line received.
#
# Technique: curl -N (disable output buffering) piped into a group command that
# reads lines until a "data:" prefix appears, then timestamps.  Command
# substitution captures only that timestamp; breaking the read loop closes the
# read end of the pipe which sends SIGPIPE to curl, terminating it early
# (sleep 1 in the command ensures curl is still running when the first frame
# arrives, so we truly measure first-frame latency, not total exec latency).
# _date_ns is a shell function; bash pipe subshells inherit parent functions.
# =============================================================================
printf '==> [4/6] exec-stream-first-frame\n'
printf '    # request-start -> first SSE data: line; curl -N | { read loop + timestamp }\n'
S4="" F4=0
for (( _i=1; _i<=N; _i++ )); do
  _curl_args=(-N -s -m 30 -X POST -H "Content-Type: application/json"
              -d '{"cmd":["/bin/sh","-c","echo x; sleep 1"],"timeout_ms":10000}')
  [ -n "$TOKEN" ] && _curl_args+=(-H "Authorization: Bearer $TOKEN")

  _t0=$(_date_ns)
  # The pipe subshell reads until the first "data:" line, timestamps, then exits.
  # That exit closes the pipe read-end, causing curl to receive SIGPIPE and exit.
  _t1=$(curl "${_curl_args[@]}" \
        "${ENDPOINT}/api/v1/sandboxes/${LL_ID}/exec?stream=1" \
    | _first_sse_ts)

  if [ -z "$_t1" ]; then
    printf '    [%2d/%d] FAIL  no SSE data: frame received\n' "$_i" "$N"
    F4=$((F4+1)); continue
  fi
  _ms=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  S4="${S4}${_ms} "
  printf '    [%2d/%d] %dms\n' "$_i" "$N" "$_ms"
done
pstats "exec-stream-first-frame" "$S4" "$F4"
printf '\n'

# =============================================================================
# Op 5: wake  (N sleep → wake cycles on the long-lived sandbox)
# Measures wall-clock (POST /wake → 200) AND the agent-reported wake_ms field
# from the response body (snapshot-restore latency measured inside hearthd/agent).
# Sleep itself is untimed — it returns synchronously with state=sleeping.
# =============================================================================
printf '==> [5/6] wake  (%d sleep->wake cycles; wall-clock and api wake_ms both reported)\n' "$N"
S5w="" S5a="" F5=0
for (( _i=1; _i<=N; _i++ )); do
  # Sleep (untimed)
  hd_req POST "/api/v1/sandboxes/${LL_ID}/sleep"
  if [ "$HD_STATUS" != "200" ]; then
    printf '    [%2d/%d] FAIL  sleep status=%s\n' "$_i" "$N" "$HD_STATUS"
    F5=$((F5+1)); continue
  fi
  if ! printf '%s' "$HD_BODY" | jq -e '.state == "sleeping"' >/dev/null 2>&1; then
    printf '    [%2d/%d] FAIL  sleep did not reach sleeping (body=%s)\n' "$_i" "$N" "$HD_BODY"
    F5=$((F5+1)); continue
  fi

  # Wake (timed)
  _t0=$(_date_ns)
  hd_req POST "/api/v1/sandboxes/${LL_ID}/wake"
  _t1=$(_date_ns)
  if [ "$HD_STATUS" != "200" ]; then
    printf '    [%2d/%d] FAIL  wake status=%s\n' "$_i" "$N" "$HD_STATUS"
    F5=$((F5+1)); continue
  fi
  _wall=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  _api=$(printf '%s' "$HD_BODY" | jq -r '.wake_ms // empty' 2>/dev/null || true)
  S5w="${S5w}${_wall} "
  [ -n "$_api" ] && S5a="${S5a}${_api} "
  printf '    [%2d/%d] wall=%dms  api_wake_ms=%s\n' "$_i" "$N" "$_wall" "${_api:-?}"
done
pstats "wake (wall-clock)"  "$S5w" "$F5"
pstats "wake (api wake_ms)" "$S5a" "$F5"
printf '\n'

# Ensure sandbox is running before fork; recover if last wake cycle failed.
hd_req GET "/api/v1/sandboxes/${LL_ID}"
if printf '%s' "$HD_BODY" | jq -e '.state == "sleeping"' >/dev/null 2>&1; then
  printf '    # sandbox still sleeping after wake cycles, waking before fork...\n'
  hd_req POST "/api/v1/sandboxes/${LL_ID}/wake" || true
fi

# =============================================================================
# Op 6: fork
# POST /sandboxes/{id}/fork {"name":"..."} on the running long-lived sandbox.
# Measures: POST sent → 201 received.  Child is deleted inline each iteration.
# Parent is briefly paused by hearthd during the fork; it returns to running.
# =============================================================================
printf '==> [6/6] fork\n'
S6="" F6=0
for (( _i=1; _i<=N; _i++ )); do
  _child_name="bench-${RUN}-fork-${_i}"
  _t0=$(_date_ns)
  hd_req POST "/api/v1/sandboxes/${LL_ID}/fork" "{\"name\":\"${_child_name}\"}"
  _t1=$(_date_ns)
  if [ "$HD_STATUS" != "201" ]; then
    printf '    [%2d/%d] FAIL  status=%s\n' "$_i" "$N" "$HD_STATUS"
    F6=$((F6+1)); continue
  fi
  _child_id=$(printf '%s' "$HD_BODY" | jq -r '.id // empty')
  if [ -z "$_child_id" ]; then
    printf '    [%2d/%d] FAIL  no child id\n' "$_i" "$N"
    F6=$((F6+1)); continue
  fi
  register_id "$_child_id"
  _ms=$(awk -v d="$((_t1-_t0))" 'BEGIN{print int(d/1000000)}')
  S6="${S6}${_ms} "
  printf '    [%2d/%d] %dms  child=%s\n' "$_i" "$N" "$_ms" "$_child_id"
  _del_one "$_child_id"
  _remove_tracked "$_child_id"
done
pstats "fork" "$S6" "$F6"
printf '\n'

# Delete the long-lived sandbox inline (trap covers any crash before we get here).
_del_one "$LL_ID"
_remove_tracked "$LL_ID"

# =============================================================================
# Markdown report (paste into docs)
# =============================================================================
printf '\n---\n\n'
printf '## Hearth Benchmark Results\n\n'
printf '**Endpoint:** `%s`  \n'  "$ENDPOINT"
printf '**Date:** `%s`  \n'      "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
printf '**N:** %s iterations per operation  \n' "$N"
printf '**Git:** `%s`  \n'       "${GIT_REV:-unknown}"
printf '**Shapes:** cold=vcpus%s/mem_mib%s · pool=vcpus%s/mem_mib%s  \n' \
  "$COLD_VCPUS" "$COLD_MEM" "$BENCH_SHAPE_VCPUS" "$BENCH_SHAPE_MEM"
printf '**Pool at start:** %s  \n\n' "$POOL_NOTE"
printf '| Operation | p50 (ms) | p95 (ms) | min (ms) | max (ms) | n | fails |\n'
printf '|-----------|----------|----------|----------|----------|---|-------|\n'
printf '%b' "$TABLE_ROWS"
printf '\n'
printf '**Notes**\n\n'
printf -- '- **Percentile method:** `idx = int(p × n / 100 + 0.999999)` clamped to [1, n],\n'
printf '  applied to sample arrays sorted ascending with `sort -n`; computed in `awk`.\n'
printf -- '- **`create-cold`**: measures POST /sandboxes → 201 only. Guest-ready polling\n'
printf '  (untimed) runs after 201 to ensure the sandbox is cleanly deleted, not mid-boot.\n'
printf -- '- **`create-claim`**: same measurement window (→ 201). Whether the warm pool was\n'
printf '  available is noted above; if `pool_size=0` this is another cold-boot measurement.\n'
printf -- '- **`exec-stream-first-frame`**: request-start → first `data:` SSE line. Uses\n'
printf '  `curl -N` (no output buffering) piped to `{ while IFS= read -r l; do case data:*)\n'
printf '  timestamp; break; done }`.  The inner command (`echo x; sleep 1`) ensures curl is\n'
printf '  still running when the first frame arrives, so this truly measures first-byte\n'
printf '  latency, not total exec time.\n'
printf -- '- **`wake (wall-clock)`**: POST /wake → 200 HTTP round-trip. **`wake (api wake_ms)`**:\n'
printf '  the integer `wake_ms` field in the wake response body — agent-measured\n'
printf '  snapshot-restore latency from inside hearthd/hearth-agent.\n'
printf -- '- **Cleanup guarantee:** every sandbox named `bench-%s-*` is deleted inline\n' "$RUN"
printf '  after each iteration (create-cold, create-claim, fork children) and the\n'
printf '  long-lived sandbox is deleted after the fork op. An EXIT trap (`_sweep`) deletes\n'
printf '  any surviving entries from `CREATED_IDS` regardless of how the script terminates.\n'
printf -- '- **Cold-boot assumption:** `mem_mib=%s` is chosen to differ from the default\n' "$COLD_MEM"
printf '  pool shape `%s`. Adjust `COLD_MEM` if your pool uses a non-default spec.\n' "$BENCH_SHAPE_MEM"
