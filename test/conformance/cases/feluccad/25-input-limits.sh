# shellcheck shell=bash
# Input bounds on the caller-supplied strings and the request body itself —
# the parts of the API surface that had neither a length nor a charset.
#
# Cheap by design: every request here is expected to be REJECTED, so nothing
# boots except the one create that proves the bound is inclusive rather than
# off by one.
CF_IL_RUN="$$"

# ── Block 1: request bodies are capped at 1 MiB ─────────────────────────────
# The cap is applied before any route sees the body — including the node-join
# route, which runs ahead of the bearer gate — so an unauthenticated POST
# cannot make feluccad buffer an arbitrary amount of the caller's choosing.
#
# The padding sits in a field feluccad ignores: minus the padding this is a
# VALID create that would return 201. That is what makes the case a real
# differential — it fails against a feluccad with no cap (201 + a live sandbox)
# and passes with one, instead of passing for some unrelated reason.
CF_IL_BIG=$(mktemp)
{
  printf '{"name":"cf-oversize-%s","namespace":"conformance","vcpus":1,"mem_mib":256,"_pad":"' "$CF_IL_RUN"
  head -c 1200000 /dev/zero | tr '\0' 'A'
  printf '"}'
} > "$CF_IL_BIG"
hd_file POST /api/v1/sandboxes "$CF_IL_BIG"
assert_rejected "POST /api/v1/sandboxes with a 1.2 MiB body refused"
rm -f "$CF_IL_BIG"

hd GET /api/v1/sandboxes
assert_status 200 "GET /api/v1/sandboxes after the oversized create"
assert_jq "[.sandboxes[] | select(.name == \"cf-oversize-$CF_IL_RUN\")] | length == 0" \
  "oversized create left no sandbox behind"
# Self-cleaning even in the failure case: if the cap ever regresses, the sandbox
# the assertion above just reported must not be left booted on a worker.
for cf_il_leftover in $(printf '%s' "$R_BODY" | jq -r ".sandboxes[] | select(.name == \"cf-oversize-$CF_IL_RUN\") | .id"); do
  hd DELETE "/api/v1/sandboxes/$cf_il_leftover"
done

# ── Block 2: sandbox names are bounded and are not markup ───────────────────
# These strings are rendered by the operator console and stored in the fleet's
# records. 1..64 bytes of printable ASCII, and the HTML-significant bytes are
# refused outright — a sandbox name is not a place that ever needs them, and
# refusing them at the edge is what keeps every consumer from having to be
# perfect at escaping.
hd POST /api/v1/sandboxes '{"name":"<script>alert(1)</script>","namespace":"conformance"}'
assert_status 400 "name with HTML metacharacters -> 400"
assert_body_exact '{"error":"invalid name"}' "invalid-name body exact"
for cf_il_name in 'cf-a<b' 'cf-a>b' 'cf-a&b' 'cf-a\"b' "cf-a'b"; do
  hd POST /api/v1/sandboxes "{\"name\":\"$cf_il_name\",\"namespace\":\"conformance\"}"
  assert_status 400 "name containing $cf_il_name -> 400"
done
# Control bytes: a newline in a name breaks every line-oriented consumer that
# ever reads it back (logs, metrics label values, the console's own rows).
hd POST /api/v1/sandboxes '{"name":"cf-a\nb","namespace":"conformance"}'
assert_status 400 "name with an embedded newline -> 400"
hd POST /api/v1/sandboxes '{"name":"cf-a\u0000b","namespace":"conformance"}'
assert_status 400 "name with an embedded NUL -> 400"

hd POST /api/v1/sandboxes '{"name":"","namespace":"conformance"}'
assert_status 400 "empty name -> 400"
assert_body_exact '{"error":"name required"}' "empty-name body exact"

CF_IL_65=$(head -c 65 /dev/zero | tr '\0' 'n')
hd POST /api/v1/sandboxes "{\"name\":\"$CF_IL_65\",\"namespace\":\"conformance\"}"
assert_status 400 "65-character name -> 400"

# ── Block 3: namespaces carry the same bounds ───────────────────────────────
# The namespace is the other free-form string on create, and it reaches the
# same consumers.
hd POST /api/v1/sandboxes '{"name":"cf-ns-probe","namespace":"<script>"}'
assert_status 400 "namespace with HTML metacharacters -> 400"
assert_body_exact '{"error":"invalid namespace"}' "invalid-namespace body exact"
CF_IL_NS65=$(head -c 65 /dev/zero | tr '\0' 's')
hd POST /api/v1/sandboxes "{\"name\":\"cf-ns-probe\",\"namespace\":\"$CF_IL_NS65\"}"
assert_status 400 "65-character namespace -> 400"

# ── Block 4: the bound is inclusive, not off by one ─────────────────────────
# A charset check that also rejected legitimate names would be a different
# defect wearing the same clothes, so the accepted boundary is asserted too.
# This is the only sandbox this case boots; it is deleted immediately.
CF_IL_64=$(head -c 64 /dev/zero | tr '\0' 'y')
CF_IL_SB=""
hd POST /api/v1/sandboxes "{\"name\":\"$CF_IL_64\",\"namespace\":\"conformance\",\"vcpus\":1,\"mem_mib\":256}"
assert_status 201 "64-character name accepted (upper bound inclusive)"
CF_IL_SB=$(printf '%s' "$R_BODY" | jq -r '.id // empty')
if [ -n "$CF_IL_SB" ]; then
  hd DELETE "/api/v1/sandboxes/$CF_IL_SB"
  assert_status 204 "cleanup: delete the boundary-name sandbox"
fi

# ── Block 5: feluccad came through all of it ─────────────────────────────────
hd GET /healthz "" none
assert_status 200 "feluccad still healthy after the input-bound cases"
assert_body_exact '{"ok":true}' "healthz body still exact"
