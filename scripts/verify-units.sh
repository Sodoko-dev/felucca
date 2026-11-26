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
# asserts that the settings the fleet's availability depends on are the ones
# IN EFFECT — see "Assertions" below. Changing one of them is a behaviour
# change that needs its own ADR, not something a refactor should be able to do
# with a green build. The script then breaks copies of the units on purpose
# and requires its own assertions to notice, so they cannot quietly stop
# asserting anything.
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
# (VERIFY_UNITS_SYS_UNIT_DIR names another directory to take them from; it is
# there so that this script can be tested against another distribution's layout.)
SYS_UNIT_DIR="${VERIFY_UNITS_SYS_UNIT_DIR:-}"
[ -z "${SYS_UNIT_DIR}" ] || [ -d "${SYS_UNIT_DIR}" ] || { echo "verify-units: VERIFY_UNITS_SYS_UNIT_DIR=${SYS_UNIT_DIR} is not a directory." >&2; exit 2; }
if [ -z "${SYS_UNIT_DIR}" ]; then
    for d in /usr/lib/systemd/system /lib/systemd/system; do
        [ -d "$d" ] && { SYS_UNIT_DIR="$d"; break; }
    done
fi
[ -n "${SYS_UNIT_DIR}" ] || { echo "verify-units: no system unit directory found." >&2; exit 2; }

ROOT="$(mktemp -d)"
trap 'rm -rf "${ROOT}"' EXIT

mkdir -p "${ROOT}/usr/lib/systemd" "${ROOT}/etc/systemd/system" \
         "${ROOT}/usr/local/bin" "${ROOT}/usr/sbin" "${ROOT}/etc/felucca" \
         "${ROOT}/srv/felucca"
cp -a "${SYS_UNIT_DIR}" "${ROOT}/usr/lib/systemd/system"

