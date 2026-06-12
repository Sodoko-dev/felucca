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
v4: `POST /v1/vms` accepts an optional `"tenant_id"` (recorded in `meta.json`, inherited by
fork children and pool claims; feeds per-tenant network isolation). The `/v1/vms` list shape
is unchanged — tenancy is never exposed on the wire.

## 3b. Tenancy & API keys (v4, hearthd)

States and sandbox JSON are unchanged; tenancy is enforced purely through scoping.

- **Admin**: the configured bearer token (`HEARTH_TOKEN`/`--token`) — unrestricted,
  unmetered; required for all `/api/v1/tenants*`, `/api/v1/keys/*`, `/api/v1/nodes`,
  and `/api/v1/agents/*` routes (tenant keys get **404** on those, not 403).
- **Tenant API keys**: `hearth_sk_<48 hex>`; only the SHA-256 is stored. Sent as a
  normal bearer token. Unknown/revoked keys → 401.

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/api/v1/tenants` | `{"name", "max_sandboxes", "max_vcpus", "max_mem_mib", "max_disk_gb"}` (0 = unlimited) | 201 `{"tenant":{...},"api_key":"hearth_sk_…","key_id":"key-…"}` — the key is shown **once**. Duplicate name → 409. |
| GET | `/api/v1/tenants` | — | 200 `{"tenants":[...]}` |
| POST | `/api/v1/tenants/{id}/keys` | — | 201 `{"api_key":"hearth_sk_…","key_id":"key-…"}` (rotation: mint new, then revoke old) |
| DELETE | `/api/v1/keys/{id}` | — | 204; revocation is immediate. Unknown/already-revoked → 404. |

Scoping rules (apply to every sandbox route): a tenant key sees and acts on only its
own sandboxes; foreign and unknown ids are indistinguishable (**404**, body
`{"error":"not found"}`). Creates and forks count against the owning tenant's quotas →
**429** `{"error":"quota exceeded: <sandboxes|vcpus|mem_mib>"}`. Fork children inherit
the parent's tenant. Every lifecycle transition appends a row to the append-only
`usage_events` metering table (created/started/stopped/paused/resumed/slept/woken/
forked/deleted, with the sandbox shape) — aggregation endpoints arrive in a later phase.

Durable state moved from `state.json` to **SQLite (WAL)** at `--db`/`HEARTH_DB`/`db_path`
(default `/var/lib/hearth/hearth.db`). On first boot with an empty store, a legacy
`state.json` (from `--state`) is imported once and renamed `*.imported`. Wire formats
are unchanged; the conformance suite passes unmodified, plus new `hearthd/17-tenancy`
cases pin the scoping/quota behavior.

## 3c. WireGuard overlay & node join (v4 P2, hearthd + agent)

Workers join a remote hearthd over a hub-and-spoke WireGuard overlay instead of
sharing its L2 segment. Design + deferred items: [ADR-0006](adr/ADR-0006-wireguard-overlay-and-node-join.md).

| Method | Path | Auth | Body | Result |
|---|---|---|---|---|
| POST | `/api/v1/join-tokens` | admin | `{"node_hint"?}` (≤64 chars) | 201 `{"id":"jt-…","token":"hearth_jt_…"}` — token shown **once**, sha256-only at rest, TTL 24h, single-use. Tenant keys → 404. (Minting works even with the overlay off; the token just can't be redeemed until it's on.) |
| POST | `/api/v1/nodes/join` | the join token itself as bearer (routed before the normal bearer gate) | `{"pubkey","hostname"}` (44-char base64 pubkey) | 200 `{"overlay_ip","overlay_prefix","server_overlay_ip","server_pubkey","server_endpoint","keepalive_s"}`. Bad/used/expired token → uniform 401; overlay off → 503 (after the credential check); bad body → 400 **without** consuming the token. |

Semantics: the token is consumed **last** (after the peer row is persisted and
the kernel peer is installed), so no failure mode burns it. Re-join with the
same pubkey keeps the allocated overlay IP (while it fits the configured
subnet). The agent enrolls with `--join <url> --join-token <tok>`, persists the
grant to `{data_dir}/wg.json` (0600), and on every later boot brings the tunnel
up from the file (which takes precedence over `--join`) and registers over the
overlay (`advertise_addr` = its overlay IP). An unreadable or corrupt `wg.json`
is fatal — an enrolled node never silently degrades to direct mode. The https
join goes through `curl --config -` with the token on stdin, never argv.

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
- **Cross-tenant isolation is live (v4 P1)**: guest-to-guest traffic on `hearth0` is
  dropped unless source and destination belong to the same tenant — the optional
  `tenant_id` on `POST /v1/vms` (§3) governs membership; a VM with no/invalid tenant
  joins no pair and is isolated from all peers. Egress NAT and host↔guest traffic are
  unaffected. Tenant networks are node-scoped until the P2 overlay (per-node CIDRs are
  node-local). Design: [ADR-0005](adr/ADR-0005-cross-tenant-network-isolation.md).

## 6. Configuration & production deployment (both binaries)

Precedence: **flags > env vars > config file > defaults**. Nothing may hardcode
localhost/Lima paths — local lab and remote servers differ only by config.

- `--config <path>` loads a JSON config file. Env: `HEARTH_CONFIG`.
- **hearthd** keys (JSON / env / flag): `bind` (`HEARTH_BIND`, default `0.0.0.0:8080`),
  `state_path` (`HEARTH_STATE`, default `/var/lib/hearth/state.json`),
  `ui_dir` (`HEARTH_UI_DIR`, default `/usr/share/hearth/ui`), `token` (`HEARTH_TOKEN`).
- **hearthd** overlay/TLS keys (v4 P2): `wg_ip` (CIDR, e.g. `10.100.0.1/24` — setting it
  enables the overlay; requires auth-on and `wg_endpoint`), `wg_port` (default 51820),
  `wg_key_path`, `wg_endpoint` (public `host:port` advertised to joiners), `wg_keepalive`
  (default 25), `tls_domain` (enables in-binary autocert on :443 + HTTP-01 on :80; the
  plain `port` listener stays for in-tunnel agents), `tls_cache_dir`.
- **hearth-agent** keys: `bind` (`HEARTH_AGENT_BIND`, default `0.0.0.0:9090`),
  `control_plane` (`HEARTH_CONTROL_PLANE`, e.g. `https://hearth.example.com` or `http://192.168.104.3:8080`),
  `advertise_addr` (`HEARTH_ADVERTISE_ADDR`; **if unset, auto-detect** the source IP used to reach
  the control plane), `data_dir` (`HEARTH_DATA_DIR`, default `/srv/ignis`), `token` (`HEARTH_TOKEN`),
  `pool_size`, `net`, `net_cidr`, `join_url`/`join_token` (`HEARTH_JOIN_URL`/`HEARTH_JOIN_TOKEN`,
  `--join`/`--join-token` — first-boot enrollment only; the persisted `wg.json` wins afterwards).
- **Auth**: when `token` is set on hearthd, every `/api/*` and agent-registration request requires
  `Authorization: Bearer <token>`; agents send it on register/heartbeat; hearthd sends it on proxy
  calls to agents (agents verify when their own `token` is set). `/healthz`, `/metrics`, and static
  UI stay open. Constant-time comparison. TLS: in-binary autocert when `tls_domain` is set
  (v4 P2.3, see above); a reverse proxy (caddy/nginx) remains a valid alternative —
  both documented in DEPLOYMENT.md.
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
