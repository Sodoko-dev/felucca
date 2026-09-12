# Felucca — Adopted Implementation Plan (Zig-first microVM platform)

> Status: implementation-ready. Phases 1–2 are to be built immediately.
> Codename: **Felucca**. Backend codenames: **feluccad** (control plane), **felucca-agent** (node agent).
> Supersedes the architecture in `docs/adr/ADR-0000-original-hearth-plan.md` (preserved as baseline).
> Key ADRs: `ADR-0001-zig-backend.md`, `ADR-0002-no-kubernetes-control-plane.md`.

---

## 0. One-paragraph product

A self-hosted IaaS that runs untrusted AI workloads inside Firecracker microVMs ("sandboxes")
with sub-second wake, fast fork/branch, tenant-isolated networking, persistent storage, token
auth, Prometheus metrics, and a static SPA UI. Same product category as Modal Sandboxes,
Fly Sprites, and Northflank — but the entire backend (control plane + node agent) is a pair of
**single static Zig 0.16.0 binaries** with **no Kubernetes on the control path**, designed to
boot on the existing Lima lab today and scale to bare metal later without rearchitecting.

---

## 1. Ground truth — the lab this plan targets (verified 2026-06-10)

| Host | Role | Specs | Verified facts |
|---|---|---|---|
| `infra-saas-lab` | control plane + Zig build box | Ubuntu 24.04 aarch64, 8 vCPU / 8 GiB | `zig 0.16.0` at `/usr/local/bin/zig`; `/dev/kvm` present; `eth0 = 192.168.5.15/24` (vzNAT) |
| `kata-lab-0` | worker | aarch64, 8 vCPU / 16 GiB | `firecracker v1.16.0` at `/usr/local/bin/firecracker`; `/dev/kvm` (root:kvm); `/srv/ignis/{bin,kernels,images,instances}`; kernel `6.8.0-124`; `ip`+`nft` present |
| `kata-lab-1` | worker | aarch64, 8 vCPU / 16 GiB | identical to kata-lab-0 |

**Critical networking finding (RESOLVED 2026-06-10):** Each Lima VM is on its own vzNAT and all three
report the *same* `eth0` address `192.168.5.15/24` — they are **not** on a shared L2. Empirically
confirmed: 100% packet loss between all pairs on the vzNAT `lima0` 192.168.64.x addresses (Apple vmnet
NAT isolates guests). **Resolution:** all three VMs now also join Lima's `user-v2` network
(`networks: - lima: user-v2`), which provides guest-to-guest connectivity (192.168.104.0/24) without
any host-level install. Control-plane↔agent traffic uses the user-v2 addresses.

**Asset finding (RESOLVED 2026-06-10):** Both workers are fully staged: Firecracker v1.16.0,
`/srv/ignis/kernels/vmlinux` (firecracker-ci v1.15, 17 MB), `/srv/ignis/images/ubuntu-base.ext4`
(2.0 GB). A manual smoke test on kata-lab-0 booted a 1-vCPU/256 MiB microVM to a root login shell
(`infra/setup-node.sh` + `sg kvm -c "firecracker --no-api --config-file vm.json"`).

---

## 2. Felucca baseline vs adopted design — what we keep / replace / defer (and why)

The baseline (ADR-0000) is a faithful clone of the E2B/Fly/Northflank stack: k3s + CRDs + operator,
Cilium, JuiceFS, Keycloak, SPIRE, Vault, Postgres, Go node-agent, gRPC, Next.js UI. That is the
*right* production target and *wrong* lab starting point — it is months of operating six stateful
distributed systems before a single microVM boots. The hard constraints (Zig backend, no k8s
dependency, Lima-only, static SPA) force a leaner first cut. The single most important architectural
decision from the baseline — **"sandboxes are not pods"; a node agent drives Firecracker directly,
off the scheduler hot path** — is *kept and strengthened*: with no k8s at all, there is no scheduler
to bypass.

