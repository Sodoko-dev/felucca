# Hearth — Project history

Everything built, verified, and learned, in order. Companion docs:
[PLAN.md](PLAN.md) (adopted architecture), [API-V2.md](API-V2.md) +
[API-V3-EXEC.md](API-V3-EXEC.md) (contracts), [ARCHITECTURE.md](ARCHITECTURE.md)
(current system), the ADRs in [adr/](adr/), and [DEPLOYMENT.md](DEPLOYMENT.md).

## v1 — foundation (2026-06-09, Zig)

Two static Zig 0.16 binaries on a three-VM Lima lab (`infra-saas-lab` control
plane + toolchain; `kata-lab-0/1` workers with KVM + Firecracker v1.16):

- `hearthd`: REST API `:8080`, scheduler (ready node, lowest vm_count), node
  registry with 5s heartbeats / 15s down-detection, JSON state persistence
  (atomic tmp+rename), static UI serving, Prometheus metrics.
- `hearth-agent`: `:9090`, drives Firecracker over per-VM unix sockets
  (boot-source/drives/machine-config/InstanceStart), instance dirs under
  `/srv/ignis` with `meta.json` enabling restart adoption.
- Hearth Console UI (vanilla JS SPA): fleet + sandbox views, 3s polling with
  flicker-free in-place updates, mock-mode fallback.
- Research reports on Modal / Northflank / Sprites (`research/`); architecture
  decisions: Zig backend (ADR-0001), **no Kubernetes on the control path**
  (ADR-0002 — sandboxes are not pods; agent drives the VMM directly).
- Lab plumbing: Lima `user-v2` network for inter-VM traffic (Apple vzNAT blocks
  guest-to-guest), nested virtualization for `/dev/kvm`.

## v2 — the hot path (2026-06-10, Zig; verified 16/16)

The features that make sandboxes a product, per the adopted plan:

- **Sleep/wake**: pause → `snapshot/create` (vmstate.bin + mem.bin) → kill FC;
  wake = fresh FC + `snapshot/load` with `resume_vm` — **wake measured 67–85 ms**.
- **Fork**: parent snapshot (reused when sleeping) + reflink/copy of rootfs +
  mem, child restored with its own tap via `network_overrides`, `parent_id` set.
- **Warm pool**: `pool_size` pre-booted paused VMs per agent; matching create
  claims one (keeping the pool `dir_id` so baked snapshot paths stay valid),
  async refill.
- **Guest networking**: bridge `hearth0`, per-VM `hth-<slot>` taps, sequential
  IPs from `net_cidr`, nftables masquerade, `ip=` kernel-arg guest config.
- **Auth**: optional bearer token, constant-time compare; `/healthz`,
  `/metrics`, UI stay open.
- **Production config**: flags > `HEARTH_*` env > `--config` JSON > defaults;
  nothing hardcoded — the same binaries run on the lab and remote servers.
  `deploy/`: systemd units, installer, config stubs, firecracker-assets script.
- Verification: `scripts/verify-v2.sh` (16 end-to-end checks incl. host→guest
  ping and wake latency).

## Go + Rust migration (2026-06-10, tag `v3.0.0`)

Decision (ADR-0003, supersedes ADR-0001): control plane → **Go** (ecosystem,
future Terraform provider), agent → **Rust** (future uffd/CoW work at the VMM
boundary), wire and on-disk formats frozen.

- **Phase 0**: `test/conformance/` — ~24-case executable contract with semantic
  goldens (key sets, types, null-vs-omitted; never byte order) recorded from
  the running Zig stack. Green vs Zig before any port code ran.
- **Phase 1**: `go/` hearthd, stdlib-only, CGO-free. Solo-verified 78/78 on a
  side port including adopting a copy of the live `state.json` (same node and
  sandbox ids — critical because agents never re-register after success).
- **Phase 2**: `rust/agent/` (tokio/axum/hyper-UDS). Hard adoption gate passed
  on a scratch stack: byte-identical `/v1/vms` view over a Zig-written data
  dir (live pids kept, sleeping VMs intact, `pooled` orphans → stopped), node
  id preserved, a **Zig-written snapshot woken by the Rust agent**. Adoption
  proven in both directions (rollback-safe).
- **Cutover**: Gate A (Go hearthd + Zig agents) and Gate B (full Rust fleet,
  one worker at a time, running guests surviving the swaps) each passed full
  conformance + verify-v2; `backend/` deleted in a revertable commit; repo
  brought under git.
- Findings now encoded in tests/docs:
  - **Mid-boot snapshots are poisoned** — a guest slept ~1–2 s after boot
    panics on resume (clock jump) under every implementation; wake reports
    `running` while FC exits ~1 s later. verify-v2 now pings after wake
    (17 checks).
  - Agent DELETE is idempotent (204 for unknown ids) — encoded in conformance.
  - The Zig agent leaked FC zombies; the Rust agent reaps via detached wait
    tasks.

## v3.1 — vsock exec + fork re-IP (2026-06-10/11; code-complete, scratch-validated; lab rollout pending)

Contract: [API-V3-EXEC.md](API-V3-EXEC.md). Three components, built in
parallel against the frozen contract:

- **`rust/guest/` → `hearth-guest`** (528 KB static): vsock server on guest
  port 52 — `exec` (timeout, output capture, truncation caps) and `set_ip`
  (iproute2). Installed into the base image as a systemd unit by
  `infra/guest-agent-install.sh`, which also drops the
  `images/.hearth-guest-v1` marker.
- **`rust/agent/`**: attaches a Firecracker vsock device at cold boot when the
  marker is present (`vsock:true` in meta.json, optional field — old metas
  parse unchanged); hybrid-vsock guest client (`CONNECT 52` handshake);
  `POST /v1/vms/{id}/exec` (501 `guest agent unavailable` for pre-v3.1 VMs);
  fork performs best-effort `set_ip` on the child after restore.
- **`go/`**: `POST /api/v1/sandboxes/{id}/exec` proxy with per-request
  timeouts; `hearth_execs_total` metric.

Scratch end-to-end validation (real Firecracker VMs from an injected image
copy; zero live impact): exec on cold-booted guests, exec after sleep/wake
(wake 66 ms with vsock attached), **fork child answering on its own IP with
`eth0` genuinely reconfigured in-guest**, parent unaffected.

Defects found by validating against real Firecracker (not mocks):

- **Stale `v.sock` breaks restore**: sleep SIGKILLs FC, leaving the vsock UDS
  on disk; the next `snapshot/load` fails entirely with
  `VsockUnixBackend: Address in use`. Wake now removes the stale socket first.
  Every production wake of a vsock VM would have failed without this.
- A same-minute intermediate cargo build masqueraded as final (`ls` minute
  precision) — binaries are now compared by sha256, not mtime.

Remaining for v3.1: the live lab rollout (`scripts/roll-v3.1.sh`: inject →
roll agents → roll hearthd → re-record goldens → extended verify) — gated on
explicit operator approval because it bakes an exec-capable agent into the
shared base images. verify-v2 already carries the v3.1 checks (fork-child
ping, parent continuity, exec round-trip) and conformance has `16-exec` /
`10-exec` cases ready.

## Backlog (v3+, in order)

branch (uffd CoW fork of running VMs) → cross-tenant nftables isolation →
overlayfs root tier → SQLite state → cross-node reschedule → OIDC →
pool-orphan reclaim → uffd lazy restore → Terraform provider.
