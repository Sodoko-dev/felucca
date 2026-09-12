# shellcheck shell=bash
# v3 exec: run a command in a guest via feluccad → agent → vsock → guest agent.
# Requires a v3 stack with injected guest assets; the sandbox from case 05 has
# been deleted by case 11, so create a short-lived one here.
hd POST /api/v1/sandboxes '{"name":"cf-exec","namespace":"conformance","vcpus":1,"mem_mib":256}'
if [ "$R_STATUS" != "201" ]; then
  bad "exec-case sandbox create failed: $R_STATUS $R_BODY"
else
  cf_exec_id=$(printf '%s' "$R_BODY" | jq -r .id)
  wait_guest_ready hd "$cf_exec_id"  # cold boots need >3s for felucca-guest
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec" '{"cmd":["/bin/sh","-c","echo cf-exec-ok"],"timeout_ms":15000}'
  assert_status 200 "POST .../exec"
  assert_jq '.ok == true and .exit_code == 0 and (.stdout | contains("cf-exec-ok"))' "exec output round-trip"
  check_golden feluccad/exec
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec" '{"cmd":[]}'
  assert_status 400 "exec with empty cmd rejected"

  # timeout_ms is bounded [1, 300000] and REJECTED outside it, not clamped.
  # A negative value used to overflow time.Duration(ms)*time.Millisecond into a
  # ~290-year POSITIVE duration, and that value feeds both the connection
  # deadline and the agent request context — so "-1" disarmed the very
  # slow-loris guard those deadlines exist to provide. Asserted against a live
  # sandbox so a 400 cannot be a disguised 404.
  for cf_exec_bad in -1 0 300001; do
    hd POST "/api/v1/sandboxes/$cf_exec_id/exec" \
      "{\"cmd\":[\"true\"],\"timeout_ms\":$cf_exec_bad}"
    assert_status 400 "exec timeout_ms=$cf_exec_bad -> 400"
    assert_body_exact '{"error":"timeout_ms out of range"}' "timeout_ms=$cf_exec_bad body exact"
  done
  # The bound is inclusive at both ends.
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec" '{"cmd":["true"],"timeout_ms":1}'
  assert_status 200 "exec timeout_ms=1 accepted (lower bound inclusive)"
  # ...and the streamed arm shares the check, so it cannot be the way around it.
  hd POST "/api/v1/sandboxes/$cf_exec_id/exec?stream=1" '{"cmd":["true"],"timeout_ms":-1}'
  assert_status 400 "streamed exec timeout_ms=-1 -> 400"

  hd POST /api/v1/sandboxes/sb-cf0000000000000000deadbeef/exec '{"cmd":["true"]}'
  assert_status 404 "exec on unknown sandbox"
  hd DELETE "/api/v1/sandboxes/$cf_exec_id"
  assert_status 204 "exec-case sandbox cleaned up"
fi
