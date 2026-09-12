# ADR-0009 — Streaming exec, lifecycle policies, and usage aggregation

Status: accepted (v4 P5, 2026-06-13). Implements PLAN-v4.md Phase 5 plus the
deferrals parked there by earlier phases: auto-wake + dynamic-expose GC
(ADR-0007), per-template tenant visibility + worker image-cache GC
(ADR-0008), usage_events retention (P0).

## Decision

### Streaming exec — NDJSON inside, SSE outside

One wire shape end to end, translated at each hop:

- **Guest (vsock)**: the exec request gains `"stream": true`. The response
  becomes a sequence of JSON lines — `{"stream":"stdout"|"stderr","data":…}`
  chunk frames, then exactly one terminal
  `{"done":true,"ok":…,"exit_code":…,"truncated":…}`. The buffered response
  stays byte-identical; `stream` simply selects which writer owns the
  connection. Chunks are forwarded as they are read (8 KiB reads, lossy
  UTF-8 like the buffered path); the per-stream cap rises from 1 MiB to
  16 MiB in stream mode — chunks aren't accumulated in guest memory, so the
  cap bounds transfer volume, not RSS, and build logs (the use case)
  routinely exceed 1 MiB. A failed frame write (peer hung up) SIGKILLs the
  command's process group: nobody is listening.
- **Agent**: `POST /v1/vms/{id}/exec` with `"stream":true` returns
  `application/x-ndjson`, piping the guest bytes through unparsed. Setup
  failures keep the buffered status mapping (409 not-running, 501 guest
  unavailable). The data phase has an absolute deadline of
  `timeout_ms + 60s` (`DeadlineStream`); on expiry the body just ends and
  the missing done frame is the consumer's error signal.
