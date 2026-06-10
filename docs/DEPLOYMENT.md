# Hearth — Production Deployment Guide

Hearth is two static Linux binaries (`hearthd` and `hearth-agent`) and a handful
of shell scripts. This guide covers a production install on generic Linux servers.
For the local Lima dev environment see `deploy/config/local-lab.md`.

---

## Table of contents

1. [Topology](#1-topology)
2. [Prerequisites](#2-prerequisites)
3. [Build the binaries](#3-build-the-binaries)
4. [Install — control plane](#4-install--control-plane)
5. [Install — worker nodes](#5-install--worker-nodes)
6. [Token generation and auth](#6-token-generation-and-auth)
7. [TLS via reverse proxy (Caddy)](#7-tls-via-reverse-proxy-caddy)
8. [Firewall guidance](#8-firewall-guidance)
9. [How local dev and production differ](#9-how-local-dev-and-production-differ)
10. [Upgrade procedure](#10-upgrade-procedure)
11. [Troubleshooting](#11-troubleshooting)

---

## 1. Topology

```
                          HTTPS :443
 Clients ─────────────► Caddy (reverse proxy)
                               │
                               │ HTTP :8080 (loopback / private only)
                               ▼
                         hearthd (control plane)
                         /var/lib/hearth/state.json
                               │
                  ┌────────────┼────────────┐
                  │            │            │
            HTTP :9090   HTTP :9090   HTTP :9090
                  ▼            ▼            ▼
           hearth-agent  hearth-agent  hearth-agent
           (worker 0)    (worker 1)    (worker N)
                  │
          Firecracker × M microVMs
          /dev/kvm, tap, nftables
```

**One control plane, N worker nodes.**

- The control plane runs `hearthd` and holds all state. It does not run
  Firecracker.
- Each worker runs `hearth-agent`, which drives Firecracker processes directly.
- The control plane proxies VM lifecycle calls to the owning agent. If an agent
  is unreachable (heartbeat older than 15 s) it is marked `down` and excluded
  from placement.
- TLS is terminated by a reverse proxy on the control plane host. Agent traffic
  (port 9090) stays on the private network and does not require TLS, though it
  is protected by the shared bearer token.

---

## 2. Prerequisites

### Control plane host

| Requirement | Notes |
|---|---|
| Linux x86_64 or aarch64 | Any modern distro (Debian 12+, Ubuntu 22.04+, Fedora 38+, RHEL 9+) |
| Caddy 2 (or nginx) | TLS termination |
| Port 443 (or 80) open to clients | Caddy handles ACME |
| Port 8080 NOT exposed publicly | Caddy proxies to it on loopback |

### Worker hosts

| Requirement | Notes |
|---|---|
| Linux x86_64 or aarch64 | Same distro constraint |
| `/dev/kvm` present | Bare-metal or KVM-enabled VM (nested virt on cloud providers) |
| `firecracker` in PATH | Installer fetches it automatically if missing |
| `nft` (nftables) | Guest networking — `apt install nftables` / `dnf install nftables` |
| `ip` (iproute2) | Bridge and tap management |
| Port 9090 accessible from the control plane | Private network only; not public |
| `squashfs-tools`, `e2fsprogs` | Only needed once to build the base rootfs |

Verify KVM is available on a worker:

```sh
ls -la /dev/kvm          # must exist
sudo dmesg | grep -i kvm # should show KVM enabled
```

---

## 3. Build the binaries

Builds run inside the `infra-saas-lab` Lima VM (Go 1.26 + Rust 1.96 toolchains —
never on the macOS host). See `docs/adr/ADR-0003-go-rust-port.md` for why the
control plane is Go and the agent is Rust.

```sh
REPO=/Users/magdy/projects/github.com/alpham/infra-saas

# hearthd (Go, static, cross-compiles to both arches from one box)
limactl shell infra-saas-lab -- bash -c \
  "export PATH=\$PATH:/usr/local/go/bin && cd $REPO/go && \
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/x86_64/hearthd ./cmd/hearthd && \
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/aarch64/hearthd ./cmd/hearthd"

# hearth-agent (Rust, static musl)
limactl shell infra-saas-lab -- bash -c \
  "source ~/.cargo/env && export CARGO_TARGET_DIR=\$HOME/.cargo-target/hearth-agent && \
   cd $REPO/rust/agent && cargo build --release --target aarch64-unknown-linux-musl && \
   cp \$CARGO_TARGET_DIR/aarch64-unknown-linux-musl/release/hearth-agent \
      $REPO/deploy/release/aarch64/hearth-agent"
```

For x86_64 agents, add the target (`rustup target add x86_64-unknown-linux-musl`
plus a cross linker) or simply build on an x86_64 machine — the agent has no
non-Rust dependencies, so `cargo build --release --target x86_64-unknown-linux-musl`
on the target arch is the path of least resistance.

The installer finds binaries at `deploy/release/<arch>/`.

> **Systemd note**: the units in `deploy/systemd/` were hardened against the
> Zig binaries and are not yet soak-tested under the Go/Rust runtimes (the lab
> runs processes directly). On first production install, watch
> `journalctl -u hearthd -u hearth-agent` for seccomp kills — in particular
> `MemoryDenyWriteExecute=true` in `hearthd.service` vs the Go runtime — and
> relax the specific directive if needed.

---

## 4. Install — control plane

Run on the control plane host as root:

```sh
# Copy the repo (or just the deploy/ subtree) to the server, then:
sudo bash deploy/install.sh control-plane
```

The installer:

1. Creates directories: `/etc/hearth`, `/var/lib/hearth`, `/srv/hearth`, `/usr/share/hearth/ui`.
2. Creates the `hearth` system user (no login shell, no home).
3. Copies `hearthd` to `/usr/local/bin/hearthd`.
4. Installs `deploy/systemd/hearthd.service` and enables it.
5. Writes config stubs at `/etc/hearth/hearthd.json` and `/etc/hearth/hearthd.env`
   (only if they do not already exist).

**Edit the config before starting the service:**

```sh
# Generate a token (see §6)
TOKEN=$(openssl rand -hex 32)

# /etc/hearth/hearthd.json
sudo tee /etc/hearth/hearthd.json <<EOF
{
  "bind": "0.0.0.0:8080",
  "state_path": "/var/lib/hearth/state.json",
  "ui_dir": "/usr/share/hearth/ui",
  "token": "${TOKEN}"
}
EOF
sudo chmod 0640 /etc/hearth/hearthd.json
sudo chown hearth:hearth /etc/hearth/hearthd.json

# Keep a copy of the token for the worker configs and Caddy auth header
echo "Token: ${TOKEN}"
```

Then start the service:

```sh
sudo systemctl start hearthd
sudo systemctl status hearthd

# Smoke test (from the server — port 8080 should be localhost-only after §7)
curl -s http://127.0.0.1:8080/healthz   # {"ok":true}
curl -s http://127.0.0.1:8080/metrics   # Prometheus text (no auth)
```

---

## 5. Install — worker nodes

Run on each worker as root:

```sh
sudo bash deploy/install.sh worker
```

The installer:

1. Checks `/dev/kvm`, fetches Firecracker if missing, checks `nft`.
2. Copies `hearth-agent` to `/usr/local/bin/hearth-agent`.
3. Installs `deploy/systemd/hearth-agent.service` and enables it.
4. Writes config stubs at `/etc/hearth/hearth-agent.json` and `/etc/hearth/agent.env`.

**Edit the config before starting the service:**

```sh
# /etc/hearth/hearth-agent.json
sudo tee /etc/hearth/hearth-agent.json <<EOF
{
  "bind": "0.0.0.0:9090",
  "control_plane": "https://hearth.internal.example.com",
  "advertise_addr": "$(hostname -I | awk '{print $1}')",
  "data_dir": "/srv/hearth",
  "token": "PASTE_YOUR_TOKEN_HERE",
  "pool_size": 2,
  "net": "on",
  "net_cidr": "10.231.0.0/24"
}
EOF
sudo chmod 0640 /etc/hearth/hearth-agent.json
```

Then fetch guest assets (kernel + rootfs) — only needed once per worker:

```sh
sudo bash deploy/firecracker-assets.sh --data-dir /srv/hearth
```

Then start the agent:

```sh
sudo systemctl start hearth-agent
sudo systemctl status hearth-agent
```

Verify the worker is registered on the control plane within ~5 s:

```sh
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.internal.example.com/api/v1/nodes
# Should show the worker with status "ready"
```

---

## 6. Token generation and auth

Hearth uses a single shared bearer token between the control plane and all agents.

Generate a token:

```sh
openssl rand -hex 32
# e.g. a3f8c21b0e4d7f69a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901234ab
```

Set it in:

- `/etc/hearth/hearthd.json` — `"token"` key (or `HEARTH_TOKEN` env var)
- `/etc/hearth/hearth-agent.json` on every worker — same `"token"` value

When `token` is set on `hearthd`:

- Every `/api/*` request requires `Authorization: Bearer <token>`.
- Agent registration and heartbeat requests require the token.
- `/healthz`, `/metrics`, and static UI files are always open (no token).
- Comparison is constant-time (no timing attacks).

The UI reads the token from `localStorage.hearth_token` or from the `?token=`
query parameter (which stores it and strips it from the URL). On a 401 the UI
shows a non-blocking banner prompting for the token.

**Keep the token out of version control.** Store it in `/etc/hearth/hearthd.env`
or `/etc/hearth/agent.env` (mode 0640, owned by the service user) if you prefer
not to put it in the JSON config:

```sh
# /etc/hearth/hearthd.env
HEARTH_TOKEN=a3f8c21b...

# /etc/hearth/agent.env
HEARTH_TOKEN=a3f8c21b...
```

Env vars take precedence over config file values (precedence: flags > env > config > defaults).

---

## 7. TLS via reverse proxy (Caddy)

Hearth does not terminate TLS itself. Run Caddy on the control plane host.

Install Caddy (Debian/Ubuntu):

```sh
apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | tee /etc/apt/sources.list.d/caddy-stable.list
apt-get update && apt-get install -y caddy
```

`/etc/caddy/Caddyfile`:

```caddy
hearth.example.com {
    # Caddy obtains and renews a Let's Encrypt certificate automatically.
    # Replace with your real domain.

    # Reverse-proxy everything to hearthd on loopback.
    reverse_proxy 127.0.0.1:8080

    # Optional: rate-limit the API to protect against token brute-force.
    # Requires the caddy-ratelimit module.
    # rate_limit {
    #     zone api {
    #         key    {remote_host}
    #         events 200
    #         window 1m
    #     }
    #     match path /api/*
    # }

    # Logging
    log {
        output file /var/log/caddy/hearth-access.log
    }
}
```

Reload Caddy:

```sh
systemctl reload caddy
# Or test the config first:
caddy validate --config /etc/caddy/Caddyfile
```

Caddy handles ACME (Let's Encrypt / ZeroSSL) automatically when port 443 and
port 80 (HTTP-01 challenge) are reachable from the internet, or use `tls
internal` for a local CA if the control plane is not internet-facing.

---

## 8. Firewall guidance

```
                     Internet
                         │
                   Port 443/80  ← open to clients
                         │
                    [ Caddy ]
                         │
                   Port 8080    ← loopback ONLY (127.0.0.1)
                         │
                   [ hearthd ]
                         │
          Private network (e.g. 10.0.1.0/24)
                         │
                   Port 9090    ← control plane → workers ONLY
                         │
                [ hearth-agent ]
                         │
                   /dev/kvm, tap
                         │
                 [ Firecracker ]
```

nftables example for the control plane host:

```sh
# Allow inbound 443/80 from anywhere; block 8080 from outside
nft add rule inet filter input tcp dport 8080 ip saddr != 127.0.0.1 drop

# Or with ufw (Ubuntu):
ufw allow 443/tcp
ufw allow 80/tcp
ufw deny 8080/tcp
```

nftables example for a worker host:

```sh
# Allow port 9090 only from the control plane IP
nft add rule inet filter input tcp dport 9090 ip saddr != 10.0.1.10 drop

# Or with ufw:
ufw allow from 10.0.1.10 to any port 9090
ufw deny 9090
```

Replace `10.0.1.10` with your control plane's private IP.

---

## 9. How local dev and production differ

The binaries are identical. Only the config values change.

| Setting | Lima lab | Production |
|---|---|---|
| `control_plane` | `http://192.168.104.3:8080` | `https://hearth.internal.example.com` |
| `advertise_addr` | `192.168.104.x` (user-v2 network) | `10.0.1.x` (private DC IP) |
| `token` | omitted (no auth in lab) | 64-hex-char secret |
| TLS | none (L2-isolated Lima network) | Caddy terminates HTTPS |
| `pool_size` | `0` (lab is too small) | `2`–`5` per node |
| `state_path` | `/var/lib/hearth/state.json` | same |
| `data_dir` | `/srv/hearth` | same |
| systemd | not used in lab | `hearthd.service` / `hearth-agent.service` |

The Lima lab uses the same JSON config structure. See `deploy/config/local-lab.md`
for the exact values used in each Lima VM.

---

## 10. Upgrade procedure

Hearth upgrades are binary-replacement restarts. **Sleeping VMs survive agent
restarts** (the Firecracker process is already stopped when a VM is sleeping;
the snapshot on disk is untouched by a restart).

### Control plane upgrade

```sh
# 1. Build new binaries (see §3 — Go build inside the toolchain VM)

# 2. Copy the new binary to the server
scp deploy/release/x86_64/hearthd root@cp-host:/usr/local/bin/hearthd.new

# 3. Atomic replace + restart (state.json is preserved)
ssh root@cp-host "
  install -m 0755 /usr/local/bin/hearthd.new /usr/local/bin/hearthd
  systemctl restart hearthd
  systemctl status hearthd --no-pager
"
```

### Worker upgrade

```sh
# 1. Build new hearth-agent binary (see §3 — Rust build inside the toolchain VM)

# 2. Copy to worker
scp deploy/release/x86_64/hearth-agent root@worker-host:/usr/local/bin/hearth-agent.new

# 3. Atomic replace + restart
#    Running VMs are unaffected — Firecracker children are independent processes.
#    Sleeping VMs are safe — their snapshot is on disk; the agent re-discovers
#    them from meta.json on restart (this is the contract from API-V2.md §6).
ssh root@worker-host "
  install -m 0755 /usr/local/bin/hearth-agent.new /usr/local/bin/hearth-agent
  systemctl restart hearth-agent
  systemctl status hearth-agent --no-pager
"
```

The worker re-registers with the control plane within ~5 s after restart.
Running sandboxes remain in the `running` state because their Firecracker
processes are independent of the agent process.

### Rolling upgrades (multiple workers)

Upgrade workers one at a time. Before restarting an agent, optionally drain it:

```sh
# The scheduler will not place new VMs on a down node.
# No built-in drain command in v2 — simply stop the agent,
# upgrade, restart; in-flight creates will fail with 503 and
# the client should retry (the control plane will choose another
# ready worker).
```

---

## 11. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `systemctl status hearthd` shows `failed` | Binary not found or config parse error | Check `journalctl -u hearthd -n 50`; verify `/usr/local/bin/hearthd` exists and is executable |
| `GET /healthz` returns `connection refused` | hearthd not listening | Check `bind` in config (default `0.0.0.0:8080`); check firewall |
| `GET /api/v1/nodes` returns `401` | Token mismatch or missing `Authorization` header | Verify `token` in hearthd config matches the `Bearer` value you're sending |
| Worker shows `status: down` in node list | Agent heartbeat older than 15 s | Check `systemctl status hearth-agent` on the worker; check connectivity on port 9090 |
| Worker never appears in node list | `control_plane` URL wrong or token mismatch | Check agent logs: `journalctl -u hearth-agent -n 50`; verify `control_plane` points to hearthd and token matches |
| Sandbox stays in `creating` | Firecracker spawn failed on worker | Check `journalctl -u hearth-agent -n 100`; check `/dev/kvm` permissions; check `firecracker --version` |
| `POST /sandboxes` returns `503 no ready node` | No workers registered and ready | Fix the worker install and verify it reaches the control plane |
| Guest networking not working (`ip: null`) | `net` is `off` or agent config missing | Set `"net": "on"` and `"net_cidr"` in agent config; verify `nft` and `ip` are installed |
| Bridge `hearth0` missing after agent restart | Idempotent setup should recreate it | Agent recreates the bridge at startup; check `journalctl -u hearth-agent` for errors |
| `POST /sandboxes/{id}/wake` is slow (> 500 ms) | No warm pool VMs available | Increase `pool_size` in agent config (requires restart); check `hearth_pool_size` metric |
| `POST /sandboxes/{id}/fork` returns `501` | Running v1 binary | Upgrade to v2 binary |
| TLS cert errors | Caddy cannot reach ACME or wrong domain | Check `journalctl -u caddy`; verify DNS and port 80/443 reachability |
| `journalctl -u hearthd` shows `Permission denied` on state file | Wrong ownership of `/var/lib/hearth` | `chown -R hearth:hearth /var/lib/hearth` |

### Useful commands

```sh
# Control plane logs (live)
journalctl -u hearthd -f

# Worker agent logs (live)
journalctl -u hearth-agent -f

# Prometheus metrics (no auth required)
curl -s http://127.0.0.1:8080/metrics | grep hearth_

# List all nodes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/nodes | jq .

# List all sandboxes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/sandboxes | jq .

# Check warm pool gauge on a specific node
curl -s http://127.0.0.1:8080/metrics | grep hearth_pool_size

# Check a sleeping sandbox's wake latency
curl -s -X POST -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/sandboxes/sb-abc123/wake | jq .wake_ms
```
