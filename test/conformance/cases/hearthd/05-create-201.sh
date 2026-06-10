# Create a sandbox: 201, synchronous boot to running, ip populated (net on).
# The created sandbox is reused by cases 06-11; name/namespace are fixed so
# they stay literal in the golden.
hd POST /api/v1/sandboxes '{"name":"cf-sb","namespace":"conformance","vcpus":1,"mem_mib":256}'
assert_status 201 "POST /api/v1/sandboxes"
CF_SB_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
assert_jq '.state == "running"' "created sandbox is running"
assert_jq '.ip != null' "guest ip assigned (net on)"
check_golden hearthd/sandbox-create
if [ -z "$CF_SB_ID" ]; then
  bad "no id in create response; dependent cases will fail"
fi
