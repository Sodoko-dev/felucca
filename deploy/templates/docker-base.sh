#!/bin/sh
# docker-base provision script — Docker Engine + compose v2 preinstalled so
# Sodoko-style docker-compose stacks run unchanged inside the guest.
# Runs inside the builder guest via scripts/build-template.sh.
set -eu
export DEBIAN_FRONTEND=noninteractive

# The FC CI guest image ships a resolv.conf pointing at a resolver that
# doesn't exist in this topology. rm first: on images where it is the
# systemd-resolved stub SYMLINK, a bare redirect would write through into
# tmpfs /run (lost at capture) or fail ENOENT under set -e.
rm -f /etc/resolv.conf
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > /etc/resolv.conf

apt-get update -qq
apt-get install -y -qq --no-install-recommends docker.io docker-compose-v2

# The Firecracker guest kernel has legacy xtables (IP_NF_*) but no nf_tables;
# Ubuntu's default iptables-nft backend fails with "Protocol not supported".
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy 2>/dev/null || true
# Docker 27+ programs ip6tables by default; the guest kernel has no IPv6
# netfilter — keep the daemon IPv4-only. The kernel also lacks the iptables
# `raw` table, which Docker 28+'s direct-access DROP rules need:
# nat-unprotected skips them. Inside a single-tenant microVM that guard is
# moot — inter-guest isolation is the host's nft job (ADR-0005).
mkdir -p /etc/docker
cat > /etc/docker/daemon.json <<'JSON'
{
  "ip6tables": false,
  "default-network-opts": {
    "bridge": { "com.docker.network.bridge.gateway_mode_ipv4": "nat-unprotected" }
  }
}
JSON

# The default docker0 bridge ignores default-network-opts (its options are
# fixed at daemon bootstrap) and needs the raw table this kernel lacks —
# pre-create a "felucca" network with the working gateway mode. It persists
# in /var/lib/docker, so every sandbox built from this template has it:
#   docker run --network felucca ...
# Compose project networks pick the mode up from default-network-opts.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl start docker
  # Idempotent: re-provisioning over a derived image keeps the network.
  docker network inspect felucca >/dev/null 2>&1 || \
    docker network create -o com.docker.network.bridge.gateway_mode_ipv4=nat-unprotected felucca
  systemctl stop docker docker.socket 2>/dev/null || true
fi
# (Non-systemd guests: the rc.local fallback starts dockerd at boot but the
# "felucca" network must be created on first use — this builder flow only
# pre-bakes it where dockerd can be cleanly started and stopped.)

# Start at boot whichever init the guest runs.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl enable docker
else
  # Non-systemd init: a minimal rc hook keeps the template self-starting.
  mkdir -p /etc/rc.local.d
  cat > /etc/rc.local <<'EOF'
#!/bin/sh
[ -x /usr/bin/dockerd ] && nohup dockerd >/var/log/dockerd.log 2>&1 &
exit 0
EOF
  chmod +x /etc/rc.local
fi

# Trim the apt cache — the rootfs becomes the template image.
apt-get clean
rm -rf /var/lib/apt/lists/*

docker --version
docker compose version
