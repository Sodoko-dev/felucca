# Sleep: 200 with the sandbox JSON, state=sleeping, and the state sticks.
hd POST "/api/v1/sandboxes/$CF_SB_ID/sleep"
assert_status 200 "POST .../sleep"
assert_jq '.state == "sleeping"' "sleep response state=sleeping"
check_golden hearthd/sandbox-sleep
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_jq '.state == "sleeping"' "GET after sleep shows sleeping"
