#!/usr/bin/env bash
# install.sh — idempotent installer for hearthd (control-plane) or hearth-agent (worker)
#
# Usage:
#   ./install.sh control-plane   # installs hearthd + systemd unit
#   ./install.sh worker          # installs hearth-agent + systemd unit + checks prerequisites
#
# The script is safe to re-run.  It will not overwrite existing config files.
# Run as root (or with sudo).
#
# Binary resolution:
#   ./release/<arch>/hearthd | hearth-agent          (pre-built static binaries)
# Build them inside the toolchain VM (see docs/DEPLOYMENT.md):
#   hearthd:      cd go && CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build ./cmd/hearthd
#   hearth-agent: cd rust/agent && cargo build --release --target <arch>-unknown-linux-musl

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
CONF_DIR=/etc/hearth
STATE_DIR=/var/lib/hearth
DATA_DIR=/srv/hearth
UI_DIR=/usr/share/hearth/ui
# (no /run/hearth: nothing uses it — FC sockets live under DATA_DIR/instances)

# ---------------------------------------------------------------------------
# Binary installation
# ---------------------------------------------------------------------------

install_binary() {
    local name="$1"  # hearthd | hearth-agent
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
  hearthd:      cd go && CGO_ENABLED=0 GOOS=linux GOARCH=$([ "${ARCH}" = aarch64 ] && echo arm64 || echo amd64) go build -ldflags='-s -w' -o hearthd ./cmd/hearthd
  hearth-agent: cd rust/agent && cargo build --release --target ${ARCH}-unknown-linux-musl
See docs/DEPLOYMENT.md."
}

# ---------------------------------------------------------------------------
# Directory setup (idempotent)
# ---------------------------------------------------------------------------

setup_directories() {
    info "Creating runtime directories"

    # Config dir — readable by hearth user and root
    install -d -m 0750 "${CONF_DIR}"
    install -d -m 0750 "${STATE_DIR}"
    # Everything under DATA_DIR is tenant data the agent (root) alone touches:
    # instances/<id>/ holds mem.bin, a verbatim dump of the guest's RAM, plus
    # the FC API socket and the guest vsock; images/ holds rootfs images and
    # templates captured from tenant sandboxes. World-readable here means any
    # local account on the worker reads another tenant's memory with no
    # privilege escalation, so the tree is root-only (hearth-agent.service also
    # sets UMask=0077 so FC's own files land 0600 inside it).
    install -d -m 0750 "${DATA_DIR}"
    install -d -m 0750 "${DATA_DIR}/kernels"
    install -d -m 0750 "${DATA_DIR}/images"
    install -d -m 0700 "${DATA_DIR}/instances"
    # UI assets are public static files served by the non-root hearthd user.
    install -d -m 0755 "${UI_DIR}"

    ok "Directories ready"
}

# ---------------------------------------------------------------------------
# hearth system user (control-plane only)
# ---------------------------------------------------------------------------

setup_hearth_user() {
    if id hearth >/dev/null 2>&1; then
        ok "User 'hearth' already exists"
    else
        info "Creating system user 'hearth'"
        useradd --system --no-create-home --shell /sbin/nologin hearth
        ok "User 'hearth' created"
    fi

    # The hearth user needs to read config and write state
    chown -R hearth:hearth "${CONF_DIR}" "${STATE_DIR}"
    chown root:hearth "${CONF_DIR}"
    chmod 0750 "${CONF_DIR}"
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
    systemctl daemon-reload
    ok "${dest}"
}

# ---------------------------------------------------------------------------
# Kernel modules (both roles)
# ---------------------------------------------------------------------------
# The hardened units set ProtectKernelModules=true, so br_netfilter and
# wireguard must be preloaded by the host (v4 P1/P2).

