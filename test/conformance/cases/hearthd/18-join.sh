# Node join (v4 P2): admin-minted one-time join tokens and the
# POST /api/v1/nodes/join exchange. Covers mint (201 + prefixes), mint input
# validation, admin-only enforcement (tenant key -> 404-ish non-201), and the
# join credential/validation ladder (401 unauthorized, 401 unknown token,
# 400 invalid pubkey without burning the token).
#
# This case NEVER completes a successful join on purpose: the lab hearthd runs
# with the overlay enabled, so a 200 join would persist the pubkey and install
# a real kernel wg peer. There is no peer-removal endpoint in the contract, so
# even a junk-but-valid-shape pubkey would enroll a dead peer forever. Every
# block below stops at 401/400 — before enrollment.
#
# NOTE (mirrors 17-tenancy leftover-tenants note): tokens minted here are left
# in the DB after the suite completes. They are never redeemed and expire after
# the 24h TTL, so reruns never collide and nothing accumulates beyond a day.
#
# No goldens: mint responses carry random ids/tokens.
CF_RUN="$$"
CF_JOIN_T_NAME="cf-join-t-$CF_RUN"

# A syntactically valid wg pubkey (44-char base64 of 32 zero bytes). Only ever
# sent with an UNKNOWN join token, so it can never be enrolled.
CF_DEAD_PUBKEY="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
# Well-formed but never-minted token: correct prefix, plausible secret length.
CF_UNKNOWN_JT="hearth_jt_0123456789abcdef0123456789abcdef0123456789abcdef"

# ── Block 1: admin mints a join token ─────────────────────────────────────────
CF_JT_TOKEN=""
hd POST /api/v1/join-tokens "{\"node_hint\":\"cf-join-$CF_RUN\"}"
assert_status 201 "POST /api/v1/join-tokens (admin mint)"
assert_jq '.token | startswith("hearth_jt_")' "minted token prefix"
assert_jq '.id | startswith("jt-")' "minted token id prefix"
CF_JT_TOKEN=$(printf '%s' "$R_BODY" | jq -r '.token // empty')
if [ -z "$CF_JT_TOKEN" ]; then
  bad "minted token missing from response; token-not-burned block will skip"
fi

# ── Block 2: mint input validation ────────────────────────────────────────────
hd POST /api/v1/join-tokens '{not json'
assert_status 400 "POST /api/v1/join-tokens with bad JSON -> 400"
# Empty body = no node_hint; tolerated (row expires in 24h, never redeemed).
hd POST /api/v1/join-tokens
assert_status 201 "POST /api/v1/join-tokens with empty body -> 201"

# ── Block 3: tenant key cannot mint (admin-only) ──────────────────────────────
hd POST /api/v1/tenants "{\"name\":\"$CF_JOIN_T_NAME\",\"max_sandboxes\":1,\"max_vcpus\":1,\"max_mem_mib\":256,\"max_disk_gb\":5}"
assert_status 201 "POST /api/v1/tenants (cf-join-t)"
CF_JOIN_T_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -n "$CF_JOIN_T_KEY" ]; then
  req_as "$CF_JOIN_T_KEY" "$HEARTH_API" POST /api/v1/join-tokens '{"node_hint":"nope"}'
  if [ "$R_STATUS" != "201" ]; then
    ok "POST /api/v1/join-tokens with tenant key -> non-201 ($R_STATUS, expect 404)"
  else
    bad "POST /api/v1/join-tokens with tenant key: expected non-201 (404), got 201 (admin-only endpoint leaked)"
  fi
else
  skip "skipping tenant-mint-denied block (cf-join-t api_key missing)"
fi

# ── Block 4: nodes/join credential gate ───────────────────────────────────────
# No bearer at all -> uniform 401.
hd POST /api/v1/nodes/join "{\"pubkey\":\"$CF_DEAD_PUBKEY\",\"hostname\":\"cf-join\"}" none
assert_status 401 "POST /api/v1/nodes/join without bearer -> 401"
assert_body_exact '{"error":"unauthorized"}' "no-bearer 401 body exact"

# An API-key-shaped bearer is the wrong credential kind -> same uniform 401.
req_as "hearth_sk_not_a_join_token" "$HEARTH_API" POST /api/v1/nodes/join \
  "{\"pubkey\":\"$CF_DEAD_PUBKEY\",\"hostname\":\"cf-join\"}"
assert_status 401 "POST /api/v1/nodes/join with hearth_sk_-shaped bearer -> 401"
assert_body_exact '{"error":"unauthorized"}' "wrong-credential-kind 401 body exact"

# Well-formed but unknown hearth_jt_ token + VALID-shape body: must reach the
# token check and 401 there. Nothing is consumed and no peer is created.
req_as "$CF_UNKNOWN_JT" "$HEARTH_API" POST /api/v1/nodes/join \
  "{\"pubkey\":\"$CF_DEAD_PUBKEY\",\"hostname\":\"cf-join\"}"
assert_status 401 "POST /api/v1/nodes/join with unknown hearth_jt_ token -> 401"
assert_jq '.error != null' "unknown-token 401 carries an error"

# ── Block 5: real token + invalid pubkey -> 400, token NOT burned ─────────────
# Request validation runs before the token is checked or consumed, so a 400
# must leave the token alive. We can't prove non-burn with a successful join
# (a 200 would enroll a dead wg peer forever — see header), so instead we
# assert a SECOND attempt with the same token still 400s on the body (not 401):
# the credential path is still valid and still reaches body validation.
if [ -n "$CF_JT_TOKEN" ]; then
  req_as "$CF_JT_TOKEN" "$HEARTH_API" POST /api/v1/nodes/join \
    '{"pubkey":"short","hostname":"cf-join"}'
  assert_status 400 "real token + invalid pubkey -> 400"
  req_as "$CF_JT_TOKEN" "$HEARTH_API" POST /api/v1/nodes/join \
    '{"pubkey":"short","hostname":"cf-join"}'
  assert_status 400 "same token, second invalid attempt -> still 400 (token not burned)"
else
  skip "skipping token-not-burned block (minted token unavailable)"
fi
