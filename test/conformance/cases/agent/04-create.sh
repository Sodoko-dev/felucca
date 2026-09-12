# shellcheck shell=bash
# Direct agent create (the id is caller-supplied — feluccad does this in
# production): 201 {"ok":true,"ip":<str|null>}, then the VM lists as running
# with a live pid. Uses a feluccad-shaped id so normalize.jq applies: that is
# now "sb-" + 26 hex with no sequence tail, and normalize.jq asserts the width.
CF_VM1="sb-cf000000000000000000000001"
# Self-cleaning entry: a crashed previous run can leave cf-vm-1 adopted on the
# node; DELETE is idempotent (204 either way) so this is contract-safe.
ag DELETE "/v1/vms/$CF_VM1"
ag POST /v1/vms "{\"id\":\"$CF_VM1\",\"name\":\"cf-vm-1\",\"vcpus\":1,\"mem_mib\":256}"
assert_status 201 "POST /v1/vms"
assert_jq '.ok == true' "create ok:true"
check_golden agent/vm-create
ag GET /v1/vms
if printf '%s' "$R_BODY" | jq -e --arg id "$CF_VM1" \
    '.vms[] | select(.id == $id) | .state == "running" and (.pid | type == "number")' >/dev/null 2>&1; then
  ok "created VM running with live pid"
else
  bad "created VM not running with pid: $R_BODY"
fi
R_BODY=$(printf '%s' "$R_BODY" | jq -c --arg id "$CF_VM1" '.vms[] | select(.id == $id)')
check_golden agent/vm-shape
