# ADR-0010 — Observability, bench, and HA groundwork

Status: accepted (v4 P6, 2026-08-09). Implements PLAN-v4.md Phase 6 minus the
two explicitly-deferred tracks (HA implementation, uffd CoW fork), which get
groundwork documents instead of code.

## Decision

### Structured logging — stdlib/ecosystem defaults, message text preserved

- **hearthd + hearth-gw**: Go `log/slog` with the stdlib TextHandler to
  stderr (journald wraps it; no JSON handler — `key=value` text is grep-able
  by operators AND parseable, and journald already supplies timestamps/units).
  `log.Fatal` sites become `slog.Error` + explicit `os.Exit(1)` so exit
  semantics stay byte-identical.
- **hearth-agent**: the `tracing` crate with `tracing-subscriber` (fmt
  writer to stderr, ansi off, `RUST_LOG` env filter, default `info`).
- **hearth-guest**: deliberately stays on `eprintln!` — it logs a handful of
  lines to the serial console and the tiny static binary in every rootfs
  image must not grow a subscriber stack.
- Migration rule: the human-readable phrase of every existing message is
  preserved (operators grep journals for "persist", "auto-slept", "guest
  unreachable"); interpolated values move to structured fields with a fixed
  vocabulary: `err`, `sandbox`/`vm`, `tenant`, `node`, `image`, `template`,
  `addr`, `request_id`.

### Request IDs — hearthd-minted, header-propagated

Every `/api/` request gets `req-<hex>` minted in the router, echoed back as
the `X-Hearth-Request-Id` response header, carried in the request context,
and forwarded on every hearthd→agent proxy call via the same header; the
agent includes it as a `request_id` field on its handler log lines.
Background actors mint their own prefixed ids (`sweep-`, `bg-`) so lifecycle
and prefetch agent calls are traceable too. No wire-shape change — headers
only; pre-P6 agents simply ignore the header (mixed-fleet safe).

### Metrics — histograms on the open endpoint, tenant series behind auth

- `/metrics` (open, unauthenticated — unchanged) gains three hand-rolled
  Prometheus histograms: `hearth_wake_duration_ms`,
  `hearth_exec_duration_ms`, `hearth_create_duration_ms`
  (`_bucket`/`_sum`/`_count`; shared ms bounds
  5,10,25,50,100,250,500,1000,2500,5000,10000,30000,+Inf — wide enough for
  ~70 ms wakes and multi-second cold creates alike). Wall-clock is measured
  at hearthd around the agent call (the number a user experiences), not
  agent-side. In-memory only; counter resets on restart are normal
  Prometheus semantics. The legacy `hearth_wake_ms_{last,sum}` /
  `hearth_wake_total` counters stay for pre-P6 dashboards.
- Per-tenant gauges (`hearth_tenant_{sandboxes,running,vcpus,mem_mib,disk_gb}`)
  — the series ADR-0009 kept OFF the open endpoint as a tenant-inventory
  leak — return on an **authenticated admin surface**:
  `GET /api/v1/metrics/tenants` (admin token; 404 to tenant keys like every
  admin route). Prometheus scrapes it as a second job with bearer
  credentials; `deploy/grafana/README.md` shows the scrape config.
- `deploy/grafana/hearth-dashboard.json`: fleet, latency (p50/p95 via
  `histogram_quantile`), throughput, and tenant rows.

### Bench — `scripts/bench.sh`, run per release

Wall-clock p50/p95/min/max over N iterations for: create-cold (off-pool
shape), create-claim (pool shape), exec (buffered), exec-stream
time-to-first-frame, wake (API wall-clock AND the agent-reported `wake_ms`),
fork. Bash+curl+jq only, endpoint-parameterized like `hearth-verify.sh`,
self-cleaning with an EXIT trap, per-op failure counts instead of aborts.
Output is a paste-ready markdown table; the lab numbers are published in
`docs/BENCHMARKS.md` and refreshed per release — the bare-metal datapoint
for the comparison table is the marketing artifact this exists for.

### HA + uffd — groundwork documents, no code

- `docs/HA-GROUNDWORK.md`: what is already HA-ready (the `Store` boundary,
  stateless gateway, self-re-registering agents), the honest blockers
  (in-memory working set + snapshot-granularity persistence, singleton wg
  hub, local images dir), and the staged path with effort estimates
  (litestream sidecar today → Postgres row-level store → N×hearthd).
  Recommendation: single hearthd until SLOs demand otherwise; litestream is
  deployable-today insurance.
- `research/uffd-cow-fork.md`: the "branch a live Odoo system" research
  track — Firecracker uffd-backed snapshot load, a per-lineage page-fault
  handler serving parent pages, parent-frozen-while-children-exist
  semantics, and a bounded prototype scope. Implementation deferred until
  the bench numbers show fork latency actually blocking a product use.

## Rejected alternatives

- **JSON log handlers** — journald + `key=value` text covers machine
  parsing; JSON doubles visual noise for zero operator gain at this scale.
- **OpenTelemetry / spans** — a tracing SDK, collector, and sampling policy
  for a three-binary system is infrastructure for its own sake; request-ID
  correlation over journald delivers the debugging property (follow one
  request across binaries) at ~zero cost. Revisit with real fleet scale.
- **Prometheus client libraries** — the hand-rolled emitter already exists
  and is conformance-frozen; a client lib would re-shape every existing
  series name for cosmetic gain.
- **Tenant series on the open endpoint, hashed labels** — hashing hides the
  id but not the cardinality (tenant count + per-tenant footprint still
  leak); auth is the correct boundary, and Prometheus supports it natively.

## Consequences

- Journals become grep-AND-field searchable; one request id follows an API
  call from hearthd into the owning agent. Log volume is unchanged (same
  sites, same levels).
- The agent gains two crates (`tracing`, `tracing-subscriber`) — a binary
  size cost accepted for the ecosystem-standard filter/env story; the guest
  binary is unchanged.
- /metrics stays safe-by-default; operators who want tenant dashboards make
  an explicit authenticated choice.
- Histogram state resets on restart (documented; standard practice).
- Bench numbers become a per-release ritual — stale numbers in
  `docs/BENCHMARKS.md` should be treated as a release blocker, not decor.

## Deferred

- HA implementation (all three stages — see docs/HA-GROUNDWORK.md).
- uffd CoW fork prototype (research/uffd-cow-fork.md).
- OpenTelemetry/spans, log sampling, per-tenant metrics on a per-tenant
  (customer-facing) dashboard surface.
- Structured logging for hearth-guest (deliberate non-goal).
- Context-scoped request logging (P6 review): request_id is hand-attached to
  key error sites, not every request-path line — a ctx-aware slog.Handler
  would stamp all of them; revisit if operators hit the gap in practice.
- Context-based reqID propagation through agentclient (currently an explicit
  string param, consistent with the agentCall test hook; a ctx param is the
  cleaner altitude when the next cross-cutting concern arrives).
- bench.sh create-op dedup (ops 1 and 2 share a ~25-line loop).
