# shellcheck shell=bash
# A correct token is accepted.
hd GET /api/v1/nodes
assert_status 200 "GET /api/v1/nodes with token"
