# shellcheck shell=bash
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

# Last response's Retry-After, in seconds ("" when absent). It is the
# documented discriminator between hearthd's two 429s (API-V2 §6): the
# credential-gate throttle carries it, the tenant quota gate does not.
R_RETRY_AFTER=""

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS+1)); say "  PASS: $*"; }
bad()  { FAIL=$((FAIL+1)); say "  FAIL: $*"; }
skip() { SKIP=$((SKIP+1)); say "  SKIP: $*"; }

# ---- Credential resolution ----
#
# Both binaries now REFUSE TO START on an empty, placeholder, or short token
# (API-V2 §6), and an empty token authorizes nobody rather than turning auth
# off. So a suite that quietly defaults to a literal — `hearth-lab-token` was
# the old default here — can only ever produce a wall of 401s against a server
# that either is not running or is not the one the operator thinks. Resolve
# explicitly, gate the value, and say exactly what to do when there is none.
#
# Order: $HEARTH_TOKEN, then a file kept outside the tree — the same contract
# as scripts/verify-v2.sh and scripts/conformance-lab.sh, so one lab token
# serves every script.
HEARTH_TOKEN_FILE="${HEARTH_TOKEN_FILE:-${HOME:-}/.config/hearth/lab-token}"

# token_is_placeholder <token> — the values shipped in deploy/config/*.example.json
# and hardcoded in the old lab scripts. Both binaries reject these outright, so
# the suite must reject them too instead of spending a full run discovering it.
# Mirrors go/internal/config.IsPlaceholderToken.
token_is_placeholder() {
  local low
  low=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case "$low" in
    *replace_with*|hearth-lab-token) return 0 ;;
  esac
  return 1
}

# resolve_token — fill HEARTH_TOKEN from the file fallback and refuse a value
# the target would refuse. Returns non-zero (caller aborts) rather than
# limping on. HEARTH_INSECURE_NO_AUTH=1 is the ONLY tokenless path, and it
# mirrors hearthd's own --insecure-no-auth: named, loud, and never a default.
resolve_token() {
  if [ -z "${HEARTH_TOKEN:-}" ] && [ -r "$HEARTH_TOKEN_FILE" ]; then
    HEARTH_TOKEN="$(tr -d ' \t\r\n' < "$HEARTH_TOKEN_FILE")"
  fi
  if [ "${HEARTH_INSECURE_NO_AUTH:-0}" = "1" ]; then
    if [ -n "${HEARTH_TOKEN:-}" ]; then
      say "ERROR: HEARTH_INSECURE_NO_AUTH=1 and a token are both set — pick one."
      say "  The flag means the target runs 'hearthd --insecure-no-auth' (no credential at all)."
      return 1
    fi
    say "WARNING: running tokenless (HEARTH_INSECURE_NO_AUTH=1). Valid ONLY against a target"
    say "         started with 'hearthd --insecure-no-auth'. The credential cases SKIP, and"
    say "         every case that proves TENANT SCOPING will FAIL — with auth off there are"
    say "         no tenants to scope, so a tenant key is the admin. Only a token-authenticated"
    say "         target can pass the whole contract."
    return 0
  fi
  if [ -z "${HEARTH_TOKEN:-}" ]; then
    say "ERROR: no admin token."
    say "  Set HEARTH_TOKEN, or write one to $HEARTH_TOKEN_FILE (HEARTH_TOKEN_FILE overrides)."
    say "  Generate one with: openssl rand -hex 32"
    say "  It must be the token the target hearthd/hearth-agent were STARTED with — both"
    say "  binaries refuse to start without a real one, so there is no tokenless target to"
    say "  fall back to. For a loopback lab on 'hearthd --insecure-no-auth', run the suite"
    say "  with HEARTH_INSECURE_NO_AUTH=1 instead."
    return 1
  fi
  if token_is_placeholder "$HEARTH_TOKEN"; then
    say "ERROR: HEARTH_TOKEN is a shipped placeholder — it is published in this repo, and both"
    say "       binaries refuse to start with it, so nothing is listening on it."
    say "  Generate a real one with: openssl rand -hex 32"
    return 1
  fi
  # 32 is hearthd's minimum (the agent's is 16): gate on the stricter of the
  # two, since the suite drives both.
  if [ "${#HEARTH_TOKEN}" -lt 32 ]; then
    say "ERROR: HEARTH_TOKEN is ${#HEARTH_TOKEN} chars; hearthd requires at least 32 and"
    say "       refuses to start below that, so nothing is listening on it."
    say "  Generate a real one with: openssl rand -hex 32"
    return 1
  fi
  return 0
}

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
  R_RETRY_AFTER=$(grep -i '^retry-after:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  # Chunked Transfer-Encoding is banned (the fleet's minimal HTTP clients are
  # Content-Length-only) EXCEPT for SSE streams (text/event-stream), which are
  # inherently unframed and consumed only by streaming-aware clients — the
  # streamed-exec endpoint (v4 P5.1), never the minimal internal clients.
  if grep -qi '^transfer-encoding:' "$hdr" && \
     ! printf '%s' "$R_CT" | grep -qi 'text/event-stream'; then
    bad "$method $path: response uses Transfer-Encoding (must be Content-Length framed)"
  fi
  rm -f "$hdr" "$bod"

  # The suite spends failed credentials on purpose (every negative auth case),
  # and past 10 the per-source guard refuses that source for a bounded window
  # EVEN WITH THE RIGHT TOKEN (API-V2 §6). So a 429 carrying Retry-After on a
  # request that presented the good credential is the suite's own footprint,
  # not a defect: wait it out and retry once, instead of reporting a cascade of
  # failures that all mean "the previous case's negative tests worked". Only
  # the good-credential arm retries — a deliberate no-token or wrong-token
  # probe must keep its refusal (assert_unauthorized accepts either door), and
  # the quota 429 carries no Retry-After and is never retried.
  if [ "${_req_retrying:-0}" = "0" ] && [ "$auth" = "token" ] && [ "$R_STATUS" = "429" ] && \
     printf '%s' "${R_RETRY_AFTER:-}" | grep -qE '^[0-9]+$' && [ "$R_RETRY_AFTER" -le 60 ]; then
    say "  NOTE: credential throttle engaged (Retry-After: ${R_RETRY_AFTER}s) — waiting it out, retrying once"
    sleep "$((R_RETRY_AFTER + 1))"
    local _req_retrying=1
    req "$base" "$method" "$path" "$body" "$auth"
  fi
}

