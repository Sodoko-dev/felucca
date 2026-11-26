#!/usr/bin/env bash
# install.sh — idempotent installer for feluccad (control-plane) or felucca-agent (worker)
#
# Usage:
#   ./install.sh control-plane   # installs feluccad + systemd unit
#   ./install.sh worker          # installs felucca-agent + systemd unit + checks prerequisites
#
# The script is safe to re-run.  It will not overwrite existing config files.
# Run as root (or with sudo).
#
# Binary resolution:
#   ./release/<arch>/feluccad | felucca-agent          (pre-built static binaries)
# Build them inside the toolchain VM (see docs/DEPLOYMENT.md):
#   feluccad:      cd go && CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build ./cmd/feluccad
#   felucca-agent: cd rust/agent && cargo build --release --target <arch>-unknown-linux-musl

set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }
ok() { echo "    [ok] $*"; }

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "This script must be run as root (try: sudo $0 ${ROLE})"
    fi
}

# Detect the host architecture and map it to the Firecracker/musl triple.
detect_arch() {
    local raw
    raw="$(uname -m)"
    case "${raw}" in
        x86_64)  echo "x86_64" ;;
        aarch64) echo "aarch64" ;;
        arm64)   echo "aarch64" ;;  # macOS uname compat — should not reach here in practice
        *) die "Unsupported architecture: ${raw}" ;;
    esac
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

ROLE="${1:-}"
case "${ROLE}" in
    control-plane|worker) ;;
    *) die "Usage: $0 control-plane|worker" ;;
esac

require_root

ARCH="$(detect_arch)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Directories created by this installer
INSTALL_BIN=/usr/local/bin
CONF_DIR=/etc/felucca
STATE_DIR=/var/lib/felucca
DATA_DIR=/srv/felucca
UI_DIR=/usr/share/felucca/ui
# (no /run/felucca: nothing uses it — FC sockets live under DATA_DIR/instances)

# ---------------------------------------------------------------------------
# Binary installation
# ---------------------------------------------------------------------------

install_binary() {
    local name="$1"  # feluccad | felucca-agent
    local dest="${INSTALL_BIN}/${name}"

    # 1) Pre-built static binary in release/<arch>/
    local release_bin="${SCRIPT_DIR}/release/${ARCH}/${name}"
    if [ -f "${release_bin}" ]; then
        info "Installing ${name} from ${release_bin}"
        install -m 0755 "${release_bin}" "${dest}"
        ok "${dest}"
        return
    fi

    die "Cannot find ${name}: no pre-built binary at ${release_bin}.
Build static binaries inside the toolchain VM and place them in ${SCRIPT_DIR}/release/${ARCH}/ :
  feluccad:      cd go && CGO_ENABLED=0 GOOS=linux GOARCH=$([ "${ARCH}" = aarch64 ] && echo arm64 || echo amd64) go build -ldflags='-s -w' -o feluccad ./cmd/feluccad
  felucca-agent: cd rust/agent && cargo build --release --target ${ARCH}-unknown-linux-musl
See docs/DEPLOYMENT.md."
}

# ---------------------------------------------------------------------------
# Directory setup (idempotent)
# ---------------------------------------------------------------------------

setup_directories() {
    info "Creating runtime directories"

    # Config dir — readable by felucca user and root
    install -d -m 0750 "${CONF_DIR}"
    install -d -m 0750 "${STATE_DIR}"
    # Everything under DATA_DIR is tenant data the agent (root) alone touches:
    # instances/<id>/ holds mem.bin, a verbatim dump of the guest's RAM, plus
    # the FC API socket and the guest vsock; images/ holds rootfs images and
    # templates captured from tenant sandboxes. World-readable here means any
    # local account on the worker reads another tenant's memory with no
    # privilege escalation, so the tree is root-only (felucca-agent.service also
    # sets UMask=0077 so FC's own files land 0600 inside it).
    install -d -m 0750 "${DATA_DIR}"
    install -d -m 0750 "${DATA_DIR}/kernels"
    install -d -m 0750 "${DATA_DIR}/images"
    install -d -m 0700 "${DATA_DIR}/instances"
    # UI assets are public static files served by the non-root feluccad user.
    install -d -m 0755 "${UI_DIR}"

    ok "Directories ready"
}

# ---------------------------------------------------------------------------
# felucca system user (control-plane only)
# ---------------------------------------------------------------------------

setup_felucca_user() {
    if id felucca >/dev/null 2>&1; then
        ok "User 'felucca' already exists"
    else
        info "Creating system user 'felucca'"
        useradd --system --no-create-home --shell /sbin/nologin felucca
        ok "User 'felucca' created"
    fi

    # The felucca user needs to read config and write state — but not the
    # firewall rules file: root loads that one, so it must never belong to the
    # service user, not even for the moment between this step and the firewall
    # step. (A plain `chown -R` over ${CONF_DIR} used to hand it over on every
    # run, and it stayed that way whenever the firewall step then bailed out.)
    find "${CONF_DIR}" -mindepth 1 \
        ! -name 'firewall.nft' ! -name 'firewall.nft.prev' ! -name '.firewall.nft.*' \
        -exec chown -h felucca:felucca {} +
    chown -R felucca:felucca "${STATE_DIR}"
    chown root:felucca "${CONF_DIR}"
    chmod 0750 "${CONF_DIR}"
    local f
    for f in "${FIREWALL_NFT}" "${FIREWALL_NFT}.prev"; do
        if [ -e "${f}" ]; then
            chown root:root "${f}"
            chmod 0640 "${f}"
        fi
    done
    ok "Ownership set"
}

# ---------------------------------------------------------------------------
# Systemd unit installation
# ---------------------------------------------------------------------------

