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

## v3.1 — vsock exec + fork re-IP (2026-06-10/11; code-complete, scratch-validated)

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

## v3.1 rollout + lifecycle hardening (2026-06-11; lab live — conformance 132/0, verify-v2 21/0)

The live rollout (`scripts/roll-v3.1.sh`: inject → roll agents → roll hearthd
→ goldens → verify) surfaced four defects. All were fixed, re-rolled, and
verified green the same day:

- **start-on-paused destroyed the VM** (user-reported: "paused a microVM,
  couldn't start it back"): `start()` deleted the *live* `fc.sock` and spawned
  a second Firecracker over the same instance dir — the paused FC was orphaned
  forever (resume → connect error from then on) and each retry leaked another
  FC. `start` is now a guarded cold boot: valid only from `stopped`/`error`,
  409 `InvalidState` otherwise, with a pid-liveness check so it can never
  spawn over a live process. `pause`/`resume` got matching guards, hearthd
  forwards agent 409s (with the real reason) instead of a blanket 502, and
  the UI dropped the Start button on paused rows. Regression cases pin the
  path shut (`agent/07-actions`, `hearthd/10-actions`).
- **Firecracker death was invisible**: a guest panic (e.g. resume from a
  mid-boot snapshot) exits FC cleanly, but the VM stayed `running` with a
  stale pid and every subsequent op returned an opaque `FcError: Connect`.
  Every spawn's reaper now flags unexpected exits → `state:error` + warn
  (deliberate kills clear the recorded pid under the lock *before*
  signalling, so they no-op the reaper); a 5 s liveness sweep covers FCs
  adopted after an agent restart; failed spawns reap their half-configured
  FC instead of leaking it; a dead pool VM is retagged to `error` and the
  create falls through to a cold boot.
- **The suites slept mid-boot guests**: post-inject boots are slower (the
  hearth-guest unit), so the cases' create-then-sleep pattern and fixed 3 s
  exec waits became systematic failures — poisoned snapshots (guest panics on
  wake, FC exits) or a guest agent not yet listening. `wait_guest_ready`
  (conformance `lib.sh`) and `wait_exec_ready` (verify-v2) poll
  `exec(["true"])` before sleeping or exec'ing.
- **Fork child cloned the parent's MAC** — exposed only once re-IP worked:
  two bridge ports sharing one MAC flap the FDB and the parent goes dark.
  Fork now gives the child a fresh locally-administered MAC (`0a:68:…`,
  clock salt + tap slot) via plain `exec` before `set_ip`; no guest-protocol
  change.

Operational notes: during the first (pre-fix) conformance record run the
kata-lab-0 Lima VM died at the hypervisor level (`VZErrorDomain Code=3`,
"no longer live"; guest journald stalled ~80 s before death, no panic
captured) — recovered with `limactl stop -f` + start + agent redeploy; not
reproduced after the fixes, with the duplicate-FC pile-up as prime suspect.
Worker agent binaries live in `/tmp` and vanish on reboot — redeploy after
any worker restart. Goldens re-recorded against the fixed stack:
`metrics-names` gains `hearth_execs_total`; new `hearthd/exec` and
`agent/vm-exec` goldens carry real 200 bodies.

## v4 P0 — tenancy, SQLite store, usage metering (2026-06-12; lab live — conformance 166/0, verify-v2 21/0)

First phase of the v4 "multi-tenant, deploy-anywhere, product-ready" plan
([ADR-0004](adr/ADR-0004-tenancy-and-sqlite.md); contract in API-V2 §3b):

- **Tenants + API keys**: `hearth_sk_…` bearer keys (sha256-at-rest, shown
  once, instant revocation); the legacy configured token is now the admin
  credential — pre-v4 deployments work unchanged. Admin-only CRUD under
  `/api/v1/tenants*`; tenant keys get 404 on infra routes.
- **Enforced scoping**: every sandbox route filters by the key's tenant;
  foreign and unknown ids are indistinguishable (404). Fork children inherit
  the parent's tenant. `tenant_id` never appears on the wire (goldens frozen);
  the agent records it in `meta.json` for P1's nftables isolation.
- **Quotas**: per-tenant max sandboxes/vcpus/mem checked at create+fork → 429.
- **Usage metering**: append-only `usage_events` row per lifecycle transition
  (shape + timestamps) from day one; aggregation lands in P5.
- **SQLite (WAL) storage** behind a new `Store` interface (`go/internal/store`,
  pure-Go `modernc.org/sqlite` — static CGO_ENABLED=0 build preserved):
  whole-snapshot transactions at the same granularity as the old JSON persist,
  plus row-level tenants/keys/usage. One-time `state.json` import (renamed
  `*.imported`) — verified live on the lab. DB file is 0600.
- Conformance grew to **166 checks**: new `hearthd/17-tenancy` (scoping,
  cross-tenant 404s, revocation, quota 429) with per-run-unique tenant names,
  a `req_as` helper in lib.sh, and self-cleaning agent cases (a crashed prior
  run can no longer cascade `AlreadyExists` failures into the next).

Operational notes: kata-lab-0's Lima VM died at the hypervisor level again
(`VZErrorDomain Code=3`, second occurrence, both during the agent suite's
rapid sleep/wake/fork) — with the duplicate-FC bug fixed since v3.1, this now
reads as a macOS Virtualization.framework nested-virt limitation, not a Hearth
bug (kata-lab-1 runs the identical binary clean). Documented as a lab
constraint; production bare-metal workers (v4 P2) are unaffected by vz.
Security review notes: tenant-name length capped; quota check has a benign
one-sandbox TOCTOU burst window (row-level counting will close it);
`usage_events` retention is deferred to P5; P1 must allowlist-sanitize
`tenant_id` before any nft shell-out.

## v4 P1 — cross-tenant network isolation (2026-06-12; lab live — conformance 178/0, verify-v2 21/0)

Second phase of the v4 plan
([ADR-0005](adr/ADR-0005-cross-tenant-network-isolation.md); spec in
PLAN-v4 Phase 1): guest-to-guest traffic on a node now drops across tenants
and flows within one, consuming the `tenant_id` P0 started recording in
`meta.json`. All in the Rust agent (`rust/agent/src/net.rs`,
`rust/agent/src/vm/mod.rs`).

- **br_netfilter**: same-subnet guests on `hearth0` talk via L2 switching,
  which bypasses the ip `forward` hook entirely — the agent loads
  `br_netfilter` and sets `bridge-nf-call-iptables=1` so bridged frames
  traverse netfilter and the forward chain can police them.
- **Single concatenated pair set**: one nft set `tenant_pairs`
  (`ipv4_addr . ipv4_addr`, table `ip hearth`) holds every allowed
  same-tenant (saddr, daddr) pair. Forward chain (policy accept): accept
  established/related, accept anything not `hearth0`→`hearth0`, accept pairs
  in `@tenant_pairs`, drop the remaining bridge-to-bridge. Egress NAT and
  host↔guest are untouched.
- **Flush-and-rebuild from full membership** (the `ensure_nat` idiom):
  `refresh_isolation()` snapshots (tenant, ip) under the state lock, releases
  it, rebuilds under a dedicated `isolation_lock` (concurrent
  create/fork/delete can't interleave nft commands). Runs at end of create
  (pool claim + cold boot), fork success (child's post-re-IP address),
  delete, and at startup after reconcile — nft sets don't survive reboot.
- **Security (closes the P0 review requirement)**: `tenant_id` is never
  interpolated into any nft command — it's only a Rust-side grouping key, so
  there is no injection surface; only agent-allocated IPs reach nft.
  `valid_tenant()` (`[A-Za-z0-9_-]`, 1..=64) is defense-in-depth; a
  missing/invalid tenant joins no pair → isolated from all peers
  (fail-closed). Rejected alternative: per-tenant named sets
  `hearth_t_<tenant>` would have put tenant strings in command lines
  (ADR-0005).
- **Caveat**: tenant networks are node-scoped until the P2 overlay —
  per-node CIDRs are node-local, cross-node same-tenant traffic is not
  bridged in v1.

Evidence: conformance **178/0** (12 new checks in `agent/11-isolation`:
cross-tenant ping FAIL / same-tenant ping PASS / egress-to-gateway OK, driven
via exec), verify-v2 **21/0** (wake 65ms), plus a live repro through the full
control-plane path — two tenants created via the admin API, four sandboxes
created with tenant keys all scheduled onto one node: cross-tenant ping exit 1
(dropped), same-tenant ping exit 0, all deletes 204.

## Backlog (v3+, in order)

branch (uffd CoW fork of running VMs) → cross-tenant nftables isolation →
overlayfs root tier → SQLite state → cross-node reschedule → OIDC →
pool-orphan reclaim → uffd lazy restore → Terraform provider.
