# Hearth

Self-hosted infrastructure SaaS that manages **Firecracker microVMs for AI workloads** —
sub-second sandbox wake, warm pools, fork/branch, and a clean dashboard. Built as a pair of
static **Zig** binaries with no Kubernetes on the control path.

Conceptually the same product category as [Modal](https://modal.com), [Northflank](https://northflank.com)
and [Sprites](https://sprites.dev) (see `docs/research/`), self-hosted on your own machines.

## Components

| Component | Path | Runs on | Role |
|---|---|---|---|
| `hearthd` | `backend/src/hearthd` | `infra-saas-lab` VM | Control plane: REST API `:8080`, scheduler, node registry, state, serves the UI |
| `hearth-agent` | `backend/src/agent` | `kata-lab-0`, `kata-lab-1` VMs | Node agent `:9090`: drives Firecracker over its unix-socket REST API |
| UI | `ui/` | served by hearthd | Static SPA dashboard (fleet, sandboxes, fork tree) |
| Node setup | `infra/setup-node.sh` | workers | Installs Firecracker + guest kernel + base rootfs into `/srv/ignis` |

## Lab topology (Lima VMs on macOS — nothing runs on the host)

- `infra-saas-lab` — control plane + Zig 0.16.0 build box (Ubuntu 24.04 aarch64, 8 CPU / 8 GiB)
- `kata-lab-0`, `kata-lab-1` — workers with `/dev/kvm`, Firecracker v1.16.0, guest kernel + ubuntu base rootfs in `/srv/ignis`
- Inter-VM traffic uses Lima's `user-v2` network (`192.168.104.0/24`); Apple vzNAT does **not** allow guest-to-guest traffic.

## Docs

- `docs/PLAN.md` — adopted architecture + phased milestones (source of truth)
- `docs/adr/ADR-0000-original-hearth-plan.md` — original baseline plan (preserved)
- `docs/adr/ADR-0001-zig-backend.md`, `docs/adr/ADR-0002-no-kubernetes-control-plane.md`
- `docs/research/` — Modal / Northflank / Sprites research reports
- `backend/README.md` — build, deploy, and run commands

## Quick start

```sh
# build (inside the build VM — never on the host)
limactl shell infra-saas-lab -- bash -c \
  'cd /Users/magdy/projects/github.com/alpham/infra-saas/backend && zig build -Dtarget=aarch64-linux-musl'

# see backend/README.md for deploy + run
```
