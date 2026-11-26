#!/usr/bin/env bash
# Lab gate for FLU-3 / ADR-0011: firewall changes do not disturb the agent.
# Run from the macOS host:
#
#   bash scripts/verify-firewall.sh [worker-vm] [token]     # default worker: kata-lab-0
#
# Two parts.
#
# 1. From the outside, while the worker reloads its rules 200 times back to
#    back: the control plane (on the allow-list) must reach port 9090 every
#    time, and another lab VM (not on it) must never get through. A reload
#    that had a moment with no rules loaded would show up as the second
#    number not being zero.
# 2. On the worker (test/lab/firewall.sh): reload, restart and stop leave the
#    agent's pid and every Firecracker alone; a rules file that does not load
#    changes nothing. With every rule flushed under a running sandbox, a
#    restart of the agent loads all of it again by itself. With a rules file
#    that cannot give it the guest fence (broken, or the line removed) the
#    agent refuses to start and the guest bridge goes down — the sandbox keeps
#    running — and once the file is right again it comes back with no reload
#    or start by hand.
#
# Optional, to also re-run the installer on the worker the way an upgrade
# does (rules file kept without the variable; values that are refused are
# refused before anything is installed):
#
#   FELUCCA_LAB_AGENT_BIN=/path/to/static/felucca-agent bash scripts/verify-firewall.sh
#
# Other VMs used: FELUCCA_LAB_CP (default infra-saas-lab) and FELUCCA_LAB_OTHER
# (default: the other of kata-lab-0 / kata-lab-1). Takes about 7 minutes. The
# agent on the worker is restarted several times and is down for about half a
# minute in two of the cases; its sandboxes are not touched.
set -u

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORKER="${1:-${FELUCCA_LAB_WORKER:-kata-lab-0}}"
CP_VM="${FELUCCA_LAB_CP:-infra-saas-lab}"
case "$WORKER" in kata-lab-0) DEF_OTHER=kata-lab-1;; *) DEF_OTHER=kata-lab-0;; esac
OTHER="${FELUCCA_LAB_OTHER:-$DEF_OTHER}"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS: %s\n' "$*"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL: %s\n' "$*"; }

echo "== 1. port 9090 from the outside while $WORKER reloads its rules =="
# Lima's user-v2 addresses move across long stops, so both are looked up now.
WORKER_IP="$(limactl shell "$WORKER" -- bash -c 'ip -4 -br addr | grep 192.168.104 | awk "{print \$3}" | cut -d/ -f1' 2>/dev/null | tr -d '\r\n')"
# The address the control plane dials for this node (the overlay address on
# an overlay worker), from its own allow-list on the worker.
CP_SEES="$(limactl shell "$WORKER" -- bash -c 'ip -4 -br addr show wg-felucca 2>/dev/null | awk "{print \$3}" | cut -d/ -f1' 2>/dev/null | tr -d '\r\n')"
CP_TARGET="${CP_SEES:-$WORKER_IP}"
if [ -z "$WORKER_IP" ]; then
  bad "could not find $WORKER's lab address"
