#!/usr/bin/env bash
# FLU-3 lab gate — runs ON a lab worker (see scripts/verify-firewall.sh).
#
# Everything an operator can do to the host firewall must leave the agent and
# every sandbox alone, and a rules file that does not load must change nothing.
#
#   firewall.sh            the whole gate
#   firewall.sh loop <n>   only: <n> reloads back to back (the host-side wrapper
#                          probes port 9090 from other VMs while this runs)
#
# Optional: INSTALLER_BUNDLE=<dir with install.sh, systemd/, config/, release/>
# to also run the installer re-run cases.
set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "${HERE}/lib.sh"

FW=/etc/felucca/firewall.nft
FWU=felucca-firewall
SAVED=/var/tmp/felucca-lab-firewall.nft.saved

rules()      { sudo nft list chain inet felucca_host input 2>/dev/null; }
rule_count() { rules | grep -c "$1" || true; }
# The port-9090 IPv4 allow-list as the kernel has it, sorted — nft prints set
# elements in its own order, so two listings are compared as sets.
allow4()     { rules | sed -n 's/.*tcp dport 9090 ip saddr != { \(.*\) } drop.*/\1/p' | tr ',' '\n' | tr -d ' ' | sort | tr '\n' ' ' | sed 's/ $//'; }
fw_reload()  { sudo systemctl reload "$FWU" >/dev/null 2>&1; }
restore_file() { [ -f "$SAVED" ] && sudo cp -p "$SAVED" "$FW" && sudo chown root:root "$FW" && sudo chmod 0640 "$FW"; }

if [ "${1:-}" = loop ]; then
  n="${2:-200}"; failed=0
  for _ in $(seq 1 "$n"); do fw_reload || failed=$((failed+1)); done
  say "reloads=$n failed=$failed"
  [ "$failed" -eq 0 ]; exit $?
fi

trap 'restore_file; fw_reload; rm -f /tmp/felucca-lab-fw.*' EXIT

say "== 0. units as installed =="
preflight_unit
eq "agent Wants= the firewall unit" "$(systemctl show $UNIT -p Wants --value | tr ' ' '\n' | grep -c "^$FWU.service$")" 1
eq "agent does not Require it"      "$(systemctl show $UNIT -p Requires --value | tr ' ' '\n' | grep -c "^$FWU.service$")" 0
eq "agent checks the table before it starts" "$(systemctl cat $UNIT | grep -c '^ExecStartPre=.*nft list chain inet felucca_host input')" 1
eq "firewall unit has no ExecStop" "$(systemctl cat $FWU | grep -c '^ExecStop')" 0
eq "firewall unit active" "$(systemctl is-active $FWU)" active
sudo cp -p "$FW" "$SAVED"
ALLOW0="$(allow4)"
say "  port 9090 is open to: ${ALLOW0}"

say "== 1. a sandbox to keep an eye on =="
cleanup_prefix fw-
eq "create fw-a" "$(vm_create fw-a 256)" 201
wait_exec fw-a 120 || bad "fw-a never became exec-ready"
AP0=$(agent_pid); FC0="$(fcs)"; UP0=$(guest_uptime fw-a)
undisturbed() { eq "$1: agent main pid unchanged" "$(agent_pid)" "$AP0"; eq "$1: every Firecracker unchanged" "$(fcs)" "$FC0"; }

say "== 2. reload and restart =="
for i in 1 2 3; do fw_reload || bad "reload $i failed"; done
eq "three reloads: each rule is loaded once" "$(rule_count 'iifname "felucca0" drop')" 1
eq "three reloads: allow-list unchanged" "$(allow4)" "$ALLOW0"
undisturbed "after reloads"
sudo systemctl restart "$FWU"; eq "restart exit code" "$?" 0
eq "restart: each rule is loaded once" "$(rule_count 'iifname "felucca0" drop')" 1
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
  eq "$1: guest drop still loaded" "$(rule_count 'iifname "felucca0" drop')" 1
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
eq "old rules still loaded" "$(rule_count 'iifname "felucca0" drop')" 1
eq "allow-list still the old one" "$(allow4)" "$ALLOW0"
eq "agent still running" "$(systemctl is-active $UNIT)" active
undisturbed "after a failed firewall restart"
restore_file
sudo systemctl restart "$FWU"; eq "restart with the good file" "$(systemctl is-active $FWU)" active

say "== 6. stopping the unit leaves the rules loaded =="
sudo systemctl stop "$FWU"
eq "unit inactive" "$(systemctl is-active $FWU)" inactive
eq "guest drop still loaded" "$(rule_count 'iifname "felucca0" drop')" 1
undisturbed "after a firewall stop"
if fw_reload; then bad "reload of a stopped unit reported success"; else ok "reload of a stopped unit is refused (start it instead)"; fi
sudo systemctl start "$FWU"; eq "start again" "$(systemctl is-active $FWU)" active

