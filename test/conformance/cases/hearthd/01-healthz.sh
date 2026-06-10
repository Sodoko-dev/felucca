# /healthz is open (no token) and returns the exact contract body.
hd GET /healthz "" none
assert_status 200 "GET /healthz"
assert_body_exact '{"ok":true}' "healthz body exact"