| Baseline component | Decision | Adopted replacement | Why |
|---|---|---|---|
| k3s cluster + CRDs + operator | **REPLACE** | Single Zig binary `feluccad` + SQLite (WAL) | No Zig k8s/CRD/operator ecosystem; reconcile loop is ~200 lines of Zig, not an operator runtime. See ADR-0002. |
| Go node-agent + gRPC | **REPLACE** | Zig `felucca-agent` + REST/JSON over HTTP/UDS | User mandate; no Zig gRPC ecosystem; Firecracker itself is REST-over-UDS — Zig std fits perfectly. See ADR-0001. |
| Postgres | **REPLACE (lab)** | SQLite WAL, one file on control plane | Single control node in lab; relational queries for quotas/audit retained; Postgres is the scale-out swap (§10). |
| Cilium (eBPF CNI) | **REPLACE** | Linux bridge + tap + `nft` per-tenant rules | No CNI without k8s; bridge/tap/nftables is the Firecracker-native datapath and is already toolchain-present (`ip`,`nft`). DSR/Maglev/XDP deferred to bare metal. |
| JuiceFS + MinIO + Redis metadata | **DEFER** | Per-VM `overlayfs` root over shared RO base ext4; named persistent data disks = plain ext4 files on the worker | overlayfs gives the dedup + fast-fork win immediately; "infinite object-backed" data tier is a Phase-5+ swap, not a Phase-1 dependency. Node-independent durable storage is explicitly out of lab scope (kills live reschedule — honest risk §11). |
| Keycloak (OIDC) | **DEFER** | Hashed bearer API tokens in SQLite; scoped per-tenant + per-sandbox | Token auth covers the lab security model; OIDC/Terraform-OIDC is a later additive layer, not a blocker. |
| SPIRE / SPIFFE mTLS | **DEFER** | Plaintext on the lab L2 + token on every call | No internal mTLS until multi-tenant prod; documented as a known gap. |
| Vault | **DEFER** | Secrets as env injected from SQLite secret rows | Real secret lifecycle is post-MVP. |
| gRPC streaming for logs | **REPLACE** | HTTP chunked / SSE + vsock for exec/PTY | Zig std.http supports chunked responses; vsock is the Firecracker-native console/exec channel. |
| Next.js + Node build toolchain | **REPLACE** | Static SPA (designer-produced) served by `feluccad` from an embedded/served dir | Mandate: no Node on the serving path. `feluccad` serves `/` from a static dir; xterm.js loads as a static asset. |
| MkDocs + OpenAPI-gen + ADRs | **KEEP (trimmed)** | Markdown docs in `docs/`, hand-maintained OpenAPI for v1, ADRs | Docs-as-code retained; CI doc-build deferred. |
| Firecracker VMM | **KEEP** | Firecracker v1.16.0 (installed) | Smallest footprint, native snapshot/restore, REST-over-UDS. CLH pluggability deferred (no live migration in lab). |
| overlayfs root tier | **KEEP** | squashfs/ext4 RO base + per-VM ext4 upper | Exactly the E2B/Modal-image-dedup pattern; fastest fork path. |
| Prometheus metrics taxonomy | **KEEP** | `/metrics` text-format endpoint on `feluccad` and `felucca-agent` | No exporter needed; Zig writes the text format directly. Scrape with any Prometheus later. |
| "Sandboxes are not pods" / node-agent hot path | **KEEP+STRENGTHEN** | felucca-agent owns warm pool, snapshot, fork on-box | Core thesis; with no k8s there is literally no slow declarative path to bypass. |
| Snapshot/restore + warm pool + CoW fork | **KEEP (hot path)** | See §3 | This *is* the product. Informed by Modal/Sprites (§4). |

**Net:** we trade away every stateful distributed dependency for two Zig binaries + SQLite + Firecracker,
keeping exactly the performance-critical primitives (warm pool, snapshot/restore, overlayfs/CoW fork)
that the competitive research says are the actual moat.

---

## 3. Hot-path architecture — maximum performance on wake / fork

The product's only defensible advantage is **time-to-ready** on the hot path. Three layered wake
mechanisms, fastest first — this is the synthesis of what Modal, Sprites, and Northflank actually do
(§4):

