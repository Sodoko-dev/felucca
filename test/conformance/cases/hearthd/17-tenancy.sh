# shellcheck shell=bash
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

# ── Block 3b: admin lists a tenant's keys (the revocation recovery path) ─────
# Without a listing, DELETE /api/v1/keys/{id} is unusable for a leaked key
# whose id nobody kept, and the only remediation left is raw SQL against the
# database. The listing must therefore identify a key WITHOUT being able to
# reconstruct it: `prefix` is the secret's first 14 chars ("hearth_sk_" + 4),
# enough to match a leaked value against a row, useless for guessing the rest.
if [ -n "$CF_T1_ID" ]; then
  hd GET "/api/v1/tenants/$CF_T1_ID/keys"
  assert_status 200 "GET /api/v1/tenants/{id}/keys (admin)"
  assert_jq '.keys | type == "array" and length >= 2' "both cf-t1 keys are listed"
  # Inside `.keys | ...` the dot is the array, so both sides of the comparison
  # count the same thing (a `(.keys | length)` after the pipe would be an error).
  assert_jq '.keys | (map(select((.id | type) == "string" and (.prefix | length) == 14)) | length) == length' \
    "every listed key carries an id and a 14-char prefix"
  assert_jq '[.keys[] | keys_unsorted[]] | unique == ["created_at","expires_at","id","prefix","revoked_at"]' \
    "key view has exactly {id,prefix,created_at,expires_at,revoked_at}"
  # The secret and its verifier must not be reachable from the listing.
  if printf '%s' "$R_BODY" | grep -qF "$CF_T1_KEY"; then
    bad "the full api_key is present in the key listing"
  else
    ok "full api_key absent from the listing"
  fi
  if printf '%s' "$R_BODY" | grep -qE '"(key_hash|hash|secret|api_key)"'; then
    bad "the key listing exposes a hash/secret field"
  else
    ok "no hash or secret field in the key listing"
  fi
  # The prefix really is the first 14 chars of the issued secret — that is what
  # makes it usable for matching a leak.
  cf_t1_pfx="${CF_T1_KEY:0:14}"
  assert_jq "[.keys[] | select(.prefix == \"$cf_t1_pfx\")] | length == 1" \
    "the first key's prefix matches the secret it was minted from"

  hd GET "/api/v1/tenants/tn-cf-nonexistent/keys"
  assert_status 404 "GET keys of an unknown tenant -> 404"
fi
if [ -n "$CF_T1_KEY" ] && [ -n "$CF_T1_ID" ]; then
  # A tenant reading its own key list would hand every holder of one key the
  # ids of all the others; the whole /api/v1/tenants surface is admin-only.
  req_as "$CF_T1_KEY" "$HEARTH_API" GET "/api/v1/tenants/$CF_T1_ID/keys"
  if [ "$R_STATUS" != "200" ]; then
    ok "GET own keys with a tenant key -> non-200 ($R_STATUS, expect 404)"
  else
    bad "GET /api/v1/tenants/{id}/keys with a tenant key: expected non-200, got 200"
  fi
fi

# ── Block 3c: keys may carry an expiry, and an expired key is dead ──────────
# An expiring key is the only way to hand out a credential that stops working
# on its own; if expiry were recorded but not enforced it would be worse than
# no expiry at all, because the operator would believe the key was gone.
CF_T1_KEY_EXP=""
if [ -n "$CF_T1_ID" ]; then
  hd POST "/api/v1/tenants/$CF_T1_ID/keys" '{"expires_in_s":-1}'
  assert_status 400 "expires_in_s=-1 -> 400"
  hd POST "/api/v1/tenants/$CF_T1_ID/keys" '{"expires_in_s":31536001}'
  assert_status 400 "expires_in_s beyond the one-year cap -> 400"

  cf_now=$(date +%s)
  hd POST "/api/v1/tenants/$CF_T1_ID/keys" '{"expires_in_s":1}'
  assert_status 201 "POST keys with expires_in_s=1 -> 201"
  assert_jq "(.expires_at | type) == \"number\" and .expires_at >= $cf_now and .expires_at <= $((cf_now + 60))" \
    "expires_at is the mint time plus the requested TTL"
  CF_T1_KEY_EXP=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
fi
if [ -n "$CF_T1_KEY_EXP" ]; then
  sleep 2
  req_as "$CF_T1_KEY_EXP" "$HEARTH_API" GET /api/v1/sandboxes
  assert_unauthorized "expired key is refused exactly like a revoked one"
else
  skip "skipping expired-key block (expiring key unavailable)"
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
  # Revocation is visible in the listing — an operator has to be able to see
  # that the key they revoked is the one that died.
  if [ -n "$CF_T1_ID" ]; then
    hd GET "/api/v1/tenants/$CF_T1_ID/keys"
    assert_jq "[.keys[] | select(.id == \"$CF_T1_KEY2_ID\" and .revoked_at != null)] | length == 1" \
      "revoked key shows revoked_at in the listing"
  fi
  hd DELETE "/api/v1/keys/$CF_T1_KEY2_ID"
  assert_status 404 "DELETE an already-revoked key -> 404"
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