install_modules_conf() {
    local src="${SCRIPT_DIR}/systemd/hearth-modules.conf"
    local dest=/etc/modules-load.d/hearth.conf

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
        *REPLACE_WITH*|hearth-lab-token) return 0 ;;
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
Re-run with a token you generated elsewhere:  HEARTH_TOKEN=\$(openssl rand -hex 32) sudo -E $0 ${ROLE}"
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
    if [ -n "${HEARTH_TOKEN:-}" ]; then
        if is_placeholder_token "${HEARTH_TOKEN}"; then
            die "HEARTH_TOKEN is a placeholder or shorter than 32 characters.
The control plane and its workers share one bearer token; generate it once with
'openssl rand -hex 32' and pass the same value to every node."
        fi
        TOKEN="${HEARTH_TOKEN}"
        ok "Using HEARTH_TOKEN from the environment"
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
# trusted to apply by hand.  They are not optional: hearth-agent listens on the
# bridge gateway that every guest has as its default route, and guest→host
# packets hit the INPUT hook, which the agent's own forward-chain isolation
# never sees.  Without the hearth0 drop, a tenant with root in their own
# sandbox (the expected design) reaches the node's root control API.
#
# The rules live in their own `inet hearth_host` table so they never collide
# with the agent's `ip hearth` table or with the host's existing filter table,
# and the file is regenerated on every run — it is derived from the role and
# HEARTH_CONTROL_PLANE_IP, not operator state.

FIREWALL_NFT="${CONF_DIR}/firewall.nft"
FIREWALL_UNIT=/etc/systemd/system/hearth-firewall.service

write_worker_firewall() {
    # Loopback stays allowed either way so `curl 127.0.0.1:9090/healthz` still
    # works for on-node diagnostics; the API is bearer-authenticated regardless.
    # An inet-table `ip saddr` match never sees IPv6 packets, so each family
    # needs its own rule.
    local v4_allow="127.0.0.1" v6_allow="::1"
    case "${HEARTH_CONTROL_PLANE_IP:-}" in
        "")   ;;  # deny by default rather than open the root API to the whole
                  # private network on the strength of an unset variable
        *:*)  v6_allow="::1, ${HEARTH_CONTROL_PLANE_IP}" ;;
        *)    v4_allow="127.0.0.1, ${HEARTH_CONTROL_PLANE_IP}" ;;
    esac
    local cp_rule="        tcp dport 9090 ip saddr != { ${v4_allow} } drop
        tcp dport 9090 ip6 saddr != { ${v6_allow} } drop"

    cat > "${FIREWALL_NFT}" <<NFTEOF
# Generated by deploy/install.sh — regenerated on every run, do not hand-edit.
# Additional local policy belongs in its own table.
table inet hearth_host {
    chain input {
        type filter hook input priority filter; policy accept;

        # Replies to connections the host opened toward a guest (the agent
        # pings guests and drives them over vsock).
        iifname "hearth0" ct state established,related accept
        # ICMP stays up: the agent and the verify scripts prove liveness by
        # pinging the guest from its worker.
        iifname "hearth0" meta l4proto { icmp, ipv6-icmp } accept
        # Everything else a guest addresses to the host is dropped — the root
        # agent API on 9090 above all.
        iifname "hearth0" drop

        # The agent API answers the control plane only.
${cp_rule}
    }
}
NFTEOF
    chmod 0640 "${FIREWALL_NFT}"
}

write_control_plane_firewall() {
    cat > "${FIREWALL_NFT}" <<'NFTEOF'
# Generated by deploy/install.sh — regenerated on every run, do not hand-edit.
# Additional local policy belongs in its own table.
table inet hearth_host {
    chain input {
        type filter hook input priority filter; policy accept;

        # hearthd answers the local reverse proxy only (DEPLOYMENT.md §7).
        # An inet-table `ip saddr` match never sees IPv6 packets, so the two
        # families need one rule each.
        tcp dport 8080 ip saddr != 127.0.0.1 drop
        tcp dport 8080 ip6 saddr != ::1 drop
    }
}
NFTEOF
    chmod 0640 "${FIREWALL_NFT}"
}