install_unit() {
    local unit_name="$1"
    local src="${SCRIPT_DIR}/systemd/${unit_name}.service"
    local dest="/etc/systemd/system/${unit_name}.service"

    [ -f "${src}" ] || die "Unit file not found: ${src}"

    info "Installing systemd unit ${unit_name}.service"
    install -m 0644 "${src}" "${dest}"
    # felucca-agent.service names nft in the two ExecStartPre= lines of its
    # start gate.
    rewrite_unit_paths "${dest}"
    # daemon-reload applies the new unit to a service that is already running
    # (KillMode= and the restart policy take effect at once); it does not
    # restart anything.
    systemctl daemon-reload
    ok "${dest}"
}

# ---------------------------------------------------------------------------
# Kernel modules (both roles)
# ---------------------------------------------------------------------------
# The hardened units set ProtectKernelModules=true, so br_netfilter and
# wireguard must be preloaded by the host (v4 P1/P2).

install_modules_conf() {
    local src="${SCRIPT_DIR}/systemd/felucca-modules.conf"
    local dest=/etc/modules-load.d/felucca.conf

    [ -f "${src}" ] || die "Modules file not found: ${src}"

    info "Installing kernel modules config (br_netfilter, wireguard)"
    install -m 0644 "${src}" "${dest}"
    ok "${dest}"

    # modules-load.d only applies at boot — load the modules for the current
    # boot too. Best-effort: warn (don't fail) if neither path works.
    if systemctl restart systemd-modules-load.service >/dev/null 2>&1 \
        || { modprobe br_netfilter && modprobe wireguard; } >/dev/null 2>&1; then
        ok "br_netfilter + wireguard loaded for the current boot"
    else
        echo "    WARNING: could not load br_netfilter/wireguard now."
        echo "    They will be loaded on the next boot via ${dest}."
    fi
}

# ---------------------------------------------------------------------------
# Shared bearer token
# ---------------------------------------------------------------------------
# The example configs are committed to a public repo, so their token value has
# to be an obvious placeholder — but a placeholder that survives the install is
# a published credential on a root-privileged API (the agent's /v1/vms/:id/exec
# runs commands inside any tenant's guest).  The installer therefore mints a
# real token here and never writes a placeholder to /etc.  Both binaries refuse
# to start on a placeholder or a token shorter than 32 characters.

is_placeholder_token() {
    local t="$1"
    case "${t}" in
        *REPLACE_WITH*|felucca-lab-token) return 0 ;;
    esac
    [ "${#t}" -lt 32 ]
}

generate_token() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    elif [ -r /dev/urandom ]; then
        od -An -vtx1 -N32 /dev/urandom | tr -d ' \n'
    else
        die "No source of randomness found (need openssl or /dev/urandom).
Re-run with a token you generated elsewhere:  FELUCCA_TOKEN=\$(openssl rand -hex 32) sudo -E $0 ${ROLE}"
    fi
}

# TOKEN is what lands in the config. TOKEN_IS_NEW records whether we minted it
# and CONFIG_WAS_WRITTEN whether it actually reached a file, so the summary only
# prints a token the operator can really use — a re-run over an existing config
# keeps that config's token, and printing a fresh unused one would just mislead.
TOKEN=""
TOKEN_IS_NEW=no
CONFIG_WAS_WRITTEN=no

resolve_token() {
    if [ -n "${FELUCCA_TOKEN:-}" ]; then
        if is_placeholder_token "${FELUCCA_TOKEN}"; then
            die "FELUCCA_TOKEN is a placeholder or shorter than 32 characters.
The control plane and its workers share one bearer token; generate it once with
'openssl rand -hex 32' and pass the same value to every node."
        fi
        TOKEN="${FELUCCA_TOKEN}"
        ok "Using FELUCCA_TOKEN from the environment"
    else
        TOKEN="$(generate_token)"
        TOKEN_IS_NEW=yes
    fi
}

# ---------------------------------------------------------------------------
# Config file stubs (only if the file doesn't exist yet)
# ---------------------------------------------------------------------------

install_config_stub() {
    local src="$1"
    local dest="$2"
    local owner="$3"

    if [ -f "${dest}" ]; then
        ok "Config ${dest} already exists — not overwriting"
        return
    fi

    # Substitute the token before the file lands in /etc, so there is never a
    # window in which the config on disk carries the public placeholder.
    # mktemp gives us 0600 in a private name, so the token is not exposed while
    # we build the file.
    local tmp
    tmp="$(mktemp)"
    sed "s|\"token\": \"REPLACE_WITH[^\"]*\"|\"token\": \"${TOKEN}\"|" "${src}" > "${tmp}"
    if grep -q 'REPLACE_WITH' "${tmp}"; then
        rm -f "${tmp}"
        die "Placeholder left in ${src} after substitution — refusing to install a config the binaries will reject."
    fi
    install -m 0640 -o "${owner%:*}" -g "${owner#*:}" "${tmp}" "${dest}"
    rm -f "${tmp}"
    CONFIG_WAS_WRITTEN=yes
    ok "Config installed with a real token: ${dest}"
    echo "    Set control_plane / advertise_addr / bind for this host before starting the service."
}

