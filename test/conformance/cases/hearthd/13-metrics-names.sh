# shellcheck shell=bash
# The /metrics scrape is ADMIN-ONLY now (it used to be open). It names every worker in
# hostname labels, reports live fleet counts, and takes the global state lock,
# so an open endpoint is both reconnaissance and a lock-contention lever for
# anyone who can reach the port. Prometheus scrapes it with a bearer_token.
#
# The auth ladder first, then the contract-bound metric names: the state label
# must carry all six sandbox states even at zero.
if [ "${HEARTH_INSECURE_NO_AUTH:-0}" = "1" ]; then
  skip "HEARTH_INSECURE_NO_AUTH=1; /metrics auth ladder needs a credential"
else
  hd GET /metrics "" none
  assert_unauthorized "GET /metrics without token"
  hd GET /metrics "" badtoken
  assert_unauthorized "GET /metrics with wrong token"
fi

hd GET /metrics
assert_status 200 "GET /metrics (admin)"
for s in creating running paused stopped sleeping error; do
  if printf '%s' "$R_BODY" | grep -q "hearth_sandboxes_total{state=\"$s\"}"; then
    ok "state series present: $s"
  else
    bad "missing hearth_sandboxes_total{state=\"$s\"}"
  fi
done
for m in hearth_wake_ms_last hearth_wake_total hearth_wake_ms_sum hearth_forks_total hearth_pool_size; do
  if printf '%s' "$R_BODY" | grep -q "^$m"; then
    ok "metric present: $m"
  else
    bad "missing metric: $m"
  fi
done
check_golden_metrics hearthd/metrics-names