```
                 ┌──────────────────────── infra-saas-lab ────────────────────────┐
  UI / CLI ─────▶│  feluccad (:8080)                                              │
  REST/JSON      │   REST API · scheduler · node registry · SQLite(WAL) · /metrics│
                 │   static SPA at  /                                             │
                 └────────────────┬───────────────────────────────────────────────┘
                                  │ REST/JSON over lab L2  (placement decision only;
                                  │ NOT on the per-request wake hot path)
                 ┌───────────────▼──────────────┐        ┌──────────────────────────┐
                 │  felucca-agent (:9090)       │  UDS   │  Firecracker per microVM │
   wake req ────▶│  warm pool · snapshot/restore│──REST─▶│  /run/felucca/<id>.sock  │
   (hot path)    │  CoW fork · tap/bridge · nft │        └──────────────────────────┘
                 └───────────────┬──────────────┘
                                 │ overlayfs root (RO base + per-VM upper)
                         /srv/ignis/{kernels,images,instances}
```

### 3.1 Wake tier A — warm pool hand-out (target: low tens of ms, no control-plane round trip)
`felucca-agent` keeps **N pre-restored, paused microVMs** per template parked on each worker. A wake
request that hits a warm VM is answered by the agent **without any call back to feluccad** — the agent
resumes the paused VM, attaches the caller, and returns. This is the Modal "pre-warmed pool +
snapshot routing" pattern and the Sprites "resume in <100 ms" pattern. The pool is refilled
asynchronously off the hot path.

### 3.2 Wake tier B — snapshot/restore from template (target: sub-200 ms locally)
When the pool is empty, restore a paused **Firecracker snapshot** (`/snapshot/load` over UDS) of a
template that was booted, warmed, networked, and mounted *once*. **Hard rule (from baseline §2.3,
kept):** the snapshot is taken *after* tap networking is attached and the root overlay is mounted, so
restore skips boot + net-attach + mount entirely. Snapshot diffing (`/snapshot/create` with the diff
strategy) keeps per-template snapshot deltas small.

### 3.3 Wake tier C — CoW fork from a warm parent (target: tens of ms per child)
`fork` spawns children from a paused parent using **Firecracker snapshot + copy-on-write disk**:
the child gets a fresh `overlayfs` upper layer over the parent's frozen lower disk, and restores from
the parent's memory snapshot. `branch` pauses a *running* sandbox, snapshots in-flight state, and
resumes (~150 ms) so a workload can be forked mid-execution. This is the forkd/E2B technique and the
"branch a live agent" pattern.

> aarch64 caveat: Firecracker memory-snapshot/restore support is x86-mature and **aarch64-partial**.
> The plan validates *the control flow and disk-CoW path* on the lab; raw aarch64 snapshot wake timings
> are a measured unknown (§11). If aarch64 memory snapshots are unstable on v1.16.0, tier-B/C degrade
> gracefully to tier-A warm-pool boot+pause, which still gives the hand-out win.

### 3.4 Disk hot path — overlayfs everywhere
One RO base image (`ubuntu-base.ext4`, soon squashfs) is shared by every VM on a worker. Each VM
gets a tiny ext4 **upper** layer in `/srv/ignis/instances/<id>/`. Create = make an upper dir + mount;
fork = new upper over the same lower. This is the dedup + instant-clone win Modal's image system and
E2B's base-image sharing deliver, implemented with stock Linux primitives.

---

## 4. What the competitors actually do (extracted from research, drives §3)

The research transcripts (`docs/research/{modal,northflank,sprites}.md`) are largely tool-permission
failure loops; the substantive, training-grounded findings worth adopting:

- **Modal** — the moat is **memory snapshot + restore**, not the runtime. They snapshot a container
  *after* heavy init (model load) and restore in ms; routing prefers warm workers → workers with the
  matching snapshot → cold start last. Content-addressed, demand-paged image FS avoids docker-pull on
  cold start. **Adopted:** snapshot-after-warmup hard rule (§3.2); pool→snapshot→cold routing order
  (§3.1); overlayfs/base-image dedup as our "lazy image" analogue (§3.4).
- **Sprites (Fly)** — Firecracker microVMs for AI agents; **snapshot/suspend-resume is the central
  feature** (resume <100 ms); per-sandbox persistent volume; per-sandbox egress control; minimal
  composable API (create/start/exec/snapshot/destroy). **Adopted:** the exact verb set (§5), warm
  resume as tier-A, per-tenant nft egress, per-sandbox data disks.
