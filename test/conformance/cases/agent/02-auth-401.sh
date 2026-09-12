# shellcheck shell=bash
# Every /v1/* route requires the bearer token (exact 401 body). The agent API
# is remote root on this node — it execs into every guest and streams their
# disks — and it has no --insecure-no-auth escape hatch: the agent refuses to
# start without a real token, so "no token configured" is not a state a
# conforming node can be in.
#
# Read AND write methods are covered: the gate is per-handler in the agent, so
# a route that forgot the check would be invisible to a GET-only probe.
if [ "${FELUCCA_INSECURE_NO_AUTH:-0}" = "1" ]; then
  skip "FELUCCA_INSECURE_NO_AUTH=1; suite is running without a credential"
else
  ag GET /v1/vms "" none
  assert_unauthorized "GET /v1/vms without token"
  ag GET /v1/vms "" badtoken
  assert_unauthorized "GET /v1/vms with wrong token"

  # Writes too: create is the route that boots a VM, delete the one that
  # recursively removes an instance directory.
  ag POST /v1/vms '{"id":"sb-cf00000000000000000000dead","name":"cf-noauth","vcpus":1,"mem_mib":256}' none
  assert_unauthorized "POST /v1/vms without token"
  ag DELETE /v1/vms/sb-cf00000000000000000000dead "" none
  assert_unauthorized "DELETE /v1/vms/{id} without token"

  # ...and the unauthenticated create must not have happened.
  ag GET /v1/vms
  assert_jq '[.vms[] | select(.name == "cf-noauth")] | length == 0' \
    "unauthenticated create left no VM behind"
fi
