# GET /v1/vms wraps records in {"vms":[...]} (element shape is goldened in
# case 04 against a VM the suite itself creates).
ag GET /v1/vms
assert_status 200 "GET /v1/vms"
assert_jq '.vms | type == "array"' "vms is an array"
assert_jq 'keys == ["vms"]' "top-level key set is exactly [vms]"
