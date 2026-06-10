# ADR-0003: Port hearthd to Go and hearth-agent to Rust; retire the Zig backend

- **Status**: Accepted (2026-06-10)
- **Supersedes**: [ADR-0001](ADR-0001-zig-backend.md) (Zig backend)
- **Related**: [ADR-0002](ADR-0002-no-kubernetes-control-plane.md) (unchanged), [API-V2.md](../API-V2.md) (the frozen contract this migration preserved)

## Context

v2 shipped as two Zig 0.16 binaries and proved the architecture: snapshot-based
sleep/wake (~70ms), warm pools, fork, guest networking, token auth, configurable
deployment. Three pressures motivated a language change for v3:

1. **Zig 0.16 std churn.** The std.Io rework forced hand-rolled HTTP, JSON, and
   UDS clients; every toolchain bump risks the I/O layer. The ecosystem provides
   none of what v3 needs (OIDC, SQLite, gRPC/Terraform plumbing).
2. **The control plane wants Go.** A Terraform provider must be written in Go
   (terraform-plugin-framework); the broader infra ecosystem (OIDC libraries,
   SQLite drivers, operational tooling) is Go-first. hearthd is a scheduler +
   REST API — exactly Go's wheelhouse.
3. **The agent wants Rust.** The v3 data-plane roadmap — userfaultfd lazy
   restore in-process, CoW fork, possibly carrying patches against Firecracker
   itself — lives in rust-vmm territory. The industry pattern agrees (Fly,
   E2B, Modal all converge on a systems language at the VMM boundary with a
   Go-ish control plane above it).

## Decision

- **hearthd → Go** (`go/`): stdlib-only, `CGO_ENABLED=0`, static binaries for
  arm64 + amd64. Hand-rendered metrics text, explicit Content-Length on every
  response (never chunked — the wire stayed byte-compatible with what minimal
  HTTP clients expect), `crypto/subtle` for token comparison.
- **hearth-agent → Rust** (`rust/agent/`): tokio + axum + serde, hyper over the
  Firecracker unix socket, static musl binary. Networking still shells out to
  `ip`/`nft` (rust-netlink is future work). Firecracker children are spawned
  with `kill_on_drop(false)` and reaped by detached tasks.
- **The wire contract was frozen at API-V2.md** and enforced by an executable
  conformance suite (`test/conformance/`): ~24 cases with semantic goldens
  (key sets, types, null-vs-omitted — never byte order) recorded from the
  running Zig v2 stack. Both ports had to go green against those goldens
  before touching the lab.
- **On-disk formats are unchanged**: hearthd `state.json` (incl. `seq`
  continuity and hostname-idempotent node registration) and the agent's
  per-instance `meta.json`/`vmstate.bin`/`mem.bin`. Adoption was hard-gated
  and tested in both directions (Rust agent adopting a Zig-written data_dir
  including waking a Zig-written snapshot; Zig re-adopting a Rust-era dir for
  rollback).
- The Zig backend (`backend/`) is deleted in its own revertable commit.

## Migration record

| Gate | Configuration | Result |
|---|---|---|
| 0 | Conformance suite vs live Zig stack | 118+43 pass, 0 fail; verify-v2 16/16 |
| solo | Go hearthd on :8081, copy of live state, real Zig agents | 78/78 incl. state adoption |
| scratch | Go hearthd + Rust agent, Zig-written fixtures | adoption identical view, node id preserved, Zig snapshot woken by Rust (69–85ms), 120/0 |
| A | Live cutover: Go hearthd + 2 Zig agents | node ids preserved, 118/0 + verify-v2 16/16 |
| B | Live cutover: + Rust agents (lab-1 then lab-0) | views identical, live FCs survived, e2e guests unaffected, 16/16, wake_ms 67 |

Findings made during migration, now part of the record:

- **Snapshots taken mid-guest-boot are poisoned**: a guest slept ~1–2s after
  boot panics/reboots on resume (clock jump during boot) under BOTH the Zig
  and Rust agents. Settled guests wake and answer pings. verify-v2 gained a
  post-wake ping check so this class of regression is visible.
- **Agent DELETE is idempotent** (204 for unknown ids) — observed Zig behavior,
  encoded in the conformance suite.
- **The Zig agent leaked Firecracker zombies** (no reaping of killed FCs) and
  **orphaned paused pool FCs across restarts**; the Rust agent reaps children
  via detached wait tasks (zombies: fixed; pool-orphan reclaim remains v3
  backlog, behavior kept 1:1).

## Consequences

- Two toolchains (Go, Rust), both living exclusively inside the `infra-saas-lab`
  Lima VM — never on the macOS host. Build commands are in DEPLOYMENT.md.
- `test/conformance/` is the contract's executable form; any future
  implementation change must keep it green, and `record.sh` re-baselines
  goldens only against a known-good reference.
- The systemd units in `deploy/systemd/` were authored for the Zig binaries;
  the lab runs nohup, so the units are not yet soak-tested under the Go/Rust
  runtimes. First production install should watch `journalctl` for seccomp
  kills (`SystemCallFilter`, and `MemoryDenyWriteExecute` vs the Go runtime).
- v3 feature work starts from this base, in backlog order: SQLite state, OIDC,
  vsock exec, fork guest re-IP, nftables tenant isolation, uffd lazy restore,
  CoW fork, overlayfs root tier, Terraform provider.
