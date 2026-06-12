# Templates & bigger guests (v4 P4): template CRUD, rootfs capture from a
# stopped sandbox, create-by-template (which exercises the full image
# distribution path — the worker pulls the captured image from hearthd and
# verifies its sha256), disk floors, and the per-tenant disk quota.
# Self-cleaning: sandboxes and the template are deleted at the end (the
# tenant row stays, like every other tenancy case).
#
# No goldens: ids, hashes, and sizes are dynamic.
CF_RUN="$$"
CF_TPL_NAME="cf-tpl-$CF_RUN"
CF_TPL_T_NAME="cf-tpl-t-$CF_RUN"

cf_tpl_exec() { # cf_tpl_exec <id> <command-string> [timeout-ms]
  hd POST "/api/v1/sandboxes/$1/exec" \
    "{\"cmd\":[\"/bin/sh\",\"-c\",\"$2\"],\"timeout_ms\":${3:-15000}}"
}

cf_tpl_wait_exec() { # cf_tpl_wait_exec <id> [tries] — until the guest agent answers
  local i=0 t="${2:-45}"
  while [ "$i" -lt "$t" ]; do
    cf_tpl_exec "$1" true 2000 2>/dev/null
    printf '%s' "$R_BODY" | grep -q '"ok":true' && return 0
    sleep 2; i=$((i+1))
  done
  return 1
}

# ── Block 1: input validation ────────────────────────────────────────────────
hd POST /api/v1/templates '{"name":"UPPER","image_file":"ubuntu-base"}'
assert_status 400 "template with uppercase name -> 400"
hd POST /api/v1/templates '{"name":"a--b","image_file":"ubuntu-base"}'
assert_status 400 "template with double-dash name -> 400"
hd POST /api/v1/templates '{"name":"8069","image_file":"ubuntu-base"}'
assert_status 400 "template with all-digit name -> 400"
hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\"}"
assert_status 400 "template without a source -> 400"
hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"image_file\":\"x\",\"from_sandbox\":\"y\"}"
assert_status 400 "template with both sources -> 400"
hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"image_file\":\"ubuntu-base\",\"vcpus\":99}"
assert_status 400 "template with vcpus over cap -> 400"
hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"from_sandbox\":\"sb-nonexistent\"}"
assert_status 404 "capture from unknown sandbox -> 404"

# ── Block 2: build a marker into a sandbox, stop it, capture it ──────────────
CF_TPL_SB=""
hd POST /api/v1/sandboxes "{\"name\":\"cf-tpl-builder-$CF_RUN\"}"
assert_status 201 "POST /api/v1/sandboxes (builder)"
CF_TPL_SB=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
if [ -z "$CF_TPL_SB" ]; then
  bad "builder id missing; remaining template blocks will mostly skip"
fi

if [ -n "$CF_TPL_SB" ]; then
  if cf_tpl_wait_exec "$CF_TPL_SB"; then
    ok "builder guest agent ready"
  else
    bad "builder guest agent never answered"
  fi
  cf_tpl_exec "$CF_TPL_SB" "echo cf-tpl-proof-$CF_RUN > /cf-marker; sync"
  assert_jq '.ok == true' "marker written into the builder rootfs"

  # Capture refuses a running (dirty-filesystem) sandbox.
  hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"from_sandbox\":\"$CF_TPL_SB\"}"
  assert_status 409 "capture of a running sandbox -> 409"

  hd POST "/api/v1/sandboxes/$CF_TPL_SB/stop"
  assert_status 200 "stop builder"
  cf_tpl_stopped=""
  for _ in $(seq 1 30); do
    hd GET "/api/v1/sandboxes/$CF_TPL_SB"
    cf_tpl_stopped=$(printf '%s' "$R_BODY" | jq -r .state)
    [ "$cf_tpl_stopped" = "stopped" ] && break
    sleep 1
  done
  if [ "$cf_tpl_stopped" = "stopped" ]; then
    ok "builder reached stopped"
  else
    bad "builder never stopped (state=$cf_tpl_stopped)"
  fi

  # The capture streams the full 2 GiB rootfs agent→hearthd and hashes it —
  # well past the suite's default 30s curl budget.
  REQ_MAX_TIME=300 hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"from_sandbox\":\"$CF_TPL_SB\",\"vcpus\":2,\"mem_mib\":512}"
  assert_status 201 "capture stopped builder as template -> 201"
  assert_jq ".name == \"$CF_TPL_NAME\" and .image == \"$CF_TPL_NAME\"" "captured template owns its image"
  assert_jq '.image_sha256 | length == 64' "template carries a sha256"
  assert_jq '.image_size_gb >= 1 and .disk_gb >= .image_size_gb' "disk_gb floored at the image size"

  hd POST /api/v1/templates "{\"name\":\"$CF_TPL_NAME\",\"from_sandbox\":\"$CF_TPL_SB\"}"
  assert_status 409 "duplicate template name -> 409"
