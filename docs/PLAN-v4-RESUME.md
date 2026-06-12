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
- **After each step and before every commit: loop /code-review → /ultraqa until
  both are green** (user directive 2026-06-12). Fix all review findings, re-run
  the QA cycle, repeat until a clean pass; only then commit. /ultrawork for
  parallelizable independent work (fable-model executors only).
- **After each phase commit: prompt the user to run /compact** (user directive
  2026-06-12; Claude can't invoke the built-in command itself).
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
- **P2 — IN PROGRESS.** Step commits: P2.1 hearthd overlay+join (`bb182f3`),
  P2.2 agent --join (`7ac7ead`), P2.4 conformance+lab overlay e2e (`ee54a9f`,
  conformance **193/0**, verify-v2 **21/0**, kata-lab-1 LIVE on the overlay as
  10.100.0.2 — mixed fleet running), P2.3 TLS autocert + agent https-join via
  curl-stdin (this commit; live TLS verify needs the domain).
- P3–P6: not started.

## P2 — remaining work

Spec: PLAN-v4.md Phase 2. Done: overlay join flow (both sides), lab overlay
e2e, TLS code. Remaining:
1. **P2.5 systemd soak (48h, lab-workable)**: run the fleet under
   deploy/systemd units; fix MemoryDenyWriteExecute-vs-Go-GC and
   seccomp-vs-FC-spawn; units gain wg deps; install.sh join support; release
   binaries both arches. Long wall-clock — start it, work on other things.
2. **P2.6 mixed-fleet acceptance — BLOCKED on user hand-off**: public hearthd
   host SSH, ≥1 external worker, domain, DNS-01 API creds. ASK THE USER.
   verify-v2 must pass unmodified against the real mixed fleet; live TLS
   (autocert) verification happens here too.
3. **Phase close-out**: ADR-0006 (overlay design; deferred items recorded in
   step-commit messages: store-transactional IP allocation for HA, hub
   ip_forward for spoke↔spoke (P3 gateway-not-beside-hearthd), wg package
   client-side shape (P3), same-host hub/worker iface collision, TOFU
   bootstrap trust note), API-V2/ARCHITECTURE/CHANGELOG updates, one summary
   reference in CHANGELOG to the step commits, prompt user to run /compact.

## Lab overlay state (live since 2026-06-12)

hearthd on infra-saas-lab runs with `--wg-ip 10.100.0.1/24 --wg-endpoint
192.168.104.3:51820 --wg-key /tmp/hearth/wg.key` (hd log: /tmp/hearth/hd.log).
kata-lab-1's agent is enrolled (wg.json + wg.key in /srv/ignis, survives agent
restarts but NOT a VM reboot of the /tmp binary — re-roll per runbook) and
registers as 10.100.0.2:9090 over the tunnel. kata-lab-0 stays direct
(192.168.104.1:9090, pool=1). wireguard-tools installed on both. Roll gotchas
(both hit this session): pkill must live in its OWN limactl invocation; agent
musl builds MUST use CARGO_TARGET_DIR=$HOME/.cargo-target/hearth-agent or the
roll ships a stale binary. kata-lab-0 vz crash #3 happened mid-conformance;
runbook recovery worked (one crash + rerun is accepted per gate).

## How to resume in a fresh session

Tell the new session: "Continue Hearth v4 from docs/PLAN-v4-RESUME.md — start P2
(lab-side code first if external infra isn't ready), then P3–P6 under the working
agreement there." Re-create the task list from PLAN-v4.md phases if you want
progress tracking.
