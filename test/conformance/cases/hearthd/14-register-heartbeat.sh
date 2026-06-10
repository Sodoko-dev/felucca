# Agent registration contract, exercised WITHOUT polluting the fleet: we
# re-register an existing node with its own current values, which must be an
# idempotent-by-hostname update that preserves the node id (the live agents
# never re-register, so id stability is load-bearing for the Go port).
hd GET /api/v1/nodes
cf_n0=$(printf '%s' "$R_BODY" | jq -c '.nodes[0]')
cf_host=$(printf '%s' "$cf_n0" | jq -r .hostname)
cf_addr=$(printf '%s' "$cf_n0" | jq -r .addr)
cf_cpus=$(printf '%s' "$cf_n0" | jq -r .cpus)
cf_mem=$(printf '%s' "$cf_n0" | jq -r .mem_total_mib)
cf_nid=$(printf '%s' "$cf_n0" | jq -r .id)

hd POST /api/v1/agents/register "{\"hostname\":\"$cf_host\",\"addr\":\"$cf_addr\",\"cpus\":$cf_cpus,\"mem_total_mib\":$cf_mem}"
assert_status 200 "re-register existing hostname"
check_golden hearthd/register
cf_rid=$(printf '%s' "$R_BODY" | jq -r .id)
if [ "$cf_rid" = "$cf_nid" ]; then
  ok "registration idempotent by hostname (id preserved)"
else
  bad "re-register returned id $cf_rid, expected $cf_nid"
fi

cf_mf=$(printf '%s' "$cf_n0" | jq -r .mem_free_mib)
cf_vc=$(printf '%s' "$cf_n0" | jq -r .vm_count)
cf_ps=$(printf '%s' "$cf_n0" | jq -r .pool_size)
hd POST /api/v1/agents/heartbeat "{\"id\":\"$cf_rid\",\"mem_free_mib\":$cf_mf,\"vm_count\":$cf_vc,\"pool_size\":$cf_ps}"
assert_status 200 "heartbeat for known node"
assert_body_empty "heartbeat response"

hd POST /api/v1/agents/heartbeat '{"id":"node-00000000-0","mem_free_mib":1,"vm_count":0,"pool_size":0}'
assert_status 404 "heartbeat for unknown node"
assert_body_exact '{"error":"unknown node"}' "unknown-node body exact"
