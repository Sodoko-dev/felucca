# shellcheck shell=bash
# Create a sandbox: 201, synchronous boot to running, ip populated (net on).
# The created sandbox is reused by cases 06-11; name/namespace are fixed so
# they stay literal in the golden.
hd POST /api/v1/sandboxes '{"name":"cf-sb","namespace":"conformance","vcpus":1,"mem_mib":256}'
assert_status 201 "POST /api/v1/sandboxes"
CF_SB_ID=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
assert_jq '.state == "running"' "created sandbox is running"
assert_jq '.ip != null' "guest ip assigned (net on)"
# The id is a bearer capability, not just a handle: it is half of the public
# ingress label "<expose-name>--<id>" (ADR-0007), which the gateway
# authenticates nothing else on. So it must be full-width crypto/rand with no
# guessable component — 26 hex chars, and NO trailing sequence number (a
# monotonic tail published every other tenant's position in the id stream).
# normalize.jq enforces the same shape on every id in every golden; this
# asserts it in the clear so a regression names itself.
assert_jq '.id | test("^sb-[0-9a-f]{26}$")' "sandbox id is sb- + 26 hex (no sequence tail)"
check_golden feluccad/sandbox-create
if [ -z "$CF_SB_ID" ]; then
  bad "no id in create response; dependent cases will fail"
fi
