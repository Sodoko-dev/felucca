# shellcheck shell=bash
# Unmatched ids, paths, and methods are all 404 with the exact body
# (the contract has no 405: wrong method on a known path is also 404).
hd GET /api/v1/sandboxes/sb-cf0000000000000000deadbeef
assert_status 404 "GET unknown sandbox id"
assert_body_exact '{"error":"not found"}' "unknown id body exact"
hd GET /api/v1/does-not-exist
assert_status 404 "GET unknown api path"
assert_body_exact '{"error":"not found"}' "unknown path body exact"
hd POST /api/v1/nodes
assert_status 404 "POST /api/v1/nodes (wrong method)"
assert_body_exact '{"error":"not found"}' "wrong method body exact"

# A path-traversal id is just another unmatched id: hearthd's route matcher
# refuses an id containing a separator, so the decoded "../../etc/hearth"
# matches no route and gets the same 404 as any other unknown id — it is never
# forwarded to a worker, where it would be a filesystem path. Percent-encoded
# so curl does not collapse it before it is sent.
hd GET /api/v1/sandboxes/%2e%2e%2f%2e%2e%2fetc%2fhearth
assert_status 404 "GET traversal-shaped sandbox id"
assert_body_exact '{"error":"not found"}' "traversal id body exact"
hd POST /api/v1/sandboxes/%2e%2e%2f%2e%2e%2fetc%2fhearth/exec '{"cmd":["true"]}'
assert_status 404 "exec on traversal-shaped sandbox id"
