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

**Which state each verb accepts (stability wave 1, [ADR-0012](adr/ADR-0012-data-safety.md)).**
A sandbox is `sleeping` only when its record says so; the agent no longer
infers it from a `vmstate.bin` lying in the instance directory. A sandbox
whose Firecracker is gone when its agent starts (a worker reboot, an OOM kill
while the agent was down) comes back `stopped`, never `sleeping`.

| Verb | Accepted from | Idempotent success | Every other state |
|---|---|---|---|
| `sleep` | `running`, `paused` | `sleeping` | 409 |
| `wake` | `sleeping` | `running` (agent answers `wake_ms` 0) | 409 |
| `start` | `stopped`, `error` | `running` | 409 |
| `pause` | `running` | `paused` | 409 |
| `resume` | `paused` | `running` | 409 |
| `stop` | any state | — | — |
| `fork` (parent) | `running`, `paused`, `sleeping` | — | 409 with the parent's `state` |

`stop` of a sleeping sandbox discards its snapshot. A 409 for the state the
sandbox is in carries that state under the key `state`, for example
`{"error":"InvalidState: stopped (only a sleeping VM can be woken)","state":"stopped"}`.
feluccad records the state the worker names (when its own record still shows
the state the request started from), queues the matching usage event, and
forwards the 409 body unchanged. A 409 without `state` (`busy`, or an older
agent) is forwarded and nothing is recorded.

feluccad itself answers sleep, wake, stop, start, pause and resume of a
sandbox whose record is still `creating` with
`409 {"error":"InvalidState: creating (the sandbox is still being created)","state":"creating"}`,
without calling the worker; delete stays allowed. A record that a stopped or
killed feluccad left `creating` is set to `error` when feluccad next starts
(`start` or `delete` settles it; a VM may exist on its worker). Its first
successful start, wake or resume (or a worker refusal that records it
`running`) takes the guest address from the worker's list of VMs, so that
expose and routing work again; when the worker cannot be asked, the request
still succeeds and the next start or wake tries again.

**One lifecycle operation at a time.** sleep, wake, start, pause, resume and
fork hold the sandbox on its worker while they run. A second request of the
same kind waits up to 25 s for the one in progress and then answers from the
state it finds; a request of a different kind, or one that waited 25 s,
answers `409 {"error":"busy"}`. Retry it. stop and delete never wait: they end
the operation in progress and answer once it has let go; if it has not within
15 s, stop answers
`409 {"error":"busy: the VM is stopped, but an operation in progress has not let go of it yet; try again","state":"stopped"}`
(the sandbox is `stopped` by then). exec, expose and unexpose are not lifecycle
operations and are never `busy`.

## 2. New/changed control-plane endpoints (feluccad :8080)

All v1 endpoints unchanged. New:

