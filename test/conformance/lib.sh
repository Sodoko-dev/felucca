# Shared helpers for the Hearth conformance suite. Sourced by run.sh.
#
# The suite is the executable form of docs/API-V2.md: it must pass against any
# implementation of hearthd / hearth-agent (Zig today, Go/Rust ports), so all
# JSON comparison is semantic (key set, types, null-vs-omitted) — never
# byte-order — via normalize.jq + `jq -S`.

PASS=0
FAIL=0
SKIP=0
RECORD="${RECORD:-0}"
CONF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GOLDEN_DIR="$CONF_DIR/goldens"

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); say "  SKIP: $*"; }

# req <base> <method> <path> [json-body] [auth: token|none|badtoken]
# Sets: R_STATUS, R_BODY, R_CT. Fails the run if the response is chunked —
# the fleet's minimal HTTP clients are Content-Length-only (API-V2 §6 risk).
# REQ_MAX_TIME overrides the 30s curl budget for known-long calls (v4 P4
# rootfs capture streams gigabytes); set it per call site, never globally.
req() {
  local base="$1" method="$2" path="$3" body="${4:-}" auth="${5:-token}"
  local hdr bod
  hdr=$(mktemp) bod=$(mktemp)
  local args=(-s -m "${REQ_MAX_TIME:-30}" -X "$method" -D "$hdr" -o "$bod" -w '%{http_code}')
  case "$auth" in
    token) [ -n "${HEARTH_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $HEARTH_TOKEN") ;;
    badtoken) args+=(-H "Authorization: Bearer definitely-not-the-token") ;;
    none) ;;
  esac
  [ -n "$body" ] && args+=(-d "$body")
  R_STATUS=$(curl "${args[@]}" "$base$path")
  R_BODY=$(cat "$bod")
  R_CT=$(grep -i '^content-type:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  if grep -qi '^transfer-encoding:' "$hdr"; then
    bad "$method $path: response uses Transfer-Encoding (must be Content-Length framed)"
  fi
  rm -f "$hdr" "$bod"
}

hd() { req "$HEARTH_API" "$@"; }
ag() { req "$AGENT_API" "$@"; }

# req_as <key> <base> <method> <path> [json-body]
# Like req with auth=token but uses <key> as the bearer token instead of
# $HEARTH_TOKEN. Used by the tenancy suite to send requests as a specific
# tenant API key without touching the global HEARTH_TOKEN.
# Sets R_STATUS, R_BODY, R_CT identically to req.
req_as() {
  local key="$1" base="$2" method="$3" path="$4" body="${5:-}"
  local hdr bod
  hdr=$(mktemp) bod=$(mktemp)
  local args=(-s -m 30 -X "$method" -D "$hdr" -o "$bod" -w '%{http_code}')
  args+=(-H "Authorization: Bearer $key")
  [ -n "$body" ] && args+=(-d "$body")
  R_STATUS=$(curl "${args[@]}" "$base$path")
  R_BODY=$(cat "$bod")
  R_CT=$(grep -i '^content-type:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  if grep -qi '^transfer-encoding:' "$hdr"; then
    bad "$method $path: response uses Transfer-Encoding (must be Content-Length framed)"
  fi
  rm -f "$hdr" "$bod"
}

assert_status() { # <expected> <label>
  if [ "$R_STATUS" = "$1" ]; then ok "$2 -> $1"; else bad "$2: expected $1, got $R_STATUS (body: $R_BODY)"; fi
}

assert_body_exact() { # <expected-body> <label>
  if [ "$R_BODY" = "$1" ]; then ok "$2"; else bad "$2: body '$R_BODY' != '$1'"; fi
}

assert_body_empty() { # <label>
  if [ -z "$R_BODY" ]; then ok "$1 (empty body)"; else bad "$1: expected empty body, got '$R_BODY'"; fi
}

assert_jq() { # <jq-expr> <label>  (evaluated against R_BODY, must be truthy)
  if printf '%s' "$R_BODY" | jq -e "$1" >/dev/null 2>&1; then ok "$2"; else bad "$2 (jq: $1; body: $R_BODY)"; fi
}

normalize() { jq -S -f "$CONF_DIR/normalize.jq"; }

# wait_guest_ready <hd|ag> <id> [timeout-s] — poll exec(["true"]) until the
# guest agent answers (default 45s). Two contract constraints at once: the
# guest must finish booting (snapshots of mid-boot guests are poisoned — the
# guest panics on resume) and hearth-guest must be listening before
# sleep/exec/fork are exercised. Raw curl on purpose: must not clobber the
# caller's R_STATUS/R_BODY from a previous req.
wait_guest_ready() {
  local suite="$1" id="$2" timeout="${3:-45}"
  local base path
  case "$suite" in
    hd) base="$HEARTH_API"; path="/api/v1/sandboxes/$id/exec" ;;
    ag) base="$AGENT_API";  path="/v1/vms/$id/exec" ;;
    *)  bad "wait_guest_ready: unknown suite '$suite'"; return 1 ;;
  esac
  local i=0
  while [ "$i" -lt "$timeout" ]; do
    if curl -s -m 5 -X POST -H "Authorization: Bearer ${HEARTH_TOKEN:-}" \
        -d '{"cmd":["true"],"timeout_ms":2000}' "$base$path" 2>/dev/null \
        | jq -e '.ok == true' >/dev/null 2>&1; then
      return 0
    fi
    sleep 1; i=$((i+1))
  done
  bad "guest $id not ready after ${timeout}s (guest agent never answered exec)"
  return 1
}

# _golden_cmp <suite/name> <content> — record or diff a golden.
_golden_cmp() {
  local name="$1" cur="$2"
  local g="$GOLDEN_DIR/$name.golden"
  if [ "$RECORD" = "1" ]; then
    mkdir -p "$(dirname "$g")"
    printf '%s\n' "$cur" > "$g"
    ok "golden $name recorded"
  elif [ ! -f "$g" ]; then
    bad "golden $name missing (run record.sh against a reference implementation)"
  else
    local d
    if d=$(diff -u "$g" <(printf '%s\n' "$cur")); then
      ok "golden $name matches"
    else
      bad "golden $name mismatch:"
      printf '%s\n' "$d" | sed 's/^/    /'
    fi
  fi
}

# check_golden <suite/name> — golden of R_STATUS + R_CT + normalized R_BODY.
check_golden() {
  local norm
  if ! norm=$(printf '%s' "$R_BODY" | normalize 2>/dev/null); then
    bad "golden $1: response body is not valid JSON: $R_BODY"
    return
  fi
  _golden_cmp "$1" "$R_STATUS $R_CT
$norm"
}

# check_golden_metrics <suite/name> — golden of the hearth_* metric-name set
# (label values erased except `state`, which is contract-bound), values stripped.
check_golden_metrics() {
  local norm
  norm=$(printf '%s' "$R_BODY" \
    | grep -oE '^hearth_[a-z_]+(\{[^}]*\})?' \
    | sed -E 's/\{node="[^"]*"\}/{node=*}/' \
    | sort -u)
  _golden_cmp "$1" "$R_STATUS $R_CT
$norm"
}