# A distribution may ship units as symlinks that leave the unit directory.
# Ubuntu 26.04 does: dracut-*.service point into /usr/lib/dracut/modules.d and
# are wanted by sysinit.target and initrd.target. Their targets are not in the
# staged root, so systemd-analyze reports every one it comes across
# ("dracut-shutdown.service: Failed to open ...: No such file or directory")
# although none of them is a unit under test. A link that cannot be followed
# inside the staged root is not a unit this root has: drop it, then the
# .wants/.requires links that pointed at it, until nothing dangles. Links to
# /dev/null are masks and stay.
while :; do
    pruned=0
    while IFS= read -r -d '' link; do
        target="$(readlink "$link")"
        case "$target" in
            /dev/null) continue ;;
            /*) [ -e "${ROOT}${target}" ] && continue ;;
            *)  [ -e "$link" ] && continue ;;
        esac
        rm -f "$link"
        pruned=$((pruned + 1))
    done < <(find "${ROOT}/usr/lib/systemd/system" -type l -print0)
    [ "$pruned" -gt 0 ] || break
done

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

# ---------------------------------------------------------------------------
# Assertions: what systemd ends up with, not what the file happens to contain
# ---------------------------------------------------------------------------
# These used to be `grep -x` for a line. That stays green when the line is
# still in the file but is no longer the one in force: systemd takes the LAST
# assignment of a setting, an empty assignment resets a list, and whitespace
# around `=` is ignored. So the units are read here the way systemd reads
# them — per section, in order — and the result is what gets compared.
# (Each one is here because losing it brings back an outage: ADR-0011.)

# The reader below does not join continuation lines; the units have none.
if cont="$(grep -nE '\\$' "${UNIT_DIR}/felucca-agent.service" "${UNIT_DIR}/felucca-firewall.service")"; then
    echo "verify-units: FAIL — a line ends in a backslash; the assertions below do not read continuation lines:" >&2
    printf '%s\n' "${cont}" >&2
    exit 1
fi

# unit_values <file> <Section> <Key>
# The values that key ends up with in that section, one per line, in order. An
# empty assignment resets the list. A section may appear more than once.
unit_values() {
    awk -v want="[$2]" -v key="$3" '
        /^[[:space:]]*[#;]/ { next }
        /^[[:space:]]*\[/ {
            sec = $0
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", sec)
            next
        }
        sec != want { next }
        {
            eq = index($0, "=")
            if (eq == 0) next
            k = substr($0, 1, eq - 1)
            v = substr($0, eq + 1)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", k)
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
            if (k != key) next
            if (v == "") { n = 0; next }
            vals[++n] = v
        }
        END { for (i = 1; i <= n; i++) print vals[i] }
    ' "$1"
}

# Settings with one value — "<unit>|<Section>|<Key>|<value that must be in effect>".
SCALARS=(
    # FLU-1: stopping, restarting or crashing the agent must not kill the
    # Firecrackers in its cgroup, and the agent must always come back.
    'felucca-agent.service|Service|KillMode|process'
    'felucca-agent.service|Service|Restart|always'
    'felucca-agent.service|Unit|StartLimitIntervalSec|0'
    # FLU-2: one OOM-killed Firecracker must not stop the unit (and with it
    # the agent), and systemd-oomd must not kill the whole cgroup.
    'felucca-agent.service|Service|OOMPolicy|continue'
    'felucca-agent.service|Service|ManagedOOMPreference|omit'
    # FLU-3: without it nft may be handed /dev/null and "load" nothing.
    'felucca-firewall.service|Service|StandardInput|data'
)
# Dependency lists — "<unit>|<Section>|<Key>|<unit that must be in the list>".
MEMBERS=(
    # FLU-3: the rules are loaded before the agent at boot, without the agent
    # being restarted (or left down) by the unit that loads them.
    'felucca-agent.service|Unit|Wants|felucca-firewall.service'
    'felucca-agent.service|Unit|After|felucca-firewall.service'
)
# Lines that must NOT come back, wherever and however they are spelled —
# "<unit>|<extended regex>".
FORBIDDEN=(
    # FLU-2: a unit-level OOM score is inherited by every Firecracker, and an
    # unprivileged VMM may lower itself back to it.
    'felucca-agent.service|^[[:space:]]*OOMScoreAdjust[[:space:]]*='
    # FLU-3: these restart or stop the agent with the firewall unit, and leave
    # it down, with no retry, when that unit fails to start.
    'felucca-agent.service|^[[:space:]]*(Requires|Requisite|BindsTo|PartOf)[[:space:]]*=.*felucca-firewall'
    # FLU-3: stopping the firewall unit must neither delete the table (that
    # exposes the host to guests that are still running) nor end sandboxes on
    # a system that is not shutting down. Its one ExecStop= is asserted
    # verbatim below; nothing may be added after it.
    'felucca-firewall.service|^[[:space:]]*ExecStopPost[[:space:]]*='
)

# FLU-1/FLU-3: at host shutdown — and only then — the sandboxes are ended
# before any other unit can unload the rules that fence them. Without the
# `= stopping || exit 0` this line kills every sandbox on every stop or restart
# of the firewall unit.
SHUTDOWN_STOP="-/bin/sh -c 'test \"\$\$(systemctl is-system-running 2>/dev/null)\" = stopping || exit 0; exec systemctl kill --kill-whom=all --signal=SIGKILL felucca-agent.service'"

# FLU-3: the agent's start gate. The fence test is the same string in both
# steps; nft is named by its absolute path (the lab's restart gate swaps the
# agent's PATH to make the AGENT's nft calls fail, and the gate must not fail
# with them).
FENCE_TEST='/usr/sbin/nft list chain inet felucca_host input 2>/dev/null | grep -q "iifname .felucca0. drop"'
# Step 1 — load the rules again when the fence is missing; best effort.
GATE_HEAL="-+/bin/sh -c '${FENCE_TEST} || systemctl reload-or-restart felucca-firewall.service'"
# Step 2 — fence present: go on. Otherwise bridge down, one message, fail.
GATE_PREFIX="/bin/sh -c '${FENCE_TEST} && exit 0; ip link set dev felucca0 down 2>/dev/null; echo \""
GATE_SUFFIX="\" >&2; exit 1'"
# What the message between them may consist of: nothing that could end the
# echo early and smuggle another command (or an `exit 0`) into the gate, and
# nothing systemd would expand.
GATE_MESSAGE_RE='^[A-Za-z0-9 _.,:()/-]+$'

# expect_list <file> <Section> <Key> <value>... — the whole list, in order.
expect_list() {
    local file="$1" sec="$2" key="$3" want got
    shift 3
    want="$(printf '%s\n' "$@")"
    got="$(unit_values "${file}" "${sec}" "${key}")"
    [ "${got}" = "${want}" ] && return 0
    echo "verify-units: FAIL — $(basename "${file}") [${sec}] ${key}= is not the list that must be in effect"
    echo "  in effect:"; printf '%s\n' "${got:-(nothing)}" | sed 's/^/    /'
    echo "  must be:";   printf '%s\n' "${want}" | sed 's/^/    /'
    return 1
}

# check_units <dir holding the unit files> — prints what is wrong; 1 if anything is.
check_units() {
    local dir="$1" bad=0 entry unit sec key want got re hit msg
    local -a pre

    for entry in "${SCALARS[@]}"; do
        IFS='|' read -r unit sec key want <<< "${entry}"
        got="$(unit_values "${dir}/${unit}" "${sec}" "${key}" | tail -n 1)"
        if [ "${got}" != "${want}" ]; then
            echo "verify-units: FAIL — ${unit} [${sec}] ${key}= is '${got}' in effect, must be '${want}'"
            bad=1
        fi
    done

    for entry in "${MEMBERS[@]}"; do
        IFS='|' read -r unit sec key want <<< "${entry}"
        # (Collected first: `grep -q` at the end of a pipeline can exit early
        # and, under pipefail, turn a match into a failure.)
        got="$(unit_values "${dir}/${unit}" "${sec}" "${key}" | tr -s ' \t' '\n')"
        if ! grep -qxF -- "${want}" <<< "${got}"; then
            echo "verify-units: FAIL — ${unit} [${sec}] ${key}= does not list ${want}"
            bad=1
        fi
    done

    for entry in "${FORBIDDEN[@]}"; do
        unit="${entry%%|*}"; re="${entry#*|}"
        if hit="$(grep -nE -- "${re}" "${dir}/${unit}")"; then
            echo "verify-units: FAIL — ${unit} has a line that must not be there: ${hit}"
            bad=1
        fi
    done

    # FLU-3: the rules are replaced in one transaction, by start and by reload.
    expect_list "${dir}/felucca-firewall.service" Service StandardInputText \
        'table inet felucca_host' \
        'delete table inet felucca_host' \
        'include "/etc/felucca/firewall.nft"' \
        'list chain inet felucca_host input' || bad=1
    expect_list "${dir}/felucca-firewall.service" Service ExecStart \
        '/usr/sbin/nft -f -' || bad=1
    expect_list "${dir}/felucca-firewall.service" Service ExecReload \
        '/usr/sbin/nft -f -' \
        '/usr/sbin/nft list chain inet felucca_host input' || bad=1
    expect_list "${dir}/felucca-firewall.service" Service ExecStop \
        "${SHUTDOWN_STOP}" || bad=1

    # FLU-3: the agent's start gate — exactly these two steps, in this order.
    mapfile -t pre < <(unit_values "${dir}/felucca-agent.service" Service ExecStartPre)
    if [ "${#pre[@]}" -ne 2 ]; then
        echo "verify-units: FAIL — felucca-agent.service has ${#pre[@]} ExecStartPre= lines in effect, must be the 2 of the start gate"
        bad=1
    else
        if [ "${pre[0]}" != "${GATE_HEAL}" ]; then
            echo "verify-units: FAIL — felucca-agent.service: the first ExecStartPre= is not the reload-the-firewall step"
            echo "  in effect: ${pre[0]}"
            echo "  must be:   ${GATE_HEAL}"
            bad=1
        fi
        if [[ "${pre[1]}" != "${GATE_PREFIX}"*"${GATE_SUFFIX}" ]]; then
            echo "verify-units: FAIL — felucca-agent.service: the second ExecStartPre= is not the gate"
            echo "  in effect: ${pre[1]}"
            echo "  must be:   ${GATE_PREFIX}<message>${GATE_SUFFIX}"
            bad=1
        else
            msg="${pre[1]#"${GATE_PREFIX}"}"; msg="${msg%"${GATE_SUFFIX}"}"
            if ! [[ "${msg}" =~ ${GATE_MESSAGE_RE} ]]; then
                echo "verify-units: FAIL — felucca-agent.service: the gate's message may only contain ${GATE_MESSAGE_RE}"
                echo "  it is: ${msg}"
                bad=1
            fi
        fi
    fi

    return "${bad}"
}

if ! check_units "${UNIT_DIR}" >&2; then
    exit 1
fi
# 4 lists in the firewall unit and the 2 steps of the gate, besides the tables.
n_assert=$(( ${#SCALARS[@]} + ${#MEMBERS[@]} + 4 + 2 ))
echo "verify-units: OK — ${n_assert} settings are in effect as required, ${#FORBIDDEN[@]} forbidden patterns absent"

# ---------------------------------------------------------------------------
# The assertions must be able to fail
# ---------------------------------------------------------------------------
# Each entry breaks a copy of one shipped unit in a way that leaves every
# original line in place (so a grep-for-a-line check would still pass), or
# changes the gate by a few characters. check_units has to reject every one.

# "<unit>|<Section>|<line appended to the end of the file under that section>"
BREAK_APPEND=(
    'felucca-agent.service|Service|KillMode=control-group'
    'felucca-agent.service|Service|Restart=no'
    'felucca-agent.service|Service|OOMPolicy=stop'
    'felucca-agent.service|Service|ManagedOOMPreference=none'
    'felucca-agent.service|Unit|StartLimitIntervalSec=10'
    'felucca-agent.service|Service|ExecStartPre='
    'felucca-agent.service|Service|ExecStartPre=/bin/true'
    'felucca-agent.service|Service|    OOMScoreAdjust=-900'
    'felucca-agent.service|Unit|Requires = felucca-firewall.service'
    'felucca-agent.service|Unit|Requisite=felucca-firewall.service'
    'felucca-agent.service|Unit|Wants='
    'felucca-agent.service|Unit|After='
    'felucca-firewall.service|Service|StandardInput=null'
    'felucca-firewall.service|Service|StandardInputText='
    'felucca-firewall.service|Service|StandardInputText=flush ruleset'
    'felucca-firewall.service|Service|ExecStart=/bin/true'
    'felucca-firewall.service|Service|ExecReload='
    'felucca-firewall.service|Service|ExecStop=/usr/sbin/nft delete table inet felucca_host'
    'felucca-firewall.service|Service|ExecStop='
    'felucca-firewall.service|Service|ExecStopPost=/usr/sbin/nft flush ruleset'
)
# "<unit>|<sed expression applied to the copy>"
BREAK_SED=(
    # the gate reports success when the fence is missing
    "felucca-agent.service|s/; exit 1'\$/; exit 0'/"
    # the message ends early and an exit 0 follows it
    'felucca-agent.service|s/not starting: /not starting"; exit 0; echo "/'
    # the reload step is no longer best-effort
    'felucca-agent.service|s/^ExecStartPre=-+/ExecStartPre=+/'
    # the fence test only looks for the chain again
    'felucca-agent.service|s/iifname \.felucca0\. drop/chain input/g'
    # nft is looked up through PATH
    'felucca-agent.service|s#/usr/sbin/nft list chain#nft list chain#g'
    # the bridge is left up
    'felucca-agent.service|s/ip link set dev felucca0 down/true/'
    # a required line that is only commented out
    'felucca-agent.service|s/^KillMode=process$/#KillMode=process/'
    # the shutdown step ends the sandboxes on EVERY stop of the firewall unit
    'felucca-firewall.service|s/ = stopping || exit 0; exec / = stopping; exec /'
    # ...or tests for the wrong state
    'felucca-firewall.service|s/" = stopping || exit 0/" = running || exit 0/'
)

SELF="${ROOT}/selftest"
caught=0; missed=0; total=0
fresh_copy() { rm -rf "${SELF}"; mkdir -p "${SELF}"; cp "${UNIT_DIR}"/*.service "${SELF}/"; }
# try_broken <what was done> <unit> — the copy must differ from the shipped
# unit and must be rejected.
try_broken() {
    total=$((total + 1))
    if cmp -s "${UNIT_DIR}/$2" "${SELF}/$2"; then
        echo "verify-units: FAIL — self-test '$1' no longer changes $2; the unit moved on, update the self-test" >&2
        missed=$((missed + 1))
    elif check_units "${SELF}" >/dev/null 2>&1; then
        echo "verify-units: FAIL — the assertions accept a unit broken by: $1" >&2
        missed=$((missed + 1))
    else
        caught=$((caught + 1))
    fi
}

for entry in "${BREAK_APPEND[@]}"; do
    IFS='|' read -r unit sec line <<< "${entry}"
    fresh_copy
    printf '\n[%s]\n%s\n' "${sec}" "${line}" >> "${SELF}/${unit}"
    try_broken "[${sec}] ${line}" "${unit}"
done
for entry in "${BREAK_SED[@]}"; do
    unit="${entry%%|*}"; expr="${entry#*|}"
    fresh_copy
    sed -i -e "${expr}" "${SELF}/${unit}"
    try_broken "sed ${expr}" "${unit}"
done

if [ "${missed}" -ne 0 ]; then
    echo "verify-units: FAIL — ${missed} of ${total} deliberately broken units were not rejected" >&2
    exit 1
fi
echo "verify-units: OK — the assertions reject all ${caught} deliberately broken variants of the units"
