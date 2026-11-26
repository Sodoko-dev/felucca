#!/usr/bin/env bash
# Run `systemd-analyze verify` over every unit in deploy/systemd/ and exit
# non-zero if any of them has a syntax error, an unknown directive, or an
# unresolvable dependency.
#
# Why this needs a staging root
# -----------------------------
# Running `systemd-analyze verify deploy/systemd/*.service` straight out of a
# checkout always fails, for two reasons that are about the checkout and not
# about the units:
#
#   1. "Command /usr/local/bin/feluccad is not executable" — verify stats every
#      ExecStart against the machine it runs on, and a dev box has no Felucca
#      binaries installed.
#   2. "Unit felucca-firewall.service not found" — felucca-agent.service orders
#      itself after felucca-firewall.service and pulls it in (DEPLOYMENT.md §8),
#      and verify resolves that against the system unit path.
#
# Both are environment artifacts, and the tempting "fixes" are both wrong:
# rewriting ExecStart to /bin/true means you stop verifying the real command
# line, and dropping the dependency would let the agent start before the
# rules that §8 rests on are loaded.
#
# Parsing is not enough
# ---------------------
# A unit can parse perfectly and still have lost the one line that keeps every
# sandbox on a worker alive. After the verify pass this script therefore also
# asserts that the lines the fleet's availability depends on are present,
# verbatim — see REQUIRED below. Deleting one of them is a behaviour change
# that needs its own ADR, not something a refactor should be able to do with a
# green build.
#
# So instead this builds a throwaway root that looks like an installed node —
# the system units copied in so targets resolve, stub executables at the exact
# paths the real ExecStart lines name — and points verify at that with --root.
# The units under test are the real files, byte for byte, with every hardening
# directive and the real ExecStart intact. Nothing on the host is touched.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UNIT_DIR="${REPO_ROOT}/deploy/systemd"

command -v systemd-analyze >/dev/null 2>&1 || {
    echo "verify-units: systemd-analyze not found — run this on a systemd host (e.g. the lab VM)." >&2
    exit 2
}

# Where the distribution keeps its own units; needed or every unit fails with
# "Unit sysinit.target not found" once --root cuts off the real search path.
SYS_UNIT_DIR=""
for d in /usr/lib/systemd/system /lib/systemd/system; do
    [ -d "$d" ] && { SYS_UNIT_DIR="$d"; break; }
done
[ -n "${SYS_UNIT_DIR}" ] || { echo "verify-units: no system unit directory found." >&2; exit 2; }

ROOT="$(mktemp -d)"
trap 'rm -rf "${ROOT}"' EXIT

mkdir -p "${ROOT}/usr/lib/systemd" "${ROOT}/etc/systemd/system" \
         "${ROOT}/usr/local/bin" "${ROOT}/usr/sbin" "${ROOT}/etc/felucca" \
         "${ROOT}/srv/felucca"