# ---------------------------------------------------------------------------
# Host firewall
# ---------------------------------------------------------------------------
# DEPLOYMENT.md §8 used to present these rules as an example the operator was
# trusted to apply by hand.  They are not optional: felucca-agent listens on the
# bridge gateway that every guest has as its default route, and guest→host
# packets hit the INPUT hook, which the agent's own forward-chain isolation
# never sees.  Without the felucca0 drop, a tenant with root in their own
# sandbox (the expected design) reaches the node's root control API.
#
# The rules live in their own `inet felucca_host` table so they never collide
# with the agent's `ip felucca` table or with the host's existing filter table.
#
# Re-running this installer must not change who can reach the node behind the
# operator's back, and must not disturb what is running on it:
#   * an existing rules file is kept exactly as it is.  The one exception is a
#     worker run that names the control plane (FELUCCA_CONTROL_PLANE_IP): that
#     is a request to rewrite the port-9090 allow-list.  (The file used to be
#     rewritten on every run, so a re-run without the variable narrowed port
#     9090 to loopback and locked the control plane out.)  The control-plane
#     role never reads the variable;
#   * the variable is checked before anything is installed, and a value that
#     names no address is an error, never a loopback-only file;
#   * a new rules file is dry-run through nft before it replaces the old one,
#     and a kept one is checked the same way before it is applied again;
#   * rules are applied with `systemctl reload` — one nft transaction, see the
#     unit — never with a restart, so neither the agent nor any sandbox is
#     touched;
#   * on a worker the result must carry the guest fence, because
#     felucca-agent.service refuses to start (and takes the guest bridge down)
#     without it.

FIREWALL_NFT="${CONF_DIR}/firewall.nft"
FIREWALL_UNIT=/etc/systemd/system/felucca-firewall.service
FIREWALL_FAILED=""
FIREWALL_TMP=""
NFT_BIN=""
# The rule felucca-agent.service looks for in the kernel before every start
# (its two ExecStartPre= lines grep the chain listing for it).  The generator
# below writes this exact line; scripts/verify-units.sh and
# test/installer/allowlist.sh hold the three spellings together.
FENCE_RULE='iifname "felucca0" drop'

# The units ship with /usr/sbin/nft and /etc/felucca/firewall.nft written into
# them — in Exec*= lines and inside the two `sh -c '…'` lines of the agent's
# start gate — so point them at this host's paths where those differ.  (/g:
# the gate lines name nft more than once per file.)
rewrite_unit_paths() {
    local unit="$1"
    [ -n "${NFT_BIN}" ] || NFT_BIN="$(command -v nft || true)"
    if [ -n "${NFT_BIN}" ] && [ "${NFT_BIN}" != /usr/sbin/nft ]; then
        sed -i "s#/usr/sbin/nft#${NFT_BIN}#g" "${unit}"
    fi
    if [ "${FIREWALL_NFT}" != /etc/felucca/firewall.nft ]; then
        sed -i "s#/etc/felucca/firewall.nft#${FIREWALL_NFT}#g" "${unit}"
    fi
}

