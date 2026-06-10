#!/usr/bin/env bash
# Ignis node setup: installs Firecracker + guest kernel/rootfs on a Lima VM.
# Run inside the VM:  bash setup-node.sh
set -euo pipefail

ARCH="$(uname -m)"
IGNIS_DIR=/srv/ignis

echo "==> Installing prerequisites"
sudo apt-get update -qq >/dev/null
sudo apt-get install -y -qq jq curl squashfs-tools e2fsprogs iproute2 iptables >/dev/null

echo "==> Installing Firecracker"
TAG=$(curl -fsSL https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | jq -r .tag_name)
echo "    latest release: ${TAG}"
curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${TAG}/firecracker-${TAG}-${ARCH}.tgz" -o /tmp/fc.tgz
tar -xzf /tmp/fc.tgz -C /tmp
sudo install "/tmp/release-${TAG}-${ARCH}/firecracker-${TAG}-${ARCH}" /usr/local/bin/firecracker
firecracker --version | head -1

echo "==> Granting KVM access to $(whoami)"
sudo usermod -aG kvm "$(whoami)"

echo "==> Preparing ${IGNIS_DIR}"
sudo mkdir -p ${IGNIS_DIR}/{kernels,images,instances,bin}
sudo chown -R "$(whoami)" ${IGNIS_DIR}

# CI artifact folders can lag releases; pick the newest folder that actually exists
CI_V=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/&delimiter=/&list-type=2" \
  | grep -oE 'firecracker-ci/v[0-9]+\.[0-9]+/' | grep -oE 'v[0-9]+\.[0-9]+' | sort -uV | tail -1)
echo "    using CI artifacts: ${CI_V}"

echo "==> Downloading guest kernel (firecracker-ci ${CI_V})"
KERNEL_KEY=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/${CI_V}/${ARCH}/vmlinux-&list-type=2" \
  | grep -oE "firecracker-ci/${CI_V}/${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3}" | sort -V | tail -1)
echo "    kernel: ${KERNEL_KEY}"
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/${KERNEL_KEY}" -o ${IGNIS_DIR}/kernels/vmlinux
ls -lh ${IGNIS_DIR}/kernels/vmlinux

echo "==> Downloading Ubuntu rootfs (squashfs) and building base ext4 image"
SQUASH_KEY=$(curl -fsSL "http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/${CI_V}/${ARCH}/ubuntu-&list-type=2" \
  | grep -oE "firecracker-ci/${CI_V}/${ARCH}/ubuntu-[0-9.]+\.squashfs" | sort -V | tail -1)
echo "    rootfs: ${SQUASH_KEY}"
curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/${SQUASH_KEY}" -o /tmp/ubuntu.squashfs

WORK=$(mktemp -d)
sudo unsquashfs -q -d "${WORK}/rootfs" /tmp/ubuntu.squashfs

# allow passwordless root login on the serial console for demo workloads
sudo sed -i 's/^root:[^:]*:/root::/' "${WORK}/rootfs/etc/passwd" || true

sudo mkfs.ext4 -q -d "${WORK}/rootfs" ${IGNIS_DIR}/images/ubuntu-base.ext4 2048M
sudo chown "$(whoami)" ${IGNIS_DIR}/images/ubuntu-base.ext4
sudo rm -rf "${WORK}" /tmp/ubuntu.squashfs /tmp/fc.tgz
ls -lh ${IGNIS_DIR}/images/ubuntu-base.ext4

echo "==> Node setup complete"
