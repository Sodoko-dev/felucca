# shellcheck shell=bash
# Agent input-boundary contract. Every VM path on a worker is derived from the
# caller-supplied id ({data_dir}/instances/<id> and every file under it), so an
# id that is not a plain path component has to be refused at the edge: 400
# {"error":"invalid id"}, before the manager builds a path from it. Delete is
# the sharpest case — it recurses over the derived path and does NOT require
# the VM to exist — but the whole surface is checked, because an id that
# escapes on one route is an id that escapes.
#
# SAFETY: the traversal payloads below resolve to paths that do not exist
# (".../instances/../../cf-conformance-no-such-path"), so running this case
# against an agent that has NOT been fixed deletes nothing and reads nothing.
# It reports the hole instead of exploiting it. Never point a traversal case at
# a path that would matter if the check were missing.
#
# Percent-encoded on purpose: `%2e%2e%2f` is what an attacker sends, curl does
# not collapse it the way it collapses a literal `/../`, and axum hands the
# handler the DECODED capture — so this is the exact string the check must
# survive.
CF_SEC_TRAV="%2e%2e%2f%2e%2e%2fcf-conformance-no-such-path"
CF_SEC_DOTDOT="%2e%2e"

# ── Block 1: auth is evaluated BEFORE the id ────────────────────────────────
# Otherwise the agent is an unauthenticated oracle for which ids are well
# formed, and every id-shaped probe is answerable without a credential.
if [ "${FELUCCA_INSECURE_NO_AUTH:-0}" = "1" ]; then
  skip "FELUCCA_INSECURE_NO_AUTH=1; auth-ordering block needs a credential"
else
  ag DELETE "/v1/vms/$CF_SEC_TRAV" "" none
  assert_unauthorized "DELETE traversal id without token -> 401 (auth precedes id validation)"
fi

# ── Block 2: traversal ids are refused on every route that takes one ────────
ag DELETE "/v1/vms/$CF_SEC_TRAV"
assert_status 400 "DELETE traversal id -> 400"
assert_body_exact '{"error":"invalid id"}' "DELETE traversal body exact"

ag POST "/v1/vms/$CF_SEC_TRAV/exec" '{"cmd":["true"],"timeout_ms":2000}'
assert_status 400 "exec on traversal id -> 400"
assert_body_exact '{"error":"invalid id"}' "exec traversal body exact"

ag GET "/v1/vms/$CF_SEC_TRAV/rootfs"
assert_status 400 "rootfs capture of traversal id -> 400"
assert_body_exact '{"error":"invalid id"}' "rootfs traversal body exact"

ag POST "/v1/vms/$CF_SEC_DOTDOT/sleep"
assert_status 400 "sleep on '..' -> 400"
ag POST "/v1/vms/$CF_SEC_DOTDOT/stop"
assert_status 400 "stop on '..' -> 400"

# A create whose BODY carries the traversal: the id is a JSON field here, not a
# path segment, so it reaches the handler without any URL decoding at all.
ag POST /v1/vms '{"id":"../../../etc/felucca","name":"cf-trav","vcpus":1,"mem_mib":256}'
assert_status 400 "create with traversal id in the body -> 400"
assert_body_exact '{"error":"invalid id"}' "create traversal body exact"

# ── Block 3: the id is bounded ──────────────────────────────────────────────
# 65 chars: one past the limit. An unbounded id is a filename an attacker
# chooses the length of.
CF_SEC_LONG=$(head -c 65 /dev/zero | tr '\0' 'a')
ag DELETE "/v1/vms/$CF_SEC_LONG"
assert_status 400 "over-long id (65 chars) -> 400"

# ── Block 4: request bodies are capped ──────────────────────────────────────
# 1 MiB. Without it a single unauthenticated-adjacent POST buffers whatever the
# caller sends into the agent's heap.
CF_SEC_BIG=$(mktemp)
{
  printf '{"id":"sb-cf000000000000000000000007","name":"cf-oversize","vcpus":1,"mem_mib":256,"_pad":"'
  head -c 1200000 /dev/zero | tr '\0' 'A'
  printf '"}'
} > "$CF_SEC_BIG"
ag_file POST /v1/vms "$CF_SEC_BIG"
assert_rejected "create with a 1.2 MiB body refused"
rm -f "$CF_SEC_BIG"

# The padding sits in a field the agent ignores, so this exact request minus
# the padding is a valid create: proving no VM appeared is what separates "the
# body was refused" from "the body was rejected for some other reason".
ag GET /v1/vms
assert_status 200 "GET /v1/vms after the oversized create"
assert_jq '[.vms[] | select(.name == "cf-oversize")] | length == 0' \
  "oversized create left no VM behind"
# Self-cleaning even in the failure case: if the cap ever regresses, the VM the
# assertion above just reported must not be left booted on the node.
for cf_sec_leftover in $(printf '%s' "$R_BODY" | jq -r '.vms[] | select(.name == "cf-oversize") | .id'); do
  ag DELETE "/v1/vms/$cf_sec_leftover"
done

# ── Block 5: the agent survived all of it ───────────────────────────────────
ag GET /healthz "" none
assert_status 200 "agent still healthy after the boundary cases"
assert_jq '.ok == true and .isolation == true' "agent still isolated after the boundary cases"
