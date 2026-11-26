#!/usr/bin/env bash
# FLU-3 lab gate — runs ON a lab worker (see scripts/verify-firewall.sh).
#
# Everything an operator can do to the host firewall must leave the agent and
# every sandbox alone, and a rules file that does not load must change nothing.
# And when the rules are gone anyway — flushed under a running sandbox — the
# agent must get them back by itself, or, if it cannot, keep the guests off
# the network until someone has fixed the rules file.
#
#   firewall.sh            the whole gate
#   firewall.sh loop <n>   only: <n> reloads back to back (the host-side wrapper
#                          probes port 9090 from other VMs while this runs)
#
# Optional: INSTALLER_BUNDLE=<dir with install.sh, systemd/, config/, release/>
# to also run the installer re-run cases.
#
# The gate breaks the rules file and flushes rules on purpose. Whatever it
# changes it puts back when it exits, however it exits; a run that was killed
# outright leaves the worker's rules file in $SAVED, and the next run starts
# by putting that back.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"

FW=/etc/felucca/firewall.nft
FWU=felucca-firewall
SAVED=/var/tmp/felucca-lab-firewall.nft.saved
RULESET_SAVED=/var/tmp/felucca-lab-ruleset.saved
BR=felucca0
FENCE='iifname "felucca0" drop'
RESTART_SEC=5   # RestartSec= of felucca-agent.service

rules()      { sudo nft list chain inet felucca_host input 2>/dev/null; }
rule_count() { rules | grep -c "$1" || true; }
# The port-9090 IPv4 allow-list as the kernel has it, sorted — nft prints set
# elements in its own order, so two listings are compared as sets.
allow4()     { rules | sed -n 's/.*tcp dport 9090 ip saddr != { \(.*\) } drop.*/\1/p' | tr ',' '\n' | tr -d ' ' | sort | tr '\n' ' ' | sed 's/ $//'; }
fw_reload()  { sudo systemctl reload "$FWU" >/dev/null 2>&1; }
restore_file() { [ -f "$SAVED" ] && sudo cp -p "$SAVED" "$FW" && sudo chown root:root "$FW" && sudo chmod 0640 "$FW"; }

# Is the guest bridge administratively up? Not the state column of
# `ip -br link`: that is the operstate, and it reads DOWN for a bridge that is
# up but has no port with a carrier — which is every bridge without a running
# sandbox. Bit 0 of the interface flags is IFF_UP.
bridge_up()    { [ $(( $(cat "/sys/class/net/$BR/flags" 2>/dev/null || echo 0) & 1 )) -eq 1 ]; }
bridge_state() { if [ ! -e "/sys/class/net/$BR" ]; then echo absent; elif bridge_up; then echo up; else echo down; fi; }
# The worker reaches the guest over the bridge: needs the bridge up, the
# guest's tap, and the ICMP rule of the host table for the reply.
ping_guest()   { local ip; ip="$(vm_ip "$1")"; [ -n "$ip" ] && ping -c2 -W1 "$ip" >/dev/null 2>&1; }
# The agent's own tables and per-tap chains, as two short strings to compare.
agent_tables() { sudo nft list tables 2>/dev/null | grep -E '^table (ip|bridge|netdev) felucca$' | sort | tr '\n' ',' | sed 's/,$//'; }
# Tables on this worker that are neither the agent's nor the installer's.
foreign_tables() { sudo nft list tables 2>/dev/null | grep -vE '^table (inet felucca_host|(ip|bridge|netdev) felucca)$' || true; }
# A timestamp for `journalctl --since` that no earlier entry shares.
stamp()        { sleep 1.2; date '+%Y-%m-%d %H:%M:%S'; }
gate_said()    { journal_since "$1" | grep -c 'not starting: the guest fence' || true; }

