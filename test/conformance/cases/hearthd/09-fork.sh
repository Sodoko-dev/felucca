# shellcheck shell=bash
# Fork: 201, child carries parent_id and boots running.
hd POST "/api/v1/sandboxes/$CF_SB_ID/fork" '{"name":"cf-sb-child"}'
assert_status 201 "POST .../fork"
assert_jq '.state == "running"' "fork child running"
# Read by later cases (10-actions, 11-delete-204) out of run.sh's scope. That
# use is invisible from inside a single sourced fragment, hence the disable.
# shellcheck disable=SC2034
CF_CHILD_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
cf_parent=$(printf '%s' "$R_BODY" | jq -r '.parent_id // empty')
if [ "$cf_parent" = "$CF_SB_ID" ]; then
  ok "child parent_id == parent id"
else
  bad "child parent_id '$cf_parent' != parent '$CF_SB_ID'"
fi
check_golden hearthd/sandbox-fork

# The child name is caller-supplied and is rendered by the operator console,
# so it is held to the same charset as create's: 1..64 printable ASCII with the
# HTML-significant bytes refused. Asserted against a REAL parent so the 400 is
# the name's doing and not a missing-parent 404.
hd POST "/api/v1/sandboxes/$CF_SB_ID/fork" '{"name":"<img src=x onerror=alert(1)>"}'
assert_status 400 "fork with HTML metacharacters in the name -> 400"
assert_body_exact '{"error":"invalid name"}' "fork invalid-name body exact"
hd GET /api/v1/sandboxes
assert_jq '[.sandboxes[] | select(.name | test("<"))] | length == 0' \
  "no sandbox carries an unescapable name"
