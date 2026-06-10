# ADR-0000 — Original "Hearth" plan (from claude.ai, provided by user 2026-06-10)

> Preserved verbatim as the baseline this project's plan was derived from.
> The adopted, amended plan lives in `docs/PLAN.md`. Notable amendment: the
> backend (control plane + node agent) is implemented in **Zig**, per explicit
> user requirement, and the local lab uses Lima VMs infra-saas-lab (control
> plane) + kata-lab-0/1 (workers).

# MicroVM Sandbox Platform — Implementation Plan

> Working codename: **Hearth** (the name evokes the warm pool that keeps microVMs ready — rename freely).
> Status: local-first prototype, designed to scale out to bare metal later.
> Audience: you (the builder). This document is the source of truth; keep it in the repo and update it as decisions change.

---

## 0. What we are building, in one paragraph

A self-hosted Infrastructure-as-a-Service platform that runs untrusted workloads inside Firecracker microVMs ("sandboxes"), with sub-second wake, fast fork/branch, tenant-isolated networking, effectively unlimited per-sandbox storage, strong authentication, full metrics, and a clean UI. It is conceptually the same product that E2B, Fly.io's Sprites, and Northflank sell — we are building our own, starting on a laptop (two Lima VMs) and growing to a bare-metal datacenter without rearchitecting.

## 1. The single most important architectural decision

**Sandboxes are not Kubernetes pods.** Kubernetes owns nodes, capacity, the control-plane database, and the slow declarative path. A custom **node-agent** on each machine drives Firecracker directly and hands out warm microVMs on the request hot path, *bypassing the kube-scheduler*. This is the "DIY path" we landed on earlier and it is what makes wake, fork, and reschedule fast — Kata/RuntimeClass would abstract away exactly the Firecracker snapshot/fork control we need most.

Everything below follows from that decision.

```

                    ┌──────────────── Control plane (k3s today) ────────────────┐
   UI / CLI / TF ──▶│  API gateway → Orchestrator (CRDs) → state DB (Postgres)   │
                    └───────┬───────────────────────────────────────────────────┘
                            │ desired state (Sandbox / Pool / Template CRDs)
                 ┌──────────▼───────────┐         ┌──────────────────────┐
   request ─────▶│  Activator / proxy   │── hot ─▶│  Node-agent (per box) │── Firecracker microVMs
   (wake)        │  (buffers, routes)   │  path   │  warm pool · fork · CoW│
                 └──────────────────────┘         └───────┬───────────────┘
                                                          │
                          ┌───────────────────────────────┼───────────────────────┐
                   storage plane                    network plane             identity plane
              overlay root + JuiceFS data         Cilium eBPF + netpol     OIDC + SPIFFE/SPIRE
              + object store + snapshots           per-namespace overlay    + per-sandbox tokens

```

---

## 2. How each required capability is delivered

This section maps your ten requirements to a concrete mechanism. The rest of the document expands on these.

### 2.1 — Add machines easily, scale horizontally (infinite)
A new machine becomes capacity by running one installer that (a) joins the k3s cluster as an agent and (b) starts the **node-agent** service, which registers itself with the orchestrator over gRPC and advertises its capacity (CPU, RAM, free `/dev/kvm` slots). The control plane is stateless behind Postgres, so capacity is simply the sum of registered node-agents. This mirrors how flintlock registers bare-metal hosts and how k3s agents join — adding a node is a single command, no reconfiguration of existing nodes.

### 2.2 — Reschedule a microVM to another machine with minimum effort and zero downtime
This is the hardest requirement and deserves an honest answer. Firecracker does **not** do true live migration; its snapshot/restore involves a brief pause (the guest sees network/vsock packet loss across the move). Two viable approaches:

- **Snapshot + restore + decoupled storage (recommended now).** Because each sandbox's persistent data lives on object-backed storage (not on the node — see §2.7), moving a sandbox means: snapshot memory → ship the (small, diff) snapshot to the target node → restore there → repoint the proxy. The **activator buffers in-flight requests** during the sub-second restore, so the *client* perceives zero downtime even though the VM technically paused. This is the Sprites model and it is achievable on Firecracker.
- **Cloud Hypervisor live migration (later option).** If you need genuinely uninterrupted long-lived TCP connections through the move, CLH supports live migration where Firecracker does not. Keep the node-agent VMM-pluggable so you can choose CLH for workloads that need it.

The design rule that makes this tractable: **never put durable state on the node.** Memory is snapshotted; disk and data are network/object-backed.

