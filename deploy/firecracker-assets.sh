#!/usr/bin/env bash
# firecracker-assets.sh — fetch guest kernel + base rootfs for the current arch
# into the Hearth data directory.
#
# The firecracker-ci S3 objects are pinned by version and digest (see the pin
# block below). They used to be discovered at run time from a cleartext HTTP
# bucket listing, which let anyone on the path choose which guest kernel the
# fleet booted its tenants on.
#
# Usage (run as root or a user that can write to DATA_DIR):
#   ./firecracker-assets.sh [--data-dir /srv/hearth]
#
# The script is idempotent: it skips the download if the target file already
# exists and is non-empty.

set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }
ok()   { echo "    [ok] $*"; }
skip() { echo "    [skip] $*"; }

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

DATA_DIR=/srv/hearth

while [[ $# -gt 0 ]]; do
    case "$1" in
        --data-dir)
            DATA_DIR="$2"
            shift 2
            ;;
        --data-dir=*)
            DATA_DIR="${1#*=}"
            shift
            ;;
        -h|--help)
            grep '^#' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            die "Unknown argument: $1  (try --help)"
            ;;
    esac
done

KERNELS_DIR="${DATA_DIR}/kernels"
IMAGES_DIR="${DATA_DIR}/images"
VMLINUX="${KERNELS_DIR}/vmlinux"
ROOTFS="${IMAGES_DIR}/ubuntu-base.ext4"

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------

for cmd in curl sha256sum unsquashfs mkfs.ext4; do
    command -v "${cmd}" >/dev/null 2>&1 || \
        die "Required command not found: ${cmd}
Install the missing tool and retry:
  apt-get install -y squashfs-tools e2fsprogs curl coreutils"
done

# ---------------------------------------------------------------------------
# Architecture detection
# ---------------------------------------------------------------------------

ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64|aarch64) ;;
    arm64) ARCH=aarch64 ;;
    *) die "Unsupported architecture: ${ARCH}" ;;
esac

# ---------------------------------------------------------------------------
# Guest asset pins
# ---------------------------------------------------------------------------
# These objects become the kernel and the base filesystem of every tenant
# sandbox on this worker, so both the version and the bytes are pinned. The
# previous run-time discovery asked a cleartext HTTP bucket listing which CI
# prefix to use, and the greps constrained the shape of the key but not its
# value — an on-path rewrite pinned the fleet to an old prefix carrying a guest
# kernel with published local-privesc and VM-escape CVEs, and the artifact then
# downloaded cleanly over HTTPS with nothing comparing a digest.
#
# To move the pins: list the bucket over HTTPS in PATH style — the bucket name
# contains dots, so `https://spec.ccfc.min.s3.amazonaws.com` has no matching
# certificate — pick the objects, and record their digests:
#   B=https://s3.amazonaws.com/spec.ccfc.min
#   curl -fsSL "$B/?prefix=firecracker-ci/&delimiter=/&list-type=2"
#   curl -fsSL "$B/?prefix=firecracker-ci/v1.15/aarch64/&list-type=2"
#   curl -fsSL "$B/firecracker-ci/v1.15/aarch64/vmlinux-6.1.155" | sha256sum
# HEARTH_CI_KERNEL_SHA256 / HEARTH_CI_ROOTFS_SHA256 override a single digest
# alongside HEARTH_CI_VERSION + HEARTH_CI_KERNEL / HEARTH_CI_ROOTFS.

S3_BASE="https://s3.amazonaws.com/spec.ccfc.min"

CI_V="${HEARTH_CI_VERSION:-v1.15}"
KERNEL_NAME="${HEARTH_CI_KERNEL:-vmlinux-6.1.155}"
ROOTFS_NAME="${HEARTH_CI_ROOTFS:-ubuntu-24.04.squashfs}"

pinned_sha256() {  # <arch>/<object name>
    case "$1" in
        aarch64/vmlinux-6.1.155)       echo e3544b10603acbf3db492cb52e000d22ba202cb4b63b9add027565683e11c591 ;;
        aarch64/ubuntu-24.04.squashfs) echo 0efb6a3ff2982baa6ca7e3d940966516ba7ddd2df5deb3e6c2161d369a15d608 ;;
        x86_64/vmlinux-6.1.155)        echo e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2 ;;
        x86_64/ubuntu-24.04.squashfs)  echo 68321e0482baeb3844dafe8a6b08a6902401a7afc41fbfd8c3d9ea08aadd244f ;;
    esac
}

