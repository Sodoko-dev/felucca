# Hearth — Architecture

Hearth is a self-hosted infrastructure SaaS that manages **Firecracker microVMs ("sandboxes") for AI workloads**.
The backend is two static binaries — `hearthd` (control plane, **Go**) and `hearth-agent` (node agent,
**Rust**) — with no Kubernetes on the control path (languages per
[ADR-0003](adr/ADR-0003-go-rust-port.md); the wire and on-disk formats are unchanged from the Zig v2
implementation and enforced by `test/conformance/`). This document reflects **v2 as implemented, verified
(17/17 end-to-end checks), and running** in the Lima lab: snapshot-based sleep/wake (~70 ms measured),
fork, warm pools, guest networking, bearer-token auth, and config-driven deployment — the same
static binaries (aarch64 + x86_64) run on the local lab and on remote production servers,
differing only by configuration. Remaining v3 items are listed in §9.

Related docs: [PLAN.md](PLAN.md) (adopted plan), [API-V2.md](API-V2.md) (v2 API/config contract),
[DEPLOYMENT.md](DEPLOYMENT.md) (production install + builds), [ADR-0000](adr/ADR-0000-original-hearth-plan.md)
(original baseline), [ADR-0001](adr/ADR-0001-zig-backend.md) (superseded),
[ADR-0002](adr/ADR-0002-no-kubernetes-control-plane.md), [ADR-0003](adr/ADR-0003-go-rust-port.md)
(Go+Rust port and migration record).

---

## 1. System overview

```mermaid
flowchart TB
    subgraph client["Client"]
        UI["Hearth Console<br/>(static SPA — vanilla JS,<br/>3s polling, mock-mode fallback)"]
        CLI["curl / scripts"]
    end

    subgraph hearthd["hearthd — control plane (Go, :8080)"]
        AUTH["Bearer-token guard<br/>(optional, constant-time;<br/>healthz/metrics/UI stay open)"]
        STATIC["Static file server<br/>(serves ui/, index.html fallback)"]
        API["REST API v1+v2<br/>(JSON over HTTP/1.1;<br/>sleep · wake · fork verbs)"]
        SCHED["Scheduler<br/>(ready node with lowest vm_count)"]
        REG["Node registry<br/>(register / heartbeat + pool_size,<br/>down after 15s silence)"]
        STATE[("state.json<br/>(atomic tmp+rename,<br/>reloaded on boot)")]
        METRICS["/metrics<br/>(Prometheus text:<br/>wake_ms, forks, pool, states)"]
    end

    subgraph agent["hearth-agent — per worker (Rust, :9090)"]
        AAPI["Agent REST API<br/>(/v1/vms · sleep · wake · fork)"]
        VMM["VM manager<br/>(spawn firecracker, configure over UDS,<br/>snapshot/restore, track pids)"]
        POOL["Warm pool<br/>(pool_size paused VMs,<br/>claim on create, async refill)"]
        NET["Networking<br/>(bridge hearth0 + tap per VM,<br/>sequential IP, nft masquerade)"]
        HB["Heartbeat loop<br/>(every 5s: mem_free,<br/>vm_count, pool_size)"]
    end

    subgraph fc["Firecracker (one process per running sandbox)"]
        FCAPI["REST over unix socket<br/>fc.sock"]
        UVM["microVM guest<br/>(vmlinux + rootfs.ext4 copy,<br/>ip= boot-arg networking)"]
    end

    ASSETS[("/srv/ignis<br/>kernels/vmlinux<br/>images/ubuntu-base.ext4<br/>instances/&lt;id&gt;/<br/>(+ vmstate.bin/mem.bin<br/>when sleeping)")]

    UI -->|"GET /  +  /api/v1/*<br/>Authorization: Bearer"| AUTH
    CLI --> AUTH
    AUTH --> STATIC
    AUTH --> API
    API --> SCHED
    API --> REG
    API --> METRICS
    SCHED -->|"proxy create/lifecycle<br/>REST + token over private L2"| AAPI
    REG <--> |"register + heartbeat"| HB
    API <--> STATE
    AAPI --> VMM
    AAPI --> POOL
    VMM --> NET
    VMM -->|"PUT /boot-source, /drives,<br/>/machine-config, /actions,<br/>/snapshot/create, /snapshot/load"| FCAPI
    FCAPI --> UVM
    VMM --> ASSETS
```

