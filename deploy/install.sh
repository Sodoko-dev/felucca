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
    install -d -m 0755 "${DATA_DIR}"
    install -d -m 0755 "${DATA_DIR}/kernels"
    install -d -m 0755 "${DATA_DIR}/images"
    install -d -m 0755 "${DATA_DIR}/instances"
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
# Config file stubs (only if the file doesn't exist yet)
# ---------------------------------------------------------------------------

install_config_stub() {
    local src="$1"
    local dest="$2"

    if [ -f "${dest}" ]; then
        ok "Config ${dest} already exists — not overwriting"
    else
        install -m 0640 "${src}" "${dest}"
        ok "Config stub installed: ${dest}"
        echo "    IMPORTANT: edit ${dest} before starting the service."
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

install_firecracker() {
    if command -v firecracker >/dev/null 2>&1; then
        ok "Firecracker already installed: $(firecracker --version 2>&1 | head -1)"
        return
    fi

    info "Firecracker not found — fetching latest release for ${ARCH}"

    if ! command -v curl >/dev/null 2>&1; then
        die "curl is required to download Firecracker. Install it and retry."
    fi
    if ! command -v jq >/dev/null 2>&1; then
        die "jq is required to parse the GitHub API response. Install it and retry."
    fi

    local tag
    tag="$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | jq -r .tag_name)"
    echo "    latest release: ${tag}"

    local tgz_url="https://github.com/firecracker-microvm/firecracker/releases/download/${tag}/firecracker-${tag}-${ARCH}.tgz"
    curl -fsSL "${tgz_url}" -o /tmp/fc.tgz
    tar -xzf /tmp/fc.tgz -C /tmp
    install -m 0755 "/tmp/release-${tag}-${ARCH}/firecracker-${tag}-${ARCH}" "${INSTALL_BIN}/firecracker"
    rm -f /tmp/fc.tgz
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

case "${ROLE}" in
    control-plane)
        check_wireguard_tools "control-plane"
        install_binary "hearthd"
        setup_hearth_user
        install_unit "hearthd"
        install_config_stub \
            "${SCRIPT_DIR}/config/hearthd.example.json" \
            "${CONF_DIR}/hearthd.json"
        # Env file stub (carries secrets; not tracked in git)
        if [ ! -f "${CONF_DIR}/hearthd.env" ]; then
            cat > "${CONF_DIR}/hearthd.env" <<'ENVEOF'
# Environment overrides for hearthd (optional — prefer the JSON config file).
# Uncomment and set HEARTH_TOKEN here to keep the token out of the JSON file.
#HEARTH_TOKEN=replace_with_openssl_rand_-hex_32
ENVEOF
            chmod 0640 "${CONF_DIR}/hearthd.env"
            chown hearth:hearth "${CONF_DIR}/hearthd.env"
            ok "Env stub installed: ${CONF_DIR}/hearthd.env"
        fi
        info "Enabling and starting hearthd"
        systemctl enable hearthd
        systemctl start hearthd
        ok "hearthd running (check: systemctl status hearthd)"
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
            "${CONF_DIR}/hearth-agent.json"
        if [ ! -f "${CONF_DIR}/agent.env" ]; then
            cat > "${CONF_DIR}/agent.env" <<'ENVEOF'
# Environment overrides for hearth-agent (optional — prefer the JSON config file).
# Uncomment and set HEARTH_TOKEN here to keep the token out of the JSON file.
#HEARTH_TOKEN=replace_with_same_token_as_hearthd
ENVEOF
            chmod 0640 "${CONF_DIR}/agent.env"
            ok "Env stub installed: ${CONF_DIR}/agent.env"
        fi
        info "Enabling and starting hearth-agent"
        systemctl enable hearth-agent
        systemctl start hearth-agent
        ok "hearth-agent running (check: systemctl status hearth-agent)"
        echo ""
        echo "Next step: fetch guest kernel + rootfs onto this worker:"
        echo "  sudo ${SCRIPT_DIR}/firecracker-assets.sh"
        echo "Enroll with the hub: hearth-agent --join https://<hub> --join-token <token>  (token from POST /api/v1/join-tokens; ${CONF_DIR}/hearth-agent.json takes over on later boots)"
        ;;
esac

echo ""
info "Installation complete."