# Take every rule on the worker away, the way `nft flush ruleset` (by hand, or
# the distribution's nftables.service reloading a config that starts with it)
# does. If the worker carries tables that are not Felucca's, those are left
# alone and Felucca's four are deleted one by one instead: this gate must hand
# the worker back as it found it, and it cannot promise to rebuild rules it
# knows nothing about.
flush_rules() {
  if [ -z "$(foreign_tables)" ]; then
    sudo nft flush ruleset
    say "  (nft flush ruleset)"
  else
    local fam
    sudo nft delete table inet felucca_host 2>/dev/null
    for fam in ip bridge netdev; do sudo nft delete table "$fam" felucca 2>/dev/null; done
    say "  (this worker has other nft tables — $(foreign_tables | tr '\n' ';') — so Felucca's four tables were deleted one by one instead of a flush)"
  fi
}
# Wait for the agent to be active and answering. Prints the seconds it took.
wait_agent() { # <timeout-s>
  local t0=$SECONDS
  while [ $((SECONDS - t0)) -lt "$1" ]; do
    if [ "$(systemctl is-active $UNIT)" = active ] && wait_up 1; then echo $((SECONDS - t0)); return 0; fi
    sleep 0.5
  done
  echo $((SECONDS - t0)); return 1
}

if [ "${1:-}" = loop ]; then
  n="${2:-200}"; failed=0
  for _ in $(seq 1 "$n"); do fw_reload || failed=$((failed+1)); done
  say "reloads=$n failed=$failed"
  [ "$failed" -eq 0 ]; exit $?
fi

# Put the worker back: the rules file it had, loaded; the agent running (a
# start is what raises the bridge again after a refusal).
cleanup() {
  restore_file
  sudo systemctl reload-or-restart "$FWU" >/dev/null 2>&1
  if [ "$(systemctl is-active $UNIT)" != active ]; then
    sudo systemctl restart "$UNIT" >/dev/null 2>&1
    wait_agent 40 >/dev/null
  fi
  sudo rm -f "$SAVED" "$RULESET_SAVED" /tmp/felucca-lab-fw.*
}
trap 'exit 130' INT TERM HUP
if [ -f "$SAVED" ]; then
  say "NOTE: $SAVED is left over from a run that was killed; putting that rules file back first."
  cleanup
fi
trap cleanup EXIT

say "== 0. units as installed =="
preflight_unit
eq "agent Wants= the firewall unit" "$(systemctl show $UNIT -p Wants --value | tr ' ' '\n' | grep -c "^$FWU.service$")" 1
eq "agent does not Require it"      "$(systemctl show $UNIT -p Requires --value | tr ' ' '\n' | grep -c "^$FWU.service$")" 0
eq "agent's start gate has two steps, both looking for the fence in the kernel" "$(systemctl cat $UNIT | grep -c "^ExecStartPre=.*nft list chain inet felucca_host input .*grep -q \"iifname .felucca0. drop\"")" 2
eq "…step 1 asks systemd to load the rules again, best effort" "$(systemctl cat $UNIT | grep -c "^ExecStartPre=-+.*|| systemctl reload-or-restart $FWU.service'\$")" 1
eq "…step 2 takes the guest bridge down and fails the start" "$(systemctl cat $UNIT | grep -c "^ExecStartPre=/.*&& exit 0; ip link set dev $BR down .*exit 1'\$")" 1
eq "firewall unit hands its script to nft (StandardInput=)" "$(systemctl show $FWU -p StandardInput --value)" data
eq "firewall unit's only ExecStop acts when the host is shutting down, and only then" "$(systemctl cat $FWU | grep '^ExecStop' | grep -c ' = stopping || exit 0; exec systemctl kill --kill-whom=all --signal=SIGKILL felucca-agent.service')/$(systemctl cat $FWU | grep -c '^ExecStop')" 1/1
eq "firewall unit active" "$(systemctl is-active $FWU)" active
eq "the guest fence is loaded" "$(rule_count "$FENCE")" 1
sudo cp -p "$FW" "$SAVED"
ALLOW0="$(allow4)"
say "  port 9090 is open to: ${ALLOW0}"

