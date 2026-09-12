# shellcheck shell=bash
# Sleep: 200 with the sandbox JSON, state=sleeping, and the state sticks.
# Never sleep a mid-boot guest: the snapshot would be poisoned (guest panics
# on resume) — wait until the guest agent answers first.
wait_guest_ready hd "$CF_SB_ID"
hd POST "/api/v1/sandboxes/$CF_SB_ID/sleep"
assert_status 200 "POST .../sleep"
assert_jq '.state == "sleeping"' "sleep response state=sleeping"
check_golden feluccad/sandbox-sleep
hd GET "/api/v1/sandboxes/$CF_SB_ID"
assert_jq '.state == "sleeping"' "GET after sleep shows sleeping"
