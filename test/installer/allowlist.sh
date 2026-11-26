#!/usr/bin/env bash
# Tests for the firewall half of deploy/install.sh (FLU-3). Nothing is
# installed and nothing on this machine is changed.
#
#   sudo bash test/installer/allowlist.sh
#
# Four parts:
#   1. The address-list parser. The value of FELUCCA_CONTROL_PLANE_IP ends up
#      inside an nftables ruleset that root loads, so only literal addresses
#      may get through — and a value that names nobody must be refused, not
#      turned into a loopback-only allow-list.
#   2. The generated rulesets: what the docs say they are, and the guest fence
#      spelled the way felucca-agent.service looks for it.
#   3. rewrite_unit_paths: a host whose nft is not /usr/sbin/nft still gets a
#      working start gate.
#   4. install_firewall itself, against a scratch config directory, a stand-in
#      systemctl and a stand-in kernel table: when the rules file is kept,
#      when it is written again, and what is left when applying it fails.
#
# The installer's functions are pulled out of the script and run under the
# same shell options the installer uses (set -euo pipefail), so a function
# that only works without them fails here too.
#
# Part 4 needs root (the installer chowns the rules file to root) and is
# skipped without it. `nft` is used for real, in check mode, where it is
# installed; where it is not, a stand-in only checks the shape of a file, and
# the summary says so.
#
# INSTALLER_UNDER_TEST=<file> points the test at another copy of the installer
# (used to show that a broken installer fails this test).
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY="${HERE}/../../deploy"
INSTALLER="${INSTALLER_UNDER_TEST:-${DEPLOY}/install.sh}"
PASS=0; FAIL=0; SKIP=0
ok()   { PASS=$((PASS+1)); printf '  PASS: %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  FAIL: %s\n' "$*"; }
skip() { SKIP=$((SKIP+1)); printf '  SKIP: %s\n' "$*"; }
# is <description> <got> <want>
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 — got '$2', want '$3'"; fi; }

# ---- the installer's functions ------------------------------------------------
# A function is cut out from its `name() {` line to the first `}` in column 0.
# The two generators contain a heredoc with such a line in it, so they are cut
# at the heredoc terminator and closed here.
fn()         { sed -n "/^$1() {/,/^}/p" "${INSTALLER}"; }
heredoc_fn() { sed -n "/^$1() {/,/^NFTEOF\$/p" "${INSTALLER}"; printf '}\n'; }
NAMES="rewrite_unit_paths parse_control_plane_ips write_worker_firewall write_control_plane_firewall
       file_has_fence kernel_has_fence firewall_port_rules firewall_check firewall_consequence install_firewall"
LIB="$(grep -E '^FENCE_RULE=' "${INSTALLER}" || true)"
for name in ${NAMES}; do
  case "${name}" in
    write_*) LIB="${LIB}"$'\n'"$(heredoc_fn "${name}")" ;;
    *)       LIB="${LIB}"$'\n'"$(fn "${name}")" ;;
  esac
done
if ! ( eval "${LIB}"
       [ -n "${FENCE_RULE:-}" ] || exit 1
       for name in ${NAMES}; do declare -F "${name}" >/dev/null || exit 1; done ) >/dev/null 2>&1; then
  echo "could not extract the firewall functions from ${INSTALLER}" >&2
  exit 2
fi
FENCE_RULE="$( eval "${LIB}"; printf '%s' "${FENCE_RULE}" )"

SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT

# How the installer's code is run here.  `set -e` is not in force inside a
# command substitution (bash does not hand it down), nor inside anything that
# sits in an `if` or on the left of `||` — there bash ignores it even when the
# code sets it again.  So every piece of installer code below runs in a
# subshell that turns the installer's options on itself, started as a plain
# command with this script's own `-e` switched off around it, and its exit
# status is looked at afterwards.

