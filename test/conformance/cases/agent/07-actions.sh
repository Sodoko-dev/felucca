# Agent actions are empty-200 with observable transitions.
cf_agent_state() { # <id> <expected> <label>
  ag GET /v1/vms
  if printf '%s' "$R_BODY" | jq -e --arg id "$1" --arg st "$2" \
      '.vms[] | select(.id == $id) | .state == $st' >/dev/null 2>&1; then
    ok "$3"
  else
    bad "$3: $R_BODY"
  fi
}
ag POST "/v1/vms/$CF_VM1/pause"
assert_status 200 "POST /v1/vms/{id}/pause"
assert_body_empty "pause response"
cf_agent_state "$CF_VM1" paused "paused after pause"
ag POST "/v1/vms/$CF_VM1/resume"
assert_status 200 "POST /v1/vms/{id}/resume"
assert_body_empty "resume response"
cf_agent_state "$CF_VM1" running "running after resume"
ag POST "/v1/vms/$CF_VM2/stop"
assert_status 200 "POST /v1/vms/{id}/stop"
assert_body_empty "stop response"
cf_agent_state "$CF_VM2" stopped "stopped after stop"
ag POST "/v1/vms/$CF_VM2/start"
assert_status 200 "POST /v1/vms/{id}/start"
assert_body_empty "start response"
cf_agent_state "$CF_VM2" running "running after start"
