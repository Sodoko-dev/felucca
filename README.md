# Hearth

Self-hosted infrastructure SaaS that manages **Firecracker microVMs for AI workloads** —
sub-second sandbox wake, warm pools, fork/branch, and a clean dashboard. Built as two
static binaries — a **Go** control plane and a **Rust** node agent — with no Kubernetes
on the control path (see `docs/adr/ADR-0003-go-rust-port.md`).

Conceptually the same product category as [Modal](https://modal.com), [Northflank](https://northflank.com)
and [Sprites](https://sprites.dev) (see `docs/research/`), self-hosted on your own machines.

## Components

| Component | Path | Runs on | Role |
|---|---|---|---|
| `hearthd` | `go/` | `infra-saas-lab` VM | Control plane (Go): REST API `:8080`, scheduler, node registry, state, serves the UI |
| `hearth-agent` | `rust/agent/` | `kata-lab-0`, `kata-lab-1` VMs | Node agent (Rust) `:9090`: drives Firecracker over its unix-socket REST API |
| UI | `ui/` | served by hearthd | Static SPA dashboard (fleet, sandboxes, fork tree) |
| Conformance | `test/conformance/` | any VM | Executable form of the API contract; goldens recorded from the v2 reference |
| Node setup | `infra/setup-node.sh` | workers | Installs Firecracker + guest kernel + base rootfs into `/srv/ignis` |

## Lab topology (Lima VMs on macOS — nothing runs on the host)

- `infra-saas-lab` — control plane + toolchain box: Go 1.26, Rust 1.96 (Ubuntu 24.04 aarch64, 8 CPU / 8 GiB)
- `kata-lab-0`, `kata-lab-1` — workers with `/dev/kvm`, Firecracker v1.16.0, guest kernel + ubuntu base rootfs in `/srv/ignis`
- Inter-VM traffic uses Lima's `user-v2` network (`192.168.104.0/24`); Apple vzNAT does **not** allow guest-to-guest traffic.

## Docs

- `docs/CHANGELOG.md` — full project history: v1 → v2 → Go+Rust migration → v3.1, with findings
- `docs/PLAN.md` — adopted architecture + phased milestones (source of truth)
- `docs/API-V2.md` — the frozen wire/config contract; `test/conformance/` enforces it
- `docs/API-V3-EXEC.md` — v3.1 contract: vsock exec + fork re-IP (guest agent protocol)
- `docs/ARCHITECTURE.md` — full architecture with diagrams (`docs/architecture-preview.html` to view)
- `docs/DEPLOYMENT.md` — production deployment, builds, upgrades
- `docs/adr/` — ADR-0000 (original plan), ADR-0001 (Zig, superseded by ADR-0003), ADR-0002 (no k8s),
  ADR-0003 (Go+Rust port), ADR-0004 (tenancy + SQLite), ADR-0005 (cross-tenant network isolation),
  ADR-0006 (wg overlay + node join), ADR-0007 (sandbox ingress), ADR-0008 (templates + images),
  ADR-0009 (streaming exec, lifecycle, usage), ADR-0010 (observability + bench).
  Several carry **2026-08-29 amendments** from the v4 security hardening — ADR-0010's `/metrics`
  decision was reversed outright; ADR-0004/0005/0006/0007 were extended.
- `docs/research/` — Modal / Northflank / Sprites research reports

## Quick start

```sh
REPO=/Users/magdy/projects/github.com/alpham/infra-saas

# build (inside the toolchain VM — never on the host)
limactl shell infra-saas-lab -- bash -c \
  "export PATH=\$PATH:/usr/local/go/bin && cd $REPO/go && \
   CGO_ENABLED=0 go build -o /tmp/hearthd ./cmd/hearthd"
limactl shell infra-saas-lab -- bash -c \
  "source ~/.cargo/env && cd $REPO/rust/agent && \
   CARGO_TARGET_DIR=\$HOME/.cargo-target/hearth-agent \
   cargo build --release --target aarch64-unknown-linux-musl"

# one-time: mint the lab token. `hearth-lab-token` is a known placeholder now
# and both binaries refuse to start on it — the lab runs on a real secret,
# same as production (deploy/config/local-lab.md).
mkdir -p ~/.config/hearth
openssl rand -hex 32 > ~/.config/hearth/lab-token
chmod 0600 ~/.config/hearth/lab-token

# verify the running lab end-to-end (scripts read the token from that file,
# or take it as $1 / $HEARTH_TOKEN)
bash scripts/verify-v2.sh          # 17 system checks incl. wake latency
bash scripts/conformance-lab.sh    # full API contract suite

# see docs/DEPLOYMENT.md for production installs (deploy/install.sh)
```

> **Auth is not optional.** `hearthd` and `hearth-agent` refuse to start on an
> empty, too-short or placeholder token; `/metrics` needs the admin token; and
> the console takes its token from a paste-in banner only — never from
> `localStorage` or a `?token=` URL. Details in
> [`docs/DEPLOYMENT.md` §6](docs/DEPLOYMENT.md#6-tokens-auth-and-bind-addresses).

> **Running hearthd behind a reverse proxy?** Read
> [`docs/DEPLOYMENT.md` §7.1](docs/DEPLOYMENT.md#71-forwarded-client-addresses-required-reading)
> first. hearthd ignores `X-Forwarded-For` unless the peer is inside
> `trusted_proxies` (default: empty, trust nothing). List a proxy there **only**
> if it overwrites the header (`header_up X-Forwarded-For {remote_host}` in
> Caddy, `proxy_set_header X-Forwarded-For $remote_addr` in nginx — nginx's bare
> `proxy_pass` forwards the client's own header verbatim). hearthd cannot tell a
> header its proxy wrote from one it copied through, so this is closed by
> configuration or not at all: half of it, in the wrong direction, lets any
> client choose its own throttle key.
