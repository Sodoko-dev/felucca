# Hearth backend

Hearth manages Firecracker microVMs ("sandboxes") for AI workloads. The backend
is two Zig binaries built from a single `zig build`:

- **hearthd** — the control plane. Listens on `0.0.0.0:8080`, serves the static
  UI, exposes the REST API, schedules sandboxes onto agents, persists state to a
  JSON file, and proxies lifecycle calls to the owning agent.
- **hearth-agent** — the node agent. Runs on each worker, registers + heartbeats
  with the control plane, and drives Firecracker over its unix API socket.

Shared HTTP/1.1, JSON, router and model code lives in `src/common/`.

## Requirements

- Zig **0.16.0**. In this lab Zig runs only inside the Lima VM `infra-saas-lab`
  (`/usr/local/bin/zig`). The project is mounted read-write at the same path
  inside that VM.
- Workers (`kata-lab-0` / `kata-lab-1`) need Firecracker (`/usr/local/bin/firecracker`),
  a guest kernel (`/srv/ignis/kernels/vmlinux`), a base rootfs
  (`/srv/ignis/images/ubuntu-base.ext4`) and `/dev/kvm` access (the `kvm` group).

## Build

All build commands run inside `infra-saas-lab`:

```sh
# Debug build for the local (native aarch64) VM -> zig-out/bin/{hearthd,hearth-agent}
limactl shell infra-saas-lab -- bash -c \
  'cd /Users/magdy/projects/github.com/alpham/infra-saas/backend && zig build'

# Unit tests (router matching + JSON round-trip + model serialization)
limactl shell infra-saas-lab -- bash -c \
  'cd /Users/magdy/projects/github.com/alpham/infra-saas/backend && zig build test --summary all'

# Static binaries that run on any aarch64 Linux node (musl, fully static)
limactl shell infra-saas-lab -- bash -c \
  'cd /Users/magdy/projects/github.com/alpham/infra-saas/backend && zig build -Dtarget=aarch64-linux-musl'

# Static x86_64 binaries (production servers are typically x86_64)
limactl shell infra-saas-lab -- bash -c \
  'cd /Users/magdy/projects/github.com/alpham/infra-saas/backend && zig build -Dtarget=x86_64-linux-musl'
```

Both `-Dtarget=aarch64-linux-musl` and `-Dtarget=x86_64-linux-musl` produce
statically linked ELF binaries in `zig-out/bin/` suitable for copying to the
worker nodes. `zig build test` covers router matching, JSON round-trip, model
serialization, config precedence, constant-time token compare, and the IP
allocator.

## Configuration (v2)

Both binaries load configuration with the precedence **flags > env vars > config
file > built-in defaults** (`API-V2.md` §6). Nothing hardcodes localhost or Lima
paths; the local lab and remote servers differ only by config.

- `--config <path>` / `HEARTH_CONFIG` selects a JSON config file.

**hearthd** keys (JSON / env / flag):

| JSON | Env | Flag | Default |
| --- | --- | --- | --- |
| `bind` | `HEARTH_BIND` | `--bind` | `0.0.0.0:8080` |
| `ui_dir` | `HEARTH_UI_DIR` | `--ui-dir` | `/usr/share/hearth/ui` |
| `state_path` | `HEARTH_STATE` | `--state` | `/var/lib/hearth/state.json` |
| `token` | `HEARTH_TOKEN` | `--token` | _(empty = auth off)_ |

**hearth-agent** keys:

| JSON | Env | Flag | Default |
| --- | --- | --- | --- |
| `bind` | `HEARTH_AGENT_BIND` | `--bind` | `0.0.0.0:9090` |
| `control_plane` | `HEARTH_CONTROL_PLANE` | `--control-plane` | `http://127.0.0.1:8080` |
| `advertise_addr` | `HEARTH_ADVERTISE_ADDR` | `--advertise-addr` | _(auto-detected)_ |
| `data_dir` | `HEARTH_DATA_DIR` | `--data-dir` | `/srv/ignis` |
| `token` | `HEARTH_TOKEN` | `--token` | _(empty = auth off)_ |
| `net` | `HEARTH_NET` | `--net` | `on` (`on`/`off`) |
| `net_cidr` | `HEARTH_NET_CIDR` | `--net-cidr` | `10.231.0.0/24` |
| `pool_size` | `HEARTH_POOL_SIZE` | `--pool-size` | `0` |

When `advertise_addr` is unset the agent auto-detects the source IP used to reach
the control plane (a route-aware UDP-connect + `getsockname`).

### Bearer-token auth

When `token` is set on hearthd, every `/api/*` request (and agent
register/heartbeat) must carry `Authorization: Bearer <token>`; comparison is
constant-time. `/healthz`, `/metrics`, and the static UI stay open; a missing or
wrong token yields `401 {"error":"unauthorized"}`. hearthd forwards the token on
proxy calls to agents; agents verify it when their own `token` is set. TLS is
terminated by a reverse proxy (caddy/nginx) in production — see DEPLOYMENT.md
under `deploy/` (which also ships the systemd units).

### Guest networking (v2)