fi

# ── Block 3: catalog ─────────────────────────────────────────────────────────
hd GET /api/v1/templates
assert_status 200 "GET /api/v1/templates"
assert_jq "[.templates[] | select(.name == \"$CF_TPL_NAME\")] | length == 1" "catalog lists the new template"

# ── Block 4: create-by-template (full image distribution path) ───────────────
CF_TPL_CHILD=""
if [ -n "$CF_TPL_SB" ]; then
  hd POST /api/v1/sandboxes "{\"name\":\"cf-tpl-child-$CF_RUN\",\"template\":\"nope-$CF_RUN\"}"
  assert_status 400 "create from unknown template -> 400"
  hd POST /api/v1/sandboxes "{\"name\":\"cf-tpl-child-$CF_RUN\",\"template\":\"$CF_TPL_NAME\",\"disk_gb\":1}"
  assert_status 400 "disk_gb below the template image size -> 400"

  # The worker may need to pull the 2 GiB image from hearthd first (prefetch
  # is async, and a cold-cache create 502s by design when the pull outlives
  # hearthd's 30s agent call — ADR-0008). Retry until the cache is warm.
  cf_tpl_create_status=""
  for _ in $(seq 1 8); do
    hd POST /api/v1/sandboxes "{\"name\":\"cf-tpl-child-$CF_RUN\",\"template\":\"$CF_TPL_NAME\"}"
    cf_tpl_create_status="$R_STATUS"
    [ "$cf_tpl_create_status" = "201" ] && break
    sleep 15
  done
  if [ "$cf_tpl_create_status" = "201" ]; then
    ok "create from template -> 201"
    CF_TPL_CHILD=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
    assert_jq ".template == \"$CF_TPL_NAME\"" "sandbox JSON carries the template name"
    assert_jq '.vcpus == 2 and .mem_mib == 512' "template shape applied"
  else
    bad "create from template: expected 201, got $cf_tpl_create_status"
  fi

  if [ -n "$CF_TPL_CHILD" ]; then
    if cf_tpl_wait_exec "$CF_TPL_CHILD"; then
      ok "template child guest agent ready"
    else
      bad "template child guest agent never answered"
    fi
    cf_tpl_exec "$CF_TPL_CHILD" "cat /cf-marker"
    assert_jq ".ok == true and (.stdout | contains(\"cf-tpl-proof-$CF_RUN\"))" "child booted from the CAPTURED image (marker present)"
  fi
fi

