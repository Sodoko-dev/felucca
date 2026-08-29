# shellcheck shell=bash
# Local-only: a candidate hearthd binary must adopt the live state.json —
# same node ids and sandbox ids (agents never re-register, so losing node ids
# strands the fleet). Runs only when CANDIDATE_HEARTHD points at a binary and
# the suite is running on the hearthd host itself.
if [ -z "${CANDIDATE_HEARTHD:-}" ]; then
  skip "CANDIDATE_HEARTHD not set (local-only case)"
elif [ ! -x "${CANDIDATE_HEARTHD}" ]; then
  bad "CANDIDATE_HEARTHD '$CANDIDATE_HEARTHD' is not executable"
else
  cf_state_src="${ADOPT_STATE_PATH:-/var/lib/hearth/state.json}"
  cf_tmp=$(mktemp -d)
  # The redirect running unprivileged is the point, not an oversight: sudo is
  # here for the READ of a root-owned 0600 state.json, while the copy must end
  # up owned by us because the candidate hearthd below runs as this user and
  # rewrites that path (state.Persist writes state.json.tmp and renames over
  # it). ShellCheck's suggested `| sudo tee` would make the copy root-owned and
  # is the wrong fix. mktemp -d is 0700, so the fleet inventory is not exposed
  # while the case runs; the trailing rm -rf removes it either way.
  # shellcheck disable=SC2024
  if ! sudo cat "$cf_state_src" > "$cf_tmp/state.json" 2>/dev/null; then
    bad "cannot read $cf_state_src (set ADOPT_STATE_PATH?)"
  else
    "$CANDIDATE_HEARTHD" --bind 127.0.0.1:8099 --state "$cf_tmp/state.json" \
      --ui-dir /nonexistent --token "${HEARTH_TOKEN:-}" \
      > "$cf_tmp/candidate.log" 2>&1 &
    cf_pid=$!
    cf_up=""
    for _ in $(seq 1 30); do
      if curl -s -m1 http://127.0.0.1:8099/healthz | grep -q '"ok":true'; then cf_up=1; break; fi
      sleep 0.2
    done
    if [ -z "$cf_up" ]; then
      bad "candidate hearthd did not come up on :8099 ($(tail -1 "$cf_tmp/candidate.log" 2>/dev/null))"
    else
      cf_auth=()
      [ -n "${HEARTH_TOKEN:-}" ] && cf_auth=(-H "Authorization: Bearer $HEARTH_TOKEN")
      cf_live_nodes=$(curl -s -m5 "${cf_auth[@]}" "$HEARTH_API/api/v1/nodes" | jq -S '[.nodes[].id] | sort')
      cf_cand_nodes=$(curl -s -m5 "${cf_auth[@]}" http://127.0.0.1:8099/api/v1/nodes | jq -S '[.nodes[].id] | sort')
      cf_live_sbs=$(curl -s -m5 "${cf_auth[@]}" "$HEARTH_API/api/v1/sandboxes" | jq -S '[.sandboxes[].id] | sort')
      cf_cand_sbs=$(curl -s -m5 "${cf_auth[@]}" http://127.0.0.1:8099/api/v1/sandboxes | jq -S '[.sandboxes[].id] | sort')
      [ "$cf_live_nodes" = "$cf_cand_nodes" ] && ok "candidate adopted node ids" \
        || bad "node ids differ: live=$cf_live_nodes candidate=$cf_cand_nodes"
      [ "$cf_live_sbs" = "$cf_cand_sbs" ] && ok "candidate adopted sandbox ids" \
        || bad "sandbox ids differ: live=$cf_live_sbs candidate=$cf_cand_sbs"
    fi
    kill "$cf_pid" 2>/dev/null
    wait "$cf_pid" 2>/dev/null
  fi
  rm -rf "$cf_tmp"
fi
