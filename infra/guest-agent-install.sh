#!/usr/bin/env bash
# Install the hearth-guest agent into the base rootfs image on a worker.
#
# Idempotent. Run as root ON A WORKER (the worker holds the base image at
# ${DATA_DIR:-/srv/ignis}/images/ubuntu-base.ext4). Loop-mounts the ext4
# read-write, installs the binary + systemd unit + enable symlink, unmounts,
# then writes the marker ${DATA_DIR}/images/.hearth-guest-v1 NEXT TO the image
# (outside it) — the node agent keys off that marker.
#
# Usage:  guest-agent-install.sh [path-to-hearth-guest]   (default /tmp/hearth-guest)
#
# Safe to re-run. Refuses nothing explicitly: if the image is busy / already
# loop-mounted elsewhere, the mount fails loudly and we abort (set -e).
set -euo pipefail

BIN_SRC="${1:-/tmp/hearth-guest}"
DATA_DIR="${DATA_DIR:-/srv/ignis}"
IMAGE="${DATA_DIR}/images/ubuntu-base.ext4"
MARKER="${DATA_DIR}/images/.hearth-guest-v1"

UNIT_PATH="/etc/systemd/system/hearth-guest.service"
WANTS_LINK="/etc/systemd/system/multi-user.target.wants/hearth-guest.service"

die() { echo "guest-agent-install: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "must run as root"
[ -f "${BIN_SRC}" ] || die "binary not found: ${BIN_SRC}"
[ -f "${IMAGE}" ] || die "base image not found: ${IMAGE}"

MNT="$(mktemp -d /tmp/hearth-guest-mnt.XXXXXX)"

cleanup() {
  # Best-effort unmount + rmdir on any exit path.
  if mountpoint -q "${MNT}"; then
    umount "${MNT}" || true
  fi
  rmdir "${MNT}" 2>/dev/null || true
}
trap cleanup EXIT

echo "==> Loop-mounting ${IMAGE} (rw) at ${MNT}"
# Fails loudly if the image is busy / mounted elsewhere.
mount -o loop,rw "${IMAGE}" "${MNT}"

echo "==> Installing binary -> /usr/local/bin/hearth-guest"
install -D -m 0755 "${BIN_SRC}" "${MNT}/usr/local/bin/hearth-guest"

echo "==> Writing systemd unit -> ${UNIT_PATH}"
install -d -m 0755 "${MNT}/etc/systemd/system"
cat >"${MNT}${UNIT_PATH}" <<'UNIT'
[Unit]
Description=Hearth guest agent (vsock exec + re-IP)

[Service]
ExecStart=/usr/local/bin/hearth-guest
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT
chmod 0644 "${MNT}${UNIT_PATH}"

echo "==> Enabling unit (multi-user.target.wants symlink)"
install -d -m 0755 "${MNT}/etc/systemd/system/multi-user.target.wants"
# Recreate the symlink idempotently; point at the in-image unit path.
ln -sf "${UNIT_PATH}" "${MNT}${WANTS_LINK}"

echo "==> Syncing and unmounting"
sync
umount "${MNT}"
rmdir "${MNT}" 2>/dev/null || true
trap - EXIT

echo "==> Writing marker ${MARKER}"
: >"${MARKER}"

echo "==> Done: hearth-guest installed into base image; marker written."
