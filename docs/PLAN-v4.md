# Hearth v4 — multi-tenant, deploy-anywhere, product-ready

## Context

Hearth v3.1 is feature-complete and lab-verified (conformance 132/0, verify-v2 21/0) but single-tenant, single-node, lab-grade. The goal now is a **public, scaled deployment** whose competitive edge is the gap found in competitor issue trackers: *self-hosted/bare-metal-flexible microVM sandboxes with hardware isolation and fork* (E2B's top-requested unmet feature; Daytona only offers it with container isolation). **Deployment flexibility is itself the product**: control plane, gateway, and workers must be placeable on any mix of bare metal, nested-virt cloud VMs, and NAT'd home-lab hardware.

First consumer: **Sodoko** (`../odoo-engineer-kosuke`) — Lovable-for-Odoo. Each sandbox runs a full self-hosted Odoo system (odoo v17/18/19-CE + postgres + agent-server, extendable with Traccar/Chatwoot) as a docker-compose stack *inside* the microVM, with AI agents exec'ing in-guest and end users reaching the Odoo web UI via per-sandbox public URLs (today: traefik `dynamic-sandboxes.yml`; sandboxes prototyped on Sprites — Hearth replaces that). Direction may broaden to more products/users, so everything is built tenant-generic.

Decisions already made with the user:

- **Tenancy v1 = API keys + enforced namespaces** (tenant as first-class entity; OIDC layers on later for humans).
- **Cross-network transport = WireGuard overlay** (NAT'd workers, zero wire-protocol changes; TLS only on the public API).
- **All deployment targets** (bare metal + cloud VMs + home lab) supported and *tested as a mixed fleet*.
- Product needs (all in scope): custom guest images/templates, long-lived + sleep/wake, public ingress, streaming exec, fork workflows, high create rate later.

Key codebase facts to build on:

- `go/internal/state/state.go` — `State` struct + `Persist()` (atomic JSON); **no storage interface yet**. `model.go:41` Sandbox (has an unenforced `namespace` label), `:56` Node.
- Auth: one shared bearer token on both sides (`check_auth` in `rust/agent/src/server.rs`; equivalent in Go). hearthd→agent uses the same token over plain HTTP (`go/internal/agentclient`).
- `rust/agent/src/net.rs` — `ip hearth` nft table with a **postrouting masquerade chain only**; no forward filtering → guests on one bridge can reach each other freely today. Shell-out style (`ip`/`nft`/`sysctl`) is the established pattern.
- `rust/agent/src/config.rs` + `go/internal/config` — flags > env (`HEARTH_*`) > JSON > defaults; new keys slot in cleanly.
- `deploy/` — `install.sh`, `firecracker-assets.sh`, `config/`, `systemd/` (units hardened for Zig-era binaries, **never soak-tested** under Go/Rust — known `MemoryDenyWriteExecute` risk for the Go runtime).
- Build constraint: `CGO_ENABLED=0` static hearthd (DEPLOYMENT.md) — SQLite must be pure-Go (`modernc.org/sqlite`), not mattn/cgo.
- `test/conformance/` is the contract; every phase below grows it. ADR-0002 already designates SQLite → Postgres → HA as the sanctioned scale path.

## Phase 0 — Tenancy + SQLite state (control plane core) — ✅ DONE 2026-06-12

Completed and committed (`490c7a4`, 25 files +1675/−96). All five gates passed:
conformance **166/0** (new `hearthd/17-tenancy`, +34 checks), verify-v2 **21/0**,
legacy state.json migration verified live, docs/ADR-0004/API-V2 §3b written.
Inline security fixes during the phase: tenant-name length cap, DB file 0600.
Known deferred: quota TOCTOU one-sandbox burst window; usage_events retention (P5).

The foundation everything else keys on.

1. **Store interface**: extract `go/internal/state` access behind a `Store` interface (CRUD for sandboxes/nodes + new tenants/keys/templates). Implement `sqlitestore` with `modernc.org/sqlite` (WAL mode, single writer — correct at this scale per ADR-0002). One-time migration: detect `state.json`, import, rename to `.imported`.
2. **Tenant + APIKey entities**: `tenants` (id, name, quotas), `api_keys` (hash — store SHA-256 only, prefix `hearth_sk_`, tenant_id, created/revoked). Admin bootstrap key from config. CRUD endpoints under `/api/v1/tenants` (admin-key only).
3. **Enforce namespace = tenant boundary**: every sandbox row gains `tenant_id`. All sandbox routes resolve key→tenant and filter; cross-tenant access = 404 (not 403 — don't leak existence). The agent learns `tenant_id` per VM (create payload → `meta.json` new optional field, like `vsock` was added).
4. **Quotas**: per-tenant max sandboxes / vcpus / mem_mib / disk_gb, checked at create/fork; 429 + `hearth_quota_rejections_total` metric.
5. **Usage metering (events now, billing later)**: `usage_events` table — one row per sandbox lifecycle transition (tenant_id, sandbox_id, event, shape vcpus/mem/disk, timestamp), written where hearthd already applies state changes. Append-only; aggregation comes in P5. Records exist from day one so any future billing can be computed retroactively.

Conformance: new `hearthd/17-tenancy` cases (key scoping, cross-tenant 404, quota 429). The existing single-token mode stays as "admin key" so all current cases keep passing.

## Phase 1 — Cross-tenant network isolation (agent) — ✅ DONE 2026-06-12

Completed and committed. All five gates passed in one run: conformance **178/0**
(new `agent/11-isolation`, +12 checks), verify-v2 **21/0**, live control-plane
repro (cross-tenant ping dropped, same-tenant passes, egress intact). ADR-0005
documents the delivered design — it differs from the sketch below in one way:
a **single concatenated pair set** (`tenant_pairs`) instead of per-tenant named
sets, so `tenant_id` never appears in an nft command (closes item 4 by
construction). Tenant networks are node-scoped until P2 (item 3 documented).

Original spec — closes the table's worst ⛔. In `rust/agent/src/net.rs`, same shell-out style:

1. Add a `forward` chain (hook forward, policy drop for bridge-to-bridge) in the existing `ip hearth` table: allow established/related, allow guest→egress (non-CIDR destinations), **drop guest→guest by default**.
2. Per-tenant nft **named sets** (`hearth_t_<tenant>`): member IPs added/removed on VM create/delete/IP-change (fork re-IP included); one rule allows intra-set traffic. Rebuilt idempotently on agent start from adopted metas (reuse the reconcile pass).
3. Cross-node, same-tenant traffic is *not* bridged in v1 (per-node CIDRs are node-local today); document that tenant networks are node-scoped until the overlay phase extends them.
4. **Security requirement (from the P0 review)**: `tenant_id` reaches the agent over the authed API but MUST still be allowlist-validated (`[a-z0-9-]`, length-capped) before any embedding in nft set names / shell-outs — defense in depth against command injection through a compromised control plane.
5. **Lab constraint**: rapid sleep/wake/fork bursts have twice killed the kata-lab-0 Lima VM at the vz hypervisor level (documented in CHANGELOG). Gate runs accept one crash-recovery + rerun; the conformance agent cases are now self-cleaning so leftovers can't cascade. Bare-metal workers (P2) eliminate this class.

Conformance: new agent case — two VMs different tenants on one node: ping must FAIL; same tenant: ping must PASS; egress still works. (Drive via exec — the suite already has the machinery.)

## Phase 2 — Deploy anywhere: WireGuard overlay, join flow, hardened deployment

The "flexibility is the edge" phase.

> **External prerequisites (user-provided, can be procured while P1 runs):**
> a public host for hearthd (small cloud VM with a public IP), at least one
> bare-metal or nested-virt worker outside the lab for the mixed-fleet test,
> a **domain** (also needed by P3's `*.sb.<domain>` wildcard), and DNS hosted
> somewhere with an API that supports ACME **DNS-01** (e.g. Cloudflare/Route53/
> Hetzner DNS) for the gateway's wildcard cert. Code work in P2 proceeds on the
> lab regardless; the mixed-fleet acceptance needs the real infra.
> **Status: user is procuring these during P1** (decided 2026-06-12) — hand-off
> at P2 start: SSH access to the hosts, the domain, and DNS API credentials.

1. **Overlay**: hearthd gains a `wg` config block (its overlay IP, listen port, key path). New `POST /api/v1/nodes/join` exchanging a one-time **join token** (admin-issued, `hearth join-token create`) for: overlay address assignment, hearthd's pubkey/endpoint, peer registration. Agent gains `--join <url> --token <t>`: generates keys, calls join, writes/starts a `wg` interface (shell-out to `wg`/`ip`, consistent with net.rs), then registers as today over the overlay IP. NAT'd workers: persistent-keepalive; only hearthd needs a public endpoint.
2. **Public API TLS**: Go `autocert` (Let's Encrypt) on hearthd's public listener — keeps the single-binary story; `--tls-domain` config. UI served over the same listener.
3. **systemd soak**: run the lab fleet under `deploy/systemd/` units for 48h; fix the known hardening conflicts (`MemoryDenyWriteExecute` vs Go GC, seccomp vs Firecracker spawn); units gain the wg dependency. `install.sh` updated for joins; binaries to `deploy/release/<arch>/` for both arches.
4. **Mixed-fleet acceptance**: one Hetzner bare-metal worker + one nested-virt cloud VM + one home-lab/Lima worker joined to a cloud hearthd — `verify-v2` must pass against that fleet unmodified. This test *is* the positioning claim.

## Phase 3 — Sandbox ingress (the Sodoko unlock) — ✅ DONE 2026-06-12 (wildcard TLS + auto-wake deferred: external infra / P5; see ADR-0007)

Users must reach the Odoo UI inside a sandbox from the internet.

1. **Named multi-service expose API**: `POST /sandboxes/{id}/expose {"port": 8069, "name": "odoo"}` → `https://odoo--<sandbox-id>.sb.<domain>`. Many exposes per sandbox (Sodoko: odoo + chatwoot + traccar + agent-server = four rows). Each expose = one route row (hostname → node overlay IP, worker port) + one worker-side DNAT rule (nft, same `hearth` table): overlay-IP:allocated-port → guest-ip:port. Hostnames are **single-label** (`name--id`), never nested subdomains — a wildcard cert only covers one DNS label.
2. **Dynamic port-in-hostname routing** (`8069--<id>.sb.<domain>`, E2B-style, no expose call needed) as a per-sandbox opt-in (`allow_dynamic_ports`), **off by default**: agent-built guests have unintended listeners (postgres, agent-server) — explicit allowlist is the multi-tenant default; the toggle covers dev/debug.
3. **Gateway component**: a small Go reverse proxy (`cmd/hearth-gw`, reusing config/auth packages) deployable anywhere (typically beside hearthd): wildcard DNS `*.sb.<domain>` → gateway; ACME DNS-01 wildcard cert; routes from hearthd's API (cached, event-refreshed). Runs over the WG overlay to reach workers — so a sandbox on a NAT'd home-lab box is still publicly reachable. **WebSocket upgrade passthrough is in the acceptance tests from day one** (Odoo longpolling/chat, Chatwoot live updates; multi-worker Odoo's 8072 is just a second expose). Sleeping sandbox → 503 wake-on-request page (later: auto-wake).
4. Per-tenant ingress toggles + rate limits at the gateway.

## Phase 4 — Templates & bigger guests (Sodoko workloads) — ✅ DONE 2026-06-12

Completed and committed. All five gates green: conformance **266/0** (new
`hearthd/20-templates`, +39 checks incl. a capture → cross-node pull →
boot-from-captured-image marker proof), verify-v2 **21/0**, cargo **117/0**,
go suite green, both static binaries link; docker-base built live through
`scripts/build-template.sh` against the systemd fleet. ADR-0008 + API-V2 §3e
written. Notable deltas from the sketch below: capture-from-stopped-sandbox
IS the image pipeline (no separate builder path); `disk_gb` is floored at the
recorded image size (quota honesty); warm pools drain as well as fill, with
sha-matched claims; warm-pool specs have exactly one delivery channel
(PUT /v1/pools, pushed on template changes + after node register). Deferred
(ADR-0008): per-template tenant visibility (P5), disk-aware scheduling (P6),
worker image-cache GC (P5).

1. **Template entity** (Phase 0 store): name, rootfs image ref, default vcpus/mem/disk, optional warm-pool size. Create accepts `template`.
2. **Image pipeline**: `scripts/build-template.sh` — boot a builder VM from base, run a provision script in-guest (via exec), shut down, capture rootfs as `images/<template>.ext4`. First templates: `docker-base` (Docker+compose preinstalled — Sodoko stacks run unchanged) and `odoo-v18` (compose images pre-pulled so first boot is seconds). Distribution: hearthd holds the catalog; nodes pull-and-cache over the overlay (sha256-addressed; reuse the existing image-dir layout).
3. **Resource flexibility**: configurable disk size at create (truncate + `resize2fs` after the reflink copy — same copy_rootfs path), guests up to e.g. 8 vCPU/16GB; **per-template warm pools** (generalize `pool.rs` from the single hardcoded 1c/256MB shape to per-template shapes/counts).
4. Per-tenant disk accounting feeds Phase 0 quotas.

## Phase 5 — Exec streaming, lifecycle policies, TS SDK — ✅ DONE 2026-06-13

Completed and committed (`4c09f4e`). All five gates green: conformance **326/0**
(new `hearthd/21-stream-exec`, `22-lifecycle`, `23-template-visibility`),
verify-v2 **21/0**, cargo **41/0 guest + 121/0 agent**, go suite green, static
hearthd+hearth-gw link. ADR-0009 + API-V2 §3f written. Also closed the
deferrals parked by earlier phases: auto-wake + dynamic-expose GC (ADR-0007),
per-template tenant visibility + worker image-cache GC (ADR-0008),
usage_events retention (P0), gratuitous-ARP-after-fork (P4). Notable deltas
from the sketch below: streamed exec is NDJSON-in-guest → chunked at the agent
→ SSE at hearthd (one wire shape, translated per hop), buffered exec
byte-frozen; usage retention prunes only the high-volume `exec` rows (lifecycle
transitions are the interval skeleton); per-tenant `/metrics` gauges were
rejected as a tenant-inventory leak on the unauthenticated endpoint (data
served via the authed usage endpoint instead). Deferred (ADR-0009): exec
stdin/PTY, `hearth` CLI binary, authenticated metrics, per-sandbox policy
PATCH, SDK npm publish.

1. **Streaming exec**: SSE on hearthd (`?stream=1`) — agent streams stdout/stderr chunks over the existing vsock protocol (protocol gains a `stream` op; hearth-guest writes incremental frames). Long-running agent commands (module installs, builds) need this; buffered exec stays for compat.
2. **Idle/TTL policies**: per-tenant defaults + per-sandbox overrides — auto-sleep after N min idle (no exec/ingress traffic), optional auto-delete after M days asleep. Reconcile loop in hearthd drives it (it owns last-activity timestamps from exec/ingress).
3. **TypeScript SDK** (`sdk/ts/`): thin typed client over REST — create/exec(stream)/sleep/wake/fork/expose; mirrors the conformance contract so Sodoko integrates in a day. Publish the conformance suite as `hearth verify <endpoint>` — the trust artifact for self-hosters.
4. **Usage aggregation** over P0's `usage_events`: `GET /api/v1/tenants/{id}/usage?from&to` → sandbox-hours, vCPU-hours, GB-hours, exec counts (computed from transition pairs); `hearth_tenant_*` gauges for dashboards. Billing integration stays out of scope — this endpoint is its future input.

## Phase 6 — Scale-out & observability (post-launch)

- Structured logging (Go `slog`, Rust `tracing`) replacing eprintln/stderr; request IDs across hearthd→agent.
- Metrics expansion (per-tenant counters, wake/exec latency histograms) + Grafana dashboard in `deploy/`.
- `scripts/bench.sh`: p50/p95 for create/claim/wake/exec/fork; run per release; publish numbers (bare-metal datapoint for the comparison table).
- HA path when needed: `Store` already abstracts — litestream replication first, Postgres swap second, N×hearthd behind LB (ADR-0002's sanctioned path).
- Research track: **uffd CoW fork** (skip `mem.bin` copy) — Sodoko's "branch a live Odoo system" differentiator; the reason the agent is Rust.

## Ordering & first milestone

Sodoko can integrate after **Phases 0–4** (keys → isolation → reachable from anywhere → odoo template + ingress). Phases 0+1 are the first milestone (security base; everything else builds on tenant identity). Suggested cut points for releases: v4.0 = P0+P1, v4.1 = P2, v4.2 = P3+P4 (Sodoko alpha), v4.3 = P5.

## Per-step review loop (applies to every remaining phase, P2–P6)

After each implementation step and ALWAYS before a commit: loop
**/code-review → fix findings → /ultraqa (test-verify-fix cycle)** until both
come back green in the same iteration (user directive 2026-06-12). The phase
gates below remain the per-phase exit bar on top of this per-step loop.

## Phase gates (checked after EVERY phase, in order)

1. **Unit & build gate** — all inside the toolchain VM, never the host:
   `cargo test` (agent) green, `go vet ./...` clean, `go test ./...` green, and the
   static `CGO_ENABLED=0` cross-build of hearthd still links (the single-binary
   deployment story is load-bearing — a phase that breaks it fails the gate).
2. **Deploy gate** — roll the phase's binaries to the lab via the runbook
   (agents first, then hearthd): healthz on all three, 2 nodes ready, state
   migration/adoption verified from the logs (not assumed).
3. **E2E gate** — full conformance suite **0 FAIL** (the suite grows with each
   phase's new cases — currently 166 checks incl. tenancy) plus verify-v2
   **0 FAIL**, plus a manual live repro of the phase's headline behavior
   (e.g. P0: cross-tenant 404 + quota 429 + DB perms; P1 will be a real
   cross-tenant ping that must fail). Test-suite bugs found at this gate are
   fixed and the gate re-run until a fully green single run exists.
4. **Docs gate** — API-V2.md (contract), ARCHITECTURE.md (status + affected
   sections), CHANGELOG.md (what/why/evidence), and a new or amended ADR
   whenever the phase embodies an architectural decision.
5. **Commit gate** — one commit per phase in the repo's style, clean tree after.

## Verification

- Every phase adds conformance cases; the suite remains the contract (`test/conformance/`), runnable against any deployment (`hearth verify`).
- Phase-gate e2e on the **mixed fleet** (bare metal + cloud VM + NAT'd home lab): full verify-v2 + new tenancy/isolation/ingress checks.
- Sodoko smoke test as the final gate: create from `odoo-v18` template → expose `odoo` + a second service (chatwoot) → both public URLs serve, WebSocket (Odoo longpolling) works through the gateway → streamed exec installs a module → sleep → wake (URLs recover) → fork → child's services reachable on the child's own URLs, parent intact.
- Lab remains the dev environment (build in `infra-saas-lab`, roll via the runbook); production fleet is config-only different — same binaries, which is itself the product claim being tested.
