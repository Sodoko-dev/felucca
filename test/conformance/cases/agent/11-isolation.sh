# v4 P1 cross-tenant network isolation: tenant_id in POST /v1/vms (flows to
# meta.json) feeds the nft tenant-pair set rebuilt on create/fork/delete.
# Guests of different tenants on one node must not reach each other,
# same-tenant guests must, and egress (bridge gateway, the CIDR's .1) must
# still work. No golden: ping output is host/timing dependent; assertions are
# on exit_code only.
CF_ISO_A="sb-cf000004-90004"
CF_ISO_B="sb-cf000005-90005"
CF_ISO_C="sb-cf000006-90006"
# Self-cleaning entries (see 04-create.sh).
ag DELETE "/v1/vms/$CF_ISO_A"
ag DELETE "/v1/vms/$CF_ISO_B"
ag DELETE "/v1/vms/$CF_ISO_C"

ag POST /v1/vms "{\"id\":\"$CF_ISO_A\",\"name\":\"cf-iso-a\",\"vcpus\":1,\"mem_mib\":256,\"tenant_id\":\"cf-tenant-a\"}"
assert_status 201 "POST /v1/vms (tenant cf-tenant-a)"
CF_ISO_A_IP=$(printf '%s' "$R_BODY" | jq -r '.ip // empty')
ag POST /v1/vms "{\"id\":\"$CF_ISO_B\",\"name\":\"cf-iso-b\",\"vcpus\":1,\"mem_mib\":256,\"tenant_id\":\"cf-tenant-b\"}"
assert_status 201 "POST /v1/vms (tenant cf-tenant-b)"
CF_ISO_B_IP=$(printf '%s' "$R_BODY" | jq -r '.ip // empty')

if [ -z "$CF_ISO_A_IP" ] || [ -z "$CF_ISO_B_IP" ]; then
  bad "isolation-case VM creates did not return guest IPs (A='$CF_ISO_A_IP' B='$CF_ISO_B_IP'); skipping isolation checks"
else
  wait_guest_ready ag "$CF_ISO_A"
  wait_guest_ready ag "$CF_ISO_B"

  # Cross-tenant: A (cf-tenant-a) -> B (cf-tenant-b) must be dropped.
  # -W2 bounds the wait so the dropped packet can't hang the suite.
  ag POST "/v1/vms/$CF_ISO_A/exec" "{\"cmd\":[\"/bin/sh\",\"-c\",\"ping -c1 -W2 $CF_ISO_B_IP\"],\"timeout_ms\":15000}"
  assert_status 200 "POST /v1/vms/{id}/exec (cross-tenant ping)"
  assert_jq '.ok == true and .exit_code != 0' "cross-tenant ping A->B dropped"

  # Same-tenant: C joins cf-tenant-a; A -> C must be allowed.
  ag POST /v1/vms "{\"id\":\"$CF_ISO_C\",\"name\":\"cf-iso-c\",\"vcpus\":1,\"mem_mib\":256,\"tenant_id\":\"cf-tenant-a\"}"
  assert_status 201 "POST /v1/vms (second VM in cf-tenant-a)"
  CF_ISO_C_IP=$(printf '%s' "$R_BODY" | jq -r '.ip // empty')
  if [ -z "$CF_ISO_C_IP" ]; then
    bad "isolation-case VM C has no guest IP; skipping same-tenant check"
  else
    wait_guest_ready ag "$CF_ISO_C"
    ag POST "/v1/vms/$CF_ISO_A/exec" "{\"cmd\":[\"/bin/sh\",\"-c\",\"ping -c1 -W2 $CF_ISO_C_IP\"],\"timeout_ms\":15000}"
    assert_status 200 "POST /v1/vms/{id}/exec (same-tenant ping)"
    assert_jq '.ok == true and .exit_code == 0' "same-tenant ping A->C allowed"
  fi

  # Egress: the bridge gateway is the CIDR's .1 (input path, not forward), so
  # it must stay reachable regardless of tenant. Derived from A's own IP to
  # stay config-agnostic; 8.8.8.8 would also test the lab's upstream NAT.
  CF_ISO_GW="${CF_ISO_A_IP%.*}.1"
  ag POST "/v1/vms/$CF_ISO_A/exec" "{\"cmd\":[\"/bin/sh\",\"-c\",\"ping -c1 -W2 $CF_ISO_GW\"],\"timeout_ms\":15000}"
  assert_status 200 "POST /v1/vms/{id}/exec (egress ping)"
  assert_jq '.ok == true and .exit_code == 0' "egress ping A->gateway allowed"
fi

# Cleanup always runs; DELETE is idempotent (204 either way).
ag DELETE "/v1/vms/$CF_ISO_A"
assert_status 204 "isolation-case VM A cleaned up"
ag DELETE "/v1/vms/$CF_ISO_B"
assert_status 204 "isolation-case VM B cleaned up"
ag DELETE "/v1/vms/$CF_ISO_C"
assert_status 204 "isolation-case VM C cleaned up"
