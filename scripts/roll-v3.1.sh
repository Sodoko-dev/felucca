#!/usr/bin/env bash
# v3.1 (vsock exec + fork re-IP) lab rollout — run ONE PHASE at a time from the
# macOS host and verify between phases:
#   bash scripts/roll-v3.1.sh inject        # guest agent into base images (workers)
#   bash scripts/roll-v3.1.sh roll-agents   # new felucca-agent on kata-lab-1 then -0
#   bash scripts/roll-v3.1.sh roll-feluccad  # new feluccad on infra-saas-lab
#   bash scripts/roll-v3.1.sh goldens       # re-record metrics + record new exec goldens
#   bash scripts/roll-v3.1.sh verify        # extended verify-v2 + full conformance
#
# Prereqs: rust/guest + rust/agent + go builds green in the toolchain VM.
set -euo pipefail

PHASE="${1:?phase required: inject|roll-agents|roll-feluccad|goldens|verify}"

# The lab token authenticates a root-privileged agent API, so it is never a
# literal in the repo: TOKEN/FELUCCA_TOKEN from the environment, then a file
# kept outside the tree.
TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
TOKEN="${TOKEN:-${FELUCCA_TOKEN:-}}"
if [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ]; then
  TOKEN="$(tr -d " \t\r\n" < "$TOKEN_FILE")"
fi
if [ -z "$TOKEN" ]; then
  echo "ERROR: no lab token." >&2
  echo "  export TOKEN=..., export FELUCCA_TOKEN=..., or write it to $TOKEN_FILE" >&2
  echo "  (generate one with: openssl rand -hex 32)" >&2
  exit 1
fi
# The phases below splice the token into `bash -c` strings over limactl, so a
# token carrying shell metacharacters would execute rather than authenticate.
case "$TOKEN" in
  *[!A-Za-z0-9._-]*)
    echo "ERROR: lab token has characters outside [A-Za-z0-9._-]; use a hex token." >&2
    exit 1 ;;
esac
REPO="$(cd "$(dirname "$0")/.." && pwd)"
CP_VM=infra-saas-lab
GUEST_BIN_VM='$HOME/.cargo-target/felucca-guest/aarch64-unknown-linux-musl/release/felucca-guest'
AGENT_BIN_VM='$HOME/.cargo-target/felucca-agent/aarch64-unknown-linux-musl/release/felucca-agent'

say() { printf '\n==> %s\n' "$*"; }

inject() {
  say "copy felucca-guest out of the toolchain VM"
  limactl shell $CP_VM -- bash -c "cp $GUEST_BIN_VM /tmp/felucca-guest"
  limactl cp $CP_VM:/tmp/felucca-guest /tmp/felucca-guest
  for vm in kata-lab-0 kata-lab-1; do
    say "inject guest agent into base image on $vm"
    limactl cp /tmp/felucca-guest $vm:/tmp/felucca-guest
    limactl cp "$REPO/infra/guest-agent-install.sh" $vm:/tmp/guest-agent-install.sh
    limactl shell $vm -- sudo env DATA_DIR=/srv/ignis bash /tmp/guest-agent-install.sh /tmp/felucca-guest
    limactl shell $vm -- ls -la /srv/ignis/images/.felucca-guest-v1
  done
}

roll_agents() {
  limactl shell $CP_VM -- bash -c "cp $AGENT_BIN_VM /tmp/felucca-agent-v31"
  limactl cp $CP_VM:/tmp/felucca-agent-v31 /tmp/felucca-agent-v31
  for vm in kata-lab-1 kata-lab-0; do
    say "roll agent on $vm"
    local pool=0
    [ "$vm" = kata-lab-0 ] && pool=1
    limactl cp /tmp/felucca-agent-v31 $vm:/tmp/felucca-agent-v31
    # capture pre-swap view
    limactl shell $vm -- bash -c 'curl -s -m5 -H "Authorization: Bearer '"$TOKEN"'" http://127.0.0.1:9090/v1/vms | jq -S "[.vms[]] | sort_by(.id)" > /tmp/pre-roll-vms.json'
    # isolated kill (pattern must not appear elsewhere in this command)
    limactl shell $vm -- pkill -f '[f]elucca-agent'
    sleep 1
    limactl shell $vm -- bash -c '
      chmod +x /tmp/felucca-agent-v31 && mv /tmp/felucca-agent-v31 /tmp/felucca-agent-rs
      nohup env FELUCCA_TOKEN='"$TOKEN"' /tmp/felucca-agent-rs \
        --control-plane http://192.168.104.3:8080 --data-dir /srv/ignis \
        --net on --net-cidr 10.231.0.0/24 --pool-size '"$pool"' \
        > /tmp/felucca-agent-rs.log 2>&1 < /dev/null &
      disown
      for i in $(seq 1 40); do curl -s -m1 http://127.0.0.1:9090/healthz 2>/dev/null | grep -q ok && break; sleep 0.25; done
      curl -s -m5 -H "Authorization: Bearer '"$TOKEN"'" http://127.0.0.1:9090/v1/vms | jq -S "[.vms[]] | sort_by(.id)" > /tmp/post-roll-vms.json
      diff /tmp/pre-roll-vms.json /tmp/post-roll-vms.json && echo "AGENT VIEW IDENTICAL ON '"$vm"'"'
  done
}

roll_feluccad() {
  say "roll feluccad on $CP_VM"
  limactl shell $CP_VM -- bash -c '
    export PATH=$PATH:/usr/local/go/bin GOCACHE=$HOME/.cache/go-build
    cd '"$REPO"'/go && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /tmp/feluccad-v31 ./cmd/feluccad
    # Back up whichever durable state exists (JSON pre-v4, SQLite from v4 on).
    cp /tmp/felucca/state.json /tmp/felucca/state.json.pre-roll 2>/dev/null || true
    cp /tmp/felucca/felucca.db /tmp/felucca/felucca.db.pre-roll 2>/dev/null || true'
  limactl shell $CP_VM -- pkill -f '[f]eluccad-go'
  sleep 1
  limactl shell $CP_VM -- bash -c '
    mv /tmp/feluccad-v31 ~/bin/feluccad-go
    nohup ~/bin/feluccad-go --state /tmp/felucca/state.json --db /tmp/felucca/felucca.db --ui-dir '"$REPO"'/ui --token '"$TOKEN"' \
      > /tmp/felucca/feluccad-go.log 2>&1 < /dev/null &
    disown
    for i in $(seq 1 50); do curl -s -m1 http://127.0.0.1:8080/healthz 2>/dev/null | grep -q ok && { echo up; break; }; sleep 0.2; done
    sleep 8
    curl -s -m5 -H "Authorization: Bearer '"$TOKEN"'" http://127.0.0.1:8080/api/v1/nodes | jq "[.nodes[] | select(.status==\"ready\")] | length"'
}

goldens() {
  say "re-baseline all goldens against the live v3.1 stack (metrics set changed; exec goldens are new)"
  bash "$REPO/scripts/conformance-lab.sh" "$TOKEN" record
  say "review the diff before committing: git diff test/conformance/goldens"
}

verify() {
  bash "$REPO/scripts/conformance-lab.sh" "$TOKEN"
  bash "$REPO/scripts/verify-v2.sh" "$TOKEN"
}

case "$PHASE" in
  inject) inject ;;
  roll-agents) roll_agents ;;
  roll-feluccad) roll_feluccad ;;
  goldens) goldens ;;
  verify) verify ;;
  *) echo "unknown phase $PHASE" >&2; exit 1 ;;
esac