- **Northflank** — k8s-native, **runc** on shared clusters (no microVM isolation) — i.e. the
  isolation gap Felucca fills. Whole-GPU allocation only; BYOC agent is outbound-only. **Adopted:**
  microVM-per-tenant *is* our differentiation; the agent model is outbound-registration-friendly
  (relevant to the vzNAT finding, §8 Phase 0). GPU/MIG and BYOC are explicitly out of lab scope.

Common thread across all three and the baseline: **warm pool + snapshot/restore + per-tenant
isolation** is the irreducible core. Everything else (CRDs, JuiceFS, SPIRE, Cilium) is operational
scaffolding we can defer.

---

## 5. Component breakdown

### 5.1 `feluccad` — Zig control plane (runs on `infra-saas-lab:8080`)
- **REST API** (`std.http.Server`): tenants, nodes, sandboxes, snapshots, volumes, templates; auth
  middleware (hashed bearer token lookup in SQLite).
- **Node registry:** workers register via `POST /v1/nodes`; heartbeat + capacity (free KVM slots,
  CPU/RAM) on a TTL; dead nodes age out.
- **Scheduler:** least-loaded placement across registered agents (free-slot + RAM fit). Placement is
  a *control* decision only; it is **never** on the per-request warm-pool wake path (§3.1).
- **Reconcile loop:** desired-vs-actual for pools/sandboxes; ~periodic, drives agent calls. Replaces
  the k8s operator (ADR-0002).
- **SQLite (WAL)** state: tenants, tokens, nodes, sandboxes, snapshots, volumes, templates, audit.
- **Static UI serving:** serves the designer's SPA from a static dir at `/`; `/v1/*` is the API.
- **`/metrics`** (Prometheus text) and **`/healthz`**.

### 5.2 `felucca-agent` — Zig node agent (runs on each worker `:9090`)
- **Firecracker lifecycle** via UDS REST: boot, configure (machine/boot-source/drives/net), pause,
  resume, snapshot create/load, kill. One Firecracker process per microVM, one UDS per VM under
  `/run/felucca/<id>.sock`.
- **Tap networking:** create `tap<n>`, attach to a `felucca0` bridge, assign tenant subnet, program
  `nft` rules (intra-tenant allow, cross-tenant default-deny, egress policy).
- **Storage:** assemble `overlayfs` root (RO base lower + per-VM upper); attach named data disks.
- **Warm pool:** maintain N paused VMs per template; hand out on the hot path without feluccad.
- **Snapshots & fork:** snapshot create/load; CoW fork/branch (§3.3).
- **Registration & heartbeat** to feluccad; **`/metrics`** and **`/healthz`** locally.
- **exec/PTY** to a guest via **vsock** (xterm.js → feluccad WS/SSE → agent → vsock).

### 5.3 `felucca` CLI — optional thin client
A small Zig (or shell, for Phase 1) client over the REST API: `felucca node ls`,
`felucca sandbox create|start|stop|fork|exec|rm`. Phase 1 ships a `curl`-based smoke script; a real
CLI is additive.

---

## 6. REST API contract v1

All bodies JSON. Auth: `Authorization: Bearer <token>` on every `/v1/*` call except `/healthz`,
`/metrics`. Errors: `{ "error": { "code": "...", "message": "..." } }`. IDs are ULIDs.

### 6.1 feluccad — control plane (`:8080`)

**Nodes**
- `POST   /v1/nodes`                 register a node `{name, addr, capacity:{vcpu,mem_mb,kvm_slots}}` → `{id, token}`
- `GET    /v1/nodes`                 list nodes + live capacity/heartbeat
- `GET    /v1/nodes/{id}`            node detail
- `POST   /v1/nodes/{id}/heartbeat`  `{free_slots, load}` (agent→feluccad, TTL refresh)
- `DELETE /v1/nodes/{id}`            drain + deregister

