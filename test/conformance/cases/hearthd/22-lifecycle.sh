# shellcheck shell=bash
# Lifecycle policies + usage aggregation (v4 P5.2/P5.3): per-tenant idle
# auto-sleep defaults, per-sandbox overrides, asleep-TTL auto-delete, and
# GET /tenants/{id}/usage. The hearthd sweep runs every 15s, so each policy
# deadline is asserted with a poll of deadline + ~20s slack.
CF_LC_RUN="$$"
CF_LC_T_NAME="cf-lc-t-$CF_LC_RUN"

# ── Block 1: validation ──────────────────────────────────────────────────────
hd POST /api/v1/tenants "{\"name\":\"cf-lc-bad-$CF_LC_RUN\",\"default_idle_sleep_s\":1}"
assert_status 400 "tenant default idle below minimum -> 400"
hd POST /api/v1/tenants "{\"name\":\"cf-lc-bad-$CF_LC_RUN\",\"default_asleep_delete_s\":-1}"
assert_status 400 "tenant default ttl -1 -> 400 (0 is the off switch)"
hd POST /api/v1/sandboxes "{\"name\":\"cf-lc-bad-$CF_LC_RUN\",\"idle_sleep_s\":2}"
assert_status 400 "sandbox idle_sleep_s below minimum -> 400"
hd POST /api/v1/sandboxes "{\"name\":\"cf-lc-bad-$CF_LC_RUN\",\"asleep_delete_s\":5}"
assert_status 400 "sandbox asleep_delete_s below minimum -> 400"

# ── Block 2: tenant default idle -> auto-sleep; wake restarts the clock ─────
hd POST /api/v1/tenants "{\"name\":\"$CF_LC_T_NAME\",\"default_idle_sleep_s\":5}"
assert_status 201 "POST /api/v1/tenants (idle default 5s)"
assert_jq '.tenant.default_idle_sleep_s == 5' "tenant carries the default"
CF_LC_T_ID=$(printf '%s' "$R_BODY" | jq -r '.tenant.id // empty')
CF_LC_T_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')

CF_LC_SB=""
if [ -n "$CF_LC_T_KEY" ]; then
  req_as "$CF_LC_T_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-lc-sb-$CF_LC_RUN\"}"
  if [ "$R_STATUS" = "201" ]; then
    ok "tenant sandbox created (inherits idle default)"
    CF_LC_SB=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
  else
    bad "tenant sandbox create: expected 201, got $R_STATUS $R_BODY"
  fi
fi

cf_lc_wait_state() { # cf_lc_wait_state <key> <id> <state> <tries-of-3s>
  local i=0
  while [ "$i" -lt "$4" ]; do
    req_as "$1" "$HEARTH_API" GET "/api/v1/sandboxes/$2"
    [ "$(printf '%s' "$R_BODY" | jq -r .state)" = "$3" ] && return 0
    sleep 3; i=$((i+1))
  done
  return 1
}

if [ -n "$CF_LC_SB" ]; then
  # Idle 5s + sweep 15s -> sleeping well within 45s.
  if cf_lc_wait_state "$CF_LC_T_KEY" "$CF_LC_SB" sleeping 15; then
    ok "idle sandbox auto-slept (tenant default)"
  else
    bad "sandbox never auto-slept (state=$(printf '%s' "$R_BODY" | jq -r .state))"
  fi

  # Wake it: activity restarts the idle clock, and execs keep it running.
  req_as "$CF_LC_T_KEY" "$HEARTH_API" POST "/api/v1/sandboxes/$CF_LC_SB/wake"
  if [ "$R_STATUS" = "200" ]; then ok "auto-slept sandbox wakes"; else bad "wake: $R_STATUS $R_BODY"; fi
  # Two spaced execs hold it awake past the 5s idle deadline...
  sleep 4
  req_as "$CF_LC_T_KEY" "$HEARTH_API" POST "/api/v1/sandboxes/$CF_LC_SB/exec" '{"cmd":["true"],"timeout_ms":5000}'
  sleep 4
  req_as "$CF_LC_T_KEY" "$HEARTH_API" GET "/api/v1/sandboxes/$CF_LC_SB"
  if [ "$(printf '%s' "$R_BODY" | jq -r .state)" = "running" ]; then
    ok "exec activity defers auto-sleep"
  else
    bad "sandbox slept despite activity (state=$(printf '%s' "$R_BODY" | jq -r .state))"
  fi
  # ...then it auto-sleeps again once left alone.
  if cf_lc_wait_state "$CF_LC_T_KEY" "$CF_LC_SB" sleeping 15; then
    ok "sandbox auto-slept again after activity stopped"
  else
    bad "second auto-sleep never happened"
  fi
