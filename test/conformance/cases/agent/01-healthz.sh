# shellcheck shell=bash
# Agent /healthz is open, and it now reports one thing beyond liveness:
# `isolation` says whether this node's guest network fences are actually in
# force — the guest input filter, the L2 fence on the bridge, IPv6 off on it,
# AND the cross-tenant isolation transaction. It is a single boolean over the
# whole nft ruleset, so it is the cheapest end-to-end assertion the suite has
# that a worker is not quietly serving every tenant on one flat network.
#
# It is deliberately NOT folded into `ok` (the agent refuses to start without
# the fences, and a health check that flipped at runtime would restart-loop the
# unit), so both fields are asserted separately: `ok` stays the process-is-up
# signal the rollout scripts poll.
ag GET /healthz "" none
assert_status 200 "GET agent /healthz"
assert_jq '.ok == true' "agent healthz ok:true"
assert_jq '.isolation | type == "boolean"' "agent healthz carries a boolean isolation flag"
assert_jq '.isolation == true' "guest network fences are in force on this node"