**Sandboxes** (CRUD + lifecycle verbs — verb set mirrors Sprites/baseline)
- `POST   /v1/sandboxes`             create `{template, tenant, vcpu, mem_mb, [node]}` → placed + (optionally) started
- `GET    /v1/sandboxes`             list (filter `?tenant=&node=&state=`)
- `GET    /v1/sandboxes/{id}`        detail (state, node, ip, ports)
- `DELETE /v1/sandboxes/{id}`        destroy
- `POST   /v1/sandboxes/{id}/start`  cold/warm start (resume from pool/snapshot)
- `POST   /v1/sandboxes/{id}/stop`   stop (free node resources)
- `POST   /v1/sandboxes/{id}/pause`  pause (snapshot memory, keep placement)
- `POST   /v1/sandboxes/{id}/resume` resume from pause
- `POST   /v1/sandboxes/{id}/fork`   `{count}` → CoW children `{children:[ids]}`
- `POST   /v1/sandboxes/{id}/exec`   `{cmd, [tty]}` → chunked/SSE stdout/stderr (via vsock)

**Snapshots / Templates / Volumes / Tenants**
- `POST /v1/sandboxes/{id}/snapshot` → `{snapshot_id}` ;  `GET /v1/snapshots` ; `DELETE /v1/snapshots/{id}`
- `GET /v1/templates` ; `POST /v1/templates` `{name, base_image, kernel, warmup?}`
- `GET /v1/volumes` ; `POST /v1/volumes` `{name, size_mb, tenant}` ; `DELETE /v1/volumes/{id}`
- `POST /v1/tenants` `{name}` → `{id}` ; `POST /v1/tenants/{id}/tokens` → `{token}` (scoped)

**Ops**
- `GET /metrics`   Prometheus text: `felucca_wake_seconds` histogram, `felucca_pool_hit_total`,
  `felucca_fork_seconds`, `felucca_sandboxes{state}`, `felucca_nodes{state}`.
- `GET /healthz`   `{ "status":"ok", "db":"ok", "nodes":N }`

### 6.2 felucca-agent — node agent (`:9090`, called by feluccad; token-authed)

- `GET  /healthz`                              `{status, fc_version, kvm:true, free_slots}`
- `GET  /metrics`                              node-local Prometheus text
- `POST /agent/v1/sandboxes`                   `{id, tenant, template, vcpu, mem_mb}` boot/restore microVM
- `POST /agent/v1/sandboxes/{id}/{start|stop|pause|resume|snapshot|fork}`
- `POST /agent/v1/sandboxes/{id}/exec`         `{cmd,[tty]}` → chunked (vsock-backed)
- `DELETE /agent/v1/sandboxes/{id}`            kill FC, tear down tap/overlay
- `POST /agent/v1/pool`                        `{template, target}` set warm-pool depth

Agent ↔ Firecracker is Firecracker's own REST over `/run/felucca/<id>.sock`
(`PUT /machine-config`, `/boot-source`, `/drives/*`, `/network-interfaces/*`, `/actions`,
`/snapshot/create`, `/snapshot/load`).

---

## 7. Repository layout

```
infra-saas/
  feluccad/           # Zig control plane (build.zig, src/{http,api,db,sched,registry,metrics}.zig)
  felucca-agent/      # Zig node agent  (src/{http,firecracker,net,overlay,pool,snapshot,vsock}.zig)
  felucca-cli/        # optional Zig CLI (Phase 1: scripts/smoke.sh stand-in)
  ui/                 # designer's static SPA (served by feluccad; no Node on serving path)
  images/             # base rootfs + template build tooling (stages /srv/ignis assets)
  scripts/            # lab bootstrap, net setup, smoke/acceptance tests
  docs/
    PLAN.md           # this file
    adr/              # ADR-0000 (baseline), ADR-0001 (Zig), ADR-0002 (no-k8s)
    research/         # modal.md, northflank.md, sprites.md
```

---

## 8. Phased milestones — acceptance tests runnable on THIS lab

Each phase has a runnable acceptance test against the named Lima VMs. Phases 1–2 are built first.

### Phase 0 — Lab network + asset bootstrap (prerequisite, small)
**Goal:** one routable inter-VM path (the vzNAT finding makes this mandatory) and Phase-1 assets present.
- Establish a control-plane↔worker path. Options, in order of preference:
  (a) add a shared Lima network (`socket_vmnet` / `user-v2`) so each VM gets a distinct routable IP; or
  (b) bootstrap registration over an SSH-forwarded tunnel from each worker to `feluccad`. Document the
  chosen path in `scripts/`.
