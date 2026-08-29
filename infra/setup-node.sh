#!/usr/bin/env bash
# Ignis node setup: installs Firecracker + guest kernel/rootfs on a Lima VM.
# Run inside the VM:  bash setup-node.sh
set -euo pipefail

ARCH="$(uname -m)"
IGNIS_DIR=/srv/ignis

# Everything downloaded here ends up as the hypervisor, the guest kernel or the
# base filesystem under every sandbox on this node, so versions and digests are
# pinned rather than resolved at run time. Keep these in step with
# deploy/firecracker-assets.sh and deploy/install.sh, which carry the same pins
# and document how to refresh them.
FC_V=v1.16.1
CI_V=v1.15
KERNEL_NAME=vmlinux-6.1.155
ROOTFS_NAME=ubuntu-24.04.squashfs
# Path-style HTTPS: the bucket name contains dots, so the virtual-host form
# (https://spec.ccfc.min.s3.amazonaws.com) has no matching certificate.
S3_BASE=https://s3.amazonaws.com/spec.ccfc.min

pinned_sha256() {  # <what>
  case "$1" in
    firecracker/aarch64)           echo 8d0e69f6d6f9a1724551f607f18504052c16c1828ee3d4d7b6e6c73380871e0e ;;
    firecracker/x86_64)            echo 382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6 ;;
    aarch64/vmlinux-6.1.155)       echo e3544b10603acbf3db492cb52e000d22ba202cb4b63b9add027565683e11c591 ;;
    aarch64/ubuntu-24.04.squashfs) echo 0efb6a3ff2982baa6ca7e3d940966516ba7ddd2df5deb3e6c2161d369a15d608 ;;
    x86_64/vmlinux-6.1.155)        echo e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2 ;;
    x86_64/ubuntu-24.04.squashfs)  echo 68321e0482baeb3844dafe8a6b08a6902401a7afc41fbfd8c3d9ea08aadd244f ;;
  esac
}

verify_sha256() {  # <file> <pin key>
  local want got
  want=$(pinned_sha256 "$2")
  [ -n "$want" ] || { echo "No pinned SHA-256 for $2" >&2; exit 1; }
  got=$(sha256sum "$1" | cut -d' ' -f1)
  [ "$got" = "$want" ] || {
    echo "SHA-256 mismatch on $2" >&2
    echo "  expected: $want" >&2
    echo "  got:      $got" >&2
    exit 1
  }
  echo "    sha256 ok"
}

# A private work dir, never a fixed /tmp name: any local user can pre-create
# /tmp/fc.tgz as a symlink to a root-owned file and have `curl -o` (run under
# sudo below) write through it.
WORK=$(mktemp -d)
trap 'sudo rm -rf "$WORK"' EXIT

echo "==> Installing prerequisites"
sudo apt-get update -qq >/dev/null
sudo apt-get install -y -qq curl squashfs-tools e2fsprogs iproute2 iptables coreutils >/dev/null

echo "==> Installing Firecracker ${FC_V}"
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_V}/firecracker-${FC_V}-${ARCH}.tgz" -o "${WORK}/fc.tgz"
verify_sha256 "${WORK}/fc.tgz" "firecracker/${ARCH}"
tar -xzf "${WORK}/fc.tgz" -C "${WORK}"
sudo install "${WORK}/release-${FC_V}-${ARCH}/firecracker-${FC_V}-${ARCH}" /usr/local/bin/firecracker
firecracker --version | head -1

echo "==> Granting KVM access to $(whoami)"
sudo usermod -aG kvm "$(whoami)"

echo "==> Preparing ${IGNIS_DIR}"
sudo mkdir -p ${IGNIS_DIR}/{kernels,images,instances,bin}
sudo chown -R "$(whoami)" ${IGNIS_DIR}

echo "==> Downloading guest kernel (firecracker-ci ${CI_V}/${ARCH}/${KERNEL_NAME})"
# The CI version and object key used to come from a cleartext HTTP bucket
# listing, which let anyone on the path downgrade the fleet to a guest kernel
# with published local-privesc and VM-escape CVEs — the artifact still came
# from the real bucket over HTTPS, so nothing looked wrong. Verify before the
# kernel is installed.
curl -fsSL "${S3_BASE}/firecracker-ci/${CI_V}/${ARCH}/${KERNEL_NAME}" -o "${WORK}/vmlinux"
verify_sha256 "${WORK}/vmlinux" "${ARCH}/${KERNEL_NAME}"
install -m 0644 "${WORK}/vmlinux" ${IGNIS_DIR}/kernels/vmlinux
ls -lh ${IGNIS_DIR}/kernels/vmlinux

echo "==> Downloading Ubuntu rootfs (squashfs) and building base ext4 image"
curl -fsSL "${S3_BASE}/firecracker-ci/${CI_V}/${ARCH}/${ROOTFS_NAME}" -o "${WORK}/ubuntu.squashfs"
verify_sha256 "${WORK}/ubuntu.squashfs" "${ARCH}/${ROOTFS_NAME}"

sudo unsquashfs -q -d "${WORK}/rootfs" "${WORK}/ubuntu.squashfs"

# allow passwordless root login on the serial console for demo workloads
sudo sed -i 's/^root:[^:]*:/root::/' "${WORK}/rootfs/etc/passwd" || true

sudo mkfs.ext4 -q -d "${WORK}/rootfs" ${IGNIS_DIR}/images/ubuntu-base.ext4 2048M
sudo chown "$(whoami)" ${IGNIS_DIR}/images/ubuntu-base.ext4
ls -lh ${IGNIS_DIR}/images/ubuntu-base.ext4

echo "==> Node setup complete"
