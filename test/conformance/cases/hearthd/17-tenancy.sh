# Tenancy: admin CRUD for tenants/keys, tenant-scoped sandbox visibility,
# cross-tenant isolation (404, not 401), and quota enforcement (429 + "quota"
# in body). Requires HEARTH_TOKEN to be the admin key. Uses req_as for all
# requests that must run as a specific tenant API key.
#
# NOTE: there is no DELETE /api/v1/tenants/{id} in the contract; tenants
# created here are left on the server after the suite completes (names carry
# a per-run suffix so reruns never collide with leftovers). All sandboxes are
# deleted at the end of this file.
CF_RUN="$$"
CF_T1_NAME="cf-t1-$CF_RUN"
CF_T2_NAME="cf-t2-$CF_RUN"
CF_TQ_NAME="cf-t-quota-$CF_RUN"

# ── Block 1: create tenant cf-t1 ─────────────────────────────────────────────
hd POST /api/v1/tenants "{\"name\":\"$CF_T1_NAME\",\"max_sandboxes\":2,\"max_vcpus\":4,\"max_mem_mib\":512,\"max_disk_gb\":10}"
assert_status 201 "POST /api/v1/tenants (cf-t1)"
assert_jq '.tenant.id != null' "cf-t1 has id"
assert_jq ".tenant.name == \"$CF_T1_NAME\"" "cf-t1 name round-trips"
assert_jq '.api_key | startswith("hearth_sk_")' "cf-t1 api_key prefix"
CF_T1_ID=$(printf '%s' "$R_BODY" | jq -r '.tenant.id // empty')
CF_T1_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -z "$CF_T1_ID" ] || [ -z "$CF_T1_KEY" ]; then
  bad "cf-t1 id or api_key missing from create response; dependent blocks will fail"
fi

# ── Block 2: create tenant cf-t2 (isolation peer) ────────────────────────────
hd POST /api/v1/tenants "{\"name\":\"$CF_T2_NAME\",\"max_sandboxes\":2,\"max_vcpus\":4,\"max_mem_mib\":512,\"max_disk_gb\":10}"
assert_status 201 "POST /api/v1/tenants (cf-t2)"
assert_jq '.api_key | startswith("hearth_sk_")' "cf-t2 api_key prefix"
CF_T2_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -z "$CF_T2_KEY" ]; then
  bad "cf-t2 api_key missing; isolation blocks will fail"
fi

# ── Block 3: rotate key — add a second key for cf-t1 ────────────────────────
if [ -n "$CF_T1_ID" ]; then
  hd POST "/api/v1/tenants/$CF_T1_ID/keys"
  assert_status 201 "POST /api/v1/tenants/{id}/keys"
  assert_jq '.api_key | startswith("hearth_sk_")' "rotated key has correct prefix"
  CF_T1_KEY2=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
  CF_T1_KEY2_ID=$(printf '%s' "$R_BODY" | jq -r '.key_id // empty')
else
  skip "skipping key-rotate block (cf-t1 id unavailable)"
  CF_T1_KEY2=""
  CF_T1_KEY2_ID=""
fi

# ── Block 4: admin lists tenants ─────────────────────────────────────────────
hd GET /api/v1/tenants
assert_status 200 "GET /api/v1/tenants (admin)"
assert_jq '.tenants | type == "array"' "tenant list is an array"
assert_jq ".tenants | map(select(.name == \"$CF_T1_NAME\")) | length >= 1" "cf-t1 present in tenant list"

# ── Block 5: tenant key cannot list tenants (must be non-200) ────────────────
if [ -n "$CF_T1_KEY" ]; then
  req_as "$CF_T1_KEY" "$HEARTH_API" GET /api/v1/tenants
  if [ "$R_STATUS" != "200" ]; then
    ok "GET /api/v1/tenants with tenant key -> non-200 ($R_STATUS)"
  else
    bad "GET /api/v1/tenants with tenant key: expected non-200, got 200 (admin-only endpoint leaked)"
  fi
else
  skip "skipping tenant-list-denied block (CF_T1_KEY unavailable)"
fi

# ── Block 6: revoke the rotated key ──────────────────────────────────────────
if [ -n "$CF_T1_KEY2_ID" ]; then
  hd DELETE "/api/v1/keys/$CF_T1_KEY2_ID"
  assert_status 204 "DELETE /api/v1/keys/{id} (revoke rotated key)"
else
  skip "skipping key-revoke block (CF_T1_KEY2_ID unavailable)"
fi

# ── Block 7: revoked key is rejected ─────────────────────────────────────────
if [ -n "$CF_T1_KEY2" ]; then
  req_as "$CF_T1_KEY2" "$HEARTH_API" POST /api/v1/sandboxes \
    '{"name":"cf-t1-revoked","vcpus":1,"mem_mib":256}'
  assert_status 401 "POST /api/v1/sandboxes with revoked key -> 401"
else
  skip "skipping revoked-key block (CF_T1_KEY2 unavailable)"
fi

# ── Block 8: tenant creates a sandbox ────────────────────────────────────────
CF_T1_SB_ID=""
if [ -n "$CF_T1_KEY" ]; then
  req_as "$CF_T1_KEY" "$HEARTH_API" POST /api/v1/sandboxes \
    '{"name":"cf-t1-sb","vcpus":1,"mem_mib":256}'
  assert_status 201 "POST /api/v1/sandboxes with cf-t1 key"
  assert_jq '.state == "running"' "cf-t1 sandbox is running"
  CF_T1_SB_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
  if [ -z "$CF_T1_SB_ID" ]; then
    bad "cf-t1 sandbox id missing; isolation blocks will fail"
  fi