cp -a "${SYS_UNIT_DIR}" "${ROOT}/usr/lib/systemd/system"
cp "${UNIT_DIR}"/*.service "${ROOT}/etc/systemd/system/"

# Stubs at exactly the paths the shipped Exec*/ConditionPathExists lines name.
# Derived from the units themselves rather than hardcoded, so a new command
# line cannot silently go unverified. Every Exec key is covered (ExecStartPre,
# ExecStartPost, ExecReload, ...), with any of systemd's prefix characters.
EXEC_KEYS='Exec(StartPre|Start|StartPost|Reload|Stop|StopPost|Condition)'
mapfile -t BINS < <(
    grep -hoE "^${EXEC_KEYS}=[-@:+!]*[^ ]+" "${UNIT_DIR}"/*.service |
        sed -E "s/^${EXEC_KEYS}=[-@:+!]*//" |
        grep -E '^/' | sort -u
    grep -hoE '^ConditionPathExists=[^ ]+' "${UNIT_DIR}"/*.service |
        sed -E 's/^ConditionPathExists=//' | sort -u
)
for b in "${BINS[@]}"; do
    mkdir -p "${ROOT}$(dirname "$b")"
    printf '#!/bin/sh\nexit 0\n' > "${ROOT}${b}"
    chmod 0755 "${ROOT}${b}"
done

# The ruleset the firewall unit applies; a file, not an executable.
: > "${ROOT}/etc/felucca/firewall.nft"

# feluccad/felucca-gw run as a dedicated non-root user that a dev box does not
# have. verify only warns about that, but the warning is noise, so declare it.
mkdir -p "${ROOT}/etc"
grep -q '^felucca:' "${ROOT}/etc/passwd" 2>/dev/null || \
    echo 'felucca:x:9999:9999::/nonexistent:/usr/sbin/nologin' >> "${ROOT}/etc/passwd"
grep -q '^felucca:' "${ROOT}/etc/group" 2>/dev/null || \
    echo 'felucca:x:9999:' >> "${ROOT}/etc/group"

units=()
for f in "${UNIT_DIR}"/*.service; do units+=("$(basename "$f")"); done

echo "verify-units: checking ${#units[@]} units against a staged root"
printf '  %s\n' "${units[@]}"

# verify writes findings to stderr and, in some versions, still exits 0 on
# recoverable ones — so treat ANY output as failure alongside the exit code.
out="$(systemd-analyze verify --root="${ROOT}" "${units[@]}" 2>&1)" && rc=0 || rc=$?
if [ -n "${out}" ] || [ "${rc}" -ne 0 ]; then
    echo "verify-units: FAIL (exit ${rc})" >&2
    [ -n "${out}" ] && printf '%s\n' "${out}" >&2
    exit 1
fi

echo "verify-units: OK — all units parse, all dependencies resolve, no unknown directives"

# Lines that must be present verbatim: "<unit>|<exact line>".
# Each one is here because removing it brings back an outage (ADR-0011):
REQUIRED=(
    # FLU-1: stopping, restarting or crashing the agent must not kill the
    # Firecrackers in its cgroup, and the agent must always come back.
    'felucca-agent.service|KillMode=process'
    'felucca-agent.service|Restart=always'
    'felucca-agent.service|StartLimitIntervalSec=0'
    # FLU-3: the agent must not start while the host firewall table is absent,
    # and must not be restarted (or left down) by the unit that loads it.
    'felucca-agent.service|Wants=felucca-firewall.service'
    'felucca-agent.service|After=felucca-firewall.service'
    'felucca-agent.service|ExecStartPre=/usr/sbin/nft list chain inet felucca_host input'
    # FLU-3: the rules are replaced in one transaction, by start and by reload.
    'felucca-firewall.service|StandardInputText=table inet felucca_host'
    'felucca-firewall.service|StandardInputText=delete table inet felucca_host'
    'felucca-firewall.service|StandardInputText=include "/etc/felucca/firewall.nft"'
    'felucca-firewall.service|StandardInputText=list chain inet felucca_host input'
    'felucca-firewall.service|ExecStart=/usr/sbin/nft -f -'
    'felucca-firewall.service|ExecReload=/usr/sbin/nft -f -'
)
# Lines that must NOT come back: "<unit>|<extended regex>".
FORBIDDEN=(
    # FLU-3: Requires= restarts the agent with the firewall unit and leaves it
    # down, with no retry, when that unit fails to start.
    'felucca-agent.service|^(Requires|BindsTo|PartOf)=.*felucca-firewall'
    # FLU-3: a stop that deletes the table exposes the host to guests that are
    # still running.
    'felucca-firewall.service|^ExecStop(Post)?='
)

fail=0
for entry in "${REQUIRED[@]}"; do
    unit="${entry%%|*}"; line="${entry#*|}"
    if ! grep -qxF -- "${line}" "${UNIT_DIR}/${unit}"; then
        echo "verify-units: FAIL — ${unit} is missing the line: ${line}" >&2
        fail=1
    fi
done
for entry in "${FORBIDDEN[@]}"; do
    unit="${entry%%|*}"; re="${entry#*|}"
    if hit="$(grep -nE -- "${re}" "${UNIT_DIR}/${unit}")"; then
        echo "verify-units: FAIL — ${unit} has a line that must not be there: ${hit}" >&2
        fail=1
    fi
done
[ "${fail}" -eq 0 ] || exit 1

echo "verify-units: OK — ${#REQUIRED[@]} required lines present, ${#FORBIDDEN[@]} forbidden patterns absent"