# FELUCCA_CONTROL_PLANE_IP → CP_V4 / CP_V6, comma-joined nft set elements.
#
# The value ends up inside a ruleset that root loads, so nothing is copied out
# of it unexamined: the string may only contain address characters, and every
# element must be a literal IPv4 or IPv6 address.  One or more, separated by
# commas or spaces — a control plane that reaches this node over both its LAN
# and its overlay address needs two.
#   * Hostnames are refused: the rules are loaded at boot, before DNS is up.
#   * A /prefix is refused.  `192.168.104.1/24` pasted from `ip addr` would
#     open the port to the whole /24, and `/0` to everyone, without a word.  A
#     subnet is a hand edit of the rules file, which later runs keep.
#   * A value that names no address besides loopback is refused.  The variable
#     is given to open port 9090 to somebody; "nobody", written over a working
#     allow-list, locks the control plane out.
# Unset or empty means "not given": both lists stay empty and nothing is
# refused.
CP_V4=""
CP_V6=""
parse_control_plane_ips() {
    CP_V4=""; CP_V6=""
    local raw="${FELUCCA_CONTROL_PLANE_IP:-}"
    [ -n "${raw}" ] || return 0

    local hint="expected literal IPv4/IPv6 addresses separated by commas or spaces — e.g. FELUCCA_CONTROL_PLANE_IP='10.0.1.10, fd00::10'"
    # Whole-string allow-list first, so braces, quotes, semicolons, '#' and
    # newlines never reach the per-element checks.
    local allowed='^[0-9A-Fa-f:./, ]+$'
    [[ "${raw}" =~ ${allowed} ]] \
        || die "FELUCCA_CONTROL_PLANE_IP contains characters that cannot be part of an address list (${hint})."

    local o='(0|[1-9][0-9]{0,2})'
    local v4="^${o}\\.${o}\\.${o}\\.${o}\$"
    local h='[0-9A-Fa-f]{1,4}'
    local v6="^((${h}:){7}${h}|((${h}:){0,6}${h})?::((${h}:){0,6}${h})?)\$"

    local -a toks=()
    IFS=', ' read -r -a toks <<< "${raw}"
    local tok octet
    # ${toks[@]+…}: before bash 4.4 an empty array is "unbound" under `set -u`.
    for tok in ${toks[@]+"${toks[@]}"}; do
        [ -n "${tok}" ] || continue
        case "${tok}" in
            */*) die "FELUCCA_CONTROL_PLANE_IP: '${tok}' has a /prefix. Give addresses only (${hint}). To allow a subnet, edit the rules file by hand afterwards — re-running the installer keeps it." ;;
        esac
        if [[ "${tok}" =~ ${v4} ]]; then
            for octet in "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[4]}"; do
                [ "${octet}" -le 255 ] \
                    || die "FELUCCA_CONTROL_PLANE_IP: '${tok}' is not an IPv4 address (${hint})."
            done
            # Loopback is always allowed by the generator; never list it twice.
            [ "${tok}" = 127.0.0.1 ] && continue
            case ", ${CP_V4}, " in
                *", ${tok}, "*) ;;
                *) CP_V4="${CP_V4:+${CP_V4}, }${tok}" ;;
            esac
        elif [[ "${tok}" =~ ${v6} ]]; then
            # One spelling per address, so FD00::10 and fd00::10 are one entry.
            tok="$(printf '%s' "${tok}" | tr 'A-F' 'a-f')"
            [ "${tok}" = ::1 ] && continue
            case ", ${CP_V6}, " in
                *", ${tok}, "*) ;;
                *) CP_V6="${CP_V6:+${CP_V6}, }${tok}" ;;
            esac
        else
            die "FELUCCA_CONTROL_PLANE_IP: '${tok}' is not an address (${hint})."
        fi
    done

    [ -n "${CP_V4}${CP_V6}" ] \
        || die "FELUCCA_CONTROL_PLANE_IP is set but names no address besides loopback (${hint}). Leave it unset to keep the firewall rules as they are."
}

write_worker_firewall() {
    local dest="$1"
    # Loopback stays allowed either way so `curl 127.0.0.1:9090/healthz` still
    # works for on-node diagnostics; the API is bearer-authenticated regardless.
    # An inet-table `ip saddr` match never sees IPv6 packets, so each family
    # needs its own rule.  With no control-plane address given the port is
    # loopback-only: deny by default rather than open the root API to the
    # whole private network on the strength of an unset variable.
    parse_control_plane_ips
    local v4_allow="127.0.0.1${CP_V4:+, ${CP_V4}}"
    local v6_allow="::1${CP_V6:+, ${CP_V6}}"

    # (Unquoted heredoc: no backticks and no dollar signs in the text below.)
    cat > "${dest}" <<NFTEOF
# Generated by deploy/install.sh for a worker.
#
# Re-running the installer keeps this file as it is.  It is regenerated only
# when FELUCCA_CONTROL_PLANE_IP is given on that run; the two port-9090 rules
# below are the only part that depends on that value.  The variable takes
# literal addresses — to allow a subnet, edit the two sets by hand.
#
# Keep this line exactly as it is:
#         iifname "felucca0" drop
# felucca-agent looks for it in the kernel before every start.  Without it the
# agent refuses to start and takes the guest bridge down.  To count or log what
# it drops, put a rule of your own in front of it.
#
# After editing by hand:    systemctl reload felucca-firewall
# The reload is one nft transaction — a file that does not load leaves the
# rules already in force untouched.  Other local policy belongs in its own
# table.
table inet felucca_host {
    chain input {
        type filter hook input priority filter; policy accept;

        # Replies to connections the host opened toward a guest (the agent
        # pings guests and drives them over vsock).
        iifname "felucca0" ct state established,related accept
        # ICMP stays up: the agent and the verify scripts prove liveness by
        # pinging the guest from its worker.
        iifname "felucca0" meta l4proto { icmp, ipv6-icmp } accept
        # Everything else a guest addresses to the host is dropped — the root
        # agent API on 9090 above all.
        iifname "felucca0" drop

        # The agent API answers the control plane only.
        tcp dport 9090 ip saddr != { ${v4_allow} } drop
        tcp dport 9090 ip6 saddr != { ${v6_allow} } drop
    }
}
NFTEOF
}

write_control_plane_firewall() {
    local dest="$1"
    cat > "${dest}" <<'NFTEOF'
# Generated by deploy/install.sh for a control plane.
#
# Re-running the installer keeps this file as it is, whatever is in the
# environment; delete it and re-run to get a fresh copy.
# After editing by hand:    systemctl reload felucca-firewall
# The reload is one nft transaction — a file that does not load leaves the
# rules already in force untouched.  Other local policy belongs in its own
# table.
table inet felucca_host {
    chain input {
        type filter hook input priority filter; policy accept;

        # feluccad answers the local reverse proxy only (DEPLOYMENT.md §7).
        # An inet-table `ip saddr` match never sees IPv6 packets, so the two
        # families need one rule each.
        tcp dport 8080 ip saddr != 127.0.0.1 drop
        tcp dport 8080 ip6 saddr != ::1 drop
    }
}
NFTEOF
}

# Does this rules file carry the guest fence — the generated line, on a line
# that is not a comment?
file_has_fence() {
    grep -Eq "^[^#]*${FENCE_RULE}" "$1"
}

# Is the guest fence in the kernel right now?  The same test as the agent
# unit's start gate.  (The listing is captured first: with `pipefail`, a
# `grep -q` that exits on its first match can turn a hit into a failure.)
kernel_has_fence() {
    local chain
    chain="$("${NFT_BIN}" list chain inet felucca_host input 2>/dev/null)" || return 1
    grep -Fq -- "${FENCE_RULE}" <<< "${chain}"
}

# The rules for the role's port that are in the kernel right now, or a line in
# parentheses saying why there are none.
firewall_port_rules() {
    local port="$1" chain out
    if ! chain="$("${NFT_BIN}" list chain inet felucca_host input 2>/dev/null)"; then
        echo "(table inet felucca_host is not loaded)"
        return 0
    fi
    out="$(printf '%s\n' "${chain}" | grep -F 'dport' \
        | grep -E "(^|[^0-9])${port}([^0-9]|\$)" | sed -E 's/^[[:space:]]+//' || true)"
    if [ -n "${out}" ]; then
        printf '%s\n' "${out}"
    else
        echo "(table inet felucca_host is loaded, with no rule for port ${port})"
    fi
}

# Dry-run a rules file through exactly the script that felucca-firewall.service
# feeds to nft.  Nothing is changed.
firewall_check() {
    local file="$1"
    printf 'table inet felucca_host\ndelete table inet felucca_host\ninclude "%s"\nlist chain inet felucca_host input\n' "${file}" \
        | "${NFT_BIN}" -c -f - >/dev/null
}

# After a failure: what the node is left with, from what the kernel says now.
firewall_consequence() {
    local role="$1"
    if [ "${role}" = worker ]; then
        if kernel_has_fence; then
            echo "    The guest fence is still loaded, so felucca-agent keeps starting; the rules in"
            echo "    force are the ones from before this run."
        else
            echo "    The guest fence is NOT loaded: felucca-agent refuses to start, and keeps the"
            echo "    guest bridge down, until it is."
        fi
    else
        echo "    feluccad does NOT wait for these rules: keep it on loopback until this is fixed."
    fi
}

install_firewall() {
    local role="$1" port=9090
    [ "${role}" = control-plane ] && port=8080

    NFT_BIN="$(command -v nft || true)"
    if [ -z "${NFT_BIN}" ]; then
        echo "    ERROR: nft not found — the host firewall rules cannot be installed."
        if [ "${role}" = worker ]; then
            echo "    felucca-agent.service looks for them before every start and will not come"
            echo "    up until they are loaded. Install nftables and re-run this installer."
        else
            echo "    Install nftables and re-run this installer before feluccad listens on"
            echo "    anything but loopback: nothing else stops it from starting unfiltered."
        fi
        FIREWALL_FAILED="nft is not installed"
        return
    fi

    info "Installing host firewall rules"

    # Staging files of a run that was killed — or of an installer from before
    # the file was removed on every exit path, which left one behind whenever
    # it refused an address. They are this script's own names, nothing reads
    # them, and an operator looking at the directory should not have to wonder.
    rm -f "${CONF_DIR}"/.firewall.nft.??????

    # 1. The unit.  A oneshot that applies the rules file on every boot, ordered
    # before the service units.  It ships in deploy/systemd/ like every other
    # unit rather than being generated here, so it gets reviewed and run
    # through `systemd-analyze verify` (scripts/verify-units.sh) instead of
    # living only inside an installer heredoc; only the two paths are
    # substituted, and only when they differ from the shipped defaults.
    #
    # It goes in — and systemd reads it — before the rules file is touched, so
    # that nothing which can fail stands between "new file in place" and "new
    # file applied" further down.
    local unit_src="${SCRIPT_DIR}/systemd/felucca-firewall.service"
    if [ ! -f "${unit_src}" ]; then
        echo "    ERROR: unit file not found: ${unit_src}"
        FIREWALL_FAILED="felucca-firewall.service is missing from the installer bundle"
        return
    fi
    install -m 0644 "${unit_src}" "${FIREWALL_UNIT}"
    rewrite_unit_paths "${FIREWALL_UNIT}"
    systemctl daemon-reload
    systemctl enable felucca-firewall >/dev/null 2>&1 || true
    ok "${FIREWALL_UNIT}"

    local before
    before="$(firewall_port_rules "${port}")"

    # 2. The rules file: keep the one that is there, or stage a new one.
    #    A new one is written on a first install (no file, or an empty one),
    #    and on a worker run that named the control plane.  Never because a
    #    variable merely happens to be set: the control-plane role does not
    #    read it at all, and a worker value that names nobody was refused
    #    before anything was installed.
    local generate=no backup=""
    if [ ! -s "${FIREWALL_NFT}" ]; then
        generate=yes
    elif [ "${role}" = worker ] && [ -n "${CP_V4}${CP_V6}" ]; then
        generate=yes
    fi

    if [ "${generate}" = yes ]; then
        # Same directory as the destination, so the final mv is a rename: no
        # reader ever sees a half-written rules file.  mktemp creates it 0600
        # and ours (root), so it is never writable by anyone else on the way
        # in; it is removed again however this script ends.
        # (install_firecracker sets and clears its own EXIT trap before this.)
        FIREWALL_TMP="$(mktemp "${CONF_DIR}/.firewall.nft.XXXXXX")"
        trap 'rm -f "${FIREWALL_TMP}"' EXIT
        chown root:root "${FIREWALL_TMP}"
        chmod 0640 "${FIREWALL_TMP}"
        case "${role}" in
            worker)        write_worker_firewall "${FIREWALL_TMP}" ;;
            control-plane) write_control_plane_firewall "${FIREWALL_TMP}" ;;
        esac
        if ! firewall_check "${FIREWALL_TMP}"; then
            rm -f "${FIREWALL_TMP}"
            echo "    ERROR: the generated firewall rules do not load (nft -c refused them)."
            echo "    ${FIREWALL_NFT} and the rules in force were not changed."
            FIREWALL_FAILED="the generated rules file does not load"
            firewall_consequence "${role}"
            return
        fi
        if [ -s "${FIREWALL_NFT}" ]; then
            backup="${FIREWALL_NFT}.prev"
            cp -p "${FIREWALL_NFT}" "${backup}"
            chown root:root "${backup}"
            chmod 0640 "${backup}"
        fi
        mv -f "${FIREWALL_TMP}" "${FIREWALL_NFT}"
        ok "${FIREWALL_NFT} written"
    else
        # root loads this file, so only root may write it.
        chown root:root "${FIREWALL_NFT}"
        chmod 0640 "${FIREWALL_NFT}"
        if ! firewall_check "${FIREWALL_NFT}"; then
            echo "    ERROR: the existing ${FIREWALL_NFT} does not load (nft -c refused it)."
            echo "    It was left as it is and was not applied. Fix it and run"
            echo "    'systemctl reload felucca-firewall', or move it away and re-run this installer"
            echo "    to get a generated one."
            FIREWALL_FAILED="the existing ${FIREWALL_NFT} does not load"
            firewall_consequence "${role}"
            return
        fi
        if [ "${role}" = worker ] && ! file_has_fence "${FIREWALL_NFT}"; then
            echo "    ERROR: the existing ${FIREWALL_NFT} has no line"
            echo "        ${FENCE_RULE}"
            echo "    felucca-agent looks for that rule in the kernel before every start and refuses"
            echo "    to start without it — it is what keeps guests away from this host's root API."
            echo "    The file was left as it is and was NOT applied (applying it would remove a"
            echo "    fence that is loaded now). Either add the line to chain input of table inet"
            echo "    felucca_host, or let the installer write the file again:"
            echo "        FELUCCA_CONTROL_PLANE_IP=<control plane address(es)> sudo -E $0 worker"
            echo "    (A file generated for the control-plane role looks like this: one host"
            echo "    carrying both roles needs both sets of rules, written by hand.)"
            FIREWALL_FAILED="the existing ${FIREWALL_NFT} has no guest fence"
            firewall_consequence "${role}"
            return
        fi
        if [ "${role}" = worker ]; then
            ok "${FIREWALL_NFT} kept as it is (FELUCCA_CONTROL_PLANE_IP not given)"
        else
            ok "${FIREWALL_NFT} kept as it is"
        fi
        if grep -q 'regenerated on every run' "${FIREWALL_NFT}"; then
            echo "    NOTE: its header still says \"regenerated on every run, do not hand-edit\". That"
            echo "    was true of the installer that wrote it and is not any more: the file is kept,"
            echo "    and hand edits are applied with 'systemctl reload felucca-firewall'."
        fi
    fi

    # 3. Apply.  reload, not restart: the unit loads the file as one
    # transaction, and a reload reaches nothing else on the node.  `start`
    # covers a first install and a unit that is stopped or failed.
    local verb=start
    if systemctl is-active --quiet felucca-firewall; then
        verb=reload
    fi
    if systemctl "${verb}" felucca-firewall >/dev/null 2>&1; then
        ok "Firewall rules applied (systemctl ${verb} felucca-firewall)"
        if [ -n "${backup}" ]; then
            rm -f "${backup}"
        fi
    else
        echo "    ERROR: 'systemctl ${verb} felucca-firewall' failed — see 'systemctl status felucca-firewall'."
        if [ -n "${backup}" ]; then
            # The load is transactional, so whatever was in force before still
            # is.  Put the previous file back so the disk matches the kernel
            # and the next boot does not trip over the file that just failed —
            # and apply it, so that the unit says the same thing.
            mv -f "${backup}" "${FIREWALL_NFT}"
            chown root:root "${FIREWALL_NFT}"
            chmod 0640 "${FIREWALL_NFT}"
            if systemctl "${verb}" felucca-firewall >/dev/null 2>&1; then
                echo "    The previous ${FIREWALL_NFT} was put back and applied again."
            else
                echo "    The previous ${FIREWALL_NFT} was put back, but applying it failed as well."
            fi
        fi
        FIREWALL_FAILED="systemctl ${verb} felucca-firewall failed"
        firewall_consequence "${role}"
        return
    fi

    # 4. What is in force now.
    if [ "${role}" = worker ] && ! kernel_has_fence; then
        echo "    ERROR: the rules loaded, but chain input of table inet felucca_host has no"
        echo "        ${FENCE_RULE}"
        echo "    rule. felucca-agent refuses to start, and keeps the guest bridge down, until it"
        echo "    has: put that line into ${FIREWALL_NFT} as it stands here."
        FIREWALL_FAILED="the guest fence is not loaded"
        return
    fi

    local after
    after="$(firewall_port_rules "${port}")"
    if [ "${before}" = "${after}" ]; then
        echo "    Port ${port} rules in force (unchanged):"
    else
        echo "    Port ${port} rules before this run:"
        printf '%s\n' "${before}" | sed 's/^/        /'
        echo "    Port ${port} rules in force now:"
    fi
    printf '%s\n' "${after}" | sed 's/^/        /'

    case "${after}" in
        "("*)
            if [ "${role}" = worker ]; then
                echo "    WARNING: no rule for port 9090 is loaded, so the agent API answers every"
                echo "    address that can reach this node (it is still bearer-authenticated). Check"
                echo "    ${FIREWALL_NFT}."
            else
                echo "    WARNING: no rule for port 8080 is loaded, so feluccad answers on whatever it"
                echo "    binds. Check ${FIREWALL_NFT}."
            fi
            ;;
    esac

    if [ "${role}" = worker ] && [ "${generate}" = yes ] && [ -z "${CP_V4}${CP_V6}" ]; then
        echo "    NOTE: FELUCCA_CONTROL_PLANE_IP was not set, so port 9090 is currently"
        echo "    loopback-only. Re-run with FELUCCA_CONTROL_PLANE_IP=<control plane address(es)>"
        echo "    (or edit ${FIREWALL_NFT} and 'systemctl reload felucca-firewall') before the"
        echo "    control plane can drive this node."
    fi
}

# ---------------------------------------------------------------------------
# Worker prerequisites
# ---------------------------------------------------------------------------

check_kvm() {
    info "Checking KVM availability"
    if [ -c /dev/kvm ]; then
        ok "/dev/kvm present"
    else
        echo "    WARNING: /dev/kvm not found."
        echo "    On bare-metal: ensure VT-x/AMD-V is enabled in BIOS."
        echo "    On a cloud VM: ensure nested virtualisation or KVM pass-through is on."
    fi
}

# --- Firecracker pin -------------------------------------------------------
# Whatever this installs becomes the hypervisor under every tenant VM on the
# node, so the version is pinned and the download is checked against a digest
# recorded here rather than trusted because it arrived over TLS.  "Latest" is
# not a security property: it is whatever the release CDN hands back today.
#
# To move the pin, take the digests the project publishes next to each asset:
#   V=v1.16.1
#   for a in aarch64 x86_64; do
#     curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${V}/firecracker-${V}-${a}.tgz.sha256.txt"
#   done
# FELUCCA_FC_VERSION + FELUCCA_FC_SHA256 override both together for a node that
# must run a different build.
FC_VERSION=v1.16.1

fc_pinned_sha256() {
    case "$1" in
        aarch64) echo 8d0e69f6d6f9a1724551f607f18504052c16c1828ee3d4d7b6e6c73380871e0e ;;
        x86_64)  echo 382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6 ;;
    esac
}

install_firecracker() {
    if command -v firecracker >/dev/null 2>&1; then
        ok "Firecracker already installed: $(firecracker --version 2>&1 | head -1)"
        return
    fi

    local version sha
    version="${FELUCCA_FC_VERSION:-${FC_VERSION}}"
    if [ -n "${FELUCCA_FC_VERSION:-}" ] || [ -n "${FELUCCA_FC_SHA256:-}" ]; then
        if [ -z "${FELUCCA_FC_VERSION:-}" ] || [ -z "${FELUCCA_FC_SHA256:-}" ]; then
            die "FELUCCA_FC_VERSION and FELUCCA_FC_SHA256 must be set together — a version without a digest is an unverified download."
        fi
        sha="${FELUCCA_FC_SHA256}"
    else
        sha="$(fc_pinned_sha256 "${ARCH}")"
        [ -n "${sha}" ] || die "No pinned Firecracker digest for ${ARCH}."
    fi

    info "Installing Firecracker ${version} for ${ARCH}"

    command -v curl >/dev/null 2>&1 || \
        die "curl is required to download Firecracker. Install it and retry."
    command -v sha256sum >/dev/null 2>&1 || \
        die "sha256sum is required to verify the Firecracker download. Install coreutils and retry."

    # A private directory, not a fixed /tmp name: with `curl -o /tmp/fc.tgz` any
    # local user could pre-create that path as a symlink to a root-owned file
    # and have this root download follow it.
    local tmpdir
    tmpdir="$(mktemp -d)"
    # shellcheck disable=SC2064
    trap "rm -rf '${tmpdir}'" EXIT

    local tgz="${tmpdir}/firecracker-${version}-${ARCH}.tgz"
    curl -fsSL \
        "https://github.com/firecracker-microvm/firecracker/releases/download/${version}/firecracker-${version}-${ARCH}.tgz" \
        -o "${tgz}"

    echo "${sha}  ${tgz}" > "${tmpdir}/fc.sha256"
    if ! sha256sum -c "${tmpdir}/fc.sha256" >/dev/null 2>&1; then
        die "Firecracker ${version} (${ARCH}) failed SHA-256 verification.
  expected: ${sha}
  got:      $(sha256sum "${tgz}" | cut -d' ' -f1)
Refusing to install an unverified hypervisor."
    fi
    ok "SHA-256 verified"

    tar -xzf "${tgz}" -C "${tmpdir}"
    install -m 0755 "${tmpdir}/release-${version}-${ARCH}/firecracker-${version}-${ARCH}" "${INSTALL_BIN}/firecracker"

    rm -rf "${tmpdir}"
    trap - EXIT
    ok "Firecracker installed: $(firecracker --version 2>&1 | head -1)"
}

check_nftables() {
    info "Checking nftables"
    if command -v nft >/dev/null 2>&1; then
        ok "nft found: $(nft --version 2>&1 | head -1)"
    else
        echo "    WARNING: nft not found. Install nftables:"
        echo "      apt-get install -y nftables   # Debian/Ubuntu"
        echo "      dnf install -y nftables        # RHEL/Fedora"
    fi
}

check_wireguard_tools() {
    local role="$1"  # control-plane | worker
    info "Checking wireguard-tools"
    if command -v wg >/dev/null 2>&1; then
        ok "wg found: $(wg --version 2>&1 | head -1)"
    else
        echo "    WARNING: wg not found. Install wireguard-tools:"
        echo "      apt-get install -y wireguard-tools   # Debian/Ubuntu"
        echo "      dnf install -y wireguard-tools        # RHEL/Fedora"
        if [ "${role}" = "control-plane" ]; then
            echo "    (only needed when the WireGuard overlay is enabled — wg_ip set in feluccad.json)"
        fi
    fi
}

check_curl() {
    info "Checking curl"
    if command -v curl >/dev/null 2>&1; then
        ok "curl found: $(curl --version 2>&1 | head -1)"
    else
        echo "    WARNING: curl not found. Install it — the join flow and"
        echo "    Firecracker/asset downloads need it."
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "Felucca installer — role: ${ROLE} — arch: ${ARCH}"

# Everything that can refuse this run on what it was given refuses it here,
# before a single file is installed. (The address list used to be looked at
# only when the firewall rules were written — after the new binary and the new
# unit were already in place — and a refusal there left a half-upgraded node
# and, on a first install, a minted token that was never shown.)
case "${ROLE}" in
    worker)
        parse_control_plane_ips
        ;;
    control-plane)
        if [ -n "${FELUCCA_CONTROL_PLANE_IP:-}" ]; then
            echo "    NOTE: FELUCCA_CONTROL_PLANE_IP is set but is only read by the worker role (it"
            echo "    is the allow-list for the agent's port 9090). It is ignored here; an existing"
            echo "    ${FIREWALL_NFT} is kept as it is."
        fi
        ;;
esac
resolve_token

setup_directories
install_modules_conf

case "${ROLE}" in
    control-plane)
        check_wireguard_tools "control-plane"
        install_binary "feluccad"
        setup_felucca_user
        install_unit "feluccad"
        install_config_stub \
            "${SCRIPT_DIR}/config/feluccad.example.json" \
            "${CONF_DIR}/feluccad.json" \
            "felucca:felucca"
        # Env file stub (carries secrets; not tracked in git)
        if [ ! -f "${CONF_DIR}/feluccad.env" ]; then
            cat > "${CONF_DIR}/feluccad.env" <<'ENVEOF'
# Environment overrides for feluccad (optional — the installer already wrote the
# token into feluccad.json). Setting FELUCCA_TOKEN here overrides that value and
# keeps the secret out of the JSON; systemd reads this file as root, so it never
# has to be readable by the felucca user.
#FELUCCA_TOKEN=
ENVEOF
            chmod 0640 "${CONF_DIR}/feluccad.env"
            chown root:felucca "${CONF_DIR}/feluccad.env"
            ok "Env stub installed: ${CONF_DIR}/feluccad.env"
        fi
        install_firewall "control-plane"
        # Enable but do NOT start: the config still needs this host's real
        # values, and starting now would serve the API before the operator has
        # reviewed the firewall rules above.
        info "Enabling feluccad (not starting it yet)"
        systemctl enable feluccad
        ok "feluccad enabled — it will start on the next boot, or when you start it"
        ;;

    worker)
        check_kvm
        install_firecracker
        check_nftables
        check_wireguard_tools "worker"
        check_curl
        install_binary "felucca-agent"
        install_unit "felucca-agent"
        install_config_stub \
            "${SCRIPT_DIR}/config/felucca-agent.example.json" \
            "${CONF_DIR}/felucca-agent.json" \
            "root:root"
        if [ ! -f "${CONF_DIR}/agent.env" ]; then
            cat > "${CONF_DIR}/agent.env" <<'ENVEOF'
# Environment overrides for felucca-agent (optional — the installer already wrote
# the token into felucca-agent.json). Setting FELUCCA_TOKEN here overrides that
# value and keeps the secret out of the JSON.
#FELUCCA_TOKEN=
ENVEOF
            chmod 0640 "${CONF_DIR}/agent.env"
            ok "Env stub installed: ${CONF_DIR}/agent.env"
        fi
        install_firewall "worker"
        # `is-active` prints the state and fails for anything but "active";
        # "activating" is a unit systemd is starting or retrying right now.
        agent_state="$(systemctl is-active felucca-agent 2>/dev/null || true)"
        case "${agent_state}" in
            active)
                # An upgrade. This installer never restarts a running agent:
                # the operator picks the moment, and the restart is the only
                # step left.
                systemctl enable felucca-agent >/dev/null 2>&1 || true
                echo ""
                echo "felucca-agent is running and was NOT restarted. The new unit is already in"
                echo "effect (KillMode=$(systemctl show felucca-agent -p KillMode --value)); the new binary is not, until you run:"
                echo "    sudo systemctl restart felucca-agent"
                echo "Sandboxes keep running across that restart when KillMode above says 'process'."
                if [ -z "${FIREWALL_FAILED}" ]; then
                    echo "The firewall rules were applied without touching the agent or any sandbox."
                else
                    echo "The firewall step did NOT complete (see the ERROR above) — sort that out"
                    echo "before the restart: the agent refuses to start without the guest fence."
                fi
                echo ""
                echo "The checklist below is for a first install."
                ;;
            activating|reloading|deactivating)
                # Not running, but systemd has it in hand (a start that keeps
                # failing is retried every RestartSec): it will run the new
                # binary under the new unit by itself.
                systemctl enable felucca-agent >/dev/null 2>&1 || true
                echo ""
                echo "felucca-agent is '${agent_state}': systemd is starting or retrying it, and its next"
                echo "attempt runs the new binary under the new unit. This installer did not start"
                echo "or stop it. See 'systemctl status felucca-agent' for what it is waiting on."
                echo ""
                echo "The checklist below is for a first install."
                ;;
            *)
                info "Enabling felucca-agent (not starting it yet)"
                systemctl enable felucca-agent
                ok "felucca-agent enabled — start it once the steps below are done"
                ;;
        esac
        echo ""
        echo "Before starting the agent:"
        echo "  1. Set control_plane, advertise_addr and bind in ${CONF_DIR}/felucca-agent.json"
        echo "     (bind must be this node's management address — never 0.0.0.0, which"
        echo "      includes the bridge gateway every guest routes through)."
        echo "  2. Confirm the token matches the control plane's."
        echo "  3. Fetch guest kernel + rootfs:  sudo ${SCRIPT_DIR}/firecracker-assets.sh"
        echo "  4. Review ${FIREWALL_NFT} and 'nft list table inet felucca_host'."
        echo "  5. sudo systemctl start felucca-agent"
        echo ""
        echo "Enroll with the hub: felucca-agent --join https://<hub> --join-token <token>  (token from POST /api/v1/join-tokens; ${CONF_DIR}/felucca-agent.json takes over on later boots)"
        ;;
esac

if [ "${TOKEN_IS_NEW}" = yes ] && [ "${CONFIG_WAS_WRITTEN}" = yes ]; then
    echo ""
    echo "Bearer token for this node (shown once — it is not printed again):"
    echo "    ${TOKEN}"
    if [ "${ROLE}" = worker ]; then
        echo "This worker minted its own token because FELUCCA_TOKEN was not supplied."
        echo "Felucca shares ONE token across the fleet, so replace it with the control"
        echo "plane's value or the agent will never register:"
        echo "    FELUCCA_TOKEN=<control plane token> sudo -E $0 worker   # on a fresh node"
        echo "    or edit \"token\" in ${CONF_DIR}/felucca-agent.json"
    else
        echo "Give this same value to every worker:  FELUCCA_TOKEN=... sudo -E $0 worker"
    fi
elif [ "${CONFIG_WAS_WRITTEN}" = no ]; then
    echo ""
    echo "The existing config was kept, so its token is still in force — nothing new to copy."
fi

if [ -n "${FIREWALL_FAILED}" ]; then
    # Everything else was installed and reported above (the token is only
    # printed once, so the run is not cut short), but an installer that says
    # "complete" when its firewall step did not go through would be lying.
    # What that leaves in force is spelled out where the step failed.
    echo ""
    die "The host firewall step did not complete: ${FIREWALL_FAILED}. Fix that and re-run this installer."
fi

echo ""
info "Installation complete."