say "== 1. a sandbox to keep an eye on =="
cleanup_prefix fw-
eq "create fw-a" "$(vm_create fw-a 256)" 201
wait_exec fw-a 120 || bad "fw-a never became exec-ready"
AP0=$(agent_pid); FC0="$(fcs)"; UP0=$(guest_uptime fw-a); PA0="$(vm_pid fw-a)"
TABLES0="$(agent_tables)"; CHAINS0="$(chain_count)"
eq "guest bridge is up" "$(bridge_state)" up
check "the worker reaches fw-a over the bridge (ping)" ping_guest fw-a
undisturbed() { eq "$1: agent main pid unchanged" "$(agent_pid)" "$AP0"; eq "$1: every Firecracker unchanged" "$(fcs)" "$FC0"; }
# After a case that restarted the agent on purpose: every sandbox is still the
# one it was, and the node is whole again.
whole_again() { # <label>
  eq "$1: every Firecracker kept its pid" "$(fcs)" "$FC0"
  eq "$1: fw-a adopted, running, same Firecracker" "$(vm_state fw-a) $(vm_pid fw-a)" "running $PA0"
  eq "$1: guest fence loaded once" "$(rule_count "$FENCE")" 1
  eq "$1: port 9090 allow-list is the worker's own" "$(allow4)" "$ALLOW0"
  eq "$1: the agent's own tables are back" "$(agent_tables)" "$TABLES0"
  eq "$1: one netdev chain per tap, as before" "$(chain_count)" "$CHAINS0"
  eq "$1: guest bridge is up" "$(bridge_state)" up
  local i=0; until ping_guest fw-a || [ $i -ge 10 ]; do sleep 1; i=$((i+1)); done
  check "$1: the worker reaches fw-a over the bridge again" ping_guest fw-a
  check "$1: exec works on fw-a" exec_ok fw-a
  local up; up=$(guest_uptime fw-a)
  [ -n "$up" ] && [ "$up" -gt "$UP0" ] && ok "$1: fw-a was never rebooted (guest uptime ${UP0}s -> ${up}s)" || bad "$1: fw-a uptime ${UP0}s -> '${up}'"
  AP0=$(agent_pid)
}

say "== 2. reload and restart =="
for i in 1 2 3; do fw_reload || bad "reload $i failed"; done
eq "three reloads: each rule is loaded once" "$(rule_count "$FENCE")" 1
eq "three reloads: allow-list unchanged" "$(allow4)" "$ALLOW0"
undisturbed "after reloads"
sudo systemctl restart "$FWU"; eq "restart exit code" "$?" 0
eq "restart: each rule is loaded once" "$(rule_count "$FENCE")" 1
undisturbed "after a firewall restart"
check "exec still works" exec_ok fw-a

say "== 3. an edit is applied by reload, and only by reload =="
sudo sed -i 's|^        iifname "felucca0" drop$|        iifname "felucca0" drop\n        tcp dport 65001 drop|' "$FW"
eq "edited file not applied before reload" "$(rule_count 'dport 65001')" 0
fw_reload; eq "reload applies the edit" "$(rule_count 'dport 65001')" 1
restore_file; fw_reload; eq "reload removes it again" "$(rule_count 'dport 65001')" 0

say "== 4. rules files that must be refused, leaving the old rules in force =="
refused() { # <label> — the file in place must not load
  if fw_reload; then bad "$1: reload reported success"; else ok "$1: reload fails"; fi
  eq "$1: firewall unit still active" "$(systemctl is-active $FWU)" active
  eq "$1: guest drop still loaded" "$(rule_count "$FENCE")" 1
  eq "$1: allow-list still the old one" "$(allow4)" "$ALLOW0"
}
echo 'table inet felucca_host { chain input { BROKEN' | sudo tee "$FW" >/dev/null; refused "syntax error"
: | sudo tee "$FW" >/dev/null;                                                    refused "empty file"
echo '# nothing but a comment' | sudo tee "$FW" >/dev/null;                       refused "comment-only file"
printf 'table inet felucca_host {\n    chain other {\n        type filter hook input priority filter; policy accept;\n    }\n}\n' | sudo tee "$FW" >/dev/null
refused "table with no input chain"
undisturbed "after four refused reloads"