# fetch_verified <object name> <override digest> <destination>
# Downloads into the private work dir and refuses to hand back anything whose
# digest does not match, so an unverified kernel never reaches KERNELS_DIR and
# an unverified squashfs is never extracted as root.
fetch_verified() {
    local name="$1" override="$2" dest="$3"
    local want got

    want="${override:-$(pinned_sha256 "${ARCH}/${name}")}"
    [ -n "${want}" ] || die "No pinned SHA-256 for ${ARCH}/${name}.
Record one (see the pin block in this script) or pass it in the matching
HEARTH_CI_*_SHA256 variable — an unverified guest kernel or rootfs is not
something this installer will place under a tenant."

    echo "    object: firecracker-ci/${CI_V}/${ARCH}/${name}"
    curl -fsSL --progress-bar "${S3_BASE}/firecracker-ci/${CI_V}/${ARCH}/${name}" -o "${dest}"

    got="$(sha256sum "${dest}" | cut -d' ' -f1)"
    if [ "${got}" != "${want}" ]; then
        die "SHA-256 mismatch on firecracker-ci/${CI_V}/${ARCH}/${name}
  expected: ${want}
  got:      ${got}"
    fi
    ok "SHA-256 verified"
}

# ---------------------------------------------------------------------------
# Prepare directories and a private work area
# ---------------------------------------------------------------------------

mkdir -p "${KERNELS_DIR}" "${IMAGES_DIR}"

WORK_DIR="$(mktemp -d)"
# shellcheck disable=SC2064
trap "rm -rf '${WORK_DIR}'" EXIT

# ---------------------------------------------------------------------------
# Guest kernel
# ---------------------------------------------------------------------------

if [ -s "${VMLINUX}" ]; then
    skip "Guest kernel already present: ${VMLINUX} ($(du -sh "${VMLINUX}" | cut -f1))"
else
    info "Downloading guest kernel for ${ARCH} (CI ${CI_V})"
    fetch_verified "${KERNEL_NAME}" "${HEARTH_CI_KERNEL_SHA256:-}" "${WORK_DIR}/vmlinux"
    install -m 0644 "${WORK_DIR}/vmlinux" "${VMLINUX}"
    ok "Guest kernel: ${VMLINUX} ($(du -sh "${VMLINUX}" | cut -f1))"
fi

# ---------------------------------------------------------------------------
# Base rootfs (squashfs -> ext4)
# ---------------------------------------------------------------------------

if [ -s "${ROOTFS}" ]; then
    skip "Base rootfs already present: ${ROOTFS} ($(du -sh "${ROOTFS}" | cut -f1))"
else
    info "Downloading Ubuntu squashfs for ${ARCH} (CI ${CI_V})"
    fetch_verified "${ROOTFS_NAME}" "${HEARTH_CI_ROOTFS_SHA256:-}" "${WORK_DIR}/rootfs.squashfs"

    info "Extracting squashfs"
    unsquashfs -q -d "${WORK_DIR}/rootfs" "${WORK_DIR}/rootfs.squashfs"

    # Allow passwordless root login on the serial console (matches setup-node.sh)
    sed -i 's/^root:[^:]*:/root::/' "${WORK_DIR}/rootfs/etc/passwd" || true

    info "Building ext4 image (2048 MiB) — this takes ~30 s"
    mkfs.ext4 -q -d "${WORK_DIR}/rootfs" "${ROOTFS}" 2048M
    ok "Base rootfs: ${ROOTFS} ($(du -sh "${ROOTFS}" | cut -f1))"

    # trap will clean up WORK_DIR
fi

echo ""
info "Firecracker assets ready in ${DATA_DIR}"
echo "    kernels/vmlinux         — guest kernel"
echo "    images/ubuntu-base.ext4 — base rootfs (shared, 2 GiB ext4)"