install_firewall() {
    local role="$1"

    local nft_bin
    nft_bin="$(command -v nft || true)"
    if [ -z "${nft_bin}" ]; then
        echo "    ERROR: nft not found — the host firewall rules cannot be installed."
        echo "    hearth-agent.service Requires=hearth-firewall.service and will refuse"
        echo "    to start until they are. Install nftables and re-run this installer."
        return
    fi

    info "Installing host firewall rules"
    case "${role}" in
        worker)        write_worker_firewall ;;
        control-plane) write_control_plane_firewall ;;
    esac
    ok "${FIREWALL_NFT}"

    # A oneshot unit re-applies the ruleset on every boot; the service units
    # order themselves after it so a node never serves its API unfiltered.
    #
    # The unit ships in deploy/systemd/ like every other one rather than being
    # generated here: hearth-agent.service's Requires= points at it, so it is
    # part of the enforcement path and belongs somewhere it gets reviewed and
    # run through `systemd-analyze verify` (scripts/verify-units.sh) instead of
    # living only inside an installer heredoc. Only the two paths are
    # substituted, and only when they differ from the shipped defaults.
    local unit_src="${SCRIPT_DIR}/systemd/hearth-firewall.service"
    [ -f "${unit_src}" ] || die "Unit file not found: ${unit_src}"
    install -m 0644 "${unit_src}" "${FIREWALL_UNIT}"
    if [ "${nft_bin}" != /usr/sbin/nft ]; then
        sed -i "s#/usr/sbin/nft#${nft_bin}#g" "${FIREWALL_UNIT}"
    fi
    if [ "${FIREWALL_NFT}" != /etc/hearth/firewall.nft ]; then
        sed -i "s#/etc/hearth/firewall.nft#${FIREWALL_NFT}#g" "${FIREWALL_UNIT}"
    fi
    ok "${FIREWALL_UNIT}"
    systemctl daemon-reload
    systemctl enable hearth-firewall >/dev/null 2>&1 || true

    if systemctl restart hearth-firewall >/dev/null 2>&1; then
        ok "Firewall rules active (table inet hearth_host)"
    else
        echo "    WARNING: could not apply the rules now — check 'systemctl status hearth-firewall'."
        echo "    ${role} services will not start until it succeeds."
    fi

    if [ "${role}" = worker ] && [ -z "${HEARTH_CONTROL_PLANE_IP:-}" ]; then
        echo "    NOTE: HEARTH_CONTROL_PLANE_IP was not set, so port 9090 is currently"
        echo "    loopback-only. Re-run with HEARTH_CONTROL_PLANE_IP=<control plane IP>"
        echo "    (or edit ${FIREWALL_NFT}) before the control plane can drive this node."
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
# HEARTH_FC_VERSION + HEARTH_FC_SHA256 override both together for a node that
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
    version="${HEARTH_FC_VERSION:-${FC_VERSION}}"
    if [ -n "${HEARTH_FC_VERSION:-}" ] || [ -n "${HEARTH_FC_SHA256:-}" ]; then
        if [ -z "${HEARTH_FC_VERSION:-}" ] || [ -z "${HEARTH_FC_SHA256:-}" ]; then
            die "HEARTH_FC_VERSION and HEARTH_FC_SHA256 must be set together — a version without a digest is an unverified download."
        fi
        sha="${HEARTH_FC_SHA256}"
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
            echo "    (only needed when the WireGuard overlay is enabled — wg_ip set in hearthd.json)"
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

info "Hearth installer — role: ${ROLE} — arch: ${ARCH}"

setup_directories
install_modules_conf
resolve_token

case "${ROLE}" in
    control-plane)
        check_wireguard_tools "control-plane"
        install_binary "hearthd"
        setup_hearth_user
        install_unit "hearthd"
        install_config_stub \
            "${SCRIPT_DIR}/config/hearthd.example.json" \
            "${CONF_DIR}/hearthd.json" \
            "hearth:hearth"
        # Env file stub (carries secrets; not tracked in git)
        if [ ! -f "${CONF_DIR}/hearthd.env" ]; then
            cat > "${CONF_DIR}/hearthd.env" <<'ENVEOF'
