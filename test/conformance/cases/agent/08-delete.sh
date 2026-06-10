# Agent delete: 204 empty, VM gone from the list; delete is idempotent —
# unknown ids also get 204 (observed Zig reference behavior).
ag DELETE "/v1/vms/$CF_VM2"
assert_status 204 "DELETE child"
assert_body_empty "delete child response"
ag DELETE "/v1/vms/$CF_VM1"
assert_status 204 "DELETE parent"
assert_body_empty "delete parent response"
ag GET /v1/vms
if printf '%s' "$R_BODY" | jq -e --arg a "$CF_VM1" --arg b "$CF_VM2" \
    '[.vms[].id] | (index($a) == null) and (index($b) == null)' >/dev/null 2>&1; then
  ok "deleted VMs absent from list"
else
  bad "deleted VMs still listed: $R_BODY"
fi
ag DELETE /v1/vms/sb-deadbeef-99999
assert_status 204 "DELETE unknown VM (idempotent)"
assert_body_empty "idempotent delete response"
