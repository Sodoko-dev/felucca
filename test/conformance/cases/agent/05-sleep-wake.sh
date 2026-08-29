# shellcheck shell=bash
# Agent sleep/wake: exact ok bodies; sleeping VM has no pid; wake reports
# integer wake_ms and the VM runs again.
# Never sleep a mid-boot guest: the snapshot would be poisoned (guest panics
# on resume) — wait until the guest agent answers first.
wait_guest_ready ag "$CF_VM1"
ag POST "/v1/vms/$CF_VM1/sleep"
assert_status 200 "POST /v1/vms/{id}/sleep"
assert_body_exact '{"ok":true}' "sleep body exact"
ag GET /v1/vms
if printf '%s' "$R_BODY" | jq -e --arg id "$CF_VM1" \
    '.vms[] | select(.id == $id) | .state == "sleeping" and .pid == null' >/dev/null 2>&1; then
  ok "sleeping VM listed with pid null"
else
  bad "VM not sleeping/pid-null after sleep: $R_BODY"
fi
ag POST "/v1/vms/$CF_VM1/wake"
assert_status 200 "POST /v1/vms/{id}/wake"
assert_jq '.ok == true and (.wake_ms | type == "number")' "wake ok with numeric wake_ms"
check_golden agent/vm-wake
ag GET /v1/vms
if printf '%s' "$R_BODY" | jq -e --arg id "$CF_VM1" \
    '.vms[] | select(.id == $id) | .state == "running"' >/dev/null 2>&1; then
  ok "VM running after wake"
else
  bad "VM not running after wake: $R_BODY"
fi