- **feluccad**: `POST /api/v1/sandboxes/{id}/exec?stream=1` returns SSE; each
  agent NDJSON line becomes one `data:` event, flushed immediately. The
  route keeps its slow-loris bound (`deadlineFor(timeout_ms + 90s)` — wider
  than the agent's deadline plus margin, still finite and request-sized). A
  stream that ends without a done frame gets a synthetic
  `{"done":true,"ok":false,"error":"stream interrupted"}` so clients never
  have to distinguish EOF flavors.

SSE (not WebSocket, not chunked JSON) because the consumer is the TS SDK and
browsers: EventSource/fetch-reader friendly, proxy-transparent, and the
gateway already handles streaming responses (FlushInterval).

### Lifecycle policies — tenant defaults, sandbox overrides, one sweep

- `tenants` gain `default_idle_sleep_s` / `default_asleep_delete_s`
  (0 = off). Sandboxes gain `idle_sleep_s` / `asleep_delete_s`
  (0 = inherit, −1 = disabled, bounded `[5s|30s, 1y]`) and two internal
  clocks excluded from the wire JSON (the pre-P5 goldens stay frozen):
  `last_activity` (stamped by create, fork, wake, both exec arms, and
  gateway ingress reports) and `slept_at`.
- A 15s sweep in feluccad (`LifecycleLoop`) auto-sleeps running sandboxes
  idle past their effective policy and auto-deletes sleeping ones past
  their TTL, via the same agent calls as the handlers (best-effort, retried
  next sweep). Decisions snapshot under the state lock; agent I/O happens
  outside it. Sleepers adopted from pre-P5 snapshots get `slept_at` stamped
  on first sight — a TTL never applies retroactively.
- Activity stamps are memory-only and ride the next snapshot persist: the
  clock steers auto-sleep, it is not billing data; losing a few seconds of
  it on crash means a sandbox sleeps marginally early, nothing worse.
- **Ingress activity**: felucca-gw batches the sandbox ids it proxied and
  POSTs `/api/v1/routes/activity` (admin-gated) every refresh tick; failed
  reports re-queue so a feluccad blip can't make an active sandbox look idle.
- **Auto-wake (ADR-0007 deferral)**: a request for a sleeping sandbox now
  wakes it (`POST …/wake` with the gateway's admin token) and polls the
  route table up to 15s before falling back to the 503 page. Gateway-wide
  default on (`--auto-wake` / `FELUCCA_GW_AUTO_WAKE=0`), per-tenant
  `auto_wake` override in the tenant-limits file. Wake-on-request is the
  product behavior (Sodoko end users reload a page, the system heals);
  combined with idle auto-sleep it makes sandboxes effectively serverless.
- **Dynamic-expose GC (ADR-0007 deferral)**: when the sweep auto-sleeps a
  sandbox it drops the ensure-created (all-digit-named) exposes — the name
  namespace IS the marker, no schema change. The next request re-ensures
  them through the same lazy path, seamless under auto-wake. Manual sleeps
  keep their exposes (frozen P3 behavior).

### Usage aggregation + retention

- `GET /api/v1/tenants/{id}/usage?from&to` folds the P0 `usage_events`
  stream into `sandbox_hours`, `vcpu_hours`, `mem_gib_hours`,
  `disk_gb_hours` (disk_gb 0 counts as the 2 GiB base image — same rule as
  the quota gate), `execs`, `events`. Pure view over events: intervals open
  on `created/started/resumed/woken/forked`, close on
  `paused/slept/stopped/deleted`, clip to the window (and to `now` for
  still-running sandboxes). The endpoint reads every tenant event up to
  `to` — but ONLY transition events span the full history; the high-volume
  `exec` rows are loaded just within `[from, to]` (pre-window exec rows feed
  no counter and no interval, so loading them would be waste). Indexed by
  `(tenant_id, ts)`.
- **Retention prunes ONLY `exec` rows.** Deleting a lifecycle transition
  would be a correctness bug: the `created`/`woken` anchor of a sandbox still
  running past the retention window would make its interval un-openable and
  silently zero its billable hours. Transitions are bounded by lifecycle
  churn (tiny next to exec rate), so keeping them indefinitely is cheap; the
  exec rows are the only ones that actually grow without bound.
- Per-tenant Prometheus gauges were **rejected for the open `/metrics`
  endpoint**: tenant-labeled series leak the tenant inventory and per-tenant
  footprint to anyone who can scrape it (the rest of the API avoids exactly
  this existence/sizing leak). The authenticated usage endpoint serves
  per-tenant data instead; an authenticated metrics surface is deferred to P6.
  *(Amended 2026-08-29: "the open `/metrics` endpoint" describes P5, not the
  code today — `/metrics` requires the admin token since the v4 hardening pass,
  and P6 delivered the authenticated surface as `GET /api/v1/metrics/tenants`.
  The rejection itself still holds: tenant-labeled series stay off `/metrics`
  regardless, because the admin scrape credential is a wider audience than the
  tenant-inventory data warrants. See the ADR-0010 amendment.)*
- Exec attempts now append an `"exec"` usage event (one INSERT per exec) —
  transition pairs cannot count execs, and per-tenant exec counts were a P5
  deliverable. Reachability: admin for any tenant; a tenant key for exactly
  its own id (the single tenant-reachable corner of `/api/v1/tenants`).
- **Retention (P0 deferral)**: the lifecycle loop hourly prunes events older
  than `usage_retention_days` (default 90, 0 = keep forever).

### Worker image-cache GC (ADR-0008 deferral)

Every ~10 min (piggybacked on the pool-refill loop, first run one interval
after start) the agent removes image-cache entries (`<image>.ext4`, sidecar,
stale `.partial`) that no VM record and no pool spec references, that are
older than 1h, and that are not `DEFAULT_IMAGE` — re-checked under the
per-image download lock before the unlink. In-flight creates are covered
because the VM record (with its image) is inserted before `ensure_image`
runs; the residual race (lock-free `decide()` fast path between re-check and
unlink) at worst fails one create with the already-documented retryable
cold-cache 502.

### Gratuitous-ARP fix (P4 deferral)

`IpRunner` (felucca-guest) gains `announce(gw)`: after a successful fork
re-IP the guest fires one throwaway UDP datagram at the gateway. Snapshot-
restored guests answer host-side ARP only after their first transmit; the
datagram (and the ARP request it triggers) is that transmit, so verify-v2's
ping checks see fork children immediately instead of after their first
organic packet.

### TS SDK + felucca verify

- `sdk/ts/` — zero-dependency typed client (global fetch, ESM, Node ≥ 18):
  create/get/list/delete, sleep/wake/fork, exec (buffered + `execStream`
  with onStdout/onStderr callbacks and proper SSE buffering), expose/
  unexpose, templates, tenantUsage. Tests use node:test with an in-process
  fake feluccad.
- `scripts/felucca-verify.sh <endpoint> [token]` — the conformance suite
  pointed at ANY deployment (the suite was already endpoint-parameterized;
  this is the published front door). Agent suite included when `AGENT_API`
  is set. A real `felucca` CLI subcommand is deferred until a CLI exists.

## Rejected alternatives

- **WebSocket for exec streaming** — heavier on every hop (upgrade through
  gateway + feluccad + agent), no benefit over SSE for a server→client
  byte stream; stdin interactivity is a different feature (P6+, needs a
  protocol redesign anyway).
- **Storing last_activity on every exec (DB write)** — the snapshot persist
  already serializes the whole working set; a per-exec write would double
  exec latency cost for a clock where seconds don't matter. (The `"exec"`
  usage event IS a per-exec write, but it's billing data — that one earns
  its INSERT.)
- **Aggregating usage with SQL window queries** — folding in Go keeps the
  event semantics (state machine, clipping, effective-disk rule) in one
  tested function and stays portable across the ADR-0002 Postgres path.
- **Dynamic-expose GC on every sleep** — would change frozen P3 behavior
  (manual sleep→wake keeps URLs working without a re-ensure); scoping GC to
  auto-sleep pairs it with auto-wake, which re-ensures transparently.
- **Refcounted image-cache GC** — a counter map shadowing create lifetimes
  adds state to keep correct forever; the VM-record-first ordering already
  covers in-flight creates, and the residual window is a retryable 502.

## Consequences

- The wire contract grows: `?stream=1` SSE exec, lifecycle fields on
  tenants/sandboxes, `/routes/activity`, `/tenants/{id}/usage`, template
  `tenant` scoping. All additive; every pre-P5 shape is byte-frozen
  (activity clocks are `json:"-"`).
- feluccad now has a second background goroutine (lifecycle) and the agent's
  refill loop does double duty (GC). Both are sweep-idempotent and
  best-effort; a missed tick delays, never corrupts.
- Auto-wake makes the gateway hold requests up to ~15s on first hit of a
  sleeping sandbox — acceptable for the reload-a-page UX it serves; bots
  hitting public URLs will keep sandboxes awake unless tenant limits say
  otherwise (rps limits + `auto_wake:false` are the lever).
- Usage events grow by one row per exec; retention bounds the table.

## Deferred

- Exec stdin / PTY interactivity (protocol redesign; post-v4).
- `felucca` CLI binary (would subsume felucca-verify.sh and join-token UX).
- Per-sandbox policy PATCH (create-only today; tenants can fork-replace).
- SDK publish to npm (needs org + CI; code ships in-repo).