hd() { req "$HEARTH_API" "$@"; }
ag() { req "$AGENT_API" "$@"; }

# req_file <base> <method> <path> <body-file> [auth] — like req, but streams
# the request body from a file. Linux caps a single argv string at 128 KiB, so
# the body-limit cases (which post more than a MiB) cannot go through req.
# On a curl-level failure R_STATUS is "000": for an oversized body that is a
# legitimate outcome (the server answers and closes while the upload is still
# in flight), so the caller must pair it with a "nothing was created" check
# rather than treating 000 as a pass on its own.
req_file() {
  local base="$1" method="$2" path="$3" file="$4" auth="${5:-token}"
  local hdr bod
  hdr=$(mktemp) bod=$(mktemp)
  local args=(-s -m "${REQ_MAX_TIME:-30}" -X "$method" -D "$hdr" -o "$bod" -w '%{http_code}')
  case "$auth" in
    token) [ -n "${HEARTH_TOKEN:-}" ] && args+=(-H "Authorization: Bearer $HEARTH_TOKEN") ;;
    badtoken) args+=(-H "Authorization: Bearer definitely-not-the-token") ;;
    none) ;;
  esac
  args+=(-H "Content-Type: application/json" --data-binary "@$file")
  R_STATUS=$(curl "${args[@]}" "$base$path")
  R_BODY=$(cat "$bod")
  R_CT=$(grep -i '^content-type:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  R_RETRY_AFTER=$(grep -i '^retry-after:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  rm -f "$hdr" "$bod"
}

hd_file() { req_file "$HEARTH_API" "$@"; }
ag_file() { req_file "$AGENT_API" "$@"; }

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
  R_RETRY_AFTER=$(grep -i '^retry-after:' "$hdr" | head -1 | tr -d '\r' | cut -d' ' -f2-)
  # Chunked Transfer-Encoding is banned (the fleet's minimal HTTP clients are
  # Content-Length-only) EXCEPT for SSE streams (text/event-stream), which are
  # inherently unframed and consumed only by streaming-aware clients — the
  # streamed-exec endpoint (v4 P5.1), never the minimal internal clients.
  if grep -qi '^transfer-encoding:' "$hdr" && \
     ! printf '%s' "$R_CT" | grep -qi 'text/event-stream'; then
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

# assert_unauthorized <label> — the request must have been refused AT THE
# CREDENTIAL GATE. 401 with the contract body is the answer; a 429 carrying
# Retry-After is the per-source brute-force guard (API-V2 §6) declining to
# evaluate the attempt at all, which is the same property reached by a
# different door — and the suite's own negative cases are what push a source
# toward that threshold, so a run must not fail because the guard engaged.
# A 429 WITHOUT Retry-After is the tenant quota gate and is not a refusal of
# the credential; anything 2xx is the hole this exists to catch.
assert_unauthorized() {
  case "$R_STATUS" in
    401)
      if [ "$R_BODY" = '{"error":"unauthorized"}' ]; then
        ok "$1 -> 401"
      else
        bad "$1: 401 but body '$R_BODY' != '{\"error\":\"unauthorized\"}'"
      fi ;;
    429)
      if [ -n "${R_RETRY_AFTER:-}" ]; then
        ok "$1 -> 429 auth throttle (Retry-After: $R_RETRY_AFTER)"
      else
        bad "$1: 429 with no Retry-After — that is the quota gate, not the credential gate (body: $R_BODY)"
      fi ;;
    *)
      bad "$1: expected 401 (or a throttled 429), got $R_STATUS (body: $R_BODY)" ;;
  esac
}

# assert_rejected <label> — the request must not have succeeded, where the
# contract pins "refused" but not which 4xx. Also passes on curl's "000": a
# server that answers and closes mid-upload (the body-limit path) can cut the
# request off before curl finishes sending it. Callers MUST pair that with a
# check that nothing was created — 000 alone proves nothing.
assert_rejected() {
  case "$R_STATUS" in
    2*) bad "$1: expected a rejection, got $R_STATUS (body: $R_BODY)" ;;
    000) ok "$1 -> refused mid-request (connection closed by the server)" ;;
    *)  ok "$1 -> $R_STATUS" ;;
  esac
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
  # The bearer is omitted entirely when there is no token rather than sent as
  # an empty "Bearer ": an empty token authorizes nobody now, so a 45-iteration
  # poll would spend 45 failed attempts and trip the per-source brute-force
  # guard — locking the rest of the suite out with 429s.
  local auth=()
  [ -n "${HEARTH_TOKEN:-}" ] && auth=(-H "Authorization: Bearer $HEARTH_TOKEN")
  local i=0
  while [ "$i" -lt "$timeout" ]; do
    if curl -s -m 5 -X POST ${auth[@]+"${auth[@]}"} \
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
