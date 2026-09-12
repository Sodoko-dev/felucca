# Felucca v2 — API & Configuration Contract

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
feluccad forwards the 409 body. (Previously `start` on a paused VM spawned a second
Firecracker over the live instance, orphaning it — that path is now refused.)

## 2. New/changed control-plane endpoints (feluccad :8080)

All v1 endpoints unchanged. New:

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/api/v1/sandboxes/{id}/sleep` | — | 200 sandbox JSON, state=`sleeping`. Pause → `/snapshot/create` (Full) → kill FC process. Snapshot lives in the instance dir (`vmstate.bin`, `mem.bin`). |
| POST | `/api/v1/sandboxes/{id}/wake` | — | 200 `{...sandbox, "wake_ms": <int>}`, state=`running`. Spawn FC → `/snapshot/load` (File backend, `resume_vm:true`). |
| POST | `/api/v1/sandboxes/{id}/fork` | `{"name": "..."}` | 201 child sandbox JSON with `parent_id` set (replaces the v1 501 stub). Parent must be `running`, `paused`, or `sleeping`; parent is briefly paused if running. Child restores from the parent's snapshot with its own tap (`network_overrides`) and a copy (reflink when possible) of the parent rootfs. **Fixed in v3.1**: right after restore the agent re-MACs and re-IPs the child in-guest over vsock (best-effort — [API-V3-EXEC.md](API-V3-EXEC.md) §3), so the child answers on its own `ip` and the parent keeps its connectivity. |

Sandbox JSON gains no new required fields; `ip` is now populated when networking is on (string, e.g. `"10.231.0.12"`), still `null` when `--net off`.

### 2b. Request bounds and id shape (v4 hardening)

Applies to every feluccad route, not only the ones above.

| Rule | Behaviour |
|---|---|
| Request body size | Capped at **1 MiB** (`http.MaxBytesReader`), applied before any route is dispatched — including `POST /api/v1/nodes/join`, which runs ahead of the bearer gate |
| `name`, `namespace` (create **and** fork child names) | 1..64 bytes, printable ASCII (0x20–0x7e), with `<`, `>`, `"`, `'`, `&` refused outright — these strings are rendered by the operator console. Otherwise **400** `{"error":"invalid name"}` / `{"error":"invalid namespace"}`. `namespace` defaults to `"default"` when absent |
| `timeout_ms` on exec | Outside `[1, 300000]` → **400** `{"error":"timeout_ms out of range"}`, and the request never reaches the agent. **Refused, not clamped**: a negative value used to become an already-expired deadline. Absent → 30000. Both the buffered and `?stream=1` arms enforce it |

**Id shape.** Sandbox and node ids are `"<prefix>-"` + **26 hex chars** — 13
bytes from `crypto/rand`, 104 bits (`sb-…`, `node-…`). The persisted `seq`
counter still advances, but it is **no longer part of the id**: a monotonic tail
disclosed every other tenant's position in the issuance stream, and it cost
label budget that entropy needs more — a sandbox id is a genuine bearer
capability, because it appears in the public ingress label `<name>--<id>` (§3d)
which the gateway does not otherwise authenticate. Clients must treat ids as
opaque; nothing in the wire contract constrains their length or alphabet beyond
this.

## 3. Agent endpoints (felucca-agent :9090)

New, mirroring the control plane: `POST /v1/vms/{id}/sleep`, `POST /v1/vms/{id}/wake`,
`POST /v1/vms/{id}/fork` (body `{"id","name"}` for the child). Existing endpoints unchanged.
v4: `POST /v1/vms` accepts an optional `"tenant_id"` (recorded in `meta.json`, inherited by
fork children and pool claims; feeds per-tenant network isolation). The `/v1/vms` list shape
is unchanged — tenancy is never exposed on the wire.

## 3b. Tenancy & API keys (v4, feluccad)

States and sandbox JSON are unchanged; tenancy is enforced purely through scoping.

- **Admin**: the configured bearer token (`FELUCCA_TOKEN`/`--token`) — unrestricted,
  unmetered; required for all `/api/v1/tenants*`, `/api/v1/keys/*`, `/api/v1/nodes`,
  and `/api/v1/agents/*` routes (tenant keys get **404** on those, not 403).
