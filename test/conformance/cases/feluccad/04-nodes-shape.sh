# shellcheck shell=bash
# Node JSON shape: key set, types, computed status. Golden is the first
# node element (volatile values type-asserted and erased by normalize.jq).
hd GET /api/v1/nodes
assert_status 200 "GET /api/v1/nodes"
assert_jq '.nodes | type == "array" and length >= 1' "nodes is a non-empty array"
assert_jq '[.nodes[].status] | all(. == "ready" or . == "down")' "status values are ready|down"
R_BODY=$(printf '%s' "$R_BODY" | jq -c '.nodes[0]')
check_golden feluccad/node-shape
