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
- **Lab quirk**: kata-lab-0 (vz nested-virt) has crashed four times at the
  hypervisor level during rapid VM-churn bursts. Recovery (since the systemd
  migration): just `limactl stop -f kata-lab-0` + `limactl start kata-lab-0` —
  the hearth-agent unit auto-starts and re-registers, nothing to re-stage.
  Agent conformance cases are self-cleaning so leftovers don't cascade. One
  crash-recovery + rerun is gate-accepted.

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
- **P3 — DONE 2026-06-12**: sandbox ingress. Step commits `ff3cdc3` (expose
  API + worker DNAT), `653e2e2` (hearth-gw), plus the phase-close commit
  (conformance `hearthd/19-expose`, hearth-gw.service, ADR-0007, docs).
  Gates: cargo 87/0, go suite green, static hearthd+hearth-gw link;
  conformance **227/0**, verify-v2 **21/0** on the systemd fleet; live e2e:
  HTTP + WebSocket 101 through gw→DNAT→guest (direct worker), dynamic route
  over the wg overlay worker, sleep→wake page→recovery, fork child URLs.
  hearth-gw runs in the lab under systemd (config /etc/hearth/hearth-gw.env,
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
  Gates: conformance **266/0** (new `hearthd/20-templates`, +39), verify-v2
  **21/0**, cargo **117/0**; docker-base built live via the public API.
  Two kata-lab-0 vz crashes (#5, #6) during the heavier P4 suites — both
  healed by `limactl stop -f` + `start`, zero manual staging. Key fixes
  found live: hearthd's global 60s WriteTimeout killed >60s capture/image
  streams AND >60s execs (per-route ResponseController deadlines — exec's
  sized to its own timeout, it's tenant-reachable); guest /tmp is noexec
  (provision runs via `sh`); bash `while read` drops fold's final
  unterminated chunk (upload silently empty for small scripts) and exec's
  `ok:true` only covers transport, never the command's exit_code; the FC
  guest kernel has legacy xtables only (docker needs iptables-legacy +
  nat-unprotected bridge networks — see deploy/templates/docker-base.sh);
  snapshot-restored guests (fork children) answer host ARP only after
  their first transmit (ping-only, self-heals; gratuitous-ARP-after-re-IP
  in hearth-guest is the P5/P6 fix).
  Deferred (ADR-0008): per-template tenant visibility (P5), disk-aware
  scheduling (P6), worker image-cache GC (P5), odoo-v18 lab build (needs
  ~12 GB guest disk + long pulls; provision script ships ready).
- **P5 — DONE 2026-06-13**, committed `4c09f4e`: streaming exec, lifecycle
  policies, usage aggregation, TS SDK. Streamed exec is NDJSON in the guest →
  chunked `application/x-ndjson` at the agent → SSE (`?stream=1`) at hearthd
  (buffered exec byte-frozen; 16 MiB/stream cap; per-stream UTF-8 carry across
  chunk boundaries). Per-tenant + per-sandbox idle auto-sleep / asleep-TTL
  auto-delete via a 15s hearthd sweep; activity from create/fork(parent+child)/
  wake/exec/gateway-ingress; gateway auto-wake (singleflighted) + dynamic-expose
  GC. `GET /tenants/{id}/usage` folds usage_events (exec events appended per
  exec; retention prunes ONLY exec rows — transitions are the interval
  skeleton). Per-template tenant visibility, worker image-cache GC,
  gratuitous-ARP fork fix, zero-dep `sdk/ts` + `scripts/hearth-verify.sh`.
  Gates: cargo **41/0 guest + 121/0 agent**, go vet/test green (17 new server
  tests), static hearthd+hearth-gw link; fleet rolled; conformance **326/0**
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
  `hearth` CLI binary, authenticated per-tenant metrics, per-sandbox policy
  PATCH, SDK npm publish.
- P6: not started.

## P2 — remaining work

Spec: PLAN-v4.md Phase 2. Done: overlay join flow (both sides), lab overlay
e2e, TLS code, fleet migrated to systemd. Remaining:
1. **P2.5 soak — RUNNING since 2026-06-12 ~11:20 CEST.** The whole lab fleet
   now runs under the deploy/systemd units (hearthd on infra-saas-lab as user
   `hearth`, state in /var/lib/hearth; agents on both kata VMs as root,
   binaries in /usr/local/bin, configs in /etc/hearth). Migration findings,
   all fixed + gate-verified (conformance 193/0, verify-v2 21/0, cargo 77/0):
   seccomp killed FC (needs `@sandbox` — FC calls seccomp(2) for its own
   filters); DeviceAllow needed /dev/net/tun; ReadWritePaths entries must
   exist (RuntimeDirectory removed — /run/hearth was dead config, also
   dropped from install.sh); StateDirectoryMode=0750 pin; missing
   CAP_DAC_OVERRIDE made root unable to read magdy-owned state → wg.rs
   load_state refactored to Result (unreadable/corrupt wg.json now FATAL,
   never silent direct-mode) + wg.key pre-flight in ensure_interface.
   kata-lab-0 vz crash #4 hit mid-conformance; systemd self-healed it on VM
   restart (unit auto-start + modules-load.d) — zero manual staging, old
   runbook re-copy steps obsolete. **Check after ~2026-06-14 11:00:** `sudo
   journalctl -u hearthd -u hearth-agent` on all 3 VMs for restarts/SIGSYS/
   MDWE kills (`systemctl show -p NRestarts`), then drop the MDWE
   soak-experiment comment in hearthd.service if clean. Release binaries for
   both arches: BUILT 2026-06-12, staged in deploy/release/{aarch64,x86_64}/
   (gitignored; rebuilt in the infra-saas-lab VM — x86_64 agent cross-built
   with rust-lld, Go via GOARCH; x86_64 pair untested until P2.6's real
   worker). ADR-0006 + API-V2 §3c + ARCHITECTURE row + CHANGELOG P2 entry
   committed (`2e9fdf0`) — phase close-out docs are DONE; the phase itself
   closes on soak verdict + P2.6.
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

## Lab fleet state (systemd since 2026-06-12)

Everything reboot-survivable now. hearthd: `systemctl {status,restart} hearthd`
on infra-saas-lab — binary /usr/local/bin/hearthd, config
/etc/hearth/hearthd.json (token, wg_ip 10.100.0.1/24, wg_endpoint
192.168.104.3:51820, wg_key/state/db under /var/lib/hearth, ui
/usr/share/hearth/ui), logs via journalctl. Agents on both kata VMs:
`hearth-agent.service` — binary /usr/local/bin/hearth-agent, config
/etc/hearth/hearth-agent.json (+ drop-in lab.conf resetting ReadWritePaths to
/srv/ignis, the lab's non-default data_dir). kata-lab-1 enrolled (wg.json/wg.key
in /srv/ignis, root-owned — the unit user); kata-lab-0 direct, pool=1.
To roll a new agent build: build with
CARGO_TARGET_DIR=$HOME/.cargo-target/hearth-agent (stale-binary gotcha), copy
to /usr/local/bin/hearth-agent, `systemctl restart hearth-agent`. The
journald `sudo: unable to open /etc/sudoers` lines at agent start are benign
(bare-then-sudo fallback probing under NoNewPrivileges). vz crash recovery is
now just `limactl stop -f` + `start` — units self-heal (crash #4 proved it).

## How to resume in a fresh session

Tell the new session: "Continue Hearth v4 from docs/PLAN-v4-RESUME.md — start P2
(lab-side code first if external infra isn't ready), then P3–P6 under the working
agreement there." Re-create the task list from PLAN-v4.md phases if you want
progress tracking.
