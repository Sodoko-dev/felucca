#!/usr/bin/env bash
# Lab gate for a worker reboot, and for what guests can reach while the host
# shuts down (FLU-62 / ADR-0011). Run from the macOS host:
#
#   bash scripts/verify-reboot.sh [worker-vm] [token]         # default worker: kata-lab-1
#   WITH_DISTRO_NFTABLES=1 bash scripts/verify-reboot.sh ...  # also the case the fix is for
#
# Starts a running, a sleeping and a second running sandbox, and a probe
# inside one guest that logs to its own disk whether it can reach the other
# guest and the host's agent port. Then the worker VM is shut down cleanly and
# started again. Pass: the units come back by themselves, the guest fence is
# loaded, the pool has no dead record, what ran is `stopped` and cold-starts
# with its disk intact, what slept wakes, and the probe never got through.
#
# Sandboxes outlive the agent unit (KillMode=process), so at shutdown they are
# only gone when something ends them. felucca-firewall.service does that, and
# it has to happen before any unit unloads firewall rules. The distribution's
# nftables.service (disabled by default on Ubuntu) flushes the whole ruleset
# when it stops: with WITH_DISTRO_NFTABLES=1 that unit is enabled for the run
# (one extra reboot to make it active from boot) and disabled again at the
# end, and the journal of the shutdown must show the first Firecracker's tap
# released BEFORE "Stopping nftables.service".
#
# With that unit enabled, `limactl start` on these lab VMs waits ten minutes
# for a "running" event that never comes although the VM answers after about
# a minute, so this script starts the VM in the background and waits for the
# agent instead. Takes about 6 minutes (10 with WITH_DISTRO_NFTABLES=1).
set -u

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORKER="${1:-${FELUCCA_LAB_WORKER:-kata-lab-1}}"
TOKEN_ARG="${2:-}"
FAIL=0
bad() { FAIL=$((FAIL+1)); printf '  FAIL: %s\n' "$*"; }
ok()  { printf '  PASS: %s\n' "$*"; }
sh_vm() { limactl shell --workdir /tmp "$WORKER" -- sudo sh -c "$1"; }
reboot_vm() {
  local t0 i=0
  limactl stop "$WORKER" 2>&1 | tail -n 1
  t0="$(date +%s)"
  ( limactl start "$WORKER" >/dev/null 2>&1 & )
  until [ "$(limactl shell --workdir /tmp "$WORKER" -- sudo systemctl is-active felucca-agent 2>/dev/null | tr -d '\r\n')" = active ] || [ $i -ge 72 ]; do sleep 5; i=$((i+1)); done
  if [ $i -ge 72 ]; then bad "felucca-agent on $WORKER is not active $(( $(date +%s) - t0 )) s after the VM was started"
  else echo "  felucca-agent on $WORKER is active again $(( $(date +%s) - t0 )) s after the VM was started"; fi
}
gate() { bash "$REPO/test/lab/run-on-worker.sh" "$1" "$WORKER" "$TOKEN_ARG" || FAIL=$((FAIL+1)); }

ENABLED_HERE=0
if [ "${WITH_DISTRO_NFTABLES:-0}" = 1 ]; then
  echo "== 0. the distribution's nftables.service on $WORKER =="
  if [ "$(sh_vm 'systemctl is-active nftables' | tr -d '\r\n')" = active ]; then
    echo "  already active (is-enabled: $(sh_vm 'systemctl is-enabled nftables 2>&1' | tr -d '\r\n')); left as found"
  else
    sh_vm 'systemctl enable nftables 2>&1 | tail -n 1'; ENABLED_HERE=1
    reboot_vm
  fi
  [ "$(sh_vm 'systemctl is-active nftables' | tr -d '\r\n')" = active ] && ok "nftables.service is active from boot" || bad "nftables.service is not active"
fi

echo "== 1. sandboxes and the probe =="
gate reboot-pre
echo "== 2. reboot =="
reboot_vm
echo "== 3. after the reboot =="
gate reboot-post

echo "== 4. the order things stopped in =="
# The previous boot's journal, from the moment the agent's unit was stopped: the
# agent stops, then the firewall unit, then the taps go, then (where it is
# active) nftables.service, then systemd's final kill pass.
ORDER="$(sh_vm 'journalctl -b -1 --no-pager -o short-precise | grep -E "Stopping felucca-agent|felucca-agent.service: Unit process .* remains running after unit stopped|Stopping felucca-firewall|Stopping nftables|felucca0: port .* entered disabled state|Sending SIGTERM to remaining" | cut -c1-170' | awk '/Stopping felucca-agent/ {n=0} {buf[n++]=$0} END {for (i=0; i<n; i++) print buf[i]}')"
printf '%s\n' "$ORDER" | sed 's/^/  | /'
# Line numbers within that excerpt.
n_of() { printf '%s\n' "$ORDER" | grep -n "$1" | tail -n 1 | cut -d: -f1; }
first_after() { printf '%s\n' "$ORDER" | awk -v from="$1" -v pat="$2" 'NR>from && $0 ~ pat {print NR; exit}'; }
A="$(n_of 'Stopping felucca-agent')"; F="$(n_of 'Stopping felucca-firewall')"; K="$(n_of 'Sending SIGTERM to remaining')"
if [ -n "$A" ] && [ -n "$F" ] && [ "$F" -gt "$A" ]; then ok "the firewall unit was stopped after the agent"; else bad "no 'Stopping felucca-firewall' after the agent stopped (the unit has no shutdown step?)"; fi
T="$( [ -n "$F" ] && first_after "$F" 'entered disabled state' )"
if [ -n "$T" ] && { [ -z "$K" ] || [ "$T" -lt "$K" ]; }; then ok "a Firecracker's tap was released before systemd's final kill pass"; else bad "no tap was released between the firewall unit's stop and the final kill pass"; fi
if [ "${WITH_DISTRO_NFTABLES:-0}" = 1 ]; then
  N="$(n_of 'Stopping nftables')"
  if [ -z "$N" ]; then bad "the journal of that shutdown has no 'Stopping nftables.service' (the unit was not active?)"
  elif [ -n "$T" ] && [ "$T" -lt "$N" ]; then ok "…and before the ruleset was flushed (Stopping nftables.service)"
  else bad "the ruleset was flushed before the sandboxes were ended"; fi
  if [ "$ENABLED_HERE" = 1 ]; then sh_vm 'systemctl disable nftables 2>&1 | tail -n 1'; echo "  nftables.service disabled again (it stays loaded until the next boot; never stop it by hand on a worker: its ExecStop flushes the ruleset)"; fi
  echo "  note: a 'limactl start $WORKER' may still be waiting in the background; it gives up by itself within ten minutes and the VM stays up"
fi

echo
if [ "$FAIL" -eq 0 ]; then echo "RESULT: reboot gate passed"; else echo "RESULT: reboot gate FAILED ($FAIL)"; fi
[ "$FAIL" -eq 0 ]
