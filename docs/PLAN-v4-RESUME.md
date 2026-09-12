# Felucca v4 — execution resume note

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
  `cargo test` + `go vet ./...` + `go test ./...` + static `CGO_ENABLED=0` feluccad
  build; (2) deploy via `scripts/roll-v3.1.sh roll-agents` then `roll-feluccad`;
  (3) e2e — `scripts/conformance-lab.sh felucca-lab-token` 0 FAIL + `scripts/verify-v2.sh`
  0 FAIL + a live repro of the phase's headline behavior; (4) docs — API-V2.md,
  ARCHITECTURE.md, CHANGELOG.md, + new/amended ADR; (5) one commit per phase.
- **All builds run inside the Lima VMs, never the macOS host** (see CLAUDE/memory).
- **After each step and before every commit: loop /code-review → /ultraqa until
  both are green** (user directive 2026-06-12). Fix all review findings, re-run
  the QA cycle, repeat until a clean pass; only then commit. /ultrawork for
  parallelizable independent work (fable-model executors only).
- **After each phase commit: prompt the user to run /compact** (user directive
  2026-06-12; Claude can't invoke the built-in command itself).
- **Lab quirk**: kata-lab-0 (vz nested-virt) has crashed four times at the
  hypervisor level during rapid VM-churn bursts. Recovery (since the systemd
  migration): just `limactl stop -f kata-lab-0` + `limactl start kata-lab-0` —
  the felucca-agent unit auto-starts and re-registers, nothing to re-stage.
  Agent conformance cases are self-cleaning so leftovers don't cascade. One
  crash-recovery + rerun is gate-accepted.

## Status

- **P0 — DONE**, committed `490c7a4` (tenancy + SQLite + metering; conformance 166/0,
  verify-v2 21/0). ADR-0004, API-V2 §3b written.
- **P1 — DONE** (this commit): cross-tenant network isolation. All five gates green
  in one run: cargo test 50/0, go vet/test clean, static feluccad links; agents +
  feluccad rolled; conformance **178/0** (new `agent/11-isolation`, +12 checks),
  verify-v2 **21/0**, live control-plane repro (two tenants via admin API, four
  sandboxes on one node: cross-tenant ping exit 1, same-tenant exit 0, deletes 204).
  ADR-0005 written; API-V2 §5 + ARCHITECTURE §1/status table updated. Design notes:
  `tenant_id` never reaches an nft command (Rust-side grouping key only);
  `Manager::refresh_isolation()` serialized by `isolation_lock`, called after
  create/fork/delete and at startup post-reconcile; fail-closed for missing/invalid
  tenants; tenant networks node-scoped until P2.
- **P2 — IN PROGRESS.** Step commits: P2.1 feluccad overlay+join (`bb182f3`),
  P2.2 agent --join (`7ac7ead`), P2.4 conformance+lab overlay e2e (`ee54a9f`,
  conformance **193/0**, verify-v2 **21/0**, kata-lab-1 LIVE on the overlay as
  10.100.0.2 — mixed fleet running), P2.3 TLS autocert + agent https-join via
  curl-stdin (this commit; live TLS verify needs the domain).
- **P3 — DONE 2026-06-12**: sandbox ingress. Step commits `ff3cdc3` (expose
  API + worker DNAT), `653e2e2` (felucca-gw), plus the phase-close commit
  (conformance `feluccad/19-expose`, felucca-gw.service, ADR-0007, docs).
  Gates: cargo 87/0, go suite green, static feluccad+felucca-gw link;
  conformance **227/0**, verify-v2 **21/0** on the systemd fleet; live e2e:
  HTTP + WebSocket 101 through gw→DNAT→guest (direct worker), dynamic route
  over the wg overlay worker, sleep→wake page→recovery, fork child URLs.
  felucca-gw runs in the lab under systemd (config /etc/felucca/felucca-gw.env,
  domain sb.lab.test, :8088). Deferred (ADR-0007): wildcard TLS (external
  infra, with P2.6), auto-wake + dynamic-expose GC (P5), delete-failure
  expose reconciliation (P6 reschedule sweep).
- **P4 — DONE 2026-06-12**: templates & bigger guests. Template entity +
  rootfs capture (`POST /api/v1/templates`, stopped-only, capture-guarded),
  sha256-addressed pull-and-cache image distribution (per-image locks,
  sidecar-first ordering, prefetch push), grow-only disk resize with the
  `image_size_gb` floor feeding the `max_disk_gb` quota, per-template warm
  pools (drain + sha-matched claims, single PUT /v1/pools channel),
  `scripts/build-template.sh` + `deploy/templates/{docker-base,odoo-v18}.sh`.
  Gates: conformance **266/0** (new `feluccad/20-templates`, +39), verify-v2
  **21/0**, cargo **117/0**; docker-base built live via the public API.
  Two kata-lab-0 vz crashes (#5, #6) during the heavier P4 suites — both
  healed by `limactl stop -f` + `start`, zero manual staging. Key fixes
  found live: feluccad's global 60s WriteTimeout killed >60s capture/image
  streams AND >60s execs (per-route ResponseController deadlines — exec's
  sized to its own timeout, it's tenant-reachable); guest /tmp is noexec
  (provision runs via `sh`); bash `while read` drops fold's final
  unterminated chunk (upload silently empty for small scripts) and exec's
  `ok:true` only covers transport, never the command's exit_code; the FC
  guest kernel has legacy xtables only (docker needs iptables-legacy +
  nat-unprotected bridge networks — see deploy/templates/docker-base.sh);
  snapshot-restored guests (fork children) answer host ARP only after
  their first transmit (ping-only, self-heals; gratuitous-ARP-after-re-IP
  in felucca-guest is the P5/P6 fix).
  Deferred (ADR-0008): per-template tenant visibility (P5), disk-aware
  scheduling (P6), worker image-cache GC (P5), odoo-v18 lab build (needs
  ~12 GB guest disk + long pulls; provision script ships ready).
- **P5 — DONE 2026-06-13**, committed `4c09f4e`: streaming exec, lifecycle
  policies, usage aggregation, TS SDK. Streamed exec is NDJSON in the guest →
  chunked `application/x-ndjson` at the agent → SSE (`?stream=1`) at feluccad
  (buffered exec byte-frozen; 16 MiB/stream cap; per-stream UTF-8 carry across
  chunk boundaries). Per-tenant + per-sandbox idle auto-sleep / asleep-TTL
  auto-delete via a 15s feluccad sweep; activity from create/fork(parent+child)/
  wake/exec/gateway-ingress; gateway auto-wake (singleflighted) + dynamic-expose
  GC. `GET /tenants/{id}/usage` folds usage_events (exec events appended per
  exec; retention prunes ONLY exec rows — transitions are the interval
  skeleton). Per-template tenant visibility, worker image-cache GC,
  gratuitous-ARP fork fix, zero-dep `sdk/ts` + `scripts/felucca-verify.sh`.
  Gates: cargo **41/0 guest + 121/0 agent**, go vet/test green (17 new server
  tests), static feluccad+felucca-gw link; fleet rolled; conformance **326/0**
  (new `21-stream-exec`, `22-lifecycle`, `23-template-visibility`), verify-v2
  **21/0**; ADR-0009 + API-V2 §3f + ARCHITECTURE + CHANGELOG. Review-loop
  catches fixed before commit: per-stream UTF-8 boundary corruption; a
  tenant-inventory leak on the unauthenticated `/metrics` (gauges removed);
  retention pruning interval-anchor events; a per-exec DB write under the
  global state lock; an un-singleflighted auto-wake storm. Two conformance
  failures surfaced were both NEW test cases (SSE-is-chunked harness
  assumption; `image_file` pointing at a worker-only image) — fixed in the
  harness/test, no production code weakened. kata-lab-0 vz crashed #8–#9 under
  the heavier P5 capture/VM churn (3 capture/lifecycle cases) — healed by
  `limactl stop -f` + `start`; every crash-run failure was `000`/empty-body,
  the recovered re-run a clean 326/0. Deferred (ADR-0009): exec stdin/PTY,
  `felucca` CLI binary, authenticated per-tenant metrics, per-sandbox policy
  PATCH, SDK npm publish.
- **P6 — DONE 2026-08-09**: observability, bench, HA groundwork. Structured
  logging (Go slog / Rust tracing; fatal + isolation diagnostics stay
  unconditional on stderr), X-Felucca-Request-Id minted per API request and
  propagated feluccad→agent (SDK captures it in FeluccaError), latency
  histograms on open /metrics + tenant gauges behind authed
  /api/v1/metrics/tenants, Grafana dashboard in deploy/grafana/,
  scripts/bench.sh + BENCHMARKS.md (wake p50 75ms, exec 41ms, stream
  first-frame 9ms, fork 1.1s), HA-GROUNDWORK.md + research/uffd-cow-fork.md.
  Gates: conformance 342/0, verify-v2 21/0, cargo 121/0+41/0. ADR-0010.
  LAB LESSON: Lima user-v2 DHCP addresses are NOT stable across long stops —
  the workers' .1/.4 swapped after a 2-month gap; the product self-healed
  (agent addr auto-detect) but hardcoded test maps broke. verify-v2 and
  conformance-lab.sh now resolve workers dynamically; never hardcode 104.x.

## P2 — remaining work

Spec: PLAN-v4.md Phase 2. Done: overlay join flow (both sides), lab overlay
e2e, TLS code, fleet migrated to systemd. Remaining:
1. **P2.5 soak — RUNNING since 2026-06-12 ~11:20 CEST.** The whole lab fleet
   now runs under the deploy/systemd units (feluccad on infra-saas-lab as user
   `felucca`, state in /var/lib/felucca; agents on both kata VMs as root,
   binaries in /usr/local/bin, configs in /etc/felucca). Migration findings,
   all fixed + gate-verified (conformance 193/0, verify-v2 21/0, cargo 77/0):
   seccomp killed FC (needs `@sandbox` — FC calls seccomp(2) for its own
   filters); DeviceAllow needed /dev/net/tun; ReadWritePaths entries must
   exist (RuntimeDirectory removed — /run/felucca was dead config, also
   dropped from install.sh); StateDirectoryMode=0750 pin; missing
   CAP_DAC_OVERRIDE made root unable to read magdy-owned state → wg.rs
   load_state refactored to Result (unreadable/corrupt wg.json now FATAL,
   never silent direct-mode) + wg.key pre-flight in ensure_interface.
   kata-lab-0 vz crash #4 hit mid-conformance; systemd self-healed it on VM
   restart (unit auto-start + modules-load.d) — zero manual staging, old
   runbook re-copy steps obsolete. **Soak verdict — HARDENING PASS
   (2026-06-13):** journals across the whole window (2026-06-12 11:20 →
   2026-06-13, all P3/P4/P5 conformance + verify load) on all 3 VMs show ZERO
   SIGSYS / seccomp denial / MDWE (W^X) kill / OOM — under `MDWE=true`
   (feluccad) and the `@sandbox` seccomp set (agent, so FC installs its own
   per-thread filters). Pure-Go feluccad tolerates MemoryDenyWriteExecute; the
   soak-experiment comment in `deploy/systemd/feluccad.service` is resolved to
   "validated, keep". All unit restarts in the window are explained by vz
   hypervisor crashes #5–#9 on kata-lab-0 (Lima/macOS nested-virt, NOT the
   hardening — they kill the whole VM, systemd cleanly restarts the unit on
   boot) plus the P5 binary redeploys. CAVEAT: the literal *continuous*-48h
   clock was reset by tonight's P5 deploy (~20:09; all units restarted with
   new binaries) — the hardening *config* (the .service files) is unchanged
   and now validated across THREE binary generations, but a frozen-binary
   48h-continuous number would need a fresh soak (completes ~2026-06-15 20:40).
   Recommendation: hardening risk is closed; a fresh soak can run as
   belt-and-suspenders at zero cost (fleet just keeps running). Release binaries for
   both arches: BUILT 2026-06-12, staged in deploy/release/{aarch64,x86_64}/
   (gitignored; rebuilt in the infra-saas-lab VM — x86_64 agent cross-built
   with rust-lld, Go via GOARCH; x86_64 pair untested until P2.6's real
   worker). ADR-0006 + API-V2 §3c + ARCHITECTURE row + CHANGELOG P2 entry
   committed (`2e9fdf0`) — phase close-out docs are DONE; the phase itself
   closes on soak verdict + P2.6.
2. **P2.6 mixed-fleet acceptance — BLOCKED on user hand-off**: public feluccad
   host SSH, ≥1 external worker, domain, DNS-01 API creds. ASK THE USER.
   verify-v2 must pass unmodified against the real mixed fleet; live TLS
   (autocert) verification happens here too.
3. **Phase close-out**: ADR-0006 (overlay design; deferred items recorded in
   step-commit messages: store-transactional IP allocation for HA, hub
   ip_forward for spoke↔spoke (P3 gateway-not-beside-feluccad), wg package
   client-side shape (P3), same-host hub/worker iface collision, TOFU
   bootstrap trust note), API-V2/ARCHITECTURE/CHANGELOG updates, one summary
   reference in CHANGELOG to the step commits, prompt user to run /compact.

## Lab fleet state — renamed to Felucca 2026-09-12

Migrated in place (backups in `/root/hearth-backup-*` on each VM): units
`feluccad`, `felucca-gw`, `felucca-agent`, `felucca-firewall`; config under
`/etc/felucca`, state under `/var/lib/felucca` (`felucca.db`), UI under
`/usr/share/felucca/ui`; a real 64-hex lab token in `~/.config/felucca/lab-token`
on the host (the old placeholder is refused by both binaries). Lima DHCP now
gives the control plane **192.168.104.1**, kata-lab-0 `.3` (direct, pool=1),
kata-lab-1 `.4` (overlay `10.100.0.2`). Node ids were regenerated to the 104-bit
form; the DB has no sandboxes from before the rename. Gates after migration:
conformance 423/0, verify-v2 21/0. The section below is kept for the systemd
layout details, which are unchanged apart from the names.

## Lab fleet state (systemd since 2026-06-12)

Everything reboot-survivable now. feluccad: `systemctl {status,restart} feluccad`
on infra-saas-lab — binary /usr/local/bin/feluccad, config
/etc/felucca/feluccad.json (token, wg_ip 10.100.0.1/24, wg_endpoint
192.168.104.3:51820, wg_key/state/db under /var/lib/felucca, ui
/usr/share/felucca/ui), logs via journalctl. Agents on both kata VMs:
`felucca-agent.service` — binary /usr/local/bin/felucca-agent, config
/etc/felucca/felucca-agent.json (+ drop-in lab.conf resetting ReadWritePaths to
/srv/ignis, the lab's non-default data_dir). kata-lab-1 enrolled (wg.json/wg.key
in /srv/ignis, root-owned — the unit user); kata-lab-0 direct, pool=1.
To roll a new agent build: build with
CARGO_TARGET_DIR=$HOME/.cargo-target/felucca-agent (stale-binary gotcha), copy
to /usr/local/bin/felucca-agent, `systemctl restart felucca-agent`. The
journald `sudo: unable to open /etc/sudoers` lines at agent start are benign
(bare-then-sudo fallback probing under NoNewPrivileges). vz crash recovery is
now just `limactl stop -f` + `start` — units self-heal (crash #4 proved it).

## How to resume in a fresh session

Tell the new session: "Continue Felucca v4 from docs/PLAN-v4-RESUME.md — start P2
(lab-side code first if external infra isn't ready), then P3–P6 under the working
agreement there." Re-create the task list from PLAN-v4.md phases if you want
progress tracking.