**Hot-path principle (kept from the baseline plan):** sandboxes are *not* k8s pods. The agent drives
Firecracker directly; the control plane only does placement and proxying. v2 delivers this: the warm
pool is claimed and refilled entirely on the agent, and wake is one `/snapshot/load` away — no
control-plane round trip on the latency-critical path.

---

## 2. Deployment topology

The same binaries serve two topologies, differing **only by configuration**
(precedence: flags > `HEARTH_*` env > `--config` JSON > defaults — see [API-V2.md](API-V2.md) §6):

```mermaid
flowchart LR
    subgraph prod["Production (remote servers — deploy/ + DEPLOYMENT.md)"]
        direction LR
        PROXY["Reverse proxy (caddy/nginx)<br/>TLS termination<br/>only 443 exposed"]
        HDP["hearthd (systemd, hearth user)<br/>/etc/hearth/hearthd.json<br/>HEARTH_TOKEN"]
        W1["worker 1..N (systemd, root)<br/>hearth-agent + firecracker<br/>/dev/kvm, CAP_NET_ADMIN"]
        PROXY --> HDP
        HDP <-->|"private network only<br/>:9090 + bearer token"| W1
    end
```

### Lima lab (development)

```mermaid
flowchart LR
    subgraph mac["macOS host (nothing installed here)"]
        BROWSER["Browser<br/>http://127.0.0.1:8080"]
        LIMACTL["limactl (VM mgmt + builds via shell)"]
    end

    subgraph lab["Lima VM: infra-saas-lab — 192.168.104.3"]
        HD["hearthd :8080"]
        ZIG["Go 1.26 + Rust 1.96 toolchains<br/>(only build environment)"]
        UIDIR[("repo mount<br/>ui/ + go/ + rust/")]
    end

    subgraph k0["Lima VM: kata-lab-0 — 192.168.104.1"]
        A0["hearth-agent :9090"]
        F0["firecracker × N<br/>(/dev/kvm, nested virt)"]
    end

    subgraph k1["Lima VM: kata-lab-1 — 192.168.104.4"]
        A1["hearth-agent :9090"]
        F1["firecracker × N<br/>(/dev/kvm, nested virt)"]
    end

    BROWSER -->|"Lima port-forward<br/>guest :8080 → host 127.0.0.1"| HD
    HD <-->|"user-v2 network 192.168.104.0/24<br/>(vzNAT blocks guest↔guest)"| A0
    HD <--> A1
    A0 --- F0
    A1 --- F1
    HD --- UIDIR
```

> **Networking gotcha:** Apple's vzNAT (`lima0`, 192.168.64.x) gives **no guest-to-guest connectivity**
> (verified: 100% loss). All inter-VM traffic uses Lima's `user-v2` network. Guests inside microVMs
> get IPs from the per-node `net_cidr` (default `10.231.0.0/24`) via the `hearth0` bridge — verified
> by host→guest ping; `--net off` restores IP-less operation.

---

## 3. Sandbox state machine

```mermaid
stateDiagram-v2
    [*] --> creating : POST /api/v1/sandboxes
    creating --> running : pool claim (warm) or InstanceStart (cold)
    creating --> error : spawn/config failure
    running --> paused : POST .../pause  (PATCH /vm Paused)
    paused --> running : POST .../resume (PATCH /vm Resumed)
    running --> sleeping : POST .../sleep (pause → snapshot → kill, RAM freed)
    paused --> sleeping : POST .../sleep
    sleeping --> running : POST .../wake (snapshot/load, ~tens of ms)
    running --> stopped : POST .../stop  (SIGTERM firecracker)
    stopped --> running : POST .../start (fresh cold boot, reuses rootfs)
    running --> [*] : DELETE (kill + rm instance dir)
    paused --> [*] : DELETE
    sleeping --> [*] : DELETE
    stopped --> [*] : DELETE
    error --> [*] : DELETE
```