say "== 7. table gone: the agent refuses to start, sandboxes keep running, and it heals by itself =="
sudo nft delete table inet felucca_host
T0=$(date '+%Y-%m-%d %H:%M:%S')
sudo systemctl restart "$UNIT" >/dev/null 2>&1
sleep 3
case "$(systemctl is-active $UNIT)" in active) bad "agent started with no host firewall table";; *) ok "agent refuses to start with no host firewall table ($(systemctl is-active $UNIT), $(systemctl show $UNIT -p SubState --value))";; esac
eq "every Firecracker still running" "$(fcs)" "$FC0"
sudo journalctl -u "$UNIT" --since "$T0" --no-pager -o cat | grep -q 'No such file or directory' && ok "journal shows the failed table check" || bad "journal does not show the failed check"
fw_reload; eq "reload puts the table back" "$(rule_count 'iifname "felucca0" drop')" 1
i=0; until [ "$(systemctl is-active $UNIT)" = active ] && wait_up 2 || [ $i -ge 15 ]; do sleep 1; i=$((i+1)); done
eq "agent came back by itself (no manual start)" "$(systemctl is-active $UNIT)" active
eq "every Firecracker survived" "$(fcs)" "$FC0"
AP0=$(agent_pid)
eq "fw-a adopted, running" "$(vm_state fw-a)" running
UP1=$(guest_uptime fw-a)
[ -n "$UP1" ] && [ "$UP1" -gt "$UP0" ] && ok "fw-a guest uptime kept counting (${UP0}s -> ${UP1}s)" || bad "fw-a uptime ${UP0}s -> '${UP1}'"

say "== 8. re-running the installer =="
B="${INSTALLER_BUNDLE:-}"
if [ -z "$B" ] || [ ! -f "$B/install.sh" ]; then
  skip "installer cases (no INSTALLER_BUNDLE)"
else
  SUM0=$(sudo sha256sum "$FW" | cut -c1-16)
  run_installer() { sudo env -u FELUCCA_CONTROL_PLANE_IP "$@" bash "$B/install.sh" worker > /tmp/felucca-lab-fw.install 2>&1; }
  run_installer; eq "re-run without the variable: exit code" "$?" 0
  eq "re-run without the variable: rules file untouched" "$(sudo sha256sum "$FW" | cut -c1-16)" "$SUM0"
  eq "re-run without the variable: allow-list unchanged" "$(allow4)" "$ALLOW0"
  undisturbed "after an installer re-run"
  grep -q 'was NOT restarted' /tmp/felucca-lab-fw.install && ok "installer says it left the agent running" || bad "installer did not say so"

  run_installer FELUCCA_CONTROL_PLANE_IP='1.2.3.4 } accept; tcp dport 9090 accept #'
  [ $? -ne 0 ] && ok "an nft injection through the variable aborts the installer" || bad "installer accepted an injection string"
  eq "injection attempt: rules file untouched" "$(sudo sha256sum "$FW" | cut -c1-16)" "$SUM0"
  eq "injection attempt: no accept rule for 9090" "$(rule_count 'dport 9090 accept')" 0
  run_installer FELUCCA_CONTROL_PLANE_IP='cp.internal.example.com'
  [ $? -ne 0 ] && ok "a hostname aborts the installer" || bad "installer accepted a hostname"
  eq "refused value: allow-list unchanged" "$(allow4)" "$ALLOW0"

  # Rewrite the allow-list on purpose: the same addresses plus one more, in both families.
  WANT="$(echo "$ALLOW0" | tr ' ' '\n' | grep -v '^127.0.0.1$' | tr '\n' ',' | sed 's/,$//')"
  run_installer FELUCCA_CONTROL_PLANE_IP="${WANT}, 10.99.0.9, fd00::99"; eq "re-run with an address list: exit code" "$?" 0
  eq "the new IPv4 list is in force" "$(allow4)" "$(printf '%s\n10.99.0.9\n' "$(echo "$ALLOW0" | tr ' ' '\n')" | sort | tr '\n' ' ' | sed 's/ $//')"
  eq "the IPv6 rule got its address" "$(rule_count 'ip6 saddr != { ::1, fd00::99 } drop')" 1
  eq "each rule is still loaded once" "$(rule_count 'iifname "felucca0" drop')" 1
  eq "rules file owner and mode" "$(sudo stat -c '%U:%G %a' "$FW")" "root:root 640"
  grep -q 'rules before this run' /tmp/felucca-lab-fw.install && ok "installer printed the rules before and after" || bad "installer did not print before/after"
  undisturbed "after rewriting the allow-list"
  [ ! -e "$FW.prev" ] && ok "no backup file left behind" || bad "$FW.prev left behind"
fi

say "== 9. back to the rules the worker had =="
restore_file; fw_reload
eq "allow-list restored" "$(allow4)" "$ALLOW0"
check "exec still works on fw-a" exec_ok fw-a
cleanup_prefix fw-
summary
