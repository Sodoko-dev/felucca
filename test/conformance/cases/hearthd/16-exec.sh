# v3 exec: run a command in a guest via hearthd → agent → vsock → guest agent.
# Requires a v3 stack with injected guest assets; the sandbox from case 05 has
# been deleted by case 11, so create a short-lived one here.
hd POST /api/v1/sandboxes '{"name":"cf-exec","namespace":"conformance","vcpus":1,"mem_mib":256}'
if [ "$R_STATUS" != "201" ]; then
  bad "exec-case sandbox create failed: $R_STATUS $R_BODY"
else
  cf_exec_id=$(printf '%s' "$R_BODY" | jq -r .id)
  sleep 3  # let the guest agent come up
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec" '{"cmd":["/bin/sh","-c","echo cf-exec-ok"],"timeout_ms":15000}'
  assert_status 200 "POST .../exec"
  assert_jq '.ok == true and .exit_code == 0 and (.stdout | contains("cf-exec-ok"))' "exec output round-trip"
  check_golden hearthd/exec
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec" '{"cmd":[]}'
  assert_status 400 "exec with empty cmd rejected"
  hd POST /api/v1/sandboxes/sb-deadbeef-99999/exec '{"cmd":["true"]}'
  assert_status 404 "exec on unknown sandbox"
  hd DELETE "/api/v1/sandboxes/$cf_exec_id"
  assert_status 204 "exec-case sandbox cleaned up"
fi
