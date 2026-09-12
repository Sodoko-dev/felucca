# shellcheck shell=bash
# Sandbox ingress (v4 P3): named multi-service expose, the admin route table,
# and the dynamic port-in-hostname ensure gate. Self-cleaning: every sandbox
# created here is deleted (worker DNAT rules die with the VM via the agent's
# flush-and-rebuild). The gateway itself is not driven here — the conformance
# contract covers feluccad's wire surface; gateway behavior is pinned by its
# unit tests and the lab e2e.
#
# No goldens: ids/node ports are dynamic.
CF_RUN="$$"
CF_EXP_NAME="cf-expose-$CF_RUN"
CF_EXP_T_NAME="cf-expose-t-$CF_RUN"

# ── Block 1: create a sandbox to expose ──────────────────────────────────────
CF_EXP_ID=""
hd POST /api/v1/sandboxes "{\"name\":\"$CF_EXP_NAME\"}"
assert_status 201 "POST /api/v1/sandboxes ($CF_EXP_NAME)"
CF_EXP_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
if [ -z "$CF_EXP_ID" ]; then
  bad "sandbox id missing; remaining expose blocks will mostly skip"
fi

# ── Block 2: expose input validation ─────────────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"Odoo","port":80}'
  assert_status 400 "expose with uppercase name -> 400"
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"a--b","port":80}'
  assert_status 400 "expose with double-dash name -> 400"
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"8080","port":80}'
  assert_status 400 "expose with all-digit name (dynamic namespace) -> 400"
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"web","port":0}'
  assert_status 400 "expose with port 0 -> 400"
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{not json'
  assert_status 400 "expose with bad JSON -> 400"
fi
hd POST /api/v1/sandboxes/sb-nonexistent/expose '{"name":"web","port":80}'
assert_status 404 "expose on unknown sandbox -> 404"

# ── Block 3: happy path ──────────────────────────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"web","port":8069}'
  assert_status 201 "expose web:8069 -> 201"
  assert_jq ".hostname == \"web--$CF_EXP_ID\"" "hostname label is name--id"
  assert_jq '.node_port >= 20000 and .node_port <= 29999' "node port in worker range"
  assert_jq '.guest_port == 8069' "guest port echoed"

  hd GET "/api/v1/sandboxes/$CF_EXP_ID"
  assert_status 200 "GET sandbox after expose"
  assert_jq '.exposes | length == 1' "sandbox JSON carries the expose"

  # Idempotent repeat (same name+port) and name conflict (other port).
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"web","port":8069}'
  assert_status 200 "expose repeat (same name+port) -> 200 idempotent"
  hd POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"web","port":9000}'
  assert_status 409 "expose name conflict (other port) -> 409"
fi

# ── Block 4: the admin route table ───────────────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  hd GET /api/v1/routes
  assert_status 200 "GET /api/v1/routes (admin)"
  assert_jq "[.routes[] | select(.hostname == \"web--$CF_EXP_ID\")] | length == 1" "route row present for the expose"
  assert_jq "[.routes[] | select(.hostname == \"web--$CF_EXP_ID\")][0] | (.node_host != \"\" and .state == \"running\")" "route row carries node host + state"
fi

# ── Block 5: tenant scoping (admin-only table; foreign sandboxes 404) ────────
hd POST /api/v1/tenants "{\"name\":\"$CF_EXP_T_NAME\",\"max_sandboxes\":1,\"max_vcpus\":1,\"max_mem_mib\":256,\"max_disk_gb\":5}"
assert_status 201 "POST /api/v1/tenants (cf-expose-t)"
CF_EXP_T_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -n "$CF_EXP_T_KEY" ] && [ -n "$CF_EXP_ID" ]; then
  req_as "$CF_EXP_T_KEY" "$FELUCCA_API" GET /api/v1/routes
  if [ "$R_STATUS" != "200" ]; then
    ok "GET /api/v1/routes with tenant key -> non-200 ($R_STATUS, expect 404)"
  else
    bad "GET /api/v1/routes with tenant key: expected non-200, got 200 (admin-only table leaked)"
  fi
  req_as "$CF_EXP_T_KEY" "$FELUCCA_API" POST "/api/v1/sandboxes/$CF_EXP_ID/expose" '{"name":"steal","port":80}'
  if [ "$R_STATUS" = "404" ]; then
    ok "expose on foreign sandbox with tenant key -> 404"
  else
    bad "expose on foreign sandbox with tenant key: expected 404, got $R_STATUS"
  fi
  req_as "$CF_EXP_T_KEY" "$FELUCCA_API" POST /api/v1/routes/ensure "{\"sandbox_id\":\"$CF_EXP_ID\",\"port\":8069}"
  if [ "$R_STATUS" != "200" ]; then
    ok "routes/ensure with tenant key -> non-200 ($R_STATUS, expect 404)"
  else
    bad "routes/ensure with tenant key: expected non-200, got 200 (admin-only endpoint leaked)"
  fi
else
  skip "skipping tenant-scoping block (key or sandbox missing)"
fi

# ── Block 6: dynamic port-in-hostname gate ───────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  # Default sandbox: opt-out -> 403, and no digit-label route appears.
  hd POST /api/v1/routes/ensure "{\"sandbox_id\":\"$CF_EXP_ID\",\"port\":8070}"
  assert_status 403 "routes/ensure on opt-out sandbox -> 403"
fi
CF_DYN_ID=""
hd POST /api/v1/sandboxes "{\"name\":\"$CF_EXP_NAME-dyn\",\"allow_dynamic_ports\":true}"
assert_status 201 "POST /api/v1/sandboxes (allow_dynamic_ports)"
CF_DYN_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
if [ -n "$CF_DYN_ID" ]; then
  hd POST /api/v1/routes/ensure "{\"sandbox_id\":\"$CF_DYN_ID\",\"port\":8070}"
  assert_status 200 "routes/ensure on opt-in sandbox -> 200"
  assert_jq ".hostname == \"8070--$CF_DYN_ID\"" "dynamic hostname is port--id"
  hd POST /api/v1/routes/ensure "{\"sandbox_id\":\"$CF_DYN_ID\",\"port\":8070}"
  assert_status 200 "routes/ensure repeat -> 200 idempotent"
fi
hd POST /api/v1/routes/ensure '{"sandbox_id":"sb-nonexistent","port":80}'
assert_status 404 "routes/ensure on unknown sandbox -> 404"

# ── Block 7: unexpose ────────────────────────────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_EXP_ID/expose/web"
  assert_status 204 "DELETE expose/web -> 204"
  hd GET /api/v1/routes
  assert_status 200 "GET /api/v1/routes after unexpose"
  assert_jq "[.routes[] | select(.hostname == \"web--$CF_EXP_ID\")] | length == 0" "route row gone after unexpose"
  hd DELETE "/api/v1/sandboxes/$CF_EXP_ID/expose/web"
  assert_status 404 "DELETE expose/web again -> 404"
fi

# ── Block 8: cleanup ─────────────────────────────────────────────────────────
if [ -n "$CF_EXP_ID" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_EXP_ID"
  assert_status 204 "cleanup: delete $CF_EXP_NAME"
fi
if [ -n "$CF_DYN_ID" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_DYN_ID"
  assert_status 204 "cleanup: delete $CF_EXP_NAME-dyn"
fi