say "== 5. restart with a broken file: unit fails, rules and agent stay =="
echo 'table inet felucca_host { chain input { BROKEN' | sudo tee "$FW" >/dev/null
if sudo systemctl restart "$FWU" >/dev/null 2>&1; then bad "restart with a broken file reported success"; else ok "restart with a broken file fails"; fi
eq "old rules still loaded" "$(rule_count "$FENCE")" 1
eq "allow-list still the old one" "$(allow4)" "$ALLOW0"
eq "agent still running" "$(systemctl is-active $UNIT)" active
undisturbed "after a failed firewall restart"
restore_file
sudo systemctl restart "$FWU"; eq "restart with the good file" "$(systemctl is-active $FWU)" active

say "== 6. stopping the unit leaves the rules loaded =="
sudo systemctl stop "$FWU"
eq "unit inactive" "$(systemctl is-active $FWU)" inactive
eq "guest drop still loaded" "$(rule_count "$FENCE")" 1
undisturbed "after a firewall stop"
if fw_reload; then bad "reload of a stopped unit reported success"; else ok "reload of a stopped unit is refused (start it instead)"; fi
sudo systemctl start "$FWU"; eq "start again" "$(systemctl is-active $FWU)" active

say "== 7. every rule flushed under a running sandbox: a restart of the agent brings all of it back =="
# The firewall unit stays "active (exited)" through a flush, so nothing but the
# agent's own start gate will load the rules again.
flush_rules
eq "flushed: the guest fence is gone" "$(rule_count "$FENCE")" 0
eq "flushed: the agent's tables are gone" "$(agent_tables)" ""
eq "flushed: the firewall unit still says active" "$(systemctl is-active $FWU)" active
eq "flushed: the agent is still running, unaware" "$(agent_pid)" "$AP0"
T0=$(stamp)
sudo systemctl restart "$UNIT"; eq "systemctl restart $UNIT: exit code" "$?" 0
secs="$(wait_agent 30)" && ok "agent active and answering after ${secs}s, with no reload or start by hand" || bad "agent not back after ${secs}s ($(systemctl is-active $UNIT))"
eq "the gate never had to refuse" "$(gate_said "$T0")" 0
whole_again "after a flush"

say "== 8. flushed AND the rules file does not load: no agent, no guest network, no sandbox lost =="
echo 'table inet felucca_host { chain input { BROKEN' | sudo tee "$FW" >/dev/null
flush_rules
T0=$(stamp)
if sudo systemctl restart "$UNIT" >/dev/null 2>&1; then bad "systemctl restart reported success with no fence to be had"; else ok "systemctl restart $UNIT fails"; fi
sleep 2
case "$(systemctl is-active $UNIT)" in active) bad "agent started with no guest fence";; *) ok "agent is not running ($(systemctl is-active $UNIT) / $(systemctl show $UNIT -p SubState --value))";; esac
eq "guest fence not loaded" "$(rule_count "$FENCE")" 0
eq "guest bridge was taken down" "$(bridge_state)" down
eq "every Firecracker is still running" "$(fcs)" "$FC0"
if alive "$PA0"; then ok "fw-a's Firecracker (pid $PA0) is alive"; else bad "fw-a's Firecracker is gone"; fi
if ping_guest fw-a; then bad "fw-a still answers on a bridge nothing fences"; else ok "fw-a is off the network while it cannot be fenced"; fi
[ "$(gate_said "$T0")" -ge 1 ] && ok "journal says why (the gate's own line)" || bad "the gate's message is not in the journal"
sleep $((RESTART_SEC + 2))
case "$(systemctl is-active $UNIT)" in active) bad "agent came up on a later retry";; *) ok "still refusing after another retry period (NRestarts=$(systemctl show $UNIT -p NRestarts --value), never 'failed': $(systemctl is-failed $UNIT))";; esac
eq "guest bridge is still down" "$(bridge_state)" down
say "  -- the rules file is repaired; nobody reloads or starts anything --"
restore_file
secs="$(wait_agent $((RESTART_SEC * 2 + 10)))" && ok "agent active and answering ${secs}s after the file was repaired" || bad "agent not back ${secs}s after the repair ($(systemctl is-active $UNIT))"
[ "${secs:-99}" -le $((RESTART_SEC * 2 + 5)) ] && ok "…within two retry periods (plus the start itself)" || bad "…took longer than two retry periods: ${secs}s"
whole_again "after the repair"