fi

# ── Block 3: per-sandbox override beats the default ─────────────────────────
CF_LC_SB2=""
if [ -n "$CF_LC_T_KEY" ]; then
  req_as "$CF_LC_T_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-lc-never-$CF_LC_RUN\",\"idle_sleep_s\":-1}"
  if [ "$R_STATUS" = "201" ]; then
    CF_LC_SB2=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
    ok "sandbox with idle_sleep_s=-1 created"
  else
    bad "override sandbox create: $R_STATUS"
  fi
fi
if [ -n "$CF_LC_SB2" ]; then
  # The tenant default would sleep it by ~20s; the -1 override must not.
  sleep 30
  req_as "$CF_LC_T_KEY" "$HEARTH_API" GET "/api/v1/sandboxes/$CF_LC_SB2"
  if [ "$(printf '%s' "$R_BODY" | jq -r .state)" = "running" ]; then
    ok "idle_sleep_s=-1 override beats the tenant default"
  else
    bad "override ignored: state=$(printf '%s' "$R_BODY" | jq -r .state)"
  fi
fi

# ── Block 4: asleep TTL -> auto-delete ───────────────────────────────────────
CF_LC_SB3=""
if [ -n "$CF_LC_T_KEY" ]; then
  req_as "$CF_LC_T_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-lc-ttl-$CF_LC_RUN\",\"idle_sleep_s\":5,\"asleep_delete_s\":30}"
  if [ "$R_STATUS" = "201" ]; then
    CF_LC_SB3=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
    ok "ttl sandbox created (idle 5s, delete after 30s asleep)"
  else
    bad "ttl sandbox create: $R_STATUS"
  fi
fi
if [ -n "$CF_LC_SB3" ]; then
  if cf_lc_wait_state "$CF_LC_T_KEY" "$CF_LC_SB3" sleeping 15; then
    ok "ttl sandbox auto-slept"
  else
    bad "ttl sandbox never slept"
  fi
  # TTL 30s + sweep 15s: gone within ~60s.
  cf_lc_gone=""
  for _ in $(seq 1 25); do
    req_as "$CF_LC_T_KEY" "$HEARTH_API" GET "/api/v1/sandboxes/$CF_LC_SB3"
    [ "$R_STATUS" = "404" ] && cf_lc_gone=1 && break
    sleep 3
  done
  if [ -n "$cf_lc_gone" ]; then
    ok "sleeping sandbox auto-deleted after its TTL"
  else
    bad "ttl sandbox still present after deadline"
  fi
fi

# ── Block 5: usage aggregation over the events this case produced ───────────
if [ -n "$CF_LC_T_ID" ]; then
  hd GET "/api/v1/tenants/$CF_LC_T_ID/usage"
  assert_status 200 "GET tenant usage (admin)"
  assert_jq ".tenant_id == \"$CF_LC_T_ID\"" "usage names the tenant"
  assert_jq '.events > 0' "events were recorded"
  assert_jq '.execs >= 1' "exec events counted"
  assert_jq '.sandbox_hours > 0' "running time accrued"
  assert_jq '.disk_gb_hours > 0' "disk accounting accrued"

  req_as "$CF_LC_T_KEY" "$HEARTH_API" GET "/api/v1/tenants/$CF_LC_T_ID/usage"
  if [ "$R_STATUS" = "200" ]; then
    ok "tenant key reads its own usage"
  else
    bad "tenant self usage: expected 200, got $R_STATUS"
  fi
  req_as "$CF_LC_T_KEY" "$HEARTH_API" GET "/api/v1/tenants/tn-other/usage"
  if [ "$R_STATUS" = "404" ]; then
    ok "tenant key cannot read another tenant's usage"
  else
    bad "cross-tenant usage: expected 404, got $R_STATUS"
  fi
  hd GET "/api/v1/tenants/$CF_LC_T_ID/usage?from=100&to=50"
  assert_status 400 "usage from>to -> 400"
  hd GET "/api/v1/tenants/tn-nonexistent/usage"
  assert_status 404 "usage of unknown tenant -> 404"
fi

# ── Block 6: cleanup (tenant rows stay, like every tenancy case) ────────────
for cf_lc_id in "$CF_LC_SB" "$CF_LC_SB2"; do
  if [ -n "$cf_lc_id" ]; then
    hd DELETE "/api/v1/sandboxes/$cf_lc_id"
    assert_status 204 "DELETE lifecycle sandbox $cf_lc_id"
  fi
done