else
  probe() { # <vm> <ip> <seconds> -> "ok=N fail=M"
    limactl shell "$1" -- bash -c "ok=0; fail=0; end=\$((SECONDS+$3)); while [ \$SECONDS -lt \$end ]; do if curl -s -m 2 -o /dev/null http://$2:9090/healthz; then ok=\$((ok+1)); else fail=\$((fail+1)); fi; done; echo ok=\$ok fail=\$fail" 2>/dev/null | tr -d '\r'
  }
  if limactl shell "$OTHER" -- ping -c2 -W2 "$WORKER_IP" >/dev/null 2>&1; then
    ok "$OTHER can reach $WORKER ($WORKER_IP) at all (ping)"
  else
    bad "$OTHER cannot ping $WORKER_IP — the 'never gets through' check below would prove nothing"
  fi
  TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
  probe "$CP_VM" "$CP_TARGET" 30 > "$TMP/cp" &
  probe "$OTHER" "$WORKER_IP" 30 > "$TMP/other" &
  sleep 2
  limactl shell "$WORKER" -- mkdir -p /tmp/felucca-lab
  limactl cp "$REPO/test/lab/lib.sh" "$WORKER:/tmp/felucca-lab/lib.sh" >/dev/null
  limactl cp "$REPO/test/lab/firewall.sh" "$WORKER:/tmp/felucca-lab/firewall.sh" >/dev/null
  TOKEN_FILE="${FELUCCA_TOKEN_FILE:-$HOME/.config/felucca/lab-token}"
  TOKEN="${2:-${FELUCCA_TOKEN:-}}"; [ -z "$TOKEN" ] && [ -r "$TOKEN_FILE" ] && TOKEN="$(tr -d " \t\r\n" < "$TOKEN_FILE")"
  LOOP="$(limactl shell "$WORKER" -- env FELUCCA_TOKEN="$TOKEN" bash /tmp/felucca-lab/firewall.sh loop 200 2>/dev/null | tr -d '\r' | tail -1)"
  wait
  [ "$LOOP" = "reloads=200 failed=0" ] && ok "200 reloads, none failed" || bad "reload loop: $LOOP"
  CP_OK="$(sed -n 's/.*ok=\([0-9]*\).*/\1/p' "$TMP/cp")"; CP_FAIL="$(sed -n 's/.*fail=\([0-9]*\).*/\1/p' "$TMP/cp")"
  OT_OK="$(sed -n 's/.*ok=\([0-9]*\).*/\1/p' "$TMP/other")"; OT_FAIL="$(sed -n 's/.*fail=\([0-9]*\).*/\1/p' "$TMP/other")"
  if [ "${CP_OK:-0}" -gt 0 ] && [ "${CP_FAIL:-1}" = 0 ]; then ok "control plane ($CP_VM -> $CP_TARGET:9090): ${CP_OK} requests, 0 failed"; else bad "control plane ($CP_VM -> $CP_TARGET:9090): ok=${CP_OK:-?} fail=${CP_FAIL:-?}"; fi
  if [ "${OT_OK:-1}" = 0 ] && [ "${OT_FAIL:-0}" -gt 0 ]; then ok "not on the allow-list ($OTHER -> $WORKER_IP:9090): ${OT_FAIL} attempts, 0 got through"; else bad "not on the allow-list ($OTHER -> $WORKER_IP:9090): ok=${OT_OK:-?} fail=${OT_FAIL:-?}"; fi
fi

# An installer bundle on the worker, when a binary was given.
BUNDLE=""
if [ -n "${FELUCCA_LAB_AGENT_BIN:-}" ] && [ -f "${FELUCCA_LAB_AGENT_BIN}" ]; then
  STAGE="$(mktemp -d)"; ARCH="$(limactl shell "$WORKER" -- uname -m | tr -d '\r\n')"
  mkdir -p "$STAGE/deploy/release/$ARCH" "$STAGE/deploy/systemd" "$STAGE/deploy/config"
  cp "$REPO/deploy/install.sh" "$STAGE/deploy/"; cp "$REPO"/deploy/systemd/* "$STAGE/deploy/systemd/"; cp "$REPO"/deploy/config/*.json "$STAGE/deploy/config/"
  cp "$FELUCCA_LAB_AGENT_BIN" "$STAGE/deploy/release/$ARCH/felucca-agent"
  limactl shell "$WORKER" -- rm -rf /tmp/felucca-lab-bundle
  limactl cp -r "$STAGE/deploy" "$WORKER:/tmp/felucca-lab-bundle" >/dev/null && BUNDLE=/tmp/felucca-lab-bundle
  rm -rf "$STAGE"
fi

echo "== 2. on the worker =="
INSTALLER_BUNDLE="$BUNDLE" bash "$REPO/test/lab/run-on-worker.sh" firewall "$WORKER" "${2:-}"; RC=$?

echo ""
echo "OUTSIDE PROBES: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] && [ "$RC" -eq 0 ]
