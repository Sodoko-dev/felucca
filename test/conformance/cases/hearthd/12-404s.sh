# Unmatched ids, paths, and methods are all 404 with the exact body
# (the contract has no 405: wrong method on a known path is also 404).
hd GET /api/v1/sandboxes/sb-deadbeef-99999
assert_status 404 "GET unknown sandbox id"
assert_body_exact '{"error":"not found"}' "unknown id body exact"
hd GET /api/v1/does-not-exist
assert_status 404 "GET unknown api path"
assert_body_exact '{"error":"not found"}' "unknown path body exact"
hd POST /api/v1/nodes
assert_status 404 "POST /api/v1/nodes (wrong method)"
assert_body_exact '{"error":"not found"}' "wrong method body exact"
