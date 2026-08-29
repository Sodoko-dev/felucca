# shellcheck shell=bash
# Delete: 204 empty; a deleted sandbox is gone (404 with the exact body).
hd DELETE "/api/v1/sandboxes/$CF_CHILD_ID"
assert_status 204 "DELETE child"
assert_body_empty "delete child response"
hd DELETE "/api/v1/sandboxes/$CF_SB_ID"
assert_status 204 "DELETE parent"
assert_body_empty "delete parent response"
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_status 404 "GET deleted sandbox"
assert_body_exact '{"error":"not found"}' "404 body exact"