say "== 9. the rules file loads but has lost the guest fence: the gate refuses =="
sudo sh -c "grep -v 'iifname \"felucca0\" drop' '$SAVED' > '$FW'"
fw_reload; eq "reload of the fence-less file succeeds (it parses)" "$?" 0
eq "guest fence no longer loaded" "$(rule_count "$FENCE")" 0
eq "the running agent does not notice (nothing re-checks at runtime: FLU-8)" "$(agent_pid)" "$AP0"
T0=$(stamp)
if sudo systemctl restart "$UNIT" >/dev/null 2>&1; then bad "systemctl restart reported success without the fence"; else ok "systemctl restart $UNIT fails"; fi
sleep 2
case "$(systemctl is-active $UNIT)" in active) bad "agent started without the guest fence";; *) ok "agent is not running ($(systemctl is-active $UNIT))";; esac
eq "guest bridge was taken down" "$(bridge_state)" down
eq "every Firecracker is still running" "$(fcs)" "$FC0"
[ "$(gate_said "$T0")" -ge 1 ] && ok "journal says why (the gate's own line)" || bad "the gate's message is not in the journal"
journal_since "$T0" | grep -m1 'not starting: the guest fence' | sed 's/^/  | /'
say "  -- the line is put back in the file; nobody reloads or starts anything --"
restore_file
secs="$(wait_agent $((RESTART_SEC * 2 + 10)))" && ok "agent active and answering ${secs}s after the file was restored" || bad "agent not back ${secs}s after the file was restored ($(systemctl is-active $UNIT))"
whole_again "after the fence came back"

say "== 10. with the fence loaded the gate is silent and runs nothing =="
T0=$(stamp)
restart_agent; eq "plain restart: agent active" "$(systemctl is-active $UNIT)" active
eq "the start gate printed no rules into the journal" "$(journal_since "$T0" | grep -cE 'type filter hook input|iifname "felucca0"|tcp dport 9090' || true)" 0
eq "the firewall unit was not run by that restart" "$(sudo journalctl -u "$FWU" --since "$T0" --no-pager -o cat | wc -l | tr -d ' ')" 0
eq "the bridge was never taken down" "$(bridge_state)" up
whole_again "after a plain restart"

say "== 11. re-running the installer =="
B="${INSTALLER_BUNDLE:-}"
if [ -z "$B" ] || [ ! -f "$B/install.sh" ]; then
  skip "installer cases (no INSTALLER_BUNDLE)"
