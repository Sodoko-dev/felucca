# /metrics is open and exposes the contract-bound metric names; the state
# label must carry all six sandbox states even at zero.
hd GET /metrics "" none
assert_status 200 "GET /metrics"
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
