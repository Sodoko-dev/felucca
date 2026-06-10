# v3 agent exec: direct agent-level exec on a VM created in this suite run.
# CF_VM1/CF_VM2 were deleted in case 08; create a short-lived VM.
CF_VM3="sb-cf000003-90003"
ag POST /v1/vms "{\"id\":\"$CF_VM3\",\"name\":\"cf-vm-3\",\"vcpus\":1,\"mem_mib\":256}"
if [ "$R_STATUS" != "201" ]; then
  bad "exec-case VM create failed: $R_STATUS $R_BODY"
else
  sleep 3  # guest agent startup
  ag POST "/v1/vms/$CF_VM3/exec" '{"cmd":["/bin/sh","-c","echo agent-exec-ok"],"timeout_ms":15000}'
  assert_status 200 "POST /v1/vms/{id}/exec"
  assert_jq '.ok == true and .exit_code == 0 and (.stdout | contains("agent-exec-ok"))' "agent exec output round-trip"
  check_golden agent/vm-exec
  ag DELETE "/v1/vms/$CF_VM3"
  assert_status 204 "exec-case VM cleaned up"
fi