With `net on` (default) each node brings up a `hearth0` bridge (gateway = the
CIDR's `.1`), enables IPv4 forwarding, and installs an `nftables` masquerade rule
for egress. Each VM gets a tap (`hth-<n>`) enslaved to the bridge, a sequential
guest IP from the CIDR, and a kernel `ip=` boot arg. The sandbox `ip` field is
populated when networking is on, `null` under `net off` (which restores v1
behavior). Network setup runs `ip`/`nft` directly, falling back to `sudo` (so it
works both as root via systemd and as an unprivileged lab user with passwordless
sudo).

## Run the control plane (hearthd)

```sh
# Inside infra-saas-lab. --ui-dir is optional (missing dir is handled gracefully).
limactl shell infra-saas-lab -- \
  /Users/magdy/projects/github.com/alpham/infra-saas/backend/zig-out/bin/hearthd \
    --state /tmp/hearth/state.json \
    --ui-dir /tmp/hearth/ui
```

State is written atomically (tmp + rename) and reloaded on boot. To enable auth,
set `HEARTH_TOKEN` (env), `token` in the config file, or `--token`.

Smoke test from inside the VM (with auth on, pass the bearer token):

```sh
curl -s http://127.0.0.1:8080/healthz                                  # {"ok":true} (open)
curl -s -H 'Authorization: Bearer <token>' http://127.0.0.1:8080/api/v1/nodes
curl -s http://127.0.0.1:8080/metrics                                  # Prometheus text (open)
```

## Deploy + run an agent (hearth-agent)

`limactl cp <host-path> kata-lab-0:/tmp/...` copies the static binary onto a
worker. (If `limactl cp` is unavailable, stage the binary into the UI dir and
pull it from the worker over hearthd's static file server.)

> Network note: the lima `lima0` network (`192.168.64.x`) is L2-isolated between
> VMs here, so VM-to-VM traffic uses the secondary lima user network
> (`192.168.104.x`): `infra-saas-lab = 192.168.104.3`, `kata-lab-0 = 192.168.104.1`,
> `kata-lab-1 = 192.168.104.4`.

```sh
# 1) Copy the static agent binary to the worker and install it.
limactl cp zig-out/bin/hearth-agent kata-lab-0:/tmp/hearth-agent
limactl shell kata-lab-0 -- sudo install -m 0755 /tmp/hearth-agent /usr/local/bin/hearth-agent

# 2) Run the agent. `sg kvm -c ...` ensures /dev/kvm access in a fresh session.
#    Token + net + pool come from env/flags/config (advertise auto-detects).
limactl shell kata-lab-0 -- sg kvm -c \
  'HEARTH_TOKEN=hearth-lab-token /usr/local/bin/hearth-agent \
     --control-plane http://192.168.104.3:8080 \
     --data-dir /srv/ignis \
     --net on --net-cidr 10.231.0.0/24 \
     --pool-size 1'
```

On start the agent: loads config, auto-detects `advertise_addr` (unless set),
brings up the bridge/NAT (when `net on`), reconciles persisted instance dirs from
their `meta.json` (a `running` instance whose pid is gone becomes `sleeping` if a
snapshot is on disk, else `stopped` — so **sleeping VMs survive agent
restarts**), prewarms the pool, then registers and heartbeats every 5s (reporting
`vm_count` and `pool_size`). A node is `down` if its heartbeat is older than 15s.

Within ~5s the node appears `ready`:

```sh
limactl shell infra-saas-lab -- curl -s http://127.0.0.1:8080/api/v1/nodes
```

## Sandbox lifecycle (through the control plane)

```sh
# Create (scheduler picks the ready node with the lowest vm_count, proxies to it).
limactl shell infra-saas-lab -- curl -s -X POST \
  http://127.0.0.1:8080/api/v1/sandboxes \
  -d '{"name":"demo","namespace":"default","vcpus":1,"mem_mib":256}'

ID=sb-...   # from the response

# Inspect / list
limactl shell infra-saas-lab -- curl -s http://127.0.0.1:8080/api/v1/sandboxes/$ID
limactl shell infra-saas-lab -- curl -s http://127.0.0.1:8080/api/v1/sandboxes

# Transitions (each proxies to the owning agent)
limactl shell infra-saas-lab -- curl -s -X POST http://127.0.0.1:8080/api/v1/sandboxes/$ID/pause   # -> paused (PATCH /vm Paused)
limactl shell infra-saas-lab -- curl -s -X POST http://127.0.0.1:8080/api/v1/sandboxes/$ID/resume  # -> running (PATCH /vm Resumed)
limactl shell infra-saas-lab -- curl -s -X POST http://127.0.0.1:8080/api/v1/sandboxes/$ID/stop    # -> stopped (SIGTERM firecracker)
limactl shell infra-saas-lab -- curl -s -X POST http://127.0.0.1:8080/api/v1/sandboxes/$ID/start   # -> running (fresh spawn, reuses rootfs)
limactl shell infra-saas-lab -- curl -s -X DELETE http://127.0.0.1:8080/api/v1/sandboxes/$ID       # 204, removes instance dir

# v2 transitions (pass -H 'Authorization: Bearer <token>' when auth is on)
limactl shell infra-saas-lab -- curl -s -X POST .../api/v1/sandboxes/$ID/sleep   # -> sleeping (pause+snapshot, kills FC, frees RAM)
limactl shell infra-saas-lab -- curl -s -X POST .../api/v1/sandboxes/$ID/wake    # -> running; body adds "wake_ms"
limactl shell infra-saas-lab -- curl -s -X POST .../api/v1/sandboxes/$ID/fork \
  -d '{"name":"child"}'                                                          # -> 201 child JSON with parent_id set
```

On the worker, a running sandbox lives in `/srv/ignis/instances/<dir_id>/`:
`rootfs.ext4` (reflink-or-copy of the base image), `fc.sock` (Firecracker API
socket), `serial.log` (guest console + Firecracker API log), `meta.json`, and —
when sleeping — `vmstate.bin` + `mem.bin` (the Full snapshot). `dir_id` equals
the sandbox id except for a VM claimed from the warm pool, which keeps the pool
directory so the firecracker drive path baked into its snapshots stays valid.

```sh
limactl shell kata-lab-0 -- pgrep -af firecracker
limactl shell kata-lab-0 -- grep -iE 'ubuntu|login:' /srv/ignis/instances/<id>/serial.log
```

## REST API summary

Control plane (`:8080`):

| Method | Path | Notes |
| --- | --- | --- |
| GET | `/healthz` | `{"ok":true}` |
| GET | `/api/v1/nodes` | node table with computed `status` |
| POST | `/api/v1/agents/register` | idempotent by hostname, returns `{"id"}` |
| POST | `/api/v1/agents/heartbeat` | `200` |
| GET | `/api/v1/sandboxes` | list |
| POST | `/api/v1/sandboxes` | `201` sandbox JSON; `503` if no ready node |
| GET | `/api/v1/sandboxes/{id}` | sandbox or `404` |
| POST | `/api/v1/sandboxes/{id}/{stop\|start\|pause\|resume}` | proxied, `200` |
| POST | `/api/v1/sandboxes/{id}/sleep` | proxied, `200` sandbox JSON, state `sleeping` |
| POST | `/api/v1/sandboxes/{id}/wake` | proxied, `200` sandbox JSON + `wake_ms` |
| POST | `/api/v1/sandboxes/{id}/fork` | `201` child JSON with `parent_id` set |
| DELETE | `/api/v1/sandboxes/{id}` | proxied, `204` |
| GET | `/metrics` | Prometheus text |
| GET | `/...` | static UI (index.html fallback) |

All `/api/*` routes require the bearer token when `token` is configured.

Agent (`:9090`): `GET /healthz`, `GET /v1/vms`, `POST /v1/vms`,
`POST /v1/vms/{id}/{pause\|resume\|stop\|start\|sleep\|wake\|fork}`,
`DELETE /v1/vms/{id}`. All `/v1/*` routes require the bearer token when the
agent's `token` is set.

`/metrics` (hearthd) adds in v2: `hearth_wake_ms_last`, `hearth_wake_total`,
`hearth_wake_ms_sum`, `hearth_forks_total`, `hearth_pool_size{node=...}`, and a
`hearth_sandboxes_total{state="sleeping"}` series.

## Implementation notes

- Built on the reworked Zig 0.16 `std.Io` networking (`std.Io.net`) and the
  `std.process.Init`/`Threaded` runtime. The accept loop is thread-per-connection
  with per-request arena allocators; shared state is guarded by a small spinlock
  (`common.SpinLock`) since `std.Io.Mutex` would require threading `Io` through
  every mutation.
- Outbound HTTP/1.1 (hearthd -> agent over TCP, agent -> Firecracker over the
  unix socket) is hand-rolled in `src/common/client.zig`.
- Config loading lives in `src/common/config.zig` (precedence flags > env > file
  > defaults); `advertise_addr` auto-detect in `src/common/netdetect.zig`; the IP
  allocator in `src/agent/ipalloc.zig`; bridge/tap/NAT in `src/agent/net.zig`.
- **sleep** pauses then `PUT /snapshot/create` (Full) and SIGKILLs + reaps
  firecracker; **wake** spawns a fresh firecracker and `PUT /snapshot/load`
  (File backend, `resume_vm:true`, `network_overrides` to re-attach the tap),
  reporting the measured `wake_ms`. **fork** snapshots the parent (resuming it
  afterwards if it was running), reflink-or-copies the parent rootfs + snapshot
  to the child dir, and restores the child on its own tap. The child inherits the
  parent's guest-internal IP (memory state) — duplicate-IP isolation is a
  documented v2 caveat, fixed in v3 (which also shares the parent's `mem.bin`
  read-only via CoW mmap to make fork near-instant).
- The **warm pool** keeps N paused 1 vCPU / 256 MiB VMs; a matching create
  (`vcpus=1, mem_mib=256`) claims one (resume + bookkeeping) and the pool refills
  asynchronously. The systemd units and reverse-proxy/TLS config live under
  `deploy/` (owned separately).
