# Hearth v2 — API & Configuration Contract

This is the binding contract between backend, UI, and deployment work for v2.
v1 behavior is unchanged unless stated. See ARCHITECTURE.md for v1.

## 1. Sandbox states (v2)

v1 states: `creating | running | paused | stopped | error`
v2 adds: **`sleeping`** — snapshotted to disk, **no Firecracker process**, RAM freed.

```
running|paused --sleep--> sleeping --wake--> running
```

`stopped` (cold, boots fresh) and `sleeping` (warm, resumes mid-execution) are distinct.

**`start` semantics (tightened in v3.1):** `start` is a cold boot and is only valid
from `stopped` or `error` (`running` → 200 no-op). From `paused` use `resume`, from
`sleeping` use `wake`; the agent answers **409** `{"error":"InvalidState: ..."}` and
hearthd forwards the 409 body. (Previously `start` on a paused VM spawned a second
Firecracker over the live instance, orphaning it — that path is now refused.)

## 2. New/changed control-plane endpoints (hearthd :8080)

All v1 endpoints unchanged. New:

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/api/v1/sandboxes/{id}/sleep` | — | 200 sandbox JSON, state=`sleeping`. Pause → `/snapshot/create` (Full) → kill FC process. Snapshot lives in the instance dir (`vmstate.bin`, `mem.bin`). |
| POST | `/api/v1/sandboxes/{id}/wake` | — | 200 `{...sandbox, "wake_ms": <int>}`, state=`running`. Spawn FC → `/snapshot/load` (File backend, `resume_vm:true`). |
| POST | `/api/v1/sandboxes/{id}/fork` | `{"name": "..."}` | 201 child sandbox JSON with `parent_id` set (replaces the v1 501 stub). Parent must be `running`, `paused`, or `sleeping`; parent is briefly paused if running. Child restores from the parent's snapshot with its own tap (`network_overrides`) and a copy (reflink when possible) of the parent rootfs. **Fixed in v3.1**: right after restore the agent re-MACs and re-IPs the child in-guest over vsock (best-effort — [API-V3-EXEC.md](API-V3-EXEC.md) §3), so the child answers on its own `ip` and the parent keeps its connectivity. |

Sandbox JSON gains no new required fields; `ip` is now populated when networking is on (string, e.g. `"10.231.0.12"`), still `null` when `--net off`.

## 3. Agent endpoints (hearth-agent :9090)

New, mirroring the control plane: `POST /v1/vms/{id}/sleep`, `POST /v1/vms/{id}/wake`,
`POST /v1/vms/{id}/fork` (body `{"id","name"}` for the child). Existing endpoints unchanged.

## 4. Warm pool (agent-internal, Tier A wake)

- Agent flag/config `pool_size` (default **0** = off). When >0 the agent keeps N **paused** generic
  VMs (1 vCPU / 256 MiB, base rootfs) pre-booted.
- `POST /v1/vms` with a matching shape (vcpus=1, mem_mib=256) claims a pool VM (resume + rename
  bookkeeping) instead of cold-booting; pool refills asynchronously.
- Non-matching shapes cold-boot as in v1. Pool size exposed as a metric (below).

## 5. Guest networking (tap + bridge + NAT)

- Agent flag/config `net_cidr` (default `10.231.0.0/24`), `net = on|off` (default **on**; `off`
  restores v1 behavior, `ip:null`).
- Per node at agent startup (idempotent): bridge `hearth0` with the CIDR's `.1` as gateway,
  `nftables` masquerade for egress, ip_forward on.
- Per VM: tap `hth-<n>` enslaved to `hearth0`; guest IP assigned sequentially from the CIDR;
  guest configured via kernel boot arg `ip=<ip>::<gw>:<mask>::eth0:off`; FC `network-interfaces`
  configured before boot. Snapshots are taken **after** networking is up; restore/fork uses
  `/snapshot/load` `network_overrides` to attach a (new) tap.
- Cross-namespace isolation (nftables drop between tenant IP sets) is **v3**; v2 delivers
  connectivity + egress NAT + populated `ip`.

## 6. Configuration & production deployment (both binaries)

Precedence: **flags > env vars > config file > defaults**. Nothing may hardcode
localhost/Lima paths — local lab and remote servers differ only by config.

- `--config <path>` loads a JSON config file. Env: `HEARTH_CONFIG`.
- **hearthd** keys (JSON / env / flag): `bind` (`HEARTH_BIND`, default `0.0.0.0:8080`),
  `state_path` (`HEARTH_STATE`, default `/var/lib/hearth/state.json`),
  `ui_dir` (`HEARTH_UI_DIR`, default `/usr/share/hearth/ui`), `token` (`HEARTH_TOKEN`).
- **hearth-agent** keys: `bind` (`HEARTH_AGENT_BIND`, default `0.0.0.0:9090`),
  `control_plane` (`HEARTH_CONTROL_PLANE`, e.g. `https://hearth.example.com` or `http://192.168.104.3:8080`),
  `advertise_addr` (`HEARTH_ADVERTISE_ADDR`; **if unset, auto-detect** the source IP used to reach
  the control plane), `data_dir` (`HEARTH_DATA_DIR`, default `/srv/ignis`), `token` (`HEARTH_TOKEN`),
  `pool_size`, `net`, `net_cidr`.
- **Auth**: when `token` is set on hearthd, every `/api/*` and agent-registration request requires
  `Authorization: Bearer <token>`; agents send it on register/heartbeat; hearthd sends it on proxy
  calls to agents (agents verify when their own `token` is set). `/healthz`, `/metrics`, and static
  UI stay open. Constant-time comparison. TLS itself is terminated by a reverse proxy
  (caddy/nginx) in production — documented in DEPLOYMENT.md, not implemented in-binary.
- Build targets: `zig build -Dtarget=aarch64-linux-musl` **and** `-Dtarget=x86_64-linux-musl`
  must both produce static binaries (production servers are typically x86_64).

## 7. Metrics additions (`/metrics`)

```
hearth_wake_ms_last        gauge   (last wake latency, per hearthd)
hearth_wake_total          counter
hearth_wake_ms_sum         counter (for avg = sum/total)
hearth_forks_total         counter
hearth_pool_size{node=...} gauge   (agent-side, aggregated by hearthd)
hearth_sandboxes_total{state="sleeping"} added to the existing state gauge set
```

## 8. UI requirements

- `sleeping` state: distinct color (cool blue/violet vs green running), moon glyph ok.
- Row actions: sleep (running/paused), wake (sleeping), fork (running/paused/sleeping — opens
  the existing fork modal, now functional), all via the endpoints above; show `wake_ms` in a toast.
- `ip` column shows the address when present.
- Optional bearer token: read from `localStorage.hearth_token` or `?token=` query param (which
  stores it and strips it from the URL); attach `Authorization: Bearer` to all API fetches; on
  401 show a non-blocking banner prompting for a token.