# ---- 1. the parser --------------------------------------------------------------
parse() { # parse <value> -> "v4=[…] v6=[…]", or "REFUSED" when the installer dies on it
  local out
  set +e
  out="$( { set -euo pipefail
            die() { echo REFUSED; exit 1; }
            CP_V4=""; CP_V6=""
            eval "${LIB}"
            FELUCCA_CONTROL_PLANE_IP="$1" parse_control_plane_ips
            echo "v4=[${CP_V4}] v6=[${CP_V6}]"; } 2>&1 )"
  set -e
  printf '%s\n' "${out:-(the parser stopped without a result)}" | tail -n 1
}
expect() { is "$1" "$(parse "$2")" "$3"; } # expect <description> <value> <wanted output>

echo "== 1a. accepted =="
expect "unset means: not given"                         ""                               "v4=[] v6=[]"
expect "one IPv4 address"                               "10.0.1.10"                      "v4=[10.0.1.10] v6=[]"
expect "two IPv4 addresses, comma + space (LAN + overlay)" "192.168.104.1, 10.100.0.1"   "v4=[192.168.104.1, 10.100.0.1] v6=[]"
expect "space separated"                                "10.0.1.10 10.0.1.11"            "v4=[10.0.1.10, 10.0.1.11] v6=[]"
expect "stray separators around real addresses"         ",10.0.1.10,,10.0.1.11, "        "v4=[10.0.1.10, 10.0.1.11] v6=[]"
expect "one IPv6 address"                               "fd00::10"                       "v4=[] v6=[fd00::10]"
expect "mixed families go to their own rule"            "10.0.1.10, fd00::10"            "v4=[10.0.1.10] v6=[fd00::10]"
expect "full-form IPv6"                                 "2001:db8:0:0:0:0:0:1"           "v4=[] v6=[2001:db8:0:0:0:0:0:1]"
expect "duplicates are listed once"                     "10.0.1.10, 10.0.1.10"           "v4=[10.0.1.10] v6=[]"
expect "IPv6 is one spelling: upper and lower case are one entry" "FD00::10, fd00::10"   "v4=[] v6=[fd00::10]"
expect "loopback is not repeated (always allowed)"      "127.0.0.1, ::1, 10.0.1.10"      "v4=[10.0.1.10] v6=[]"

echo "== 1b. refused: not a literal address =="
expect "nft injection through the value"                '1.2.3.4 } accept; tcp dport 9090 accept #' "REFUSED"
expect "a closing brace on its own"                     '10.0.1.10 }'                    "REFUSED"
expect "a newline in the value"                         $'10.0.1.10\n10.0.1.11'          "REFUSED"
expect "a quote"                                        '10.0.1.10"'                     "REFUSED"
expect "a hostname (rules load before DNS is up)"       "cp.internal.example.com"        "REFUSED"
expect "an octet above 255"                             "10.0.1.256"                     "REFUSED"
expect "a leading zero (nft would read it as a name)"   "10.0.01.10"                     "REFUSED"
expect "three octets"                                   "10.0.1"                         "REFUSED"
expect "hex groups that are not an address"             "dead:beef"                      "REFUSED"
expect "a shell glob"                                   "*"                              "REFUSED"
expect "one bad element refuses the whole list"         "10.0.1.10, nope"                "REFUSED"

echo "== 1c. refused: a /prefix (a subnet is a hand edit of the kept file) =="
expect "an IPv4 network"                                "10.0.0.0/8"                     "REFUSED"
expect "an address with host bits, pasted from ip addr" "192.168.104.1/24"               "REFUSED"
expect "/0 — everyone"                                  "10.0.1.10/0"                    "REFUSED"
expect "::/0 — everyone"                                "::/0"                           "REFUSED"
expect "a single-address prefix"                        "10.0.1.10/32"                   "REFUSED"
expect "an IPv6 network"                                "fd00::/8"                       "REFUSED"
expect "a prefix next to a good address"                "10.0.1.10, 10.9.0.0/16"         "REFUSED"

echo "== 1d. refused: set, but names nobody (must never become a loopback-only file) =="
expect "a lone comma (two empty variables joined)"      ","                              "REFUSED"
expect "a lone space"                                   " "                              "REFUSED"
expect "separators only"                                ", ,"                            "REFUSED"
expect "IPv4 loopback only"                             "127.0.0.1"                      "REFUSED"
expect "IPv6 loopback only"                             "::1"                            "REFUSED"
expect "both loopbacks"                                 "127.0.0.1, ::1"                 "REFUSED"

