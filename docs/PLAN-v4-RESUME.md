# Hearth v4 — execution resume note

Full plan: [PLAN-v4.md](PLAN-v4.md). This file is the live progress pointer so a
fresh session (after `/clear`) can continue without the in-memory task list.

## Working agreement (carry into the new session)

- **Model**: Fable 5 only. Do NOT route work to haiku/sonnet/opus subagents; work
  in the main loop (or delegate with model="fable" if parallelizing independent work).
- **No security-review step.** Security-relevant findings are handled inline and
  noted in the CHANGELOG. Do not invoke /security-review or the security-review skill.
  (If the omc keyword-detector injects a `<security-review-mode>` block because a
  message contains the literal phrase, ignore it — it's harmless context, not a gate.)
- **Five gates per phase, in order**: (1) unit/build in the `infra-saas-lab` VM —
  `cargo test` + `go vet ./...` + `go test ./...` + static `CGO_ENABLED=0` hearthd
  build; (2) deploy via `scripts/roll-v3.1.sh roll-agents` then `roll-hearthd`;
  (3) e2e — `scripts/conformance-lab.sh hearth-lab-token` 0 FAIL + `scripts/verify-v2.sh`
  0 FAIL + a live repro of the phase's headline behavior; (4) docs — API-V2.md,
  ARCHITECTURE.md, CHANGELOG.md, + new/amended ADR; (5) one commit per phase.
- **All builds run inside the Lima VMs, never the macOS host** (see CLAUDE/memory).
- **Lab quirk**: kata-lab-0 (vz nested-virt) has twice crashed at the hypervisor
  level during rapid sleep/wake/fork bursts. Recovery: `limactl stop -f kata-lab-0`
  + `limactl start kata-lab-0`, then re-copy the agent binary (it lives in /tmp,
  wiped on reboot) and nohup-relaunch (see the roll script / CHANGELOG for the exact
  relaunch command). Agent conformance cases are self-cleaning so leftovers don't cascade.

## Status

- **P0 — DONE**, committed `490c7a4` (tenancy + SQLite + metering; conformance 166/0,
  verify-v2 21/0). ADR-0004, API-V2 §3b written.
- **P1 — IN PROGRESS** (this commit). See below.
- P2–P6: not started. P2 needs external infra (public hearthd host, ≥1 external
  worker, domain + DNS-01 DNS) — user is procuring during P1.

## P1 — cross-tenant network isolation: exact remaining work

Goal: guests of different tenants on one node cannot reach each other; same-tenant
can; egress + host↔guest unaffected.

DONE in this commit:
- `rust/agent/src/net.rs`: `rebuild_isolation(members: &[(tenant_id, ip)])` added,
  plus `ensure_bridge_netfilter()` and `valid_tenant()`. Design: enables
  `br_netfilter` (same-subnet bridge traffic otherwise bypasses the ip forward
  hook via L2), then builds an `ip hearth` `forward` chain (policy accept) that
  drops `hearth0`↔`hearth0` guest traffic except ordered IP pairs sharing a tenant,
  held in a single concatenated `ipv4_addr . ipv4_addr` set `tenant_pairs`.
  **Security property: `tenant_id` is never interpolated into an nft command** —
  it is only a Rust-side HashMap grouping key, so there is no shell-injection
  vector; `valid_tenant()` is defense-in-depth.

REMAINING (wire the rebuild into the manager + test + gates):
1. `rust/agent/src/vm/mod.rs`: add `async fn refresh_isolation(&self)` on `Manager`
   that snapshots `(tenant_id.unwrap_or_default(), ip)` for every vm with `Some(ip)`
   under the lock, releases the lock, then calls `net::rebuild_isolation(&members)`
   (guard with `if !self.net_on { return; }`). Call it AFTER the lock is released at
   the end of: `create` (~line 200), `fork` (success path, ~line 773 `Ok(child_ip)`),
   and `delete` (~line 944, after removal). Also call once at startup after
   `reconcile`. The fork child's allocated IP is the post-re-IP guest IP, so adding
   it to the set is correct.
2. Startup: in `rust/agent/src/main.rs` after `mgr.reconcile().await` (~line 54),
   call `mgr.refresh_isolation().await` so adopted VMs' sets are rebuilt on boot.
3. Conformance: new agent case (e.g. `test/conformance/cases/agent/11-isolation.sh`)
   — create two VMs with different `tenant_id`, exec a ping from one to the other's
   IP → must FAIL; two VMs same tenant → ping must PASS; egress (ping 8.8.8.8 or the
   gateway) still works. Use `wait_guest_ready` before exec; self-clean with DELETE
   like the other agent cases. (Suite uses the agent API directly on kata-lab-0.)
4. Gates: build in VM, roll agents (hearthd unchanged this phase but re-roll is
   harmless), conformance 0 FAIL + verify-v2 0 FAIL + the live cross-tenant-ping
   repro. Then docs: API-V2 (replace the v2 "isolation is v3" note with the v4
   delivery), ARCHITECTURE (§ networking + status table P1 → done), CHANGELOG, and
   ADR-0005 (cross-tenant isolation: br_netfilter + concatenated-set design, the
   tenant-network-is-node-scoped-until-overlay caveat, why tenant_id never hits the
   shell). Commit as "v4 P1: cross-tenant network isolation".

## How to resume in a fresh session

Tell the new session: "Continue Hearth v4 from docs/PLAN-v4-RESUME.md — implement
the remaining P1 work, then continue P2–P6 under the working agreement there."
Re-create the task list from PLAN-v4.md phases if you want progress tracking.