| Method | Path | Body | Result |
|---|---|---|---|
| POST | `/api/v1/sandboxes/{id}/sleep` | — | 200 sandbox JSON, state=`sleeping`. Pause → `/snapshot/create` (Full; the state file as `vmstate.bin.tmp`) → fsync the disk image → rename to `vmstate.bin` → fsync the directory → record `sleeping` on disk → kill the FC process. Any failure before the kill removes the snapshot files and resumes the guest; the record is unchanged. 409 from the agent forwarded (§1); other agent failure → 502 `{"error":"agent sleep failed"}`. |
| POST | `/api/v1/sandboxes/{id}/wake` | — | 200 `{...sandbox, "wake_ms": <int>}`, state=`running`. Spawn FC → `/snapshot/load` (File backend, `resume_vm:false`) → record `running` on disk → `PATCH /vm Resumed`. A snapshot that is incomplete, or older than the disk it goes with, makes the sandbox `stopped` and answers 409 with `"state":"stopped"`. 409 from the agent forwarded (§1); other agent failure → 502 `{"error":"agent wake failed"}`. |
| POST | `/api/v1/sandboxes/{id}/fork` | `{"name": "..."}` | 201 child sandbox JSON with `parent_id` set (replaces the v1 501 stub). Parent must be `running`, `paused`, or `sleeping`. A running parent is **paused for the snapshot and for the copy of its disk** (`cp --reflink=auto`, so that the disk is of the same instant as the memory), then resumed; a sleeping parent's files are copied without a pause. The child's snapshot is loaded without resuming, its root drive is pointed at its own `rootfs.ext4` (`PATCH /drives/rootfs`, checked with `GET /vm/config`), and only then is it resumed: the child runs on its own disk (FLU-72; before this release it ran on its parent's). A parent busy with another operation → **409 `{"error":"busy"}` with `Retry-After: 2`**; nothing is created. A fork the worker refuses for good — a parent in a state that cannot be forked, or one forked before this release that runs on its parent's image — answers **409 with the worker's body**, e.g. `{"error":"InvalidState: this sandbox was forked before this release and runs on its parent's disk image, not on its own; it cannot be forked","state":"running"}`, with no `Retry-After`; feluccad records the parent's state when the body names another one. In both refusals the reserved child record is removed and the parent's activity clocks are given back (a fork that did not happen is no activity). Any other worker failure → 502 `{"error":"agent fork failed"}` with the child in `error`. **Fixed in v3.1**: right after restore the agent re-MACs and re-IPs the child in-guest over vsock (best-effort — [API-V3-EXEC.md](API-V3-EXEC.md) §3), so the child answers on its own `ip` and the parent keeps its connectivity. |

`stop`, `start`, `pause` and `resume` (`POST /api/v1/sandboxes/{id}/<verb>`)
answer 200 with no body; a 409 from the agent is forwarded (§1), any other
agent failure is 502 `{"error":"agent action failed"}`.

`DELETE /api/v1/sandboxes/{id}`: when the worker refuses with a 409 whose
`error` starts with `busy` (an operation in progress has not let go of the VM
yet), feluccad answers **409 with the worker's body and `Retry-After: 2`** and
keeps the record; any other worker refusal is the existing 502
`{"error":"agent delete failed: …"}` (the worker's text is logged, not
relayed). Either way the state the worker names is recorded. `?force=1`
(admin) is unchanged.

### 2a. When feluccad's database cannot be written (stability wave 1)

A request that changes durable state is answered 2xx only once the change is
in feluccad's database. While the last write failed and no save has succeeded
since (the **store is failing**), such requests are refused before anything is
done, and a background loop retries the save every 2 s until it succeeds.

| Answer | Header | Meaning | Retry? |
|---|---|---|---|
| 503 `{"error":"store unavailable"}` | `Retry-After: 5` | Refused before anything was done (or a create/fork whose record could not be saved: nothing exists on the worker, or what was made there is removed again after the answer). Also the answer to a store **read** error: key authentication (not counted as a failed credential), quota and template lookups, the tenant row of `POST /api/v1/tenants/{id}/keys`, node join. Every read error is counted in `felucca_store_read_errors_total` | Yes |
| 503 `{"error":"applied but not saved","applied":true}` | `Retry-After: 5` | The worker carried the change out; feluccad holds it in memory and the recovery loop will write it. Nothing is undone on the worker | Yes — every verb that answers this is idempotent on the worker |
| 500 `{"error":"store error"}` | — | A row-level write (tenant, key, template, join token, WireGuard peer, node credential) failed, and the store is now failing; or a listing could not be read (`GET` of tenants, keys, templates, usage) | Yes, once `/healthz` answers 200 again (for a write) |
| 409 (route-specific body) | — | A row-level write broke a constraint (a name that exists): the request's conflict, not the database's | Not as is |

Refused while the store is failing (`503 store unavailable`): every non-`GET`
request under `/api/v1/sandboxes` (create, delete, sleep, **wake**, stop, start,
pause, resume, fork, expose, unexpose) except `POST /api/v1/sandboxes/{id}/exec`;
`POST /api/v1/routes/ensure`; every non-`GET` request under `/api/v1/tenants`,
`/api/v1/keys/` and `/api/v1/templates`; `POST /api/v1/join-tokens`;
`POST /api/v1/nodes/join`; and `POST /api/v1/agents/register` when it carries a
`join_token` (no join token is ever spent while the store is failing).

Not refused: every `GET` (including `GET /api/v1/routes`), exec (buffered and
streamed), `POST /api/v1/agents/heartbeat`, registration of a node without a
join token, `POST /api/v1/routes/activity`, `/metrics` and `/healthz`. The
lifecycle sweep (auto-sleep, auto-delete) does nothing while the store is
failing. Running sandboxes keep serving; sleeping ones are not woken.

`mem_total_mib` (register) and `mem_free_mib` (heartbeat) above 2^40 are
refused with 400 `{"error":"mem_total_mib out of range"}` /
`{"error":"mem_free_mib out of range"}`: one such value used to make every
later save fail.

A tenant and its first key are one transaction, and so are a node credential
and the join token it consumes. Exec metering rows are queued in memory (at
most 10 000; the oldest are dropped and counted in
`felucca_usage_rows_dropped_total`) and written once a second, so exec never
waits on the database.

**Stopping feluccad.** On SIGTERM or SIGINT every listener stops accepting at
once, and the requests in flight get 10 s in all to finish (their saves
included); what is left after that is cut, and its clients get no answer.
Then the queued exec rows are written, retried once a second until just
before the 20 s limit, and feluccad exits with status 0. A second signal, or
20 s after the first, ends it at once with status 1.

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

**Lifecycle answers (stability wave 1).** The states each verb accepts are in §1.

| Endpoint | Success | Refusals |
|---|---|---|
| `POST /v1/vms/{id}/sleep` | 200 `{"ok":true}` | 409 `{"error":"InvalidState: <state> (only a running or paused VM can sleep)","state":"<state>"}`; 409 `{"error":"busy"}`; 404 `{"error":"NotFound"}`; 500 `{"error":"<reason>"}` (the snapshot failed; the guest runs on) |
| `POST /v1/vms/{id}/wake` | 200 `{"ok":true,"wake_ms":<n>}` | 409 `{"error":"InvalidState: <state> (only a sleeping VM can be woken)","state":"<state>"}`; 409 with `"state":"stopped"` when the snapshot is incomplete, was outrun by the disk, the resume may have run the guest, or (a sandbox forked before this release) its parent's image changed since it slept: `{"error":"InvalidState: this sandbox was forked before this release and runs on its parent's disk image, which has changed since it slept; it cannot be woken and is now stopped (use start)","state":"stopped"}`; 409 `{"error":"busy"}`; 404; 500 `{"error":"IsolationFailed"}` (new guests refused, §5); 500 otherwise (the record stays `sleeping`) |
| `POST /v1/vms/{id}/{start,stop,pause,resume}` | 200, empty body | 409 `{"error":"InvalidState: …","state":"<state>"}`; 409 `{"error":"busy"}` (not stop); stop's give-up 409 with `"state":"stopped"` (§1); start: 500 `{"error":"IsolationFailed"}` (new guests refused, §5); 500 |
| `DELETE /v1/vms/{id}` | 204 | 409 `{"error":"busy: the VM is stopped, but an operation in progress has not let go of it yet; try again","state":"stopped"}`; 409/500 as before |
| `POST /v1/vms/{id}/fork` | 201 `{"ok":true,"ip":…}` | 409 `{"error":"busy"}` (the parent is held by another operation; a fork never waits); 409 `{"error":"AlreadyExists"}` (the child id, or a directory of that name, is taken — checked before anything is written); 409 `{"error":"InvalidState: …","state":"<parent's state>"}` — a parent in a state that cannot be forked (`InvalidState: <state> (only a running, paused or sleeping VM can be forked)`), or one forked before this release, running, paused or sleeping (`InvalidState: this sandbox was forked before this release and runs on its parent's disk image, not on its own; it cannot be forked`); the texts name no path (the log does); 500 `{"error":"IsolationFailed"}` (new guests refused, §5); 500 |

`meta.json` gains one optional key, `snapshot_valid`, written (`true`, as the
last key) only by a sleep that has finished its snapshot, and absent otherwise;
an older agent ignores it.

**Expose (`POST /v1/vms/{id}/expose`).** The body is
`{"guest_port":<n>,"avoid":[<port>,…],"prefer":<port>}`. `avoid` (at most
10 000 entries) and `prefer` are optional; absent or `null` means none. The
agent never hands out a port in `avoid`; it returns `prefer` when no sandbox on
the worker holds it, and otherwise the next free port round-robin from a cursor
that starts at a random port of 20000–29999 when the agent starts. An existing
mapping for that guest port is returned as it is, whatever `avoid` says.

| Answer | Meaning |
|---|---|
| 200 `{"node_port":<n>,"avoid_honored":true}` | The DNAT rule is in the kernel. `avoid_honored` tells feluccad this agent read `avoid` and `prefer`; an older agent's answer lacks it |
| 409 `{"error":"ports exhausted"}` | Every port of the range is in use or in `avoid` |
| 409 `{"error":"no ip"}` | The VM holds no address |
| 500 `{"error":"ingress rule not installed"}` | The nft transaction was rejected; the mapping was taken back out |
| 400 `{"error":"invalid avoid"}`, `{"error":"invalid prefer"}`, `{"error":"avoid too long"}`, `{"error":"guest_port required"}`, `{"error":"bad json"}`, `{"error":"bad request"}` | Malformed body (over 128 KiB is `bad request`) |

The ingress chain is rebuilt as one `nft -f -` transaction. Expose and unexpose
run to their end when the caller hangs up. Delete rebuilds the isolation and
ingress rules without the sandbox before it frees its tap and its address; if a
rebuild fails, the address is held until the latest isolation and ingress
rebuilds have both succeeded (a later change, or an isolation-watch tick, §5).
Two deletes of one sandbox that overlap free its address once. An expose
writes `meta.json` only once its rule is in the kernel.

**Agent `/healthz`.** The shape is unchanged, `{"ok":true,"isolation":<bool>}`,
and the heartbeat's `isolation_ok` carries the same value. With the isolation
watch running (§5), `isolation` is the watch's verdict: true when the last
completed tick (at most 45 s old) left no fence, switch or tap pin unrepaired,
no fence chain's latest rebuild failed, the guest bridge exists, and — when it
was in place as the agent started — the host fence `iifname "felucca0" drop`
is in `inet felucca_host`. With the watch on but no tick completed within 45 s
of its start, it is `false`. Before that, and with the watch `off`, it is
start-up's verdict and that of the last rule change. NAT and ingress never
enter it. In every watch mode, `off` included, it is `false` while start-up
could neither rebuild nor empty the ingress chain (no guest address is handed
out then), and true again once the retry has rebuilt it.

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
| POST | `/api/v1/sandboxes/{id}/expose` | owner | `{"name","port"}` | 201 `{"name","guest_port","node_port","hostname","url"?}`; 200 idempotent repeat (same name+port); 409 name taken by another port; 400 bad name/port; 409 sandbox has no address. Since wave 1 also: **429 `{"error":"expose churn limit reached"}`** with `Retry-After` (seconds until the oldest quarantined port that counts against the cap is released); **503 `{"error":"no free node port on this worker"}`** with `Retry-After: 30`; **503 `{"error":"node port conflict, retry"}`** with `Retry-After: 5`; 502 `{"error":"agent expose failed"}`; and the store answers of §2a. Names: `[a-z0-9-]` 1..=32, no edge/double dash, not all digits. |
| DELETE | `/api/v1/sandboxes/{id}/expose/{name}` | owner | — | 204; 404 unknown name; 502 when the worker can't remove the rule (row kept — retry). A worker that answers 404 (it does not know the VM) holds no mapping either: the row is removed. |
| GET | `/api/v1/routes` | admin (the gateway) | — | 200 `{"routes":[{hostname, sandbox_id, tenant_id, name, node_host, node_port, guest_port, state, allow_dynamic_ports}]}`. Node-less sandboxes keep rows with `node_host:""`. |
| POST | `/api/v1/routes/ensure` | admin (the gateway) | `{"sandbox_id","port"}` | 200 lazy expose named after the port — only when the sandbox opted in via `allow_dynamic_ports` at create (403 otherwise). The 429 and 503 answers of expose apply. |

**Node-port quarantine (stability wave 1, FLU-6).** When the last route row of
a sandbox that maps a guest port to a node port is removed (unexpose, delete,
the sweep, the undo of a create or fork), feluccad keeps that node port away
from every other sandbox on the same worker host for **900 s**. Every expose
request to a worker carries `avoid` (all node ports feluccad has recorded for
that worker host, and the quarantined ones, except this sandbox's own for this
guest port) and, when this sandbox last held a still-quarantined port for this
guest port, `prefer` — so a sandbox that sleeps and wakes, or is unexposed and
exposed again, gets its old port back. feluccad checks the worker's answer: a
port another sandbox's row names (that row is stale and is removed), or a
quarantined port that another sandbox (or this one for another guest port)
released, from an agent that said `avoid_honored`, is removed from the worker
again and answered `503 node port conflict, retry`. A port this sandbox holds
for another guest port is removed from the worker again and answered 503 with
`Retry-After: 5` and a body that names the expose holding it:
`{"error":"node port conflict: the worker answered node port N, which this sandbox's expose \"<name>\" (guest port G) holds; removing that expose ends it"}`.
An older agent's quarantined port is accepted with a warning in feluccad's
log. When the worker maps a guest port the sandbox has rows for on other node
ports, the rows follow the worker and the other ports are quarantined.

A quarantine runs on the wall clock. A clock stepped backwards lengthens it by
at most the step; the remaining time (the `Retry-After` below) is never more
than 900 s.

Churn caps, checked only when a request needs a fresh port: at most **512**
quarantined ports per tenant per worker host (the admin tenant is unmetered)
and **64** distinct (node port, guest port) pairs per sandbox on that worker
host, whoever owns it. A tenant that keeps creating and
deleting exposes reaches the 429 above.

Create accepts `"allow_dynamic_ports": true` (default false) enabling
E2B-style `8069--<id>` hostnames. Sandbox JSON carries `exposes` (omitted
when empty — pre-P3 wire shape unchanged). Forks re-expose the parent's
services on the child with fresh node ports. Sleeping sandboxes get a 503
wake page at the gateway; the expose survives sleep/wake (the guest IP
persists). `felucca-gw` flags (env): `--listen`
(`FELUCCA_GW_LISTEN`, default :8088), `--feluccad` (`FELUCCA_GW_FELUCCAD`),
`--token` (`FELUCCA_TOKEN`, admin — the route table is admin-only; the flag
has no default, the variable is read after the flags are parsed, so
`--help` never prints the token), `--domain` (`FELUCCA_GW_DOMAIN`),
`--refresh` (flag-only, seconds, default 5), `--max-stale`
(`FELUCCA_GW_MAX_STALE`, default 600 s), and
`--tenant-limits` (`FELUCCA_GW_TENANT_LIMITS`, path to a JSON file:
per-tenant `enabled` toggle + token-bucket `rps`). feluccad's
`ingress_domain` config key only renders the `url` field in expose
responses.

**Route-table age (stability wave 1).** The gateway serves from a route table
whose fetch started at most `--max-stale` ago. `--max-stale` takes whole
seconds (`600`) or a Go duration (`10m`); the flag beats the variable. The
gateway refuses to start when `--refresh` is outside 1 to 60 s, or
`--max-stale` is below `2 x refresh + 20s` (30 s at the default refresh) or
above 600 s. A table is stamped with the time its fetch started, and a fetch
that started earlier never replaces a newer table.

| Gateway answer | When |
|---|---|
| 503 `{"error":"route table unavailable"}`, `Retry-After: 5` | Any sandbox hostname, while the table is older than `--max-stale` or no fetch has succeeded yet |
| `GET /readyz` → 200 `ok` | The table is fresh enough to route from (for load-balancer health checks) |
| `GET /readyz` → the 503 above | Otherwise |
| `GET /healthz` → 200 `ok` | Always, while the process runs (liveness) |
| 429 `{"error":"too many new dynamic ports for this tenant"}` | `routes/ensure` answered 429; its `Retry-After` is passed on (5 when absent) |
| 503 `{"error":"dynamic port temporarily unavailable"}` | `routes/ensure` answered 503 (its `Retry-After` is passed on) or anything else that is not 200, 403 or 404 (`Retry-After: 5`); it used to be a 404 |

`/readyz` and `/healthz` are the gateway's own only on a Host without `--`.
Upstream connections are pooled per sandbox, guest port, node host and node
port, so a pooled connection never carries a request for another sandbox.

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
Since stability wave 1 auto-sleep keeps the sandbox's **dynamic**
(all-digit-named) exposes; the sweep drops them once the sandbox has been
asleep for at least 900 s (the node-port quarantine, §3d), so a sandbox that
wakes sooner keeps its ports. It does so only for sandboxes on `ready`
workers, with a limit of 5 s per worker call and 5 s of worker time per pass;
a drop that got no answer in time is repeated on later passes — also after the
sandbox woke — until the worker answers (the gateway then re-creates the
route on the next request). Named exposes are untouched. A worker that
refuses an auto-sleep with a 409 naming a state has that state recorded. The activity clock counts:
create, fork, wake, both exec arms, and gateway traffic
(`POST /api/v1/routes/activity {"sandbox_ids":[…]}`, admin/gateway-only,
batched by felucca-gw every refresh tick).

**Auto-wake (felucca-gw)** — a request for a sleeping sandbox wakes it and
waits ≤15s for the route to go running before falling back to the 503 page.
Concurrent requests for one sleeping sandbox share one wake; afterwards each
request looks up its own hostname again, so two services of one sandbox are
each answered by their own (FLU-7). The gateway reads the wake's answer: after
a 2xx it fetches the table up to 15 times, 1 s apart, until any route of the
sandbox shows `running`; after a 503 with `Retry-After` it sends the wake again
after that delay (at least 250 ms, at most 2 s) while the 15 s last; after a
404 or 409 it fetches the table once and sends no wake for that sandbox for
5 s (requests in that time are answered from the table without a wake).
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
- **The isolation watch (stability wave 1, FLU-8).** Every 5 s the agent checks its
  chains (each starts with a marker rule `counter comment "felucca:<16 hex>"`, a hash
  of what it applied), the `br_netfilter` and IPv6 switches, the anti-spoof chain of
  every tap with carrier, the guest bridge itself, and the host fence; it rebuilds
  what drifted. Something is taken down only on positive evidence twice: the chain
  was seen positively not in place, and the kernel refused the repair, or accepted it
  and a second look still does not find it. Then `enforce` takes the guest bridge down
  (one tap, for its anti-spoof chain) until a tick repairs it; `report` only logs. In
  both modes, while a fence the agent owns is positively not in place (or its repair
  has no answer yet), or the guest bridge is gone, **new guests are refused**: create,
  fork, wake, start and pool refill answer `IsolationFailed` (the agent's 500
  `{"error":"IsolationFailed"}`). Setting: `isolation_watch`
  (`FELUCCA_ISOLATION_WATCH`, `--isolation-watch` or `--isolation-watch=<value>`):
  `enforce` (default), `report`, `off`; any other value, or the flag without one,
  refuses to start. Every `nft` and `ip` command the agent runs is killed after 10 s
  (`wg`, 60 s). Details: DEPLOYMENT.md §8.2.

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
  `pool_size`, `net`, `net_cidr`, `isolation_watch` (`FELUCCA_ISOLATION_WATCH`,
  `--isolation-watch`, default `enforce`; §5), `join_url`/`join_token` (`FELUCCA_JOIN_URL`/`FELUCCA_JOIN_TOKEN`,
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
  `/healthz` and static UI stay open. `/healthz` answers 200 `{"ok":true}`, or **503
  `{"ok":false,"store":"failing"}`** while feluccad's database cannot be written (§2a); it is
  answered before the state lock is taken and is not counted in `felucca_api_requests_total`. Constant-time comparison. TLS: in-binary autocert when
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
felucca_persist_failures_total     counter (wave 1; failed writes: snapshot saves, exec usage flushes, row-level writes)
felucca_store_healthy              gauge   (wave 1; 1 when the database has caught up with memory, 0 while saves fail)
felucca_usage_write_failures_total counter (wave 1; failed write attempts that carried usage rows; the rows stay queued and are retried)
felucca_usage_rows_dropped_total   counter (wave 1; exec usage rows dropped from the full queue, lost, each counted once)
felucca_store_read_errors_total    counter (wave 1; failed store reads: key lookups, quota and policy reads, credentials, listings)
felucca_node_ports_quarantined     gauge   (wave 1; released node ports still in quarantine, distinct worker host and port pairs)
felucca_nodes_unfenced             gauge   (wave 1; ready nodes whose last heartbeat reported isolation_ok false)
```

`felucca_nodes_unfenced` counts only `ready` nodes: a node that goes silent
while it reports `false` leaves the count, although its sandboxes may keep
running. The heartbeat's `isolation_ok` is held in memory only (absent counts
as fine); feluccad logs `node reports its isolation down: …` when a node turns
`false`, and `node reports its isolation in place again` when it stops.

**Placement.** A create goes to the ready worker with the fewest sandboxes
among those that do not report `isolation_ok` false (a worker that never
sends it counts as fine). Only when every ready worker reports it false does a
create go to the one with the fewest, as before, with a WARN at most once a
minute (`creates are being placed on workers that report their isolation
down: …`). Fork, wake and start stay on the sandbox's own worker.


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