# ---- 2. the generated rulesets ---------------------------------------------------
echo "== 2. generated rulesets =="
tmp="${SCRATCH}/generated.nft"
GENERATED=1
generate() { # generate <worker|control-plane> <value> — writes ${tmp}; GENERATED=0 when the generator succeeded
  : > "${tmp}"
  set +e
  ( set -euo pipefail
    die() { exit 1; }
    CP_V4=""; CP_V6=""
    eval "${LIB}"
    case "$1" in
      worker)        FELUCCA_CONTROL_PLANE_IP="$2" write_worker_firewall "${tmp}" ;;
      control-plane) write_control_plane_firewall "${tmp}" ;;
    esac ) >/dev/null 2>&1
  GENERATED=$?
  set -e
}
generated() { [ "${GENERATED}" -eq 0 ]; }
has() { grep -qF -- "$1" "${tmp}"; }

generate worker "192.168.104.1, 10.100.0.1, fd00::10"
if generated; then ok "worker ruleset is generated"; else bad "the worker generator failed"; fi
if has 'tcp dport 9090 ip saddr != { 127.0.0.1, 192.168.104.1, 10.100.0.1 } drop'; then ok "IPv4 rule lists loopback and both addresses"; else bad "IPv4 rule wrong: $(grep 'ip saddr' "${tmp}" || true)"; fi
if has 'tcp dport 9090 ip6 saddr != { ::1, fd00::10 } drop'; then ok "IPv6 rule lists loopback and the address"; else bad "IPv6 rule wrong: $(grep 'ip6 saddr' "${tmp}" || true)"; fi
is "defines table inet felucca_host once" "$(grep -c '^table inet felucca_host {' "${tmp}" || true)" 1
is "the guest fence is there once, as its own rule" "$(grep -cE "^[[:space:]]*${FENCE_RULE}\$" "${tmp}" || true)" 1
if ( eval "${LIB}"; file_has_fence "${tmp}" ); then ok "the installer's own check finds the fence in it"; else bad "file_has_fence does not accept the generated file"; fi
if has 'Keep this line exactly as it is'; then ok "the header says the fence line must stay as it is"; else bad "header does not warn about the fence line"; fi