### 2.3 — Wake as fast as possible
Layered, in order of speed:
- **Copy-on-write memory fork from a warm parent** — the fastest path. A parent VM is booted once, warmed (runtime imported, app initialized), and paused. Children `mmap` the parent's memory image `MAP_PRIVATE`; the kernel does page-level CoW. This is the forkd/E2B technique and lands in the **single-digit-to-tens-of-milliseconds** range, because nothing cold-boots.
- **Snapshot/restore from a template** — restore a pre-warmed snapshot (≈5–150 ms depending on image and memory size).
- **Pre-warmed pool** — fully-ready VMs parked and handed out by the node-agent off the scheduler path.
- **Hard rule:** the snapshot must be taken *after* networking is attached and the JuiceFS/data mount is live (and after app warmup, e.g. JVM), so restore skips those stages. Wake on the hot path must never include CNI/IPAM, mount, or boot.

### 2.4 — Same-namespace networking, isolation across namespaces
Use **Cilium** (eBPF CNI). A namespace is the tenant boundary. Within a namespace, sandboxes share an overlay and can address each other as if on one LAN. Across namespaces, a **default-deny** `CiliumNetworkPolicy` blocks traffic, with explicit allow only for platform ingress. Cilium's policies are identity-based (labels/namespace), not IP-based, which suits ephemeral microVMs whose IPs churn constantly. Hubble gives per-namespace flow visibility for debugging "why was this allowed/denied."