- Stage `/srv/ignis/kernels/vmlinux` and `/srv/ignis/images/ubuntu-base.ext4` on both workers.
**Acceptance:** from `kata-lab-0`, `curl http://<feluccad-addr>:8080/healthz` returns `ok`; both
workers show non-empty `/srv/ignis/kernels` and `/srv/ignis/images`; `firecracker --version` = v1.16.0.

### Phase 1 — Boot one microVM end-to-end: feluccad → agent → Firecracker (FIRST)
**Goal:** the full vertical slice. `feluccad` accepts a create, places it on a registered
`felucca-agent`, the agent boots a Firecracker microVM with an overlayfs root + tap NIC, and a command
runs inside it.
- `felucca-agent` on `kata-lab-1` registers with `feluccad` on `infra-saas-lab`.
- `POST /v1/sandboxes {template:"ubuntu-base"}` → placed on the agent → FC microVM boots → tap up.
- `POST /v1/sandboxes/{id}/exec {cmd:"uname -a"}` returns guest output over vsock.
- `DELETE` tears everything down (FC killed, tap removed, overlay unmounted).
**Acceptance (runnable):**
```
# on infra-saas-lab
feluccad &                                    # :8080
# on kata-lab-1
felucca-agent --feluccad http://<cp>:8080 &   # :9090, registers
# from anywhere with a token
curl -XPOST .../v1/sandboxes -d '{"template":"ubuntu-base"}'   # -> {id}
curl -XPOST .../v1/sandboxes/<id>/exec -d '{"cmd":"uname -a"}' # -> "Linux ... aarch64"
curl -XDELETE .../v1/sandboxes/<id>                            # -> 204; ps shows no firecracker
curl .../v1/nodes                                              # kata-lab-1 Ready, free_slots decremented then restored
```

### Phase 2 — Snapshot/restore + warm pool + wake histogram
**Goal:** tier-A/B wake (§3.1–3.2) and measurement from day one.
- Agent boots+warms+networks+mounts a template, then `snapshot/create` (paused, post-mount).
- `POST /agent/v1/pool {template, target:3}` keeps 3 paused VMs ready.
- A `start` on a pooled template resumes a warm VM with no feluccad round trip.
- `felucca_wake_seconds` histogram + `felucca_pool_hit_total` exported on `/metrics`.
**Acceptance:** cold create vs warm-pool start both measured; warm wake p50 < 200 ms locally (or, if
aarch64 snapshot is unstable, warm boot+pause hand-out < 1 s — recorded either way);
`curl .../metrics | grep felucca_wake_seconds` shows a populated histogram; pool refills after hand-out.

### Phase 3 — Fork / branch (CoW)
**Goal:** tier-C (§3.3).
**Acceptance:** `POST /v1/sandboxes/{id}/fork {count:10}` produces 10 children in < 1 s total on a
worker; each child gets an independent overlay upper and diverges (write a file in one, absent in
siblings); `branch` of a running sandbox resumes both parent and child.

### Phase 4 — Tenant networking isolation (bridge + nft)
**Goal:** baseline §2.4 without Cilium.
**Acceptance:** two sandboxes in tenant A ping each other; a sandbox in tenant B cannot reach them
(`nft` default-deny cross-tenant); per-tenant egress policy enforced; rule set dumpable via `nft list`.

### Phase 5 — Persistent data volumes + auth hardening
**Goal:** named data disks + scoped tokens/audit.
**Acceptance:** a volume created, attached, written, sandbox destroyed, new sandbox re-attaches same
volume and reads the data back; an unauthenticated `/v1/*` call is `401`; a per-sandbox token works
only for its sandbox; audit rows recorded in SQLite.

### Phase 6 — Static UI + web terminal
**Goal:** designer SPA served by feluccad; xterm.js over vsock.
**Acceptance:** from a browser, create a sandbox, open a terminal into it, run a command — entirely in
the SPA, with no Node process on the serving path.

