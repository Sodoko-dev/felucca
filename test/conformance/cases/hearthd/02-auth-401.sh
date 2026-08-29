# shellcheck shell=bash
# Every /api/* route is bearer-guarded: missing and wrong tokens both get 401 with the
# exact contract body. There is no "auth disabled by omission" any more — an
# empty token authorizes nobody and hearthd refuses to start with one, so the
# only tokenless target is an explicit `hearthd --insecure-no-auth` lab, which
# the suite reaches through HEARTH_INSECURE_NO_AUTH=1.
#
# This is the FIRST case in the run that spends a credential failure, which is
# why it can still assert the exact 401 rather than tolerating the per-source
# throttle: nothing has accumulated yet.
if [ "${HEARTH_INSECURE_NO_AUTH:-0}" = "1" ]; then
  skip "HEARTH_INSECURE_NO_AUTH=1; target is running --insecure-no-auth"
else
  hd GET /api/v1/nodes "" none
  assert_status 401 "GET /api/v1/nodes without token"
  assert_body_exact '{"error":"unauthorized"}' "401 body exact (no token)"
  hd GET /api/v1/nodes "" badtoken
  assert_status 401 "GET /api/v1/nodes with wrong token"
  assert_body_exact '{"error":"unauthorized"}' "401 body exact (wrong token)"

  # The gate is not read-only: the route that creates sandboxes is behind it
  # too, and an unauthenticated attempt must leave nothing behind.
  hd POST /api/v1/sandboxes '{"name":"cf-noauth","namespace":"conformance"}' none
  assert_unauthorized "POST /api/v1/sandboxes without token"
  hd GET /api/v1/sandboxes
  assert_jq '[.sandboxes[] | select(.name == "cf-noauth")] | length == 0' \
    "unauthenticated create left no sandbox behind"
fi