# ── Block 5: tenant scoping + disk quota ─────────────────────────────────────
hd POST /api/v1/tenants "{\"name\":\"$CF_TPL_T_NAME\",\"max_disk_gb\":5}"
assert_status 201 "POST /api/v1/tenants (cf-tpl-t)"
CF_TPL_T_KEY=$(printf '%s' "$R_BODY" | jq -r '.api_key // empty')
if [ -n "$CF_TPL_T_KEY" ]; then
  req_as "$CF_TPL_T_KEY" "$HEARTH_API" GET /api/v1/templates
  if [ "$R_STATUS" = "200" ]; then
    ok "GET /api/v1/templates with tenant key -> 200 (catalog is tenant-visible)"
  else
    bad "GET /api/v1/templates with tenant key: expected 200, got $R_STATUS"
  fi
  req_as "$CF_TPL_T_KEY" "$HEARTH_API" POST /api/v1/templates '{"name":"cf-t-no","image_file":"ubuntu-base"}'
  if [ "$R_STATUS" = "404" ]; then
    ok "POST /api/v1/templates with tenant key -> 404"
  else
    bad "POST /api/v1/templates with tenant key: expected 404, got $R_STATUS"
  fi
  req_as "$CF_TPL_T_KEY" "$HEARTH_API" GET "/api/v1/images/ubuntu-base"
  if [ "$R_STATUS" = "404" ]; then
    ok "GET /api/v1/images with tenant key -> 404"
  else
    bad "GET /api/v1/images with tenant key: expected 404, got $R_STATUS"
  fi

  # Disk quota: 4 GiB fits in 5; the next sandbox (2 GiB effective) busts it.
  CF_TPL_Q1=""
  req_as "$CF_TPL_T_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-tpl-q1-$CF_RUN\",\"disk_gb\":4}"
  if [ "$R_STATUS" = "201" ]; then
    ok "tenant create disk_gb=4 within quota -> 201"
    CF_TPL_Q1=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
  else
    bad "tenant create disk_gb=4: expected 201, got $R_STATUS"
  fi
  req_as "$CF_TPL_T_KEY" "$HEARTH_API" POST /api/v1/sandboxes "{\"name\":\"cf-tpl-q2-$CF_RUN\"}"
  if [ "$R_STATUS" = "429" ] && printf '%s' "$R_BODY" | grep -q disk_gb; then
    ok "tenant disk quota exceeded -> 429 disk_gb"
  else
    bad "tenant disk quota: expected 429 disk_gb, got $R_STATUS $R_BODY"
  fi
  if [ -n "$CF_TPL_Q1" ]; then
    req_as "$CF_TPL_T_KEY" "$HEARTH_API" DELETE "/api/v1/sandboxes/$CF_TPL_Q1"
    if [ "$R_STATUS" = "204" ]; then ok "tenant quota sandbox deleted"; else bad "tenant quota sandbox delete: $R_STATUS"; fi
  fi
fi

# ── Block 6: cleanup (self-cleaning) ─────────────────────────────────────────
if [ -n "$CF_TPL_CHILD" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_TPL_CHILD"
  assert_status 204 "DELETE template child"
fi
if [ -n "$CF_TPL_SB" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_TPL_SB"
  assert_status 204 "DELETE builder"
fi
# Sweep error-state leftovers from cold-cache create retries (each 502'd
# attempt leaves one errored sandbox row). Scoped to THIS run's suffix —
# a concurrent/previous run's cf-tpl-* sandboxes are not ours to delete.
hd GET /api/v1/sandboxes
for cf_tpl_leftover in $(printf '%s' "$R_BODY" | jq -r ".sandboxes[] | select((.name | startswith(\"cf-tpl-\")) and (.name | endswith(\"-$CF_RUN\"))) | .id"); do
  hd DELETE "/api/v1/sandboxes/$cf_tpl_leftover"
done
hd DELETE "/api/v1/templates/$CF_TPL_NAME"
assert_status 204 "DELETE template"
hd GET /api/v1/templates
assert_jq "[.templates[] | select(.name == \"$CF_TPL_NAME\")] | length == 0" "template gone from the catalog"
hd DELETE /api/v1/templates/cf-tpl-never-existed
assert_status 404 "DELETE unknown template -> 404"
