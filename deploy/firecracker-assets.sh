#!/usr/bin/env bash
# firecracker-assets.sh — fetch guest kernel + base rootfs for the current arch
# into the Hearth data directory.
#
# Adapts the approach from infra/setup-node.sh: the firecracker-ci S3 bucket
# organises assets by CI version which can lag the GitHub releases, so we
# enumerate the available CI version prefixes and pick the newest one that
# actually has matching kernel and rootfs objects.
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

for cmd in curl jq unsquashfs mkfs.ext4; do
    command -v "${cmd}" >/dev/null 2>&1 || \
        die "Required command not found: ${cmd}
Install the missing tool and retry:
  apt-get install -y squashfs-tools e2fsprogs curl jq"
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
# Discover the newest firecracker-ci S3 version prefix that has artifacts
# for this arch.  The bucket listing uses the S3 list-objects-v2 API
# (no credentials required — the bucket is public).
#
# This mirrors infra/setup-node.sh's approach: the CI artifact version can lag
# the latest GitHub release, so we never hardcode a version.
# ---------------------------------------------------------------------------

S3_BASE="http://spec.ccfc.min.s3.amazonaws.com"

info "Discovering latest firecracker-ci version for ${ARCH}"
CI_V="$(curl -fsSL "${S3_BASE}/?prefix=firecracker-ci/&delimiter=/&list-type=2" \
    | grep -oE 'firecracker-ci/v[0-9]+\.[0-9]+/' \
    | grep -oE 'v[0-9]+\.[0-9]+' \
    | sort -uV \
    | tail -1)"

[ -n "${CI_V}" ] || die "Could not determine firecracker-ci version from S3 listing"
echo "    CI version: ${CI_V}"

# ---------------------------------------------------------------------------
# Prepare directories
# ---------------------------------------------------------------------------

mkdir -p "${KERNELS_DIR}" "${IMAGES_DIR}"

# ---------------------------------------------------------------------------
# Guest kernel
# ---------------------------------------------------------------------------

if [ -s "${VMLINUX}" ]; then
    skip "Guest kernel already present: ${VMLINUX} ($(du -sh "${VMLINUX}" | cut -f1))"
else
    info "Discovering guest kernel object for ${ARCH} (CI ${CI_V})"
    KERNEL_KEY="$(curl -fsSL \
        "${S3_BASE}/?prefix=firecracker-ci/${CI_V}/${ARCH}/vmlinux-&list-type=2" \
        | grep -oE "firecracker-ci/${CI_V}/${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3}" \
        | sort -V \
        | tail -1)"

    [ -n "${KERNEL_KEY}" ] || die "No vmlinux object found under firecracker-ci/${CI_V}/${ARCH}/"
    echo "    kernel key: ${KERNEL_KEY}"

    info "Downloading vmlinux"
    curl -fsSL --progress-bar "https://s3.amazonaws.com/spec.ccfc.min/${KERNEL_KEY}" \
        -o "${VMLINUX}"
    ok "Guest kernel: ${VMLINUX} ($(du -sh "${VMLINUX}" | cut -f1))"
fi

# ---------------------------------------------------------------------------
# Base rootfs (squashfs -> ext4)
# ---------------------------------------------------------------------------

if [ -s "${ROOTFS}" ]; then
    skip "Base rootfs already present: ${ROOTFS} ($(du -sh "${ROOTFS}" | cut -f1))"
else
    info "Discovering Ubuntu squashfs object for ${ARCH} (CI ${CI_V})"
    SQUASH_KEY="$(curl -fsSL \
        "${S3_BASE}/?prefix=firecracker-ci/${CI_V}/${ARCH}/ubuntu-&list-type=2" \
        | grep -oE "firecracker-ci/${CI_V}/${ARCH}/ubuntu-[0-9.]+\.squashfs" \
        | sort -V \
        | tail -1)"

    [ -n "${SQUASH_KEY}" ] || die "No ubuntu squashfs object found under firecracker-ci/${CI_V}/${ARCH}/"
    echo "    rootfs key: ${SQUASH_KEY}"

    SQUASH_TMP="$(mktemp /tmp/hearth-rootfs-XXXXXX.squashfs)"
    WORK_DIR="$(mktemp -d /tmp/hearth-work-XXXXXX)"
    # Ensure cleanup on exit
    # shellcheck disable=SC2064
    trap "rm -rf '${SQUASH_TMP}' '${WORK_DIR}'" EXIT

    info "Downloading Ubuntu squashfs"
    curl -fsSL --progress-bar \
        "https://s3.amazonaws.com/spec.ccfc.min/${SQUASH_KEY}" \
        -o "${SQUASH_TMP}"

    info "Extracting squashfs"
    unsquashfs -q -d "${WORK_DIR}/rootfs" "${SQUASH_TMP}"

    # Allow passwordless root login on the serial console (matches setup-node.sh)
    sed -i 's/^root:[^:]*:/root::/' "${WORK_DIR}/rootfs/etc/passwd" || true

    info "Building ext4 image (2048 MiB) — this takes ~30 s"
    mkfs.ext4 -q -d "${WORK_DIR}/rootfs" "${ROOTFS}" 2048M
    ok "Base rootfs: ${ROOTFS} ($(du -sh "${ROOTFS}" | cut -f1))"

    # trap will clean up SQUASH_TMP and WORK_DIR
fi

echo ""
info "Firecracker assets ready in ${DATA_DIR}"
echo "    kernels/vmlinux         — guest kernel"
echo "    images/ubuntu-base.ext4 — base rootfs (shared, 2 GiB ext4)"
