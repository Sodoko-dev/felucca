# Per-template tenant visibility (v4 P5.4, ADR-0008 deferral): a template
# created with "tenant" is visible to (and usable by) only that tenant; the
# admin sees everything; public ("" tenant) templates stay visible to all.
#
# The scoped template is built by CAPTURE, not image_file: hearthd's images_dir
# only holds captured/registered images — the base rootfs lives on the workers,
# so an image_file registration would 404. Capture is the realistic path for a
# tenant-owned template anyway (a tenant snapshots its own sandbox).
CF_TV_RUN="$$"
CF_TV_TPL="cf-tv-tpl-$CF_TV_RUN"

cf_tv_exec() { hd POST "/api/v1/sandboxes/$1/exec" "{\"cmd\":[\"true\"],\"timeout_ms\":2000}"; }
cf_tv_wait_exec() {
  local i=0
  while [ "$i" -lt 45 ]; do
    cf_tv_exec "$1" 2>/dev/null
    printf '%s' "$R_BODY" | grep -q '"ok":true' && return 0
    sleep 2; i=$((i+1))
  done
  return 1
}

hd POST /api/v1/tenants "{\"name\":\"cf-tv-a-$CF_TV_RUN\"}"
assert_status 201 "POST /api/v1/tenants (cf-tv-a)"
CF_TV_A_ID=$(printf '%s' "$R_BODY" | jq -r '.tenant.id // empty')
CF_TV_A_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
hd POST /api/v1/tenants "{\"name\":\"cf-tv-b-$CF_TV_RUN\"}"
assert_status 201 "POST /api/v1/tenants (cf-tv-b)"
CF_TV_B_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')

# Unknown owning tenant is rejected before any image work (tenant lookup
# precedes the source check).
hd POST /api/v1/templates "{\"name\":\"$CF_TV_TPL\",\"image_file\":\"ubuntu-base\",\"tenant\":\"nope-$CF_TV_RUN\"}"
assert_status 400 "template with unknown owning tenant -> 400"

# ── Build the tenant-scoped template by capturing a stopped sandbox ─────────
CF_TV_SB=""
if [ -n "$CF_TV_A_ID" ] && [ -n "$CF_TV_A_KEY" ] && [ -n "$CF_TV_B_KEY" ]; then
  hd POST /api/v1/sandboxes "{\"name\":\"cf-tv-builder-$CF_TV_RUN\"}"
  assert_status 201 "POST /api/v1/sandboxes (cf-tv builder)"
  CF_TV_SB=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
fi

if [ -n "$CF_TV_SB" ]; then
  if cf_tv_wait_exec "$CF_TV_SB"; then ok "cf-tv builder guest ready"; else bad "cf-tv builder never answered"; fi
  hd POST "/api/v1/sandboxes/$CF_TV_SB/stop"
  assert_status 200 "stop cf-tv builder"
  cf_tv_stopped=""
  for _ in $(seq 1 30); do
    hd GET "/api/v1/sandboxes/$CF_TV_SB"
    [ "$(printf '%s' "$R_BODY" | jq -r .state)" = "stopped" ] && cf_tv_stopped=1 && break
    sleep 1
  done
  [ -n "$cf_tv_stopped" ] && ok "cf-tv builder stopped" || bad "cf-tv builder never stopped"

  # Capture it as a tenant-A-owned template (capture streams ~2 GiB).
  REQ_MAX_TIME=300 hd POST /api/v1/templates "{\"name\":\"$CF_TV_TPL\",\"from_sandbox\":\"$CF_TV_SB\",\"tenant\":\"$CF_TV_A_ID\"}"
  assert_status 201 "tenant-scoped template captured"
  assert_jq ".tenant_id == \"$CF_TV_A_ID\"" "template carries its owning tenant"

  # Catalog: admin and the owner see it; the other tenant doesn't.
  hd GET /api/v1/templates
  assert_jq "[.templates[] | select(.name == \"$CF_TV_TPL\")] | length == 1" "admin sees the scoped template"
  req_as "$CF_TV_A_KEY" "$HEARTH_API" GET /api/v1/templates
  if printf '%s' "$R_BODY" | jq -e "[.templates[] | select(.name == \"$CF_TV_TPL\")] | length == 1" >/dev/null; then
    ok "owning tenant sees its template"
  else
    bad "owner missing its template: $R_BODY"
  fi
  req_as "$CF_TV_B_KEY" "$HEARTH_API" GET /api/v1/templates
  if printf '%s' "$R_BODY" | jq -e "[.templates[] | select(.name == \"$CF_TV_TPL\")] | length == 0" >/dev/null; then
    ok "foreign tenant does not see the scoped template"
  else
    bad "foreign tenant sees the scoped template"
  fi

  # Create-by-template: foreign tenant gets the same 400 as a missing template
  # (no existence leak).
  req_as "$CF_TV_B_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-tv-no-$CF_TV_RUN\",\"template\":\"$CF_TV_TPL\"}"
  if [ "$R_STATUS" = "400" ] && printf '%s' "$R_BODY" | grep -q "unknown template"; then
    ok "foreign tenant create-by-template -> 400 unknown template"
  else
    bad "foreign create-by-template: expected 400 unknown template, got $R_STATUS $R_BODY"
  fi

  hd DELETE "/api/v1/templates/$CF_TV_TPL"
  assert_status 204 "DELETE scoped template"
  hd DELETE "/api/v1/sandboxes/$CF_TV_SB"
  assert_status 204 "DELETE cf-tv builder"
fi
