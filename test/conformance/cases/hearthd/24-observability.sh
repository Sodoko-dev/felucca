# Observability (v4 P6): histogram series on the open /metrics, the
# tenant-inventory leak guard (ADR-0009/0010 — no tenant-labeled series on
# the unauthenticated endpoint), the authenticated admin metrics surface,
# and X-Hearth-Request-Id minting. Deliberately boots NO VMs — presence and
# auth semantics only, so this case stays cheap and vz-crash-proof.
CF_OB_RUN="$$"

# ── Block 1: open /metrics — histograms present, tenant series absent ───────
hd GET /metrics '' none
assert_status 200 "GET /metrics (open)"
for cf_ob_h in hearth_wake_duration_ms hearth_exec_duration_ms hearth_create_duration_ms; do
  if printf '%s' "$R_BODY" | grep -q "^${cf_ob_h}_bucket"; then
    ok "$cf_ob_h histogram present"
  else
    bad "$cf_ob_h histogram missing from /metrics"
  fi
  if printf '%s' "$R_BODY" | grep -q "^${cf_ob_h}_bucket{le=\"+Inf\"}"; then
    ok "$cf_ob_h has the +Inf bucket"
  else
    bad "$cf_ob_h +Inf bucket missing"
  fi
done
# Leak guard: tenant-labeled series must NEVER appear on the open endpoint.
if printf '%s' "$R_BODY" | grep -q "hearth_tenant_"; then
  bad "tenant-labeled series leaked onto the open /metrics"
else
  ok "no hearth_tenant_* on the open /metrics (leak guard)"
fi
# Legacy wake counters stay (pre-P6 dashboards).
if printf '%s' "$R_BODY" | grep -q "^hearth_wake_ms_sum"; then
  ok "legacy hearth_wake_ms_sum still emitted"
else
  bad "legacy hearth_wake_ms_sum dropped"
fi

# ── Block 2: admin metrics surface auth ladder ──────────────────────────────
hd GET /api/v1/metrics/tenants '' none
assert_status 401 "GET /api/v1/metrics/tenants without token -> 401"
hd GET /api/v1/metrics/tenants
assert_status 200 "GET /api/v1/metrics/tenants (admin)"
if printf '%s' "$R_BODY" | grep -q "hearth_tenant_sandboxes"; then
  ok "admin surface serves hearth_tenant_sandboxes"
else
  bad "admin surface missing hearth_tenant_sandboxes: $(printf '%s' "$R_BODY" | head -c 200)"
fi

hd POST /api/v1/tenants "{\"name\":\"cf-ob-t-$CF_OB_RUN\"}"
assert_status 201 "POST /api/v1/tenants (cf-ob-t)"
CF_OB_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -n "$CF_OB_KEY" ]; then
  req_as "$CF_OB_KEY" "$HEARTH_API" GET /api/v1/metrics/tenants
  if [ "$R_STATUS" = "404" ]; then
    ok "tenant key gets 404 on the admin metrics surface"
  else
    bad "tenant key on admin metrics: expected 404, got $R_STATUS"
  fi
fi

# ── Block 3: request-id minting (raw curl — lib req() discards headers) ─────
cf_ob_h1=$(curl -s -m 10 -D - -o /dev/null -H "Authorization: Bearer $HEARTH_TOKEN" \
  "$HEARTH_API/api/v1/sandboxes" | grep -i '^x-hearth-request-id:' | tr -d '\r' | awk '{print $2}')
cf_ob_h2=$(curl -s -m 10 -D - -o /dev/null -H "Authorization: Bearer $HEARTH_TOKEN" \
  "$HEARTH_API/api/v1/sandboxes" | grep -i '^x-hearth-request-id:' | tr -d '\r' | awk '{print $2}')
case "$cf_ob_h1" in
  req-*) ok "X-Hearth-Request-Id minted with req- prefix ($cf_ob_h1)" ;;
  *)     bad "X-Hearth-Request-Id missing or malformed: '$cf_ob_h1'" ;;
esac
if [ -n "$cf_ob_h1" ] && [ "$cf_ob_h1" != "$cf_ob_h2" ]; then
  ok "request ids are unique per request"
else
  bad "request ids not unique: '$cf_ob_h1' vs '$cf_ob_h2'"
fi