### 2.5 — Metrics
Prometheus scrapes three tiers: **platform** (API latency, pool-hit rate, wake p50/p99, fork time, restore time), **node** (capacity, KVM slot usage, host load), and **per-sandbox** (CPU/mem/IO/net from cgroups + Firecracker's own metrics endpoint). Network flows come from **Hubble**; traces via **OpenTelemetry**. Grafana dashboards, embedded into the UI. The pool-hit rate and wake-time histograms are the KPIs that tell you whether the platform is meeting its latency promise.

### 2.6 — Fork / branch a microVM fast
Same CoW-memory mechanism as §2.3: **fork** spawns N children from a warm parent in ~tens of ms; **branch** pauses a *running* sandbox, snapshots in-flight state, and resumes — roughly 150 ms — so a workload can be branched mid-execution, not only from a clean template. Disk forking is handled by the CoW disk layer (§2.7). Expose this as first-class API verbs (`fork`, `branch`) and visualize the resulting tree in the UI.

### 2.7 — Effectively infinite storage (and why this exact design)
There are **two different storage needs**, and conflating them is the usual mistake:

**(a) Per-VM root / ephemeral disk** — where boot speed and fork matter. Use **overlayfs over a shared read-only base image** (squashfs lower layer, small ext4 upper layer per VM). This is what E2B does: one base image is shared across thousands of VMs; each VM only stores its diffs, so creation is fast and space-efficient. Forking a disk = a new upper layer over the same lower layer.

**(b) Per-sandbox persistent "infinite" data volume** — use **JuiceFS**. *Why JuiceFS specifically, after evaluating the field:*

| Option | Model | Why not the primary choice |
|---|---|---|
| **JuiceFS** ✅ | POSIX FS; files chunked to object storage, metadata in Redis/TiKV/Postgres, local + distributed cache | **Chosen.** Most mature open POSIX-on-object-store; capacity is the object store (effectively infinite, cheap); pluggable HA metadata; read-through NVMe cache; data is node-independent (critical for §2.2 reschedule) |
| Ceph RBD / CephFS | Distributed block/file cluster | Heavy operational burden; you run and babysit a storage cluster. Object-native JuiceFS is cheaper to scale to "infinite" and simpler to operate |
| ZFS zvol/dataset | Local CoW with instant clones | Excellent CoW *but node-local* — pins data to a machine, which fights zero-downtime reschedule. Good for the root tier on a single node, wrong for the durable tier |
| dm-thin | Local thin-provisioned CoW (flintlock uses it) | Node-local again; same reschedule problem |
| Raw S3 | Object only | No POSIX semantics; sandbox apps expect a filesystem |
| Sprites-style chunked-disk + SQLite + Litestream | Custom object-backed block device with local metadata | The most elegant for disk specifically, but it is custom engineering. Worth building later for the root/disk tier; JuiceFS gets you there now for the data tier |

**Net recommendation:** overlayfs for the root disk (speed + dedup), **JuiceFS for the persistent data tier** (infinite, object-backed, node-independent, cached). The decisive factor is that JuiceFS keeps durable data *off the node*, which is what makes §2.2 reschedule and §2.1 scale-out clean. Keep an eye on building the Sprites-style chunked block device later if you want the disk tier itself to be object-backed and instantly forkable.

> Note: JuiceFS local-only mode (single machine, local disk backend) is fine for the laptop phase; switch the backend to MinIO (local) then real object storage as you scale, with no application change.

### 2.8 — Fast, high-throughput networking (it is an IaaS)
Cilium's eBPF datapath replaces kube-proxy (no iptables rule explosion), supports **DSR** and **Maglev** for north-south load balancing, **XDP** for high-throughput ingress, and an eBPF **bandwidth manager** for per-tenant shaping. VM NICs use `virtio-net` with `vhost` offload. On real bare metal later, evaluate **SR-IOV / macvtap** to give hot sandboxes near-line-rate NICs. Disable tunnel encapsulation (native routing) where the network topology allows, for lowest overhead.

### 2.9 — Strong authentication, API auth, and a future Terraform provider
Design the auth model now so the Terraform provider is thin later. Four planes:

- **Humans / UI:** OIDC via a local **Keycloak**, issuing short-lived JWTs.
- **API / machine clients:** scoped, hashed **API keys** for simple use, plus **OAuth2 client-credentials / OIDC** for stronger machine auth. A token-exchange endpoint mints short-lived access tokens.
- **Terraform provider:** the modern, recommended pattern is **OIDC dynamic credentials** — Terraform (Cloud/Enterprise or CI) presents a signed workload-identity JWT, your API validates it against the IdP's public keys and issues a short-lived token. Support both `token = "..."` (static, local dev) and an `oidc { ... }` block in the provider config, mirroring how mature providers (azurerm, Infisical) do it. Design API resources to be resource-oriented and declarative from day one (see §5) so each maps cleanly to a TF resource.
- **Workload identity (VM-to-VM, services):** **SPIFFE/SPIRE** issues an SVID to each microVM/service, so internal calls use mutual TLS with no shared secrets. SPIRE can federate to OIDC, so the same identity model extends outward.
- **Per-sandbox tokens:** like E2B, a dual system — a platform API key to manage sandboxes, and separate **short-lived per-sandbox tokens** to talk *to* a specific sandbox.

### 2.10 — Document everything
Docs-as-code in the repo: **MkDocs Material** (or Docusaurus) site, **OpenAPI** spec generated from the API, **Architecture Decision Records** (ADRs) for every significant choice (this document is ADR-0000), runbooks per failure mode, and a per-component README. Docs build in CI; a broken doc build fails the pipeline.

---

## 3. Component stack (the concrete choices)

| Concern | Choice | Notable alternative | Rationale |
|---|---|---|---|
| Hypervisor / VMM | Firecracker (CLH pluggable) | Cloud Hypervisor | Smallest footprint, fastest snapshot/fork; CLH when live-migration needed |
| Cluster / capacity | k3s (now) → kubeadm/k8s (later) | Nomad | Single binary, easy join, you already have it running on Lima |
| Sandbox lifecycle | Custom node-agent (Go) | Kata RuntimeClass | Direct Firecracker control for fork/snapshot; off the scheduler hot path |
| Control-plane API | CRDs + operator (declarative) + REST/gRPC gateway | Pure custom DB API | Stable declarative surface for kubectl *and* the TF provider |
| State DB | Postgres | etcd only | Relational queries for billing/quotas/audit |
| Root disk | overlayfs / squashfs base | ZFS, dm-thin | Shared read-only base, tiny per-VM diffs, fast fork |
| Data volume | JuiceFS on MinIO → object store | Ceph, raw S3 | Infinite, object-backed, node-independent, cached (see §2.7) |
| Networking | Cilium (eBPF) + Hubble | Calico, flannel | Throughput, identity-based policy, observability |
| Identity | Keycloak (OIDC) + SPIRE (SPIFFE) | Auth0, custom JWT | Standards-based; OIDC lines up with TF dynamic credentials |
| Metrics | Prometheus + Grafana + Hubble + OTel | Datadog | Self-hosted, local-friendly; Datadog later if desired |
| Secrets | Vault (or SOPS for local) | k8s Secrets only | Real secret lifecycle; SPIRE reduces shared secrets anyway |
| UI | Next.js + Tailwind + xterm.js | SvelteKit | Web terminal over vsock, fork-tree viz, Grafana embeds |
| Node-agent API | gRPC | REST | Low-latency hot path, streaming for logs/metrics |
| Docs | MkDocs Material + OpenAPI + ADRs | Docusaurus | Markdown-native, CI-built |

---

## 4. Build plan — local-first, phased

Each phase has a goal, a concrete deliverable, and an acceptance test you can actually run. Do them in order; each builds on the last.

**Phase 0 — Cluster bootstrap (done).** Two Lima VMs, k3s server + agent over vzNAT, `/dev/kvm` confirmed on the worker.
*Acceptance:* `kubectl get nodes` shows both Ready; `ls /dev/kvm` works in the worker.

**Phase 1 — Single sandbox lifecycle.** Node-agent (Go, `firecracker-go-sdk`) exposing gRPC `Create/Start/Stop/Delete`. A thin CLI. One Alpine/Ubuntu arm64 microVM boots and runs a command.
*Acceptance:* CLI creates a VM, runs `uname -a` inside it via vsock, deletes it.

**Phase 2 — Snapshot/restore + warm pool + activator.** Node-agent snapshots a booted VM; restores on demand; maintains N warm VMs. A front proxy buffers a request and triggers restore (wake-on-request).
*Acceptance:* cold create vs warm-pool wake measured; warm wake < 200 ms locally; the histogram is exported to Prometheus.

**Phase 3 — Fork / branch.** Integrate CoW-memory fork (forkd-style `MAP_PRIVATE` of parent memory). `fork` and `branch` gRPC verbs.
*Acceptance:* fork 10 children from one parent in < 0.5 s total; each child diverges independently.

**Phase 4 — Storage tiers.** overlayfs root over a shared squashfs base; JuiceFS data volume backed by local MinIO with an NVMe cache dir; mount established *before* the snapshot.
*Acceptance:* two sandboxes share a base image (disk usage proves dedup); each writes to its own JuiceFS volume; a restored snapshot already has its mount live.

**Phase 5 — Networking.** Install Cilium; per-namespace overlay; default-deny cross-namespace `CiliumNetworkPolicy`; bandwidth manager on.
*Acceptance:* two sandboxes in namespace A ping each other; a sandbox in namespace B cannot reach them; Hubble shows the allow/deny.

**Phase 6 — Control plane + auth.** Sandbox/Pool/Template CRDs + operator reconciling to node-agent calls; Postgres for state; Keycloak OIDC; API keys; SPIRE issuing SVIDs; per-sandbox tokens.
*Acceptance:* `kubectl apply` of a `Sandbox` CR produces a running VM; an unauthenticated API call is rejected; a per-sandbox token only works for its sandbox.

**Phase 7 — Metrics & observability.** Prometheus + Grafana + Hubble + OTel traces; the three-tier metric taxonomy from §2.5; audit log.
*Acceptance:* a Grafana dashboard shows wake p50/p99, pool-hit rate, per-sandbox CPU/mem, and live network flows.

**Phase 8 — UI.** Next.js app: machines, namespaces, sandbox list with wake/fork/branch actions, fork-tree visualization, embedded Grafana, web terminal (xterm.js over a websocket to vsock).
*Acceptance:* create, fork, and open a terminal into a sandbox entirely from the browser.

**Phase 9 — Reschedule / migration.** Snapshot → ship → restore on the other Lima node, with the proxy buffering; measure perceived downtime.
*Acceptance:* a running sandbox moves from node-0 to node-1; an in-flight HTTP request succeeds across the move (buffered), data volume intact.

**Phase 10 — Terraform provider + docs.** A provider exposing `hearth_namespace`, `hearth_sandbox`, `hearth_template`, `hearth_pool`; OIDC + token auth. MkDocs site, OpenAPI, ADRs, runbooks.
*Acceptance:* `terraform apply` creates a namespace + sandbox; `terraform destroy` removes them; the docs site builds in CI.

---

## 5. Control-plane API and resource model

Design the API resource-oriented and declarative so the operator, the UI, and the Terraform provider all target the same nouns. Core resources:

- **Machine** — a registered node-agent host and its capacity (read-mostly; created by the installer, not the user).
- **Namespace** — the tenant boundary; owns network isolation, quotas, and members.
- **Template** — a named, snapshotted base environment (built from a Dockerfile-like spec, converted to a microVM snapshot).
- **Pool** — desired count of pre-warmed VMs for a template (drives the warm-pool node-agent).
- **Sandbox** — a running microVM: its template, namespace, resources, and lifecycle state. Supports `fork` and `branch` sub-operations.
- **Snapshot** — a saved sandbox state (for restore, fork parents, or migration).
- **Volume** — a JuiceFS-backed persistent data volume attachable to sandboxes.

Internal transport is gRPC (orchestrator ↔ node-agent, streaming for logs/metrics). External is REST/JSON via a gRPC-gateway plus gRPC for power users. Because the declarative objects are CRDs, the operator pattern handles reconciliation and the Terraform provider can target a stable, versioned API.

---

## 6. The node-agent (where most of the hard, novel work lives)

Responsibilities: manage Firecracker processes (via jailer for seccomp/cgroup confinement), take and restore snapshots, perform CoW memory forks, assemble overlay root disks, attach Cilium endpoints / tap devices, ensure the JuiceFS mount is live before snapshotting, and emit metrics. It owns the **hot path**: on a warm-pool hit it hands out a ready VM without any control-plane round-trip to the scheduler.

Language: start in **Go** (`firecracker-go-sdk`, fast iteration, good gRPC story). If the fork/restore hot path needs every microsecond later, rewrite just that path in **Rust** (pairs naturally with Firecracker). Keep the VMM behind an interface so Cloud Hypervisor can be slotted in for live-migration workloads.

---

## 7. Security model (defense in depth)

Isolation layers, outermost to innermost: Firecracker hardware virtualization → jailer (seccomp, cgroups, chroot, namespaces) → Cilium network policy (default-deny across tenants) → namespace = tenant boundary with **no VM reuse across tenants** (restore a fresh instance from a clean snapshot, discard after use) → SPIFFE mTLS for internal calls → OIDC for users, short-lived tokens for APIs and sandboxes → secrets in Vault. Snapshot hygiene is mandatory: after every restore, re-seed entropy/RNG, fix the guest clock, and rotate any baked-in secrets, since restoring the same image repeatedly is otherwise a known Firecracker footgun.

---

## 8. Honest risks and limitations

- **Zero-downtime reschedule is the hardest promise.** Firecracker gives near-zero (sub-second buffered pause), not true live migration. Genuine uninterrupted migration requires Cloud Hypervisor. Set expectations accordingly and keep the VMM pluggable.
- **Local arm64 vs production x86.** The Lima lab validates *architecture and wiring* (CRDs, node-agent, fork, mounts, policies), not binary artifacts or exact production boot timings. Plan a bare-metal x86 validation pass before any real workloads.
- **Operational complexity.** This is rebuilding what Northflank/Fly/E2B sell; it is months of work and an ongoing operational commitment. Each subsystem (storage metadata HA, network, identity) is a service you will operate forever.
- **"Infinite" is bounded by the object store and the JuiceFS metadata engine.** Capacity scales with the object store, but the metadata engine is the critical path and SPOF — it must be HA from Phase 6 onward.
- **Fork CoW requires a recent kernel** (`userfaultfd`, a Firecracker fork-capable build); pin and document kernel/feature requirements per node.

---

## 9. Repository and documentation layout

```

hearth/
  node-agent/        # Go/Rust: Firecracker lifecycle, fork, snapshots, hot path (gRPC)
  control-plane/     # operator + CRDs + REST/gRPC gateway + Postgres
  proxy/             # activator: wake-on-request, request buffering, routing
  cli/               # developer + admin CLI
  ui/                # Next.js app (terminal, fork-tree, dashboards)
  terraform-provider-hearth/
  images/            # base rootfs builds (squashfs), template build tooling
  deploy/            # k3s, Cilium, JuiceFS/MinIO, Keycloak, SPIRE, Prometheus manifests
  docs/              # MkDocs Material site
    adr/             # architecture decision records (this file = ADR-0000)
    api/             # generated OpenAPI
    runbooks/        # per-failure-mode procedures
  scripts/           # bootstrap (the Lima/k3s script), dev loops

```

---

## 10. Immediate next three actions

1. Stand up the Phase 1 node-agent skeleton and boot one Firecracker microVM on `lima-kata-lab-1` via gRPC.
2. Add snapshot/restore and a fixed warm pool (Phase 2); start exporting the wake-time histogram so you measure from day one.
3. Write ADR-0001 (sandboxes-are-not-pods) and ADR-0002 (storage tiering) while the reasoning is fresh — these are the two decisions everything else depends on.
