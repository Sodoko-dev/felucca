# /api/* is bearer-guarded when a token is configured: missing and wrong
# tokens both get 401 with the exact contract body.
if [ -n "${HEARTH_TOKEN:-}" ]; then
  hd GET /api/v1/nodes "" none
  assert_status 401 "GET /api/v1/nodes without token"
  assert_body_exact '{"error":"unauthorized"}' "401 body exact (no token)"
  hd GET /api/v1/nodes "" badtoken
  assert_status 401 "GET /api/v1/nodes with wrong token"
  assert_body_exact '{"error":"unauthorized"}' "401 body exact (wrong token)"
else
  skip "HEARTH_TOKEN not set; target has auth disabled"
fi
