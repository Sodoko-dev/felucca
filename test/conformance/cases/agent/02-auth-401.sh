# All /v1/* requires the bearer token when configured (exact 401 body).
if [ -n "${HEARTH_TOKEN:-}" ]; then
  ag GET /v1/vms "" none
  assert_status 401 "GET /v1/vms without token"
  assert_body_exact '{"error":"unauthorized"}' "agent 401 body exact"
else
  skip "HEARTH_TOKEN not set; target agent has auth disabled"
fi
