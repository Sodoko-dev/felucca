# Agent fork: child id+name supplied by the caller, 201 {"ok":true,"ip":...},
# child lists as running.
CF_VM2="sb-cf000002-90002"
ag POST "/v1/vms/$CF_VM1/fork" "{\"id\":\"$CF_VM2\",\"name\":\"cf-vm-2\"}"
assert_status 201 "POST /v1/vms/{id}/fork"
assert_jq '.ok == true' "fork ok:true"
check_golden agent/vm-fork
ag GET /v1/vms
if printf '%s' "$R_BODY" | jq -e --arg id "$CF_VM2" \
    '.vms[] | select(.id == $id) | .state == "running"' >/dev/null 2>&1; then
  ok "fork child running"
else
  bad "fork child not running: $R_BODY"
fi