else
  skip "skipping cf-t1 sandbox create (CF_T1_KEY unavailable)"
fi

# ── Block 9: cf-t1 key lists only its own sandbox ────────────────────────────
if [ -n "$CF_T1_KEY" ] && [ -n "$CF_T1_SB_ID" ]; then
  req_as "$CF_T1_KEY" "$HEARTH_API" GET /api/v1/sandboxes
  assert_status 200 "GET /api/v1/sandboxes with cf-t1 key"
  assert_jq ".sandboxes | map(select(.id == \"$CF_T1_SB_ID\")) | length == 1" "cf-t1 sandbox visible to cf-t1"
else
  skip "skipping cf-t1 list-own block"
fi

# ── Block 10: cf-t2 creates a sandbox ────────────────────────────────────────
CF_T2_SB_ID=""
if [ -n "$CF_T2_KEY" ]; then
  req_as "$CF_T2_KEY" "$HEARTH_API" POST /api/v1/sandboxes \
    '{"name":"cf-t2-sb","vcpus":1,"mem_mib":256}'
  assert_status 201 "POST /api/v1/sandboxes with cf-t2 key"
  assert_jq '.state == "running"' "cf-t2 sandbox is running"
  CF_T2_SB_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
  if [ -z "$CF_T2_SB_ID" ]; then
    bad "cf-t2 sandbox id missing; isolation blocks will fail"
  fi
else
  skip "skipping cf-t2 sandbox create (CF_T2_KEY unavailable)"
fi

# ── Block 11: cross-tenant GET by id is 404 (existence not leaked) ────────────
# Contract: GET/POST on another tenant's sandbox id → 404, body {"error":"not found"}
if [ -n "$CF_T2_KEY" ] && [ -n "$CF_T1_SB_ID" ]; then
  req_as "$CF_T2_KEY" "$HEARTH_API" GET "/api/v1/sandboxes/$CF_T1_SB_ID"
  assert_status 404 "GET cf-t1 sandbox with cf-t2 key -> 404 (isolation)"
  assert_body_exact '{"error":"not found"}' "cross-tenant 404 body exact"
else
  skip "skipping cross-tenant GET block"
fi

# ── Block 12: cf-t2 list does not include cf-t1's sandbox ────────────────────
if [ -n "$CF_T2_KEY" ] && [ -n "$CF_T1_SB_ID" ]; then
  req_as "$CF_T2_KEY" "$HEARTH_API" GET /api/v1/sandboxes
  assert_status 200 "GET /api/v1/sandboxes with cf-t2 key"
  assert_jq ".sandboxes | map(select(.id == \"$CF_T1_SB_ID\")) | length == 0" "cf-t1 sandbox absent from cf-t2 list"
else
  skip "skipping cf-t2 list-isolation block"
fi

# ── Block 13: admin list sees both sandboxes ──────────────────────────────────
if [ -n "$CF_T1_SB_ID" ] && [ -n "$CF_T2_SB_ID" ]; then
  hd GET /api/v1/sandboxes
  assert_status 200 "GET /api/v1/sandboxes (admin)"
  assert_jq ".sandboxes | map(select(.id == \"$CF_T1_SB_ID\")) | length == 1" "admin sees cf-t1 sandbox"
  assert_jq ".sandboxes | map(select(.id == \"$CF_T2_SB_ID\")) | length == 1" "admin sees cf-t2 sandbox"
else
  skip "skipping admin-sees-all block (one or both sandbox ids unavailable)"
fi

# ── Block 14: quota enforcement (max_sandboxes=1) ────────────────────────────
CF_TQUOTA_SB1=""
hd POST /api/v1/tenants "{\"name\":\"$CF_TQ_NAME\",\"max_sandboxes\":1,\"max_vcpus\":2,\"max_mem_mib\":256,\"max_disk_gb\":5}"
assert_status 201 "POST /api/v1/tenants (cf-t-quota)"
CF_TQUOTA_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -n "$CF_TQUOTA_KEY" ]; then
  req_as "$CF_TQUOTA_KEY" "$HEARTH_API" POST /api/v1/sandboxes \
    '{"name":"cf-tq-sb1","vcpus":1,"mem_mib":256}'
  assert_status 201 "first sandbox under quota -> 201"
  CF_TQUOTA_SB1=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
  req_as "$CF_TQUOTA_KEY" "$HEARTH_API" POST /api/v1/sandboxes \
    '{"name":"cf-tq-sb2","vcpus":1,"mem_mib":256}'
  assert_status 429 "second sandbox exceeds quota -> 429"
  assert_jq '.error | ascii_downcase | contains("quota")' "429 body mentions quota"
else
  skip "skipping quota block (cf-t-quota api_key missing)"
fi

# ── Block 15: cleanup — delete all sandboxes created in this case ─────────────
# Tenants are NOT deleted: DELETE /api/v1/tenants/{id} is not in the contract.
# cf-t1, cf-t2, and cf-t-quota remain on the server after this suite runs.
if [ -n "$CF_TQUOTA_SB1" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_TQUOTA_SB1"
  assert_status 204 "DELETE cf-tquota sandbox"
fi
if [ -n "$CF_T2_SB_ID" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_T2_SB_ID"
  assert_status 204 "DELETE cf-t2 sandbox"
fi
if [ -n "$CF_T1_SB_ID" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_T1_SB_ID"
  assert_status 204 "DELETE cf-t1 sandbox"
fi