**v2 wake path (verified on the lab):** `sleep` = pause → `PUT /snapshot/create` (vmstate.bin +
mem.bin in the instance dir) → kill the Firecracker process (RAM freed). `wake` = fresh process →
`PUT /snapshot/load {resume_vm:true, network_overrides:[tap]}` → guest resumes mid-execution
(**measured wake_ms=73** on this lab). `fork` snapshots the parent (reusing snapshot files if
sleeping), copies rootfs + mem, restores the child with its own tap, sets `parent_id` — caveat:
the child inherits the parent's guest-internal IP until v3. `stopped` vs `sleeping`: cold boot
vs warm resume.

---

## 4. User flow — create a sandbox

```mermaid
sequenceDiagram
    autonumber
    actor U as User (UI / curl)
    participant H as hearthd :8080
    participant S as Scheduler
    participant A as hearth-agent :9090<br/>(chosen worker)
    participant F as firecracker<br/>(new child process)

    U->>H: POST /api/v1/sandboxes {name, namespace, vcpus, mem_mib}
    H->>S: place(sandbox)
    S->>S: filter nodes status=ready,<br/>pick lowest vm_count
    alt no ready node
        H-->>U: 503 no ready node
    end
    H->>A: POST /v1/vms {id, name, vcpus, mem_mib}
    alt warm-pool hit (1 vCPU / 256 MiB shape, pool_size > 0)
        A->>A: claim paused pool VM<br/>(rename bookkeeping, resume)
        A->>A: refill pool asynchronously
    else cold boot
        A->>A: mkdir /srv/ignis/instances/id/<br/>cp ubuntu-base.ext4 → rootfs.ext4
        A->>A: create tap hth-N on bridge hearth0,<br/>allocate next IP from net_cidr
        A->>F: spawn firecracker --api-sock fc.sock
        A->>A: poll fc.sock (≤3s)
        A->>F: PUT /boot-source (vmlinux,<br/>boot args incl. ip=10.231.0.x)
        A->>F: PUT /drives/rootfs
        A->>F: PUT /machine-config {vcpus, mem}
        A->>F: PUT /network-interfaces/eth0 {tap}
        A->>F: PUT /actions {InstanceStart}
        F-->>A: 204 — guest boots (serial.log)
    end
    A-->>H: 201 {state: running, ip: 10.231.0.x}
    H->>H: persist state.json (atomic)
    H-->>U: 201 sandbox JSON
    Note over U,H: UI poll picks up the new<br/>sandbox within 3s
```

On error at any agent step the sandbox is marked `error` and surfaced in the UI/API.

---

## 5. User flow — lifecycle operation (pause / resume / stop / delete)

```mermaid
sequenceDiagram
    autonumber
    actor U as User (UI / curl)
    participant H as hearthd
    participant A as owning hearth-agent
    participant F as firecracker

    U->>H: POST /api/v1/sandboxes/{id}/pause
    H->>H: look up owning node (node_id)
    H->>A: POST /v1/vms/{id}/pause
    A->>F: PATCH /vm {"state":"Paused"} over fc.sock
    F-->>A: 204
    A-->>H: 200
    H->>H: state=paused, persist
    H-->>U: 200

    Note over U,F: stop = SIGTERM to firecracker pid<br/>start = fresh spawn reusing rootfs.ext4<br/>DELETE = kill + rm -rf instances/id → 204
```

---

## 5b. User flow — sleep, wake, fork (the v2 hot path)

