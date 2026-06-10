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

- `docs/PLAN.md` — adopted architecture + phased milestones (source of truth)
- `docs/API-V2.md` — the frozen wire/config contract; `test/conformance/` enforces it
- `docs/ARCHITECTURE.md` — full architecture with diagrams (`docs/architecture-preview.html` to view)
- `docs/DEPLOYMENT.md` — production deployment, builds, upgrades
- `docs/adr/` — ADR-0000 (original plan), ADR-0001 (Zig, superseded), ADR-0002 (no k8s), ADR-0003 (Go+Rust port)
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

# verify the running lab end-to-end
bash scripts/verify-v2.sh hearth-lab-token        # 17 system checks incl. wake latency
bash scripts/conformance-lab.sh hearth-lab-token  # full API contract suite

# see docs/DEPLOYMENT.md for production installs (deploy/install.sh)
```