# The same rule, spelled three times: FENCE_RULE in the installer, the line the
# generator writes (checked above), and the grep pattern in the agent unit's
# start gate. The pattern must match the rule, in both gate lines.
UNIT_PATTERNS="$(grep '^ExecStartPre=' "${DEPLOY}/systemd/felucca-agent.service" | grep -o 'grep -q "[^"]*"' | sed -e 's/^grep -q "//' -e 's/"$//' || true)"
is "the agent unit's start gate greps for the fence in two lines" "$(printf '%s\n' "${UNIT_PATTERNS}" | grep -c . || true)" 2
unit_matches=yes
while IFS= read -r pattern; do
  [ -n "${pattern}" ] || continue
  if ! grep -q -- "${pattern}" <<< "${FENCE_RULE}"; then unit_matches=no; fi
done <<< "${UNIT_PATTERNS}"
is "each of those patterns matches the rule the installer writes" "${unit_matches}" yes

generate worker ""
if generated; then ok "worker ruleset without the variable is generated"; else bad "the worker generator failed with no value"; fi
if has 'tcp dport 9090 ip saddr != { 127.0.0.1 } drop'; then ok "no address given: port 9090 is loopback-only"; else bad "loopback-only rule wrong: $(grep 'ip saddr' "${tmp}" || true)"; fi
generate worker ","
if generated; then bad "the generator wrote a file for a value that names nobody"; else ok "the generator refuses a value that names nobody"; fi
is "…and leaves nothing behind in the file it was given" "$(wc -c < "${tmp}" | tr -d ' ')" 0

generate control-plane ""
if generated; then ok "control-plane ruleset is generated"; else bad "the control-plane generator failed"; fi
if has 'tcp dport 8080 ip saddr != 127.0.0.1 drop' && has 'tcp dport 8080 ip6 saddr != ::1 drop'; then ok "control plane: port 8080 is loopback-only, both families"; else bad "control-plane 8080 rules wrong"; fi
if ( eval "${LIB}"; file_has_fence "${tmp}" ); then bad "file_has_fence accepts a control-plane file"; else ok "a control-plane file has no guest fence, and the check says so"; fi

REAL_NFT="$(command -v nft || true)"
if [ -n "${REAL_NFT}" ] && [ "$(id -u)" = 0 ]; then
  for role in worker control-plane; do
    generate "${role}" "192.168.104.1, 10.100.0.1, fd00::10, 10.9.0.9"
    if generated && { [ "${role}" != worker ] || has '10.9.0.9'; }; then
      # A scratch table name, so the check never looks at (or collides with) a
      # felucca_host table that is really loaded on this machine.
      sed -i 's/table inet felucca_host/table inet felucca_allowlist_test/' "${tmp}"
      if "${REAL_NFT}" -c -f "${tmp}" 2>/dev/null; then ok "nft accepts the generated ${role} ruleset (check mode)"; else bad "nft refused the generated ${role} ruleset"; fi
    else
      bad "could not generate the ${role} ruleset for the nft check"
    fi
  done
else
  skip "nft parse check of the generated rulesets (needs nft and root)"
fi

# ---- 3. unit paths ---------------------------------------------------------------
echo "== 3. units on a host whose nft is somewhere else =="
mkdir -p "${SCRATCH}/units"
cp "${DEPLOY}/systemd/felucca-agent.service" "${DEPLOY}/systemd/felucca-firewall.service" "${SCRATCH}/units/"
set +e
( set -euo pipefail
  eval "${LIB}"
  NFT_BIN=/opt/felucca/sbin/nft; FIREWALL_NFT=/etc/felucca/firewall.nft
  rewrite_unit_paths "${SCRATCH}/units/felucca-agent.service"
  rewrite_unit_paths "${SCRATCH}/units/felucca-firewall.service" ) >/dev/null 2>&1
rc=$?
set -e
is "rewrite_unit_paths runs (exit status)" "${rc}" 0
is "no /usr/sbin/nft is left in either unit" "$(cat "${SCRATCH}"/units/*.service | grep -c '/usr/sbin/nft' || true)" 0
is "both lines of the agent's start gate call the relocated nft" \
   "$(grep -c "^ExecStartPre=.*/bin/sh -c '/opt/felucca/sbin/nft list chain inet felucca_host input " "${SCRATCH}/units/felucca-agent.service" || true)" 2
is "the firewall unit loads with the relocated nft" "$(grep -cx 'ExecStart=/opt/felucca/sbin/nft -f -' "${SCRATCH}/units/felucca-firewall.service" || true)" 1

# ---- 4. install_firewall ---------------------------------------------------------
echo "== 4. install_firewall: keep, rewrite, and what a failure leaves =="
ETC="${SCRATCH}/etc"; STATE="${SCRATCH}/state"; KERNEL="${STATE}/kernel"; CALLS="${STATE}/calls"; OUT="${SCRATCH}/out"
FW="${ETC}/firewall.nft"
fresh() { rm -rf "${ETC}" "${STATE}" "${SCRATCH}/unit"; mkdir -p "${ETC}" "${STATE}" "${SCRATCH}/unit"; : > "${CALLS}"; : > "${OUT}"; }

# run <role> <value | "-" for unset> — what the installer's main part does for
# the firewall: refuse a bad value first, then install_firewall. Stand-ins:
#   systemctl  records the call; start/reload of the firewall unit load the
#              rules file into ${KERNEL} if it passes the unit's own dry run,
#              and change nothing if it does not (the unit's one transaction)
#   nft        `list chain` prints ${KERNEL}; `-c -f -` is the real nft in
#              check mode where there is one
run() {
  local rc
  : > "${CALLS}"
  set +e
  ( set -euo pipefail
    info() { echo "==> $*"; }; ok() { echo "    [ok] $*"; }; die() { echo "DIED: $*"; exit 1; }
    # The installer's globals; read by the functions that are eval'd below.
    # shellcheck disable=SC2034
    {
      SCRIPT_DIR="${DEPLOY}"; CONF_DIR="${ETC}"
      FIREWALL_NFT="${FW}"; FIREWALL_UNIT="${SCRATCH}/unit/felucca-firewall.service"
      FIREWALL_FAILED=""; FIREWALL_TMP=""; NFT_BIN=""; CP_V4=""; CP_V6=""
    }
    nft() {
      case "${1:-}" in
        list) [ -s "${KERNEL}" ] || { echo "Error: No such file or directory" >&2; return 1; }
              cat "${KERNEL}" ;;
        -c)   if [ -n "${REAL_NFT}" ]; then "${REAL_NFT}" -c -f -; return $?; fi
              # No nft on this machine: the shape of the included file only.
              local f; f="$(sed -n 's/^include "\(.*\)"$/\1/p')"
              grep -q '^table inet felucca_host {' "${f}" && grep -q 'chain input {' "${f}" \
                && ! grep -q 'BROKEN' "${f}" \
                && [ "$(tr -cd '{' < "${f}" | wc -c)" = "$(tr -cd '}' < "${f}" | wc -c)" ] ;;
        *)    echo "stand-in nft: unexpected arguments: $*" >&2; return 2 ;;
      esac
    }
    systemctl() {
      printf '%s\n' "$*" >> "${CALLS}"
      case "${1:-}" in
        is-active) [ -e "${STATE}/fw-active" ] ;;
        start|reload)
          if [ -e "${STATE}/fail-next-apply" ]; then rm -f "${STATE}/fail-next-apply"; return 1; fi
          if printf 'table inet felucca_host\ndelete table inet felucca_host\ninclude "%s"\nlist chain inet felucca_host input\n' "${FW}" \
               | nft -c -f - >/dev/null 2>&1; then
            grep -v '^[[:space:]]*#' "${FW}" > "${KERNEL}"
            : > "${STATE}/fw-active"
          else
            return 1
          fi ;;
        *) return 0 ;;
      esac
    }
    eval "${LIB}"
    if [ "$2" = - ]; then unset FELUCCA_CONTROL_PLANE_IP; else export FELUCCA_CONTROL_PLANE_IP="$2"; fi
    if [ "$1" = worker ]; then parse_control_plane_ips; fi
    install_firewall "$1"
    echo "FIREWALL_FAILED=[${FIREWALL_FAILED}]"
  ) > "${OUT}" 2>&1
  rc=$?
  set -e
  if [ "${rc}" -ne 0 ]; then echo "SUBSHELL-EXIT=${rc}" >> "${OUT}"; fi
}
said()      { grep -qF -- "$1" "${OUT}"; }
# What install_firewall left in FIREWALL_FAILED ("" = it went through). A run
# that never got to the end is not "no failure".
failed_as() {
  if grep -q '^FIREWALL_FAILED=\[' "${OUT}"; then
    sed -n 's/^FIREWALL_FAILED=\[\(.*\)\]$/\1/p' "${OUT}"
  else
    echo "(install_firewall did not finish)"
  fi
}
sum()       { sha256sum "$1" | cut -c1-16; }
applied()   { grep -cE '^(start|reload) felucca-firewall$' "${CALLS}" || true; }
left()      { ls -A "${ETC}" | tr '\n' ' ' | sed 's/ $//'; }
loaded()    { grep -cF -- "$1" "${KERNEL}" 2>/dev/null || true; }
show_out()  { sed 's/^/      | /' "${OUT}"; }

