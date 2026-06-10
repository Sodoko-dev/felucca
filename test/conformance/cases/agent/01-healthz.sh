# Agent /healthz is open and exact.
ag GET /healthz "" none
assert_status 200 "GET agent /healthz"
assert_body_exact '{"ok":true}' "agent healthz body exact"
