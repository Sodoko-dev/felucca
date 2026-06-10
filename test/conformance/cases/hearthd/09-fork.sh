# Fork: 201, child carries parent_id and boots running.
hd POST "/api/v1/sandboxes/$CF_SB_ID/fork" '{"name":"cf-sb-child"}'
assert_status 201 "POST .../fork"
assert_jq '.state == "running"' "fork child running"
CF_CHILD_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
cf_parent=$(printf '%s' "$R_BODY" | jq -r '.parent_id // empty')
if [ "$cf_parent" = "$CF_SB_ID" ]; then
  ok "child parent_id == parent id"
else
  bad "child parent_id '$cf_parent' != parent '$CF_SB_ID'"
fi
check_golden hearthd/sandbox-fork