if [ "$(id -u)" != 0 ]; then
  skip "install_firewall scenarios (need root: the installer chowns the rules file to root)"
else
  [ -n "${REAL_NFT}" ] || echo "  note: nft is not installed here — the dry runs below check the shape of a file only"

  echo "-- worker, first install, variable not given"
  fresh; run worker -
  is "no failure reported" "$(failed_as)" ""
  if grep -qF 'tcp dport 9090 ip saddr != { 127.0.0.1 } drop' "${FW}"; then ok "the file is written, port 9090 loopback-only"; else bad "rules file wrong or missing"; show_out; fi
  if said 'FELUCCA_CONTROL_PLANE_IP was not set'; then ok "the NOTE says the control plane cannot reach the node yet"; else bad "no NOTE about the loopback-only port"; fi
  is "rules file owner and mode" "$(stat -c '%U:%G %a' "${FW}" 2>/dev/null || true)" "root:root 640"
  is "nothing else is left in the config directory" "$(left)" "firewall.nft"
  is "applied once, with start" "$(grep -c '^start felucca-firewall$' "${CALLS}" || true)/$(applied)" "1/1"
  is "the unit went in before the rules file was applied" "$(grep -nE '^(daemon-reload|start felucca-firewall)$' "${CALLS}" | cut -d: -f2 | tr '\n' ' ' | sed 's/ $//')" "daemon-reload start felucca-firewall"
  is "the installed unit hands its script to nft (StandardInput=data)" "$(grep -cx 'StandardInput=data' "${SCRATCH}/unit/felucca-firewall.service" || true)" 1
  is "the guest fence is loaded" "$(loaded "${FENCE_RULE}")" 1

  echo "-- worker, re-run without the variable: the file is kept, hand edit and all"
  printf '# a hand edit\n' >> "${FW}"; before="$(sum "${FW}")"
  # What an installer from before this one left behind each time it refused an address.
  : > "${ETC}/.firewall.nft.Old001"; : > "${ETC}/.firewall.nft.Old002"
  run worker -
  is "no failure reported" "$(failed_as)" ""
  is "the file is byte for byte the same" "$(sum "${FW}")" "${before}"
  if said 'kept as it is'; then ok "the installer says it kept the file"; else bad "no 'kept as it is'"; show_out; fi
  is "applied once, with reload" "$(grep -c '^reload felucca-firewall$' "${CALLS}" || true)/$(applied)" "1/1"
  is "nothing else is left in the config directory, an older installer's staging files included" "$(left)" "firewall.nft"

  echo "-- worker, re-run naming the control plane: the allow-list is rewritten"
  run worker "10.0.1.10, fd00::10"
  is "no failure reported" "$(failed_as)" ""
  if grep -qF 'tcp dport 9090 ip saddr != { 127.0.0.1, 10.0.1.10 } drop' "${FW}" && grep -qF 'tcp dport 9090 ip6 saddr != { ::1, fd00::10 } drop' "${FW}"; then ok "both families carry the new address"; else bad "allow-list not rewritten"; show_out; fi
  is "the new IPv4 list is the one loaded" "$(loaded '{ 127.0.0.1, 10.0.1.10 }')" 1
  if said 'rules before this run'; then ok "the installer printed the rules before and after"; else bad "no before/after"; fi
  is "no backup or temp file is left" "$(left)" "firewall.nft"
  is "rules file owner and mode" "$(stat -c '%U:%G %a' "${FW}")" "root:root 640"

  echo "-- worker, new allow-list, but applying it fails: everything goes back"
  before="$(sum "${FW}")"; : > "${STATE}/fail-next-apply"
  run worker "10.9.9.9"
  is "the failure is reported" "$(failed_as)" "systemctl reload felucca-firewall failed"
  is "the previous file is back, byte for byte" "$(sum "${FW}")" "${before}"
  is "…and was applied again (two applies: the failed one, then the old file)" "$(applied)" 2
  is "the old allow-list is still the one loaded" "$(loaded '{ 127.0.0.1, 10.0.1.10 }')/$(loaded '10.9.9.9')" "1/0"
  if said 'was put back and applied again'; then ok "the installer says what it did"; else bad "no message about the restore"; show_out; fi
  if said 'The guest fence is still loaded'; then ok "…and that the agent still starts"; else bad "no statement about the fence"; fi
  is "no backup or temp file is left" "$(left)" "firewall.nft"
  is "rules file owner and mode" "$(stat -c '%U:%G %a' "${FW}")" "root:root 640"

  echo "-- worker, a value that names nobody: refused before anything is touched"
  for value in "," " " "127.0.0.1"; do
    before="$(sum "${FW}")"
    run worker "${value}"
    if said 'DIED: FELUCCA_CONTROL_PLANE_IP is set but names no address'; then ok "'${value}' is refused"; else bad "'${value}' was not refused"; show_out; fi
    is "'${value}': the file is untouched and systemd was never called" "$(sum "${FW}")/$(wc -l < "${CALLS}" | tr -d ' ')" "${before}/0"
  done
  run worker "10.0.0.0/8"
  if said "DIED: FELUCCA_CONTROL_PLANE_IP: '10.0.0.0/8' has a /prefix"; then ok "a /prefix is refused, and the message says what to do instead"; else bad "a /prefix was not refused"; show_out; fi

  echo "-- worker, rules file from before this change (stale header): kept, and the installer says so"
  fresh
  ( die() { exit 1; }; CP_V4=""; CP_V6=""; eval "${LIB}"; FELUCCA_CONTROL_PLANE_IP="10.0.1.10" write_worker_firewall "${FW}" )
  sed -i '1s/.*/# Generated by deploy\/install.sh — regenerated on every run, do not hand-edit./' "${FW}"
  before="$(sum "${FW}")"
  run worker -
  is "no failure reported" "$(failed_as)" ""
  is "the file is byte for byte the same" "$(sum "${FW}")" "${before}"
  if said 'its header still says'; then ok "the NOTE says the old header no longer holds"; else bad "no NOTE about the stale header"; show_out; fi

  echo "-- worker, existing file has the fence but no rule for port 9090: kept, with a warning"
  fresh
  printf 'table inet felucca_host {\n    chain input {\n        type filter hook input priority filter; policy accept;\n        %s\n    }\n}\n' "${FENCE_RULE}" > "${FW}"
  run worker -
  is "no failure reported" "$(failed_as)" ""
  if said 'WARNING: no rule for port 9090 is loaded'; then ok "the missing port rule is a warning"; else bad "no warning about port 9090"; show_out; fi

  echo "-- worker, existing file has no guest fence (a control-plane file): refused, and NOT applied"
  fresh
  ( die() { exit 1; }; eval "${LIB}"; write_control_plane_firewall "${FW}" )
  before="$(sum "${FW}")"
  run worker -
  is "the failure is reported" "$(failed_as)" "the existing ${FW} has no guest fence"
  is "the file is untouched" "$(sum "${FW}")" "${before}"
  is "it was never applied" "$(applied)" 0
  if said "${FENCE_RULE}" && said 'FELUCCA_CONTROL_PLANE_IP=<control plane address(es)>'; then ok "the message names the missing line and the way out"; else bad "the message does not say what to do"; show_out; fi
  if said 'The guest fence is NOT loaded'; then ok "…and that the agent will refuse to start"; else bad "no statement about the agent"; fi

  echo "-- worker, existing file does not load: refused, and NOT applied"
  fresh
  echo 'table inet felucca_host { chain input { BROKEN' > "${FW}"; before="$(sum "${FW}")"
  run worker -
  is "the failure is reported" "$(failed_as)" "the existing ${FW} does not load"
  is "the file is untouched" "$(sum "${FW}")" "${before}"
  is "it was never applied" "$(applied)" 0

  echo "-- control plane, first install"
  fresh; run control-plane -
  is "no failure reported" "$(failed_as)" ""
  if grep -qF 'tcp dport 8080 ip saddr != 127.0.0.1 drop' "${FW}"; then ok "the file is written, port 8080 loopback-only"; else bad "control-plane rules file wrong or missing"; show_out; fi
  is "rules file owner and mode" "$(stat -c '%U:%G %a' "${FW}" 2>/dev/null || true)" "root:root 640"

  echo "-- control plane, hand-widened file, FELUCCA_CONTROL_PLANE_IP in the environment: kept"
  sed -i 's/tcp dport 8080 ip saddr != 127.0.0.1 drop/tcp dport 8080 ip saddr != { 127.0.0.1, 10.0.1.11, 10.0.1.12 } drop/' "${FW}"
  chown 12345:12345 "${FW}"; before="$(sum "${FW}")"
  run control-plane "10.0.1.10"
  is "no failure reported" "$(failed_as)" ""
  is "the file is byte for byte the same" "$(sum "${FW}")" "${before}"
  is "the widened rule is the one loaded" "$(loaded '{ 127.0.0.1, 10.0.1.11, 10.0.1.12 }')" 1
  is "…and the file is root's again" "$(stat -c '%U:%G %a' "${FW}")" "root:root 640"
fi

echo "== summary: ${PASS} passed, ${FAIL} failed, ${SKIP} skipped =="
[ "${FAIL}" -eq 0 ]