```mermaid
sequenceDiagram
    autonumber
    actor U as User (UI / curl)
    participant H as hearthd
    participant A as owning hearth-agent
    participant F as firecracker

    rect rgb(35,35,45)
    Note over U,F: SLEEP — free the RAM, keep the moment
    U->>H: POST /api/v1/sandboxes/{id}/sleep
    H->>A: POST /v1/vms/{id}/sleep
    A->>F: PATCH /vm {"state":"Paused"}
    A->>F: PUT /snapshot/create<br/>{vmstate.bin + mem.bin → instance dir}
    A->>A: kill firecracker, reap<br/>(RAM freed, files remain)
    A-->>H: 200 → state=sleeping, persisted
    end

    rect rgb(30,40,35)
    Note over U,F: WAKE — measured 73 ms on this lab
    U->>H: POST /api/v1/sandboxes/{id}/wake
    H->>A: POST /v1/vms/{id}/wake
    A->>F: spawn fresh firecracker
    A->>F: PUT /snapshot/load {resume_vm:true,<br/>network_overrides:[tap]}
    F-->>A: guest resumes mid-execution<br/>(no boot, no init, no warmup)
    A-->>H: 200 {state: running, wake_ms}
    H-->>U: sandbox + wake_ms (UI toast)
    end

    rect rgb(40,35,30)
    Note over U,F: FORK — one parent, N children
    U->>H: POST /api/v1/sandboxes/{id}/fork {"name"}
    H->>A: POST /v1/vms/{id}/fork
    A->>A: ensure parent snapshot<br/>(reuse if sleeping — pause→snap→resume if running)
    A->>A: reflink/copy rootfs + mem.bin to child dir,<br/>create child tap
    A->>F: spawn + /snapshot/load (child tap override)
    A-->>H: 201 child {parent_id: parent}
    end
```

Snapshot hygiene caveats (documented, v3 work): forked children inherit the parent's
guest-internal IP, and restored guests should re-seed entropy / fix clocks before
multi-tenant production use. **Do not sleep a guest that is still booting**: a snapshot
taken ~1–2 s after boot captures a guest that panics/reboots on resume (wall-clock jump
mid-init) — the wake API reports `running` but the Firecracker process exits about a
second later. Found during the Go/Rust migration and present in every implementation;
`verify-v2.sh` now pings the guest after wake to keep this visible.

---

## 6. Node lifecycle — registration, heartbeat, failure detection

```mermaid
sequenceDiagram
    autonumber
    participant A as hearth-agent (worker)
    participant H as hearthd
    actor U as UI

    A->>H: POST /api/v1/agents/register<br/>{hostname, addr, cpus, mem_total_mib}
    Note right of H: idempotent by hostname —<br/>re-register keeps node id
    H-->>A: {id}
    loop every 5s
        A->>H: POST /api/v1/agents/heartbeat<br/>{id, mem_free_mib, vm_count, pool_size}
        H-->>A: 200
    end
    U->>H: GET /api/v1/nodes
    H->>H: lazily compute status:<br/>heartbeat > 15s old → "down"
    H-->>U: nodes with status ready/down
```

The scheduler reads `vm_count` from heartbeats, so placement freshness is bounded by the 5s
heartbeat interval (known wart: rapid back-to-back creates can land on one node). Registration
and heartbeats carry the bearer token when auth is enabled. Sleeping VMs survive agent restarts:
the agent reconciles instance dirs and `meta.json` on startup.

---

## 7. User flow — UI data loop (live vs mock, flicker-free updates)

```mermaid
flowchart TD
    START(["page load"]) --> TOK["?token= → localStorage,<br/>stripped from URL;<br/>Authorization header on every fetch"]
    TOK --> R0["initial render"]
    R0 --> P["poll /api/v1/nodes + /sandboxes<br/>every 3s"]
    P -->|fetch fails| MOCK["mock mode:<br/>built-in demo data,<br/>amber MOCK DATA badge"]
    MOCK --> P
    P -->|"401"| BANNER["auth banner:<br/>paste token inline → retry"]
    BANNER --> P
    P -->|fetch ok| DIFF{"structural change?<br/>(ignores volatile fields:<br/>last_heartbeat, mem_free_mib)"}
    DIFF -->|yes: create/delete/state/node change| FULL["full render()<br/>(rebuild view, entry animations)"]
    DIFF -->|no| PATCH["in-place DOM patch:<br/>heartbeat ages, memory bars,<br/>header stats, LIVE dot"]
    FULL --> P
    PATCH --> P
```