else
  SUM0=$(sudo sha256sum "$FW" | cut -c1-16)
  BIN=/usr/local/bin/felucca-agent; UNITF=/etc/systemd/system/$UNIT.service
  installed() { sudo stat -c '%Y' "$BIN" "$UNITF" | tr '\n' ' '; }
  debris()    { sudo sh -c 'ls -A /etc/felucca' | grep -c '^\.firewall\.nft\.\|^firewall\.nft\.prev$' || true; }
  # An installer older than this gate left its staging file behind whenever it
  # refused an address. Those are not this run's: say so and clear them, so
  # that every count below is about the installer under test.
  if [ "$(debris)" -gt 0 ]; then
    say "  note: $(debris) staging/backup file(s) from an earlier installer were already in /etc/felucca; removed before the cases below"
    sudo sh -c 'rm -f /etc/felucca/.firewall.nft.?????? /etc/felucca/firewall.nft.prev'
  fi
  # (The log is this user's own file in /tmp; only the installer runs as root.)
  # shellcheck disable=SC2024
  run_installer() { sudo env -u FELUCCA_CONTROL_PLANE_IP "$@" bash "$B/install.sh" worker > /tmp/felucca-lab-fw.install 2>&1; }
  run_installer; eq "re-run without the variable: exit code" "$?" 0
  eq "re-run without the variable: rules file untouched" "$(sudo sha256sum "$FW" | cut -c1-16)" "$SUM0"
  eq "re-run without the variable: allow-list unchanged" "$(allow4)" "$ALLOW0"
  undisturbed "after an installer re-run"
  grep -q 'was NOT restarted' /tmp/felucca-lab-fw.install && ok "installer says it left the agent running" || bad "installer did not say so"
  grep -q 'kept as it is' /tmp/felucca-lab-fw.install && ok "installer says it kept the rules file" || bad "installer did not say it kept the rules file"

  # Values that must be refused — and refused before anything is installed:
  # the binary and the unit on disk must not even be rewritten.
  refuse() { # <label> <value>
    local m0; m0="$(installed)"
    sleep 1.1
    run_installer FELUCCA_CONTROL_PLANE_IP="$2"
    [ $? -ne 0 ] && ok "$1 aborts the installer" || bad "installer accepted $1"
    eq "$1: installed binary and unit were not rewritten" "$(installed)" "$m0"
    eq "$1: rules file untouched" "$(sudo sha256sum "$FW" | cut -c1-16)" "$SUM0"
    eq "$1: allow-list unchanged" "$(allow4)" "$ALLOW0"
    eq "$1: no temp or backup file left in /etc/felucca" "$(debris)" 0
  }
  refuse "an nft injection through the variable" '1.2.3.4 } accept; tcp dport 9090 accept #'
  eq "injection attempt: no accept rule for 9090" "$(rule_count 'dport 9090 accept')" 0
  refuse "a hostname" 'cp.internal.example.com'
  refuse "a lone comma (a value that names nobody)" ','
  refuse "loopback only (a value that names nobody)" '127.0.0.1'
  refuse "an address with a /prefix" '192.168.104.1/24'
  undisturbed "after five refused installer runs"

  # Rewrite the allow-list on purpose: the same addresses plus one more, in both families.
  WANT="$(echo "$ALLOW0" | tr ' ' '\n' | grep -v '^127.0.0.1$' | tr '\n' ',' | sed 's/,$//')"
  run_installer FELUCCA_CONTROL_PLANE_IP="${WANT}, 10.99.0.9, fd00::99"; eq "re-run with an address list: exit code" "$?" 0
  eq "the new IPv4 list is in force" "$(allow4)" "$(printf '%s\n10.99.0.9\n' "$(echo "$ALLOW0" | tr ' ' '\n')" | sort | tr '\n' ' ' | sed 's/ $//')"
  eq "the IPv6 rule got its address" "$(rule_count 'ip6 saddr != { ::1, fd00::99 } drop')" 1
  eq "each rule is still loaded once" "$(rule_count "$FENCE")" 1
  eq "rules file owner and mode" "$(sudo stat -c '%U:%G %a' "$FW")" "root:root 640"
  grep -q 'rules before this run' /tmp/felucca-lab-fw.install && ok "installer printed the rules before and after" || bad "installer did not print before/after"
  undisturbed "after rewriting the allow-list"
  eq "no temp or backup file left in /etc/felucca" "$(debris)" 0
fi

say "== 12. back to the rules the worker had =="
restore_file; fw_reload
eq "allow-list restored" "$(allow4)" "$ALLOW0"
eq "guest bridge is up" "$(bridge_state)" up
check "exec still works on fw-a" exec_ok fw-a
cleanup_prefix fw-
summary
