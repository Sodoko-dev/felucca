#!/usr/bin/env bash
# Unit test for the address-list parsing in deploy/install.sh (FLU-3).
#
#   bash test/installer/allowlist.sh
#
# The value of FELUCCA_CONTROL_PLANE_IP ends up inside an nftables ruleset that
# root loads. This checks, without installing anything, that only literal
# addresses get through and that the generated worker ruleset is what the
# docs say it is. It needs bash; `nft` is used for a parse check when present
# and the caller is root, and skipped otherwise.
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLER="${HERE}/../../deploy/install.sh"
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  PASS: %s\n' "$*"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL: %s\n' "$*"; }

# Pull the two functions out of the installer and run them in a subshell, so
# that `die` (exit 1) ends the subshell and not this test. The generator's
# body is a heredoc that itself contains lines starting with `}`, so it is cut
# at the heredoc terminator and closed here.
FUNCS="$(sed -n '/^parse_control_plane_ips() {/,/^}/p' "${INSTALLER}")
$(sed -n '/^write_worker_firewall() {/,/^NFTEOF$/p' "${INSTALLER}")
}"
case "${FUNCS}" in
  *'parse_control_plane_ips() {'*'write_worker_firewall() {'*'NFTEOF'*) ;;
  *) echo "could not extract the functions from ${INSTALLER}" >&2; exit 2 ;;
esac

parse() { # parse <value> -> prints "v4=[…] v6=[…]" or "REFUSED"
  ( die() { echo REFUSED; exit 1; }
    CP_V4=""; CP_V6=""
    eval "${FUNCS}"
    FELUCCA_CONTROL_PLANE_IP="$1" parse_control_plane_ips
    echo "v4=[${CP_V4}] v6=[${CP_V6}]" ) 2>/dev/null | tail -1
}

expect() { # expect <description> <value> <wanted output>
  local got; got="$(parse "$2")"
  if [ "${got}" = "$3" ]; then ok "$1"; else bad "$1 — input '$2' gave '${got}', wanted '$3'"; fi
}

echo "== accepted =="
expect "unset means no control-plane address"           ""                               "v4=[] v6=[]"
expect "one IPv4 address"                               "10.0.1.10"                      "v4=[10.0.1.10] v6=[]"
expect "two IPv4 addresses, comma + space (LAN + overlay)" "192.168.104.1, 10.100.0.1"   "v4=[192.168.104.1, 10.100.0.1] v6=[]"
expect "space separated"                                "10.0.1.10 10.0.1.11"            "v4=[10.0.1.10, 10.0.1.11] v6=[]"
expect "comma without space, stray separators"          ",10.0.1.10,,10.0.1.11, "        "v4=[10.0.1.10, 10.0.1.11] v6=[]"
expect "an IPv4 prefix"                                 "10.0.0.0/8"                     "v4=[10.0.0.0/8] v6=[]"
expect "one IPv6 address"                               "fd00::10"                       "v4=[] v6=[fd00::10]"
expect "mixed families go to their own rule"            "10.0.1.10, fd00::10"            "v4=[10.0.1.10] v6=[fd00::10]"
expect "full-form IPv6 and an IPv6 prefix"              "2001:db8:0:0:0:0:0:1 fd00::/8"  "v4=[] v6=[2001:db8:0:0:0:0:0:1, fd00::/8]"
expect "duplicates are listed once"                     "10.0.1.10, 10.0.1.10"           "v4=[10.0.1.10] v6=[]"
expect "loopback is not repeated (always allowed)"      "127.0.0.1, ::1, 10.0.1.10"      "v4=[10.0.1.10] v6=[]"

echo "== refused =="
expect "nft injection through the value"                '1.2.3.4 } accept; tcp dport 9090 accept #' "REFUSED"
expect "a closing brace on its own"                     '10.0.1.10 }'                    "REFUSED"
expect "a newline in the value"                         $'10.0.1.10\n10.0.1.11'          "REFUSED"
expect "a quote"                                        '10.0.1.10"'                     "REFUSED"
expect "a hostname (rules load before DNS is up)"       "cp.internal.example.com"        "REFUSED"
expect "an octet above 255"                             "10.0.1.256"                     "REFUSED"
expect "a leading zero (nft would read it as a name)"   "10.0.01.10"                     "REFUSED"
expect "three octets"                                   "10.0.1"                         "REFUSED"
expect "an IPv4 prefix above 32"                        "10.0.0.0/33"                    "REFUSED"
expect "hex groups that are not an address"             "dead:beef"                      "REFUSED"
expect "an IPv6 prefix above 128"                       "fd00::/129"                     "REFUSED"
expect "a shell glob"                                   "*"                              "REFUSED"
expect "one bad element refuses the whole list"         "10.0.1.10, nope"                "REFUSED"

echo "== generated worker ruleset =="
tmp="$(mktemp)"; trap 'rm -f "${tmp}"' EXIT
( die() { exit 1; }; CP_V4=""; CP_V6=""; eval "${FUNCS}"
  FELUCCA_CONTROL_PLANE_IP="192.168.104.1, 10.100.0.1, fd00::10" write_worker_firewall "${tmp}" )
grep -qF 'tcp dport 9090 ip saddr != { 127.0.0.1, 192.168.104.1, 10.100.0.1 } drop' "${tmp}" \
  && ok "IPv4 rule lists loopback and both addresses" || bad "IPv4 rule wrong: $(grep 'ip saddr' "${tmp}")"
grep -qF 'tcp dport 9090 ip6 saddr != { ::1, fd00::10 } drop' "${tmp}" \
  && ok "IPv6 rule lists loopback and the address" || bad "IPv6 rule wrong: $(grep 'ip6 saddr' "${tmp}")"
grep -qF 'iifname "felucca0" drop' "${tmp}" && ok "guest→host drop is present" || bad "guest→host drop missing"
[ "$(grep -c '^table inet felucca_host {' "${tmp}")" = 1 ] && ok "defines table inet felucca_host once" || bad "table definition wrong"
( die() { exit 1; }; CP_V4=""; CP_V6=""; eval "${FUNCS}"; FELUCCA_CONTROL_PLANE_IP="" write_worker_firewall "${tmp}" )
grep -qF 'tcp dport 9090 ip saddr != { 127.0.0.1 } drop' "${tmp}" \
  && ok "no address given: port 9090 is loopback-only" || bad "loopback-only rule wrong: $(grep 'ip saddr' "${tmp}")"

if command -v nft >/dev/null 2>&1 && [ "$(id -u)" = 0 ]; then
  ( die() { exit 1; }; CP_V4=""; CP_V6=""; eval "${FUNCS}"
    FELUCCA_CONTROL_PLANE_IP="192.168.104.1, 10.100.0.1, fd00::10, 10.9.0.0/16" write_worker_firewall "${tmp}" )
  # A scratch table name, so the check never looks at (or collides with) a
  # felucca_host table that is really loaded on this machine.
  sed -i 's/table inet felucca_host/table inet felucca_allowlist_test/' "${tmp}"
  if [ -s "${tmp}" ] && nft -c -f "${tmp}" 2>/dev/null; then ok "nft accepts the generated ruleset (check mode)"; else bad "nft refused the generated ruleset"; fi
else
  echo "  SKIP: nft parse check (needs nft and root)"
fi

echo "== summary: ${PASS} passed, ${FAIL} failed =="
[ "${FAIL}" -eq 0 ]