### Phase 7 — Metrics dashboard + scale-out dry run
**Goal:** prove the §10 path.
**Acceptance:** Prometheus scrapes feluccad + both agents; wake p50/p99, pool-hit rate, per-sandbox
CPU/mem visible; a third agent (e.g. a transient Lima worker) joins by running one binary and appears
in `/v1/nodes` with zero reconfiguration of existing nodes.

---

## 9. Security model (lab cut, defense-in-depth retained where cheap)

Outer→inner: Firecracker HW virtualization (kept) → jailer (seccomp/cgroup/chroot — add in Phase 5)
→ `nft` cross-tenant default-deny (Phase 4) → tenant = isolation boundary, **no VM reuse across
tenants** (restore fresh, discard after use — kept from baseline) → bearer token on every API call.
**Snapshot hygiene (kept, mandatory):** after every restore, re-seed guest entropy/RNG and fix the
guest clock — restoring the same image repeatedly is a known Firecracker footgun. SPIRE/mTLS, Vault,
and OIDC are deferred (§2) and documented as gaps.

---

## 10. Scale-out path (lab → bare metal, no rearchitect)

The lab choices are deliberately swap-points, not dead ends:
- **SQLite → Postgres:** `feluccad` talks to a DB module behind an interface; swap the driver. Schema
  is already relational.
- **Single feluccad → HA feluccad:** state is in the DB, control plane is otherwise stateless; run N
  feluccad behind a load balancer once on Postgres.
- **Static node list → dynamic fleet:** node registry already supports one-command join (Phase 7);
  adding bare-metal hosts = run `felucca-agent` on them.
- **bridge/nft → Cilium/SR-IOV; overlayfs-only → JuiceFS/object-backed data tier; token → OIDC +
  Terraform dynamic credentials; FC-only → CLH for live-migration workloads.** Each is an additive
  module behind an interface the lab code already defines.

---

## 11. Honest risks and limitations

- **aarch64 Firecracker snapshot maturity.** Memory snapshot/restore is x86-first; on aarch64 v1.16.0
  it may be partial/unstable. Mitigation: tier-A warm-pool hand-out (boot+pause+resume) works without
  full memory snapshots; tiers B/C degrade gracefully. Phase 2 *measures* this rather than assuming it.
- **Lima/vz nested-virt performance.** Firecracker runs nested inside a vz guest; wake/boot timings
  are *not* representative of bare metal. The lab validates control flow, wiring, and correctness — a
  bare-metal pass is required before timing claims.
- **No live migration.** Durable data is node-local (overlayfs/ext4 on the worker), so zero-downtime
  reschedule from the baseline is **out of scope** for the lab. It returns only with a node-independent
  data tier (JuiceFS) + CLH; documented as deferred, not solved.
- **vzNAT inter-VM addressing.** All VMs share `192.168.5.15` on their own NATs; Phase 0 must
  establish a routable path (shared Lima net or SSH tunnel) before agents can register. This is the
  single most likely Phase-1 blocker.
- **Empty `/srv/ignis` assets.** Kernel + rootfs are still staging; Phase 1 is gated on a
  asset-presence pre-check so failures are loud, not mysterious.
- **Zig 0.16 std.http surface.** `std.http.Server` exists but is comparatively young; expect to hand-
  roll chunked/SSE and connection handling. UDS + Firecracker REST is a clean fit and de-risked by
  Firecracker's simple API.

---

## 12. Immediate next actions (for the implementing agent)

1. **Phase 0 net path:** pick shared-Lima-net vs SSH-tunnel registration; script it; confirm
   `kata-lab-1 → feluccad:/healthz`. Stage kernel + `ubuntu-base.ext4` into `/srv/ignis` on both workers.
2. **Phase 1 slice:** scaffold `feluccad` (std.http + SQLite + `/v1/nodes`, `/v1/sandboxes`,
   `/healthz`, `/metrics`) and `felucca-agent` (register + Firecracker-over-UDS boot + tap + overlay +
   vsock exec). Land the runnable Phase-1 acceptance script in `scripts/`.
3. **Phase 2:** snapshot-after-warmup + warm pool + `felucca_wake_seconds` histogram; measure aarch64
   snapshot behavior and record the degrade path.
```
