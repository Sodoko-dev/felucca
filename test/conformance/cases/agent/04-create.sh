# Direct agent create (the id is caller-supplied — hearthd does this in
# production): 201 {"ok":true,"ip":<str|null>}, then the VM lists as running
# with a live pid. Uses a hearthd-shaped id so normalize.jq applies.
CF_VM1="sb-cf000001-90001"
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