- **Tenant API keys**: `felucca_sk_<48 hex>`; only the SHA-256 is stored. Sent as a
  normal bearer token. Unknown/revoked keys → 401.

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/api/v1/tenants` | `{"name", "max_sandboxes", "max_vcpus", "max_mem_mib", "max_disk_gb"}` (0 = unlimited) | 201 `{"tenant":{...},"api_key":"felucca_sk_…","key_id":"key-…"}` — the key is shown **once**. Duplicate name → 409. |
| GET | `/api/v1/tenants` | — | 200 `{"tenants":[...]}` |
| POST | `/api/v1/tenants/{id}/keys` | `{"expires_in_s"}` (optional; 0/absent = never expires, max 1 year) | 201 `{"api_key":"felucca_sk_…","key_id":"key-…","expires_at":<unix s, 0 = never>}` (rotation: mint new, then revoke old) |
| GET | `/api/v1/tenants/{id}/keys` | — | 200 `{"keys":[{"id","prefix","created_at","expires_at","revoked_at"}]}` — never the secret or its hash |
| DELETE | `/api/v1/keys/{id}` | — | 204; revocation is immediate. Unknown/already-revoked → 404. |

The listing is the recovery path for a leaked key whose `key_id` nobody kept:
`prefix` is the secret's first 15 chars (`felucca_sk_` + 4), enough to match a
leaked value against a row and revoke it, useless for guessing the rest.
Without it, `DELETE /api/v1/keys/{id}` is unusable and the only remediation is
raw SQL. An expired key is rejected exactly like a revoked one (401).

Scoping rules (apply to every sandbox route): a tenant key sees and acts on only its
own sandboxes; foreign and unknown ids are indistinguishable (**404**, body
`{"error":"not found"}`). Creates and forks count against the owning tenant's quotas →
**429** `{"error":"quota exceeded: <sandboxes|vcpus|mem_mib>"}`. Fork children inherit
the parent's tenant. Every lifecycle transition appends a row to the append-only
`usage_events` metering table (created/started/stopped/paused/resumed/slept/woken/
forked/deleted, with the sandbox shape) — aggregation endpoints arrive in a later phase.

Durable state moved from `state.json` to **SQLite (WAL)** at `--db`/`FELUCCA_DB`/`db_path`
(default `/var/lib/felucca/felucca.db`). On first boot with an empty store, a legacy
`state.json` (from `--state`) is imported once and renamed `*.imported`. Wire formats
are unchanged; the conformance suite passes unmodified, plus new `feluccad/17-tenancy`
cases pin the scoping/quota behavior.

## 3c. WireGuard overlay & node join (v4 P2, feluccad + agent)

Workers join a remote feluccad over a hub-and-spoke WireGuard overlay instead of
sharing its L2 segment. Design + deferred items: [ADR-0006](adr/ADR-0006-wireguard-overlay-and-node-join.md).

| Method | Path | Auth | Body | Result |
|---|---|---|---|---|
| POST | `/api/v1/join-tokens` | admin | `{"node_hint"?}` (≤64 chars) | 201 `{"id":"jt-…","token":"felucca_jt_…"}` — token shown **once**, sha256-only at rest, TTL 24h, single-use. Tenant keys → 404. (Minting works even with the overlay off; the token just can't be redeemed until it's on.) |
| POST | `/api/v1/nodes/join` | the join token itself as bearer (routed before the normal bearer gate) | `{"pubkey","hostname","node_token"?}` (44-char base64 pubkey) | 200 `{"overlay_ip","overlay_prefix","server_overlay_ip","server_pubkey","server_endpoint","keepalive_s","agent_token"}`. `agent_token` is this node's own `felucca_nt_…` credential, shown **once**; a re-join mints a fresh one and retires the old. Bad/used/expired token → uniform 401; **already-enrolled pubkey without a matching `node_token` → 409** (see below); overlay off → 503 (after the credential check); bad body → 400 **without** consuming the token. |

Semantics: the token is consumed **last** (after the peer row is persisted and
the kernel peer is installed), so no failure mode burns it. Re-join with the
same pubkey keeps the allocated overlay IP (while it fits the configured
subnet) — but must **prove possession** of that node's current credential in
`node_token`, because a join token authorizes *an* enrollment without naming
*which* node and the pubkey is caller-written; otherwise a token holder could
rotate a live worker's credential out from under it. Unproven re-joins get
`409`. `felucca-agent` does not send `node_token` (its body is `pubkey` +
`hostname` only), so the supported re-enrollment is a **fresh WireGuard key**:
remove `{data_dir}/wg.json` *and* `{data_dir}/wg.key` before restarting with a
new join token. The agent enrolls with `--join <url> --join-token <tok>`, persists the
grant to `{data_dir}/wg.json` (0600), and on every later boot brings the tunnel
up from the file (which takes precedence over `--join`) and registers over the
overlay (`advertise_addr` = its overlay IP). An unreadable or corrupt `wg.json`
is fatal — an enrolled node never silently degrades to direct mode. The https
join goes through `curl --config -` with the token on stdin, never argv.

## 3d. Sandbox ingress (v4 P3, feluccad + felucca-gw)

Services inside sandboxes get public URLs `https://<name>--<id>.<ingress
domain>` via the `felucca-gw` reverse proxy (wildcard DNS → gateway → worker
node port → DNAT → guest). Design: [ADR-0007](adr/ADR-0007-sandbox-ingress.md).

| Method | Path | Auth | Body | Result |
|---|---|---|---|---|
| POST | `/api/v1/sandboxes/{id}/expose` | owner | `{"name","port"}` | 201 `{"name","guest_port","node_port","hostname","url"?}`; 200 idempotent repeat (same name+port); 409 name taken by another port; 400 bad name/port; 409 sandbox has no address. Names: `[a-z0-9-]` 1..=32, no edge/double dash, not all digits. |
| DELETE | `/api/v1/sandboxes/{id}/expose/{name}` | owner | — | 204; 404 unknown name; 502 when the worker can't remove the rule (row kept — retry). |
| GET | `/api/v1/routes` | admin (the gateway) | — | 200 `{"routes":[{hostname, sandbox_id, tenant_id, name, node_host, node_port, guest_port, state, allow_dynamic_ports}]}`. Node-less sandboxes keep rows with `node_host:""`. |
| POST | `/api/v1/routes/ensure` | admin (the gateway) | `{"sandbox_id","port"}` | 200 lazy expose named after the port — only when the sandbox opted in via `allow_dynamic_ports` at create (403 otherwise). |

Create accepts `"allow_dynamic_ports": true` (default false) enabling
E2B-style `8069--<id>` hostnames. Sandbox JSON carries `exposes` (omitted
when empty — pre-P3 wire shape unchanged). Forks re-expose the parent's
services on the child with fresh node ports. Sleeping sandboxes get a 503
wake page at the gateway; the expose survives sleep/wake (the guest IP
persists). `felucca-gw` flags (env): `--listen`
(`FELUCCA_GW_LISTEN`, default :8088), `--feluccad` (`FELUCCA_GW_FELUCCAD`),
`--token` (`FELUCCA_TOKEN`, admin — the route table is admin-only),
`--domain` (`FELUCCA_GW_DOMAIN`), `--refresh` (flag-only, seconds), and
`--tenant-limits` (`FELUCCA_GW_TENANT_LIMITS`, path to a JSON file:
per-tenant `enabled` toggle + token-bucket `rps`). feluccad's
`ingress_domain` config key only renders the `url` field in expose
responses.

## 3e. Templates & bigger guests (v4 P4, feluccad + agent)

Sandboxes can boot from custom rootfs images with bigger shapes (caps:
16 vCPU, 32768 MiB, 128 GB disk). A template = a catalog row + one image file
on feluccad (`images_dir`, default `/var/lib/felucca/images`); workers
pull-and-cache images sha256-addressed. Design:
[ADR-0008](adr/ADR-0008-templates-and-image-distribution.md).

| Method | Path | Auth | Body | Result |
|---|---|---|---|---|
| POST | `/api/v1/templates` | admin | `{"name", "from_sandbox"\|"image_file", "vcpus"?, "mem_mib"?, "disk_gb"?, "pool_size"?}` | 201 template JSON (`image_sha256`, `image_size_gb` computed). `from_sandbox` captures a **stopped** sandbox's rootfs (409 running / capture in progress / image file exists); `image_file` registers a pre-provisioned image. `disk_gb` defaults to the image size and may not be below it (400). Names use the expose-label grammar; 409 duplicate. |
| GET | `/api/v1/templates` | any key | — | 200 `{"templates":[...]}` — the catalog is tenant-visible. |
| DELETE | `/api/v1/templates/{name}` | admin | — | 204 (captured templates delete their image file; registered ones keep the shared file); 404 unknown. |
| GET | `/api/v1/images/{name}` | admin/node token | — | 200 image bytes (streaming; workers pull with the node token). Tenant keys 404. |

Sandbox create accepts `"template": "<name>"` (image + default shape from the
row; explicit `vcpus`/`mem_mib`/`disk_gb` override — `disk_gb` never below
the image size, or the 2 GiB base image without a template) and grows the
rootfs on the worker (`truncate` + `resize2fs`, grow-only). Sandbox JSON
carries `template`/`disk_gb` (omitted when unset — pre-P4 wire shape
unchanged); forks inherit both. Per-tenant `max_disk_gb` quota counts each
sandbox's effective disk (declared `disk_gb`, or 2 GiB for unresized base
sandboxes). Agent: `POST /v1/vms` gains `image`/`image_sha256`/`disk_gb`;
`GET /v1/vms/{id}/rootfs` streams a stopped VM's rootfs (capture);
`POST /v1/images/prefetch` warms the cache; `PUT /v1/pools` replaces the
node's template warm-pool specs (feluccad pushes on template changes and to
every node right after it registers — the register response stays `{"id"}`).

## 3f. Streaming exec, lifecycle policies, usage (v4 P5)

Design: [ADR-0009](adr/ADR-0009-streaming-lifecycle-usage.md).

**Streaming exec** — `POST /api/v1/sandboxes/{id}/exec?stream=1` (same body
as buffered exec) returns `text/event-stream`; each `data:` payload is one
JSON frame: zero or more `{"stream":"stdout"|"stderr","data":"…"}` chunks in
arrival order, then exactly one terminal
`{"done":true,"ok":true,"exit_code":N,"truncated":bool}` (or
`{"done":true,"ok":false,"error":"…"}`; a relay cut mid-stream is closed
with `"stream interrupted"`). Pre-stream failures keep the buffered status
mapping (404/409/501/502 plain JSON). Stream mode caps each output stream at
16 MiB (buffered stays 1 MiB); guest timeouts surface as `exit_code` 124.
Agent: `POST /v1/vms/{id}/exec` with `"stream":true` → chunked
`application/x-ndjson`, one frame per line. Guest vsock: the exec request
gains `"stream":true` and the response becomes those frames as JSON lines.

**Lifecycle policies** — tenants gain `default_idle_sleep_s` /
`default_asleep_delete_s` (create-time, seconds, 0 = off); sandbox create
(and fork, inherited) accepts `idle_sleep_s` / `asleep_delete_s`
(0 = inherit tenant default, -1 = disabled, else bounded `[5|30, 31536000]`,
400 outside). A 15s feluccad sweep auto-sleeps running sandboxes idle past
the effective policy (no exec, no ingress, no wake since) and auto-deletes
sleeping ones past their TTL (events recorded as ordinary `slept`/`deleted`).
Auto-sleep also drops the sandbox's **dynamic** (all-digit-named) exposes;
named exposes and manual sleeps are untouched. The activity clock counts:
create, fork, wake, both exec arms, and gateway traffic
(`POST /api/v1/routes/activity {"sandbox_ids":[…]}`, admin/gateway-only,
batched by felucca-gw every refresh tick).

**Auto-wake (felucca-gw)** — a request for a sleeping sandbox wakes it and
waits ≤15s for the route to go running before falling back to the 503 page.
Default on; `--auto-wake=false` / `FELUCCA_GW_AUTO_WAKE=0` gateway-wide,
`"auto_wake": false` per tenant in the tenant-limits file.

**Usage** — `GET /api/v1/tenants/{id}/usage?from=<unix>&to=<unix>` (defaults
`to`=now, `from`=to−30d; admin for any tenant, a tenant key for its own id
only) folds `usage_events` into
`{"tenant_id","from","to","sandbox_hours","vcpu_hours","mem_gib_hours",
"disk_gb_hours","execs","events"}` — intervals open on
created/started/resumed/woken/forked, close on paused/slept/stopped/deleted,
clipped to the window and to now; `disk_gb` 0 counts as the 2 GiB base
image. Every exec attempt appends an `"exec"` event. Retention: feluccad
hourly prunes **`exec`** events older than `usage_retention_days`
(config/`FELUCCA_USAGE_RETENTION_DAYS`/`--usage-retention-days`, default 90,
0 = keep forever) — lifecycle transition events are kept (they are the
interval skeleton the aggregation replays, and are low-volume).

**Template tenant scoping (P5.4)** — `POST /api/v1/templates` accepts
`"tenant": "<tenant-id>"` (400 unknown): the row carries `tenant_id`, the
catalog GET filters to public + own entries for tenant keys, and a foreign
tenant's create-by-template gets the same `400 unknown template` as a
missing one. Admin sees and uses everything.

**SDK & verify** — `sdk/ts/` ships `@felucca/sdk` (zero-dep typed client:
create/exec/execStream/sleep/wake/fork/expose/templates/tenantUsage);
`scripts/felucca-verify.sh <endpoint> [token]` runs this conformance suite
against any deployment.

## 3g. Observability (v4 P6, feluccad + agent)

Design: [ADR-0010](adr/ADR-0010-observability-and-bench.md).

**Request IDs** — every `/api/` request is assigned `req-<hex>`, echoed back
as the `X-Felucca-Request-Id` response header and forwarded on every
feluccad→agent proxy call via the same header; both binaries log it
(`request_id` field). Headers only — no wire-shape change; pre-P6 agents
ignore it. Background actors mint `sweep-`/`bg-` prefixed ids.

**Structured logs** — feluccad/felucca-gw log via Go `slog` (text, stderr),
the agent via Rust `tracing` (`RUST_LOG` filter, default `info`). Message
phrases are stable (grep-able); values are structured fields. The guest
stays on plain stderr by design.

**Admin metrics surface** — `GET /api/v1/metrics/tenants` (admin token only;
404 to tenant keys) serves the per-tenant Prometheus gauges
(`felucca_tenant_{sandboxes,running,vcpus,mem_mib,disk_gb}`). They are kept
off `/metrics` even though `/metrics` is itself admin-gated now (§7): the
tenant inventory is a narrower audience than the fleet scrape credential —
ADR-0009, and the ADR-0010 amendment that closed `/metrics`. **Both** jobs
need `bearer_token`; dashboard + scrape config in `deploy/grafana/`.

**Bench** — `scripts/bench.sh <endpoint> [token]` measures p50/p95 for
create (cold + pool-claim), exec (buffered + stream first-frame), wake, and
fork; lab numbers live in [BENCHMARKS.md](BENCHMARKS.md).

## 4. Warm pools (agent-internal, Tier A wake; per-template since v4 P4)

- Agent flag/config `pool_size` (default **0** = off). When >0 the agent keeps N **paused** generic
  VMs (1 vCPU / 256 MiB, base rootfs) pre-booted — the always-present default spec.
- Templates with `pool_size > 0` add per-template specs (image+sha+shape+count),
  delivered via `PUT /v1/pools` (see §3e). The 5s refill loop tops up AND
  drains: pooled VMs matching no current spec (deleted templates, re-captured
  shas) are deleted; failures back off 300s per spec.
- `POST /v1/vms` claims a pool VM only on an exact (image, sha, vcpus, mem, disk)
  match; anything else cold-boots. `node.pool_size` (heartbeat + metric) counts
  ALL pooled VMs across specs.

## 5. Guest networking (tap + bridge + NAT)

- Agent flag/config `net_cidr` (default `10.231.0.0/24`), `net = on|off` (default **on**; `off`
  restores v1 behavior, `ip:null`).
- Per node at agent startup (idempotent): bridge `felucca0` with the CIDR's `.1` as gateway,
  `nftables` masquerade for egress, ip_forward on.
- Per VM: tap `hth-<n>` enslaved to `felucca0`; guest IP assigned sequentially from the CIDR;
  guest configured via kernel boot arg `ip=<ip>::<gw>:<mask>::eth0:off`; FC `network-interfaces`
  configured before boot. Snapshots are taken **after** networking is up; restore/fork uses
  `/snapshot/load` `network_overrides` to attach a (new) tap.
- **Cross-tenant isolation is live (v4 P1)**: guest-to-guest traffic on `felucca0` is
  dropped unless source and destination belong to the same tenant — the optional
  `tenant_id` on `POST /v1/vms` (§3) governs membership; a VM with no/invalid tenant
  joins no pair and is isolated from all peers. Egress NAT and host↔guest traffic are
  unaffected. Tenant networks are node-scoped until the P2 overlay (per-node CIDRs are
  node-local). Design: [ADR-0005](adr/ADR-0005-cross-tenant-network-isolation.md).
- **Three more fences around the bridge (v4 hardening)**, because the tenant `forward`
  ruleset only ever sees IPv4 guest-to-guest traffic:
  - `ip felucca input` — guest→host. Guests route through the bridge gateway, and that
    traffic lands on the INPUT hook the forward chain never sees. Drops everything
    arriving on `felucca0` except ICMP and `ct state established,related`, with the
    agent's own port in an explicit drop rule.
  - `bridge felucca forward` — accepts only ARP and IPv4 between bridge ports, dropping
    the rest (link-local IPv6 above all). IPv6 is also disabled on `felucca0`.
  - `netdev felucca <tap>` — one `policy drop` chain per tap pinning that guest's source
    MAC, ARP sender MAC, ARP sender IP, and IPv4 source address. Without the ARP pin a
    guest can move a victim's address in the bridge FDB onto its own port.

  With `net = on`, the agent **refuses to serve** if any of these is not in force —
  a node with no isolation must not keep accepting placements.

## 6. Configuration & production deployment (both binaries)

Precedence: **flags > env vars > config file > defaults**. Nothing may hardcode
localhost/Lima paths — local lab and remote servers differ only by config.

- `--config <path>` loads a JSON config file. Env: `FELUCCA_CONFIG`.
- **feluccad** keys (JSON / env / flag): `bind` (`FELUCCA_BIND`, default `0.0.0.0:8080`),
  `state_path` (`FELUCCA_STATE`, default `/var/lib/felucca/state.json`),
  `ui_dir` (`FELUCCA_UI_DIR`, default `/usr/share/felucca/ui`), `token` (`FELUCCA_TOKEN`).
- **feluccad** overlay/TLS keys (v4 P2): `wg_ip` (CIDR, e.g. `10.100.0.1/24` — setting it
  enables the overlay; requires auth-on and `wg_endpoint`), `wg_port` (default 51820),
  `wg_key_path`, `wg_endpoint` (public `host:port` advertised to joiners), `wg_keepalive`
  (default 25), `tls_domain` (enables in-binary autocert on :443 + HTTP-01 on :80; the
  plain `port` listener stays for in-tunnel agents), `tls_cache_dir`.
- **feluccad** template keys (v4 P4): `images_dir` (`FELUCCA_IMAGES_DIR`,
  `--images-dir`, default `/var/lib/felucca/images`) — where template images
  live (captures land here; workers pull from `/api/v1/images/{name}`).
- **feluccad** proxy keys: `trusted_proxies` (`FELUCCA_TRUSTED_PROXIES`,
  `--trusted-proxies`) — CIDRs whose `X-Forwarded-For` is honoured when feluccad
  resolves the client address (the throttle key below). JSON array in the config
  file, comma-separated for env/flag; a bare IP is normalised to `/32`/`/128`.
  **Default empty = trust nothing** (use the connection peer); a malformed entry
  is a startup failure, not a skipped line. Chain walk is right-to-left, stopping
  at the first untrusted hop — the left-most (client-written) entry is never
  used. `trust_x_real_ip` (`FELUCCA_TRUST_X_REAL_IP`, `--trust-x-real-ip`,
  **default false**) is a separate opt-in for `X-Real-IP`: while it is false that
  header is ignored from every peer, declared or not, because it carries no chain
  of custody. Setting it with `trusted_proxies` empty is a **startup failure**.
  DEPLOYMENT.md §7.1 has the Caddy and nginx directives that must accompany
  `trusted_proxies` — an edge proxy that forwards the client's own header while
  being listed as trusted hands every client its own throttle key, and no code
  in feluccad can detect that.
- **felucca-agent** keys: `bind` (`FELUCCA_AGENT_BIND`, default `0.0.0.0:9090` —
  but the wildcard is **not honoured verbatim**; see below), `bind_any`
  (`FELUCCA_BIND_ANY`, `--bind-any`, default **off**),
  `control_plane` (`FELUCCA_CONTROL_PLANE`, e.g. `https://felucca.example.com` or `http://192.168.104.3:8080`),
  `advertise_addr` (`FELUCCA_ADVERTISE_ADDR`; **if unset, auto-detect** the source IP used to reach
  the control plane), `data_dir` (`FELUCCA_DATA_DIR`, default `/srv/ignis`), `token` (`FELUCCA_TOKEN`),
  `pool_size`, `net`, `net_cidr`, `join_url`/`join_token` (`FELUCCA_JOIN_URL`/`FELUCCA_JOIN_TOKEN`,
  `--join`/`--join-token` — first-boot enrollment only; the persisted `wg.json` wins afterwards).