This split is what fixed the visible flicker: heartbeats mutate every poll, so only genuinely
structural changes rebuild the DOM; volatile values are patched into existing elements.

---

## 8. Storage layout (per worker)

```mermaid
flowchart LR
    subgraph srv["/srv/ignis"]
        K["kernels/vmlinux<br/>(shared guest kernel, firecracker-ci)"]
        B["images/ubuntu-base.ext4<br/>(2 GB shared base rootfs)"]
        subgraph inst["instances/&lt;sandbox-id&gt;/"]
            RF["rootfs.ext4 (private copy of base)"]
            SK["fc.sock (Firecracker API)"]
            SL["serial.log (guest console)"]
            MJ["meta.json (state, ip, snapshot paths —<br/>survives agent restarts)"]
            SN["vmstate.bin + mem.bin<br/>(present while sleeping /<br/>as fork parent snapshot)"]
        end
        subgraph pool["instances/pool-&lt;rand&gt;/"]
            PV["pre-booted paused pool VM<br/>(renamed on claim)"]
        end
    end
    B -->|"cp --reflink when supported"| RF
```

v3 replaces the full rootfs copy with **overlayfs**: shared read-only base + tiny per-VM upper layer
(the E2B/Modal image-dedup pattern), which also makes disk fork O(1) — today fork copies
`rootfs.ext4` and `mem.bin` (simple and safe; shared read-only memory mapping is the v3 optimization).

---

## 9. v2 status (shipped) and what remains for v3

Shipped in v2, then ported to Go (`hearthd`) and Rust (`hearth-agent`) under the frozen
API-V2 contract — all verified end-to-end on the lab (17/17 system checks +
`test/conformance/` contract suite, goldens recorded from the Zig reference;
migration record in [ADR-0003](adr/ADR-0003-go-rust-port.md)):

| Capability | v2 implementation |
|---|---|
| `sleep` / `wake` | snapshot/create → kill; snapshot/load resume — **wake_ms≈70 measured** (67–85 across both implementations) |
| `fork` | parent snapshot + rootfs/mem copy + own tap via `network_overrides`, `parent_id` set |
| Warm pool | `pool_size` paused VMs per agent; matching create claims one, async refill |
| Guest networking | bridge `hearth0` + per-VM tap + sequential IP (`net_cidr`) + nftables masquerade; `ip` populated; host→guest ping verified |
| Auth | optional bearer token (`HEARTH_TOKEN`/config/flag); 401 without; constant-time compare; UI `?token=` support |
| Production config | flags > env (`HEARTH_*`) > `--config` JSON > defaults; no hardcoded paths; static binaries for **aarch64 + x86_64**; systemd units + installer in `deploy/`, guide in `DEPLOYMENT.md` |

Remaining for v3:

| Capability | v3 mechanism |
|---|---|
| `branch` (fork a *running* VM without pausing perception) | diff snapshots + uffd shared-memory CoW (fork currently copies mem.bin) |
| Fork guest-IP uniqueness | re-IP child guests (vsock agent or DHCP) — today the child inherits the parent's IP |
| Cross-tenant network isolation | nftables drop between namespace IP sets |
| Root-disk dedup | overlayfs shared RO base (today: full rootfs copy per VM) |
| State store | JSON file → SQLite WAL → Postgres at scale-out |
| Reschedule across nodes | snapshot → ship → restore (needs node-decoupled storage) |
| OIDC / multi-user auth | token-exchange on top of the bearer layer |
| Pool-orphan reclaim | a paused pool FC orphaned by an agent restart is reconciled to `stopped` but its process is not reaped (1:1 with v2 behavior) |
| uffd lazy restore / CoW fork | in-process userfaultfd in the Rust agent (the reason the agent is Rust — rust-vmm territory) |
| Terraform provider | Go provider against the hearthd API (the reason the control plane is Go) |
