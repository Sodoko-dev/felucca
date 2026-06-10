# GET by id returns the same shape; the list endpoint wraps sandboxes in
# {"sandboxes":[...]} and contains the one we created.
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_status 200 "GET /api/v1/sandboxes/{id}"
check_golden hearthd/sandbox-get
hd GET /api/v1/sandboxes
assert_status 200 "GET /api/v1/sandboxes"
assert_jq '.sandboxes | type == "array"' "sandboxes is an array"
if printf '%s' "$R_BODY" | jq -e --arg id "$CF_SB_ID" '.sandboxes[] | select(.id == $id)' >/dev/null 2>&1; then
  ok "created sandbox present in list"
else
  bad "created sandbox $CF_SB_ID missing from list"
fi