- **The agent does not bind the wildcard by default.** `0.0.0.0` (or `::`, `*`,
  or an empty host) in `bind` is treated as "unset" and the listener uses the
  node's `advertise_addr` instead, **plus** `127.0.0.1` on the same port. A
  concrete host in `bind` is honoured as written. The wildcard includes the
  bridge gateway every guest routes through, which would put this root API one
  curl away from inside any tenant's sandbox; `bind_any` is the named opt-out
  for operators who front the agent with their own firewall. A `bind` that
  resolves to an address **inside `net_cidr`** is a startup failure.
- **Auth**: every `/api/*` and agent-registration request requires
  `Authorization: Bearer <token>`. `/metrics` requires the **admin** token (a tenant key gets 404);
  `/healthz` and static UI stay open. Constant-time comparison. TLS: in-binary autocert when
  `tls_domain` is set (v4 P2.3, see above); a reverse proxy (caddy/nginx) remains a valid
  alternative — both documented in DEPLOYMENT.md.
- **Three principals, and a node is not a tenant.** The admin token is unrestricted; a tenant API
  key (`felucca_sk_`) reaches its own sandboxes; a **node credential** (`felucca_nt_`) reaches an
  explicit allowlist of two routes — `POST /api/v1/agents/register` and
  `POST /api/v1/agents/heartbeat` — and 404s on everything else. Within those two it is bound to
  its own node: register only **its own address** (403 otherwise), never a hostname another node
  holds (403), heartbeat only **its own node id** (404 otherwise, the same answer as an unknown
  id), and it may **not** spend a `join_token` (403 — enrolling is the operator's act). Node
  authentication costs **no store read**: tokens are resolved through an in-memory sha256 index
  built at startup and updated on every rotation.
- **The per-node credential is used in BOTH directions, not just outbound.** A node's address is
  caller-supplied, so dialing it with the admin key made "point feluccad at a host I control" equal
  to "hand me the admin key"; requiring the admin token back on register/heartbeat meant every
  worker held the fleet key. Enrollment with a one-time join token (`POST /api/v1/nodes/join`, or
  `POST /api/v1/agents/register` carrying `join_token`) mints `felucca_nt_<48 hex>` and returns it
  **once** as an additive `agent_token` field. feluccad stores it keyed on the node's
  **`host:port`** (two agents on one host are two nodes; rows written under the older bare-host key
  are migrated on first use) and presents only that when dialing. `felucca-agent` persists it to
  `<data_dir>/node-token` mode 0600, presents it outbound on the two agent routes, and accepts it
  inbound alongside its configured `token`; it never falls back to the shared token after the node
  token is rejected. A node with no credential row is dialed with **no** bearer. Nodes present
  before the change are grandfathered onto the shared token exactly once.
  ⚠️ The worker still needs the shared token configured: **template image pulls**
  (`GET /api/v1/images/{name}`) are admin-only and a node credential cannot reach them. See
  DEPLOYMENT.md §6.7.
- **No token is no longer "auth off"**: an empty `token` authorizes nobody, and both binaries
  **refuse to start** on an empty, placeholder (`REPLACE_WITH…`, `felucca-lab-token`) or short
  token — under 32 chars for feluccad, under 16 for the agent. Open mode is a deliberate,
  named choice: `feluccad --insecure-no-auth` (loopback labs; logged as a WARN at every start).
  The agent has no equivalent flag. `bind`'s host part is honoured, so a configured
  `127.0.0.1:8080` really is loopback-only — see DEPLOYMENT.md §6.3 for which topology wants which.
- **Brute-force guard**: failed credentials are counted per source address, on both credential
  gates (the bearer gate — every `/api/*` path and `/metrics` — and `POST /api/v1/nodes/join`).
  Past 10 failures the wait doubles (1s, 2s, 4s …) and a *further failure inside that window* is
  answered **429** `{"error":"too many failed attempts"}` with `Retry-After` (whole seconds,
  minimum 1). **The credential is evaluated first and a correct token is always served** — the
  guard shapes the answer to an attempt that already failed, so it can never refuse a request
  that would have succeeded, and one anonymous client cannot deny the control plane to everyone
  who shares its key. (An earlier revision evaluated the backoff first and did exactly that; any
  text claiming a correct token is refused while throttled is stale.) Distinguish this 429 from
  the quota 429 by the header: the quota response carries none. A success does **not** clear the
  record — only quiet time decays it (one forgiven failure per 15s), because a wipe-on-success is
  reachable by anyone sharing the key. The wait is capped at 60s for a source feluccad can
  attribute to one client, and at **2s** when the key provably stands for many (peer is a declared
  proxy that forwarded nothing attributable, or nothing is declared and the peer is
  loopback/private — the shipped topology). "Shared" is decided from config and peer address only,
  never from a header. The source key is the connection peer unless that peer is inside
  `trusted_proxies` *and* forwarded a hop the chain can attest to.
- Build targets: `zig build -Dtarget=aarch64-linux-musl` **and** `-Dtarget=x86_64-linux-musl`
  must both produce static binaries (production servers are typically x86_64).

## 7. Metrics additions (`/metrics`)

```
felucca_wake_ms_last        gauge   (last wake latency, per feluccad)
felucca_wake_total          counter
felucca_wake_ms_sum         counter (for avg = sum/total)
felucca_forks_total         counter
felucca_pool_size{node=...} gauge   (agent-side, aggregated by feluccad)
felucca_sandboxes_total{state="sleeping"} added to the existing state gauge set
felucca_wake_duration_ms_{bucket,sum,count}    histogram (v4 P6; ms buckets 5..30000,+Inf)
felucca_exec_duration_ms_{bucket,sum,count}    histogram (v4 P6; buffered exec, feluccad wall-clock)
felucca_create_duration_ms_{bucket,sum,count}  histogram (v4 P6; to 201, cold+claim mixed)
```

Histogram state is in-memory (resets on restart — standard Prometheus
counter semantics). `/metrics` itself now requires the **admin** bearer token:
the scrape names every worker in hostname labels, reports live fleet counts,
and takes the global state lock, so an open endpoint is both reconnaissance and
a lock-contention lever for anyone who can reach the port. Scrape it with
`bearer_token`. Per-tenant gauges stay off it regardless (ADR-0009) — a
tenant-labeled series set leaks the tenant inventory to every holder of the
admin scrape credential, and belongs on the separate
`GET /api/v1/metrics/tenants` surface (§3g); per-tenant billable history stays
on `GET /api/v1/tenants/{id}/usage` (§3f).

## 8. UI requirements

- `sleeping` state: distinct color (cool blue/violet vs green running), moon glyph ok.
- Row actions: sleep (running/paused), wake (sleeping), fork (running/paused/sleeping — opens
  the existing fork modal, now functional), all via the endpoints above; show `wake_ms` in a toast.
- `ip` column shows the address when present.
- Bearer token: held in a module-scoped variable that dies with the tab. **Not** read from
  `localStorage`/`sessionStorage` (web storage hands the admin token to any script on the
  origin) and **not** accepted from `?token=` (that value is already in browser history and in
  every fronting proxy's access log — the console strips it from the URL and ignores it).
  The paste-in banner is the only entry point; an upgraded console also purges any
  `felucca_token` an older build left in storage.
- Attach `Authorization: Bearer` to all API fetches. **Do not poll without a token**: each
  attempt is counted by feluccad's per-source brute-force guard (§6), so a 3s timer with no
  credential walks a shared source key past the threshold in ~20s. That does not cost the
  operator their own access (a correct token is served regardless), but it turns every
  unauthenticated answer on that key — including anyone else behind the same proxy address —
  into a `429`, and it buries the real 401 the operator needs to see.
- Distinguish live from not-live at all times. A header badge plus a banner name the state —
  `LIVE`, `NO TOKEN`, `NO AUTH` (401), `THROTTLED` (429, with the `Retry-After` countdown),
  `STALE` (last good data retained), `MOCK` (fixtures; the control plane was never reached).
  Silently substituting fixtures for a fleet the operator believes is real is a defect.
- 429 is two different answers: with `Retry-After` it is the auth throttle (wait); without, it
  is the tenant quota gate (the caller's own answer).
