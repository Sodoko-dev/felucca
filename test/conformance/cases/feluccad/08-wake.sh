# shellcheck shell=bash
# Wake: 200 with the sandbox JSON plus integer wake_ms, back to running.
hd POST "/api/v1/sandboxes/$CF_SB_ID/wake"
assert_status 200 "POST .../wake"
assert_jq '.state == "running"' "wake response state=running"
assert_jq '.wake_ms | type == "number"' "wake_ms is a number"
check_golden feluccad/sandbox-wake