# Environment overrides for hearthd (optional — the installer already wrote the
# token into hearthd.json). Setting HEARTH_TOKEN here overrides that value and
# keeps the secret out of the JSON; systemd reads this file as root, so it never
# has to be readable by the hearth user.
#HEARTH_TOKEN=
ENVEOF
            chmod 0640 "${CONF_DIR}/hearthd.env"
            chown root:hearth "${CONF_DIR}/hearthd.env"
            ok "Env stub installed: ${CONF_DIR}/hearthd.env"
        fi
        install_firewall "control-plane"
        # Enable but do NOT start: the config still needs this host's real
        # values, and starting now would serve the API before the operator has
        # reviewed the firewall rules above.
        info "Enabling hearthd (not starting it yet)"
        systemctl enable hearthd
        ok "hearthd enabled — it will start on the next boot, or when you start it"
        ;;

    worker)
        check_kvm
        install_firecracker
        check_nftables
        check_wireguard_tools "worker"
        check_curl
        install_binary "hearth-agent"
        install_unit "hearth-agent"
        install_config_stub \
            "${SCRIPT_DIR}/config/hearth-agent.example.json" \
            "${CONF_DIR}/hearth-agent.json" \
            "root:root"
        if [ ! -f "${CONF_DIR}/agent.env" ]; then
            cat > "${CONF_DIR}/agent.env" <<'ENVEOF'
# Environment overrides for hearth-agent (optional — the installer already wrote
# the token into hearth-agent.json). Setting HEARTH_TOKEN here overrides that
# value and keeps the secret out of the JSON.
#HEARTH_TOKEN=
ENVEOF
            chmod 0640 "${CONF_DIR}/agent.env"
            ok "Env stub installed: ${CONF_DIR}/agent.env"
        fi
        install_firewall "worker"
        info "Enabling hearth-agent (not starting it yet)"
        systemctl enable hearth-agent
        ok "hearth-agent enabled — start it once the steps below are done"
        echo ""
        echo "Before starting the agent:"
        echo "  1. Set control_plane, advertise_addr and bind in ${CONF_DIR}/hearth-agent.json"
        echo "     (bind must be this node's management address — never 0.0.0.0, which"
        echo "      includes the bridge gateway every guest routes through)."
        echo "  2. Confirm the token matches the control plane's."
        echo "  3. Fetch guest kernel + rootfs:  sudo ${SCRIPT_DIR}/firecracker-assets.sh"
        echo "  4. Review ${FIREWALL_NFT} and 'nft list table inet hearth_host'."
        echo "  5. sudo systemctl start hearth-agent"
        echo ""
        echo "Enroll with the hub: hearth-agent --join https://<hub> --join-token <token>  (token from POST /api/v1/join-tokens; ${CONF_DIR}/hearth-agent.json takes over on later boots)"
        ;;
esac

if [ "${TOKEN_IS_NEW}" = yes ] && [ "${CONFIG_WAS_WRITTEN}" = yes ]; then
    echo ""
    echo "Bearer token for this node (shown once — it is not printed again):"
    echo "    ${TOKEN}"
    if [ "${ROLE}" = worker ]; then
        echo "This worker minted its own token because HEARTH_TOKEN was not supplied."
        echo "Hearth shares ONE token across the fleet, so replace it with the control"
        echo "plane's value or the agent will never register:"
        echo "    HEARTH_TOKEN=<control plane token> sudo -E $0 worker   # on a fresh node"
        echo "    or edit \"token\" in ${CONF_DIR}/hearth-agent.json"
    else
        echo "Give this same value to every worker:  HEARTH_TOKEN=... sudo -E $0 worker"
    fi
elif [ "${CONFIG_WAS_WRITTEN}" = no ]; then
    echo ""
    echo "The existing config was kept, so its token is still in force — nothing new to copy."
fi

echo ""
info "Installation complete."
