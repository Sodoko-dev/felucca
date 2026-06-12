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
- **Run /ultraqa after every step** (user directive 2026-06-12), scoped to that
  step's verifiable goal; /ultrawork for parallelizable independent work
  (fable-model executors only).
- **Lab quirk**: kata-lab-0 (vz nested-virt) has twice crashed at the hypervisor
  level during rapid sleep/wake/fork bursts. Recovery: `limactl stop -f kata-lab-0`
  + `limactl start kata-lab-0`, then re-copy the agent binary (it lives in /tmp,
  wiped on reboot) and nohup-relaunch (see the roll script / CHANGELOG for the exact
  relaunch command). Agent conformance cases are self-cleaning so leftovers don't cascade.

## Status

- **P0 — DONE**, committed `490c7a4` (tenancy + SQLite + metering; conformance 166/0,
  verify-v2 21/0). ADR-0004, API-V2 §3b written.
- **P1 — DONE** (this commit): cross-tenant network isolation. All five gates green
  in one run: cargo test 50/0, go vet/test clean, static hearthd links; agents +
  hearthd rolled; conformance **178/0** (new `agent/11-isolation`, +12 checks),
  verify-v2 **21/0**, live control-plane repro (two tenants via admin API, four
  sandboxes on one node: cross-tenant ping exit 1, same-tenant exit 0, deletes 204).
  ADR-0005 written; API-V2 §5 + ARCHITECTURE §1/status table updated. Design notes:
  `tenant_id` never reaches an nft command (Rust-side grouping key only);
  `Manager::refresh_isolation()` serialized by `isolation_lock`, called after
  create/fork/delete and at startup post-reconcile; fail-closed for missing/invalid
  tenants; tenant networks node-scoped until P2.
- P2–P6: not started.

## P2 — next: WireGuard overlay, join flow, hardened deployment

Spec: PLAN-v4.md Phase 2. **External prerequisites (user-provided)**: public
hearthd host, ≥1 external worker (bare-metal or nested-virt), a domain, DNS with
ACME DNS-01 API. User was procuring these during P1 — ask for the hand-off
(SSH access, domain, DNS API credentials) at P2 start. Code work (wg config
block, join tokens, `POST /api/v1/nodes/join`, agent `--join`, autocert TLS,
systemd soak fixes) proceeds on the lab regardless; the mixed-fleet acceptance
needs the real infra.

## How to resume in a fresh session

Tell the new session: "Continue Hearth v4 from docs/PLAN-v4-RESUME.md — start P2
(lab-side code first if external infra isn't ready), then P3–P6 under the working
agreement there." Re-create the task list from PLAN-v4.md phases if you want
progress tracking.
