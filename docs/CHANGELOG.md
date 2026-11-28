# Felucca — Project history

Everything built, verified, and learned, in order. Companion docs:
[PLAN.md](PLAN.md) (adopted architecture), [API-V2.md](API-V2.md) +
[API-V3-EXEC.md](API-V3-EXEC.md) (contracts), [ARCHITECTURE.md](ARCHITECTURE.md)
(current system), the ADRs in [adr/](adr/), and [DEPLOYMENT.md](DEPLOYMENT.md).

## Rename: Hearth → Felucca (2026-09-12)

The product, both binaries, on-disk paths, and every public identifier were
renamed from **Hearth** to **Felucca** via a case-preserving find/replace over
the whole tree. History below has been rewritten to the new name throughout —
the binaries, config paths, and everything else described in older entries
always refer to what is now called Felucca. Commit hashes and tags (`v3.0.0`,
`v3.1.0`) are **unchanged**; only the working tree content was rewritten.

| Old (Hearth) | New (Felucca) |
|---|---|
| `hearthd` | `feluccad` |
| `hearth-agent` | `felucca-agent` |
| `hearth-gw` | `felucca-gw` |
| `hearth-guest` | `felucca-guest` |
| `hearth0` (bridge) | `felucca0` |
| `wg-hearth` | `wg-felucca` |
| `HEARTH_*` env vars | `FELUCCA_*` |
| `X-Hearth-Request-Id` | `X-Felucca-Request-Id` |
| `hearth_*` metrics | `felucca_*` |
| `hearth_sk_` / `hearth_nt_` / `hearth_jt_` key prefixes | `felucca_sk_` / `felucca_nt_` / `felucca_jt_` |
| `/etc/hearth` | `/etc/felucca` |
| `/var/lib/hearth` | `/var/lib/felucca` |
| `/usr/share/hearth` | `/usr/share/felucca` |
| `/srv/hearth` | `/srv/felucca` |
| `~/.config/hearth` | `~/.config/felucca` |
| `hearth.db` | `felucca.db` |
| `.hearth-guest-v1` marker | `.felucca-guest-v1` |
| `hearthd.service`, `hearth-agent.service`, `hearth-gw.service`, `hearth-firewall.service` | `feluccad.service`, `felucca-agent.service`, `felucca-gw.service`, `felucca-firewall.service` |
| `modules-load.d/hearth.conf` | `modules-load.d/felucca.conf` |
| `@hearth/sdk` (npm) | `@felucca/sdk` |
| Go module `…/hearth` | `…/felucca` |

### Migration for existing deployments (lab included)

- [ ] Stop the old units before touching anything: `systemctl stop hearthd hearth-agent hearth-gw hearth-firewall` (as applicable per host).
- [ ] Move config: `/etc/hearth` → `/etc/felucca`, renaming `hearthd.json` → `feluccad.json` and `hearth-agent.json` → `felucca-agent.json` inside it.
- [ ] Move state: `/var/lib/hearth` → `/var/lib/felucca`, **and rename `hearth.db` → `felucca.db` inside it** — `feluccad` looks for `felucca.db` by name, so skipping this rename starts the new binary with **empty state**.
- [ ] Move shared assets: `/usr/share/hearth` → `/usr/share/felucca`.
- [ ] Move the lab token: `~/.config/hearth/lab-token` → `~/.config/felucca/lab-token`.
- [ ] Install the new unit files (`feluccad.service`, `felucca-agent.service`, `felucca-gw.service`, `felucca-firewall.service`) and `systemctl disable` (and remove) the old `hearth*` units so they can't race the new ones on next boot.
- [ ] Move `modules-load.d/hearth.conf` → `modules-load.d/felucca.conf`.
- [ ] On workers, rename the per-image marker `{data_dir}/images/.hearth-guest-v1` → `.felucca-guest-v1`. Existing rootfs images/templates still carry the **old** `hearth-guest` binary and unit baked in — they keep working as-is over vsock (the wire protocol is unchanged), but should be re-injected with `infra/guest-agent-install.sh` at the next template rebuild so newly captured images carry `felucca-guest`.
- [ ] Bridges `hearth0` and `wg-hearth` are recreated under their new names (`felucca0`, `wg-felucca`) automatically the next time the agent starts; the old interfaces are orphaned and can be removed with `ip link del hearth0` / `ip link del wg-hearth`.
- [ ] Re-import the Grafana dashboard — the metric names changed (`hearth_*` → `felucca_*`), so a dashboard built against the old names will show empty panels.
- [ ] Update any client code to send `FELUCCA_*` env vars, expect `X-Felucca-Request-Id`, and use `felucca_sk_…` tenant keys going forward.
- [ ] Per-node credentials: the worker's `{data_dir}/node-token` and the `node_creds` rows in `felucca.db` still carry the `hearth_nt_` prefix, which the bearer gate now rejects. Either re-join every worker with a fresh join token, or rewrite the prefix on both sides before starting (`UPDATE node_creds SET token = replace(token, 'hearth_nt_', 'felucca_nt_')` and `sed -i 's/^hearth_nt_/felucca_nt_/' {data_dir}/node-token`).
- [ ] Tear down the old nftables tables on every worker — the agent creates `ip/bridge/netdev felucca` idempotently but never deletes the old-named ones, so `ip hearth` / `bridge hearth` / `netdev hearth` (and `inet hearth_host` from the old firewall unit) keep matching alongside the new rules until removed: `for f in ip bridge netdev; do nft delete table $f hearth 2>/dev/null; done; nft delete table inet hearth_host 2>/dev/null`.
- [ ] The Grafana dashboard `uid` changed (`hearth-fleet` → `felucca-fleet`), so the import creates a new board; delete the old one and fix any permalinks/alert rules pointing at it.

**Lab migration, 2026-09-12 — what the checklist above missed, found live:**

- `felucca-agent.service` `Requires=felucca-firewall.service` (v4 hardening), so a
  worker upgraded from a pre-hardening install also needs `/etc/felucca/firewall.nft`
  and the firewall unit, or the agent never starts. Over the overlay the control
  plane reaches `:9090` from `10.100.0.1`, so the worker ruleset must allow the
  hub's overlay address as well as its LAN address.
- A direct-mode (non-overlay) worker is admitted only through `knownNodeAddr` —
  its existing node record — once `wg_ip` is configured. Deleting `nodes` rows
  (e.g. to shed pre-hardening ids the conformance normalizer no longer accepts)
  strands such a worker with `invalid addr`; re-seed its record or enroll it with
  a join token.
- Lima `user-v2` DHCP reshuffled the lab again (control plane `.3` → `.1`):
  `node_creds.host` and `nodes.addr` are pinned to the old address and must be
  re-pointed by an operator, exactly as the address-pin design intends.
- `scripts/verify-v2.sh` §8 fetched `/metrics` unauthenticated; it needs the admin
  token since the ADR-0010 amendment. Fixed.
- API-key `prefix` is now **15** chars (`felucca_sk_` + 4) — API-V2 §3b and
  `feluccad/17-tenancy` updated to match; the old `[:14]` literal would have
  silently shortened it to prefix + 3.
- Gates on the migrated fleet: conformance **423/0** (2 documented skips),
  verify-v2 **21/0**, wake p50 ≈ 80 ms.

Two safety nets exist for the upgrade path, both deliberate exceptions to the
"no old name anywhere" rule: `feluccad` **refuses to start** when its database
is absent but a pre-rename `hearth.db` exists at the corresponding old path
(so a skipped state move cannot masquerade as a clean boot), and the
placeholder-token denylist in both binaries keeps the pre-rename lab literal
alongside the new one (it is exactly the agent's minimum token length, so
nothing else would catch it). The console also purges the `hearth_token`
browser-storage key that pre-rename builds wrote.

**On old credentials — verified against `go/internal/server/tenants.go` and
`join.go`:** existing tenant API keys, per-node tokens, and join tokens minted
before the rename **do not** keep validating. The bearer gate in
`go/internal/server/server.go` checks the literal string prefix of every
credential (`strings.HasPrefix(key, nodeTokenPrefix)` for node tokens,
`strings.HasPrefix(key, apiKeyPrefix) && validAPIKeyShape(key)` for tenant
keys, and a literal `"felucca_jt_"` prefix check for join tokens in
`nodeJoin`), and `apiKeyPrefix`/`nodeTokenPrefix` are now `"felucca_sk_"` /
`"felucca_nt_"` — one character longer than the old `hearth_` prefixes.
An old `hearth_sk_…` key or `hearth_nt_…` node token fails the prefix (and,
for tenant keys, the exact-length shape) check before the credential ever
reaches the database lookup, so it is rejected outright — it does **not**
fall back to being treated as an opaque, still-valid secret. Every tenant API
key and per-node token must be rotated (mint new, revoke old) after
upgrading; join tokens are single-use and short-lived (24h TTL) so old ones
were already expected to be replaced.

## v1 — foundation (2026-06-09, Zig)

Two static Zig 0.16 binaries on a three-VM Lima lab (`infra-saas-lab` control
plane + toolchain; `kata-lab-0/1` workers with KVM + Firecracker v1.16):

- `feluccad`: REST API `:8080`, scheduler (ready node, lowest vm_count), node
  registry with 5s heartbeats / 15s down-detection, JSON state persistence
  (atomic tmp+rename), static UI serving, Prometheus metrics.
- `felucca-agent`: `:9090`, drives Firecracker over per-VM unix sockets
  (boot-source/drives/machine-config/InstanceStart), instance dirs under
  `/srv/ignis` with `meta.json` enabling restart adoption.
- Felucca Console UI (vanilla JS SPA): fleet + sandbox views, 3s polling with
  flicker-free in-place updates, mock-mode fallback.
- Research reports on Modal / Northflank / Sprites (`research/`); architecture
  decisions: Zig backend (ADR-0001), **no Kubernetes on the control path**
  (ADR-0002 — sandboxes are not pods; agent drives the VMM directly).
- Lab plumbing: Lima `user-v2` network for inter-VM traffic (Apple vzNAT blocks
  guest-to-guest), nested virtualization for `/dev/kvm`.

## v2 — the hot path (2026-06-10, Zig; verified 16/16)

The features that make sandboxes a product, per the adopted plan:

- **Sleep/wake**: pause → `snapshot/create` (vmstate.bin + mem.bin) → kill FC;
  wake = fresh FC + `snapshot/load` with `resume_vm` — **wake measured 67–85 ms**.
- **Fork**: parent snapshot (reused when sleeping) + reflink/copy of rootfs +
  mem, child restored with its own tap via `network_overrides`, `parent_id` set.
- **Warm pool**: `pool_size` pre-booted paused VMs per agent; matching create
  claims one (keeping the pool `dir_id` so baked snapshot paths stay valid),
  async refill.
- **Guest networking**: bridge `felucca0`, per-VM `hth-<slot>` taps, sequential
  IPs from `net_cidr`, nftables masquerade, `ip=` kernel-arg guest config.
- **Auth**: optional bearer token, constant-time compare; `/healthz`,
  `/metrics`, UI stay open.
- **Production config**: flags > `FELUCCA_*` env > `--config` JSON > defaults;
  nothing hardcoded — the same binaries run on the lab and remote servers.
  `deploy/`: systemd units, installer, config stubs, firecracker-assets script.
- Verification: `scripts/verify-v2.sh` (16 end-to-end checks incl. host→guest
  ping and wake latency).

## Go + Rust migration (2026-06-10, tag `v3.0.0`)

Decision (ADR-0003, supersedes ADR-0001): control plane → **Go** (ecosystem,
future Terraform provider), agent → **Rust** (future uffd/CoW work at the VMM
boundary), wire and on-disk formats frozen.

- **Phase 0**: `test/conformance/` — ~24-case executable contract with semantic
  goldens (key sets, types, null-vs-omitted; never byte order) recorded from
  the running Zig stack. Green vs Zig before any port code ran.
- **Phase 1**: `go/` feluccad, stdlib-only, CGO-free. Solo-verified 78/78 on a
  side port including adopting a copy of the live `state.json` (same node and
  sandbox ids — critical because agents never re-register after success).
- **Phase 2**: `rust/agent/` (tokio/axum/hyper-UDS). Hard adoption gate passed
  on a scratch stack: byte-identical `/v1/vms` view over a Zig-written data
  dir (live pids kept, sleeping VMs intact, `pooled` orphans → stopped), node
  id preserved, a **Zig-written snapshot woken by the Rust agent**. Adoption
  proven in both directions (rollback-safe).
- **Cutover**: Gate A (Go feluccad + Zig agents) and Gate B (full Rust fleet,
  one worker at a time, running guests surviving the swaps) each passed full
  conformance + verify-v2; `backend/` deleted in a revertable commit; repo
  brought under git.
- Findings now encoded in tests/docs:
  - **Mid-boot snapshots are poisoned** — a guest slept ~1–2 s after boot
    panics on resume (clock jump) under every implementation; wake reports
    `running` while FC exits ~1 s later. verify-v2 now pings after wake
    (17 checks).
  - Agent DELETE is idempotent (204 for unknown ids) — encoded in conformance.
  - The Zig agent leaked FC zombies; the Rust agent reaps via detached wait
    tasks.

## v3.1 — vsock exec + fork re-IP (2026-06-10/11; code-complete, scratch-validated)

Contract: [API-V3-EXEC.md](API-V3-EXEC.md). Three components, built in
parallel against the frozen contract:

- **`rust/guest/` → `felucca-guest`** (528 KB static): vsock server on guest
  port 52 — `exec` (timeout, output capture, truncation caps) and `set_ip`
  (iproute2). Installed into the base image as a systemd unit by
  `infra/guest-agent-install.sh`, which also drops the
  `images/.felucca-guest-v1` marker.
- **`rust/agent/`**: attaches a Firecracker vsock device at cold boot when the
  marker is present (`vsock:true` in meta.json, optional field — old metas
  parse unchanged); hybrid-vsock guest client (`CONNECT 52` handshake);
  `POST /v1/vms/{id}/exec` (501 `guest agent unavailable` for pre-v3.1 VMs);
  fork performs best-effort `set_ip` on the child after restore.
- **`go/`**: `POST /api/v1/sandboxes/{id}/exec` proxy with per-request
  timeouts; `felucca_execs_total` metric.

Scratch end-to-end validation (real Firecracker VMs from an injected image
copy; zero live impact): exec on cold-booted guests, exec after sleep/wake
(wake 66 ms with vsock attached), **fork child answering on its own IP with
`eth0` genuinely reconfigured in-guest**, parent unaffected.

Defects found by validating against real Firecracker (not mocks):

- **Stale `v.sock` breaks restore**: sleep SIGKILLs FC, leaving the vsock UDS
  on disk; the next `snapshot/load` fails entirely with
  `VsockUnixBackend: Address in use`. Wake now removes the stale socket first.
  Every production wake of a vsock VM would have failed without this.
- A same-minute intermediate cargo build masqueraded as final (`ls` minute
  precision) — binaries are now compared by sha256, not mtime.

## v3.1 rollout + lifecycle hardening (2026-06-11; lab live — conformance 132/0, verify-v2 21/0)

The live rollout (`scripts/roll-v3.1.sh`: inject → roll agents → roll feluccad
→ goldens → verify) surfaced four defects. All were fixed, re-rolled, and
verified green the same day:

- **start-on-paused destroyed the VM** (user-reported: "paused a microVM,
  couldn't start it back"): `start()` deleted the *live* `fc.sock` and spawned
  a second Firecracker over the same instance dir — the paused FC was orphaned
  forever (resume → connect error from then on) and each retry leaked another
  FC. `start` is now a guarded cold boot: valid only from `stopped`/`error`,
  409 `InvalidState` otherwise, with a pid-liveness check so it can never
  spawn over a live process. `pause`/`resume` got matching guards, feluccad
  forwards agent 409s (with the real reason) instead of a blanket 502, and
  the UI dropped the Start button on paused rows. Regression cases pin the
  path shut (`agent/07-actions`, `feluccad/10-actions`).
- **Firecracker death was invisible**: a guest panic (e.g. resume from a
  mid-boot snapshot) exits FC cleanly, but the VM stayed `running` with a
  stale pid and every subsequent op returned an opaque `FcError: Connect`.
  Every spawn's reaper now flags unexpected exits → `state:error` + warn
  (deliberate kills clear the recorded pid under the lock *before*
  signalling, so they no-op the reaper); a 5 s liveness sweep covers FCs
  adopted after an agent restart; failed spawns reap their half-configured
  FC instead of leaking it; a dead pool VM is retagged to `error` and the
  create falls through to a cold boot.
- **The suites slept mid-boot guests**: post-inject boots are slower (the
  felucca-guest unit), so the cases' create-then-sleep pattern and fixed 3 s
  exec waits became systematic failures — poisoned snapshots (guest panics on
  wake, FC exits) or a guest agent not yet listening. `wait_guest_ready`
  (conformance `lib.sh`) and `wait_exec_ready` (verify-v2) poll
  `exec(["true"])` before sleeping or exec'ing.
- **Fork child cloned the parent's MAC** — exposed only once re-IP worked:
  two bridge ports sharing one MAC flap the FDB and the parent goes dark.
  Fork now gives the child a fresh locally-administered MAC (`0a:68:…`,
  clock salt + tap slot) via plain `exec` before `set_ip`; no guest-protocol
  change.

Operational notes: during the first (pre-fix) conformance record run the
kata-lab-0 Lima VM died at the hypervisor level (`VZErrorDomain Code=3`,
"no longer live"; guest journald stalled ~80 s before death, no panic
captured) — recovered with `limactl stop -f` + start + agent redeploy; not
reproduced after the fixes, with the duplicate-FC pile-up as prime suspect.
Worker agent binaries live in `/tmp` and vanish on reboot — redeploy after
any worker restart. Goldens re-recorded against the fixed stack:
`metrics-names` gains `felucca_execs_total`; new `feluccad/exec` and
`agent/vm-exec` goldens carry real 200 bodies.

## v4 P0 — tenancy, SQLite store, usage metering (2026-06-12; lab live — conformance 166/0, verify-v2 21/0)

First phase of the v4 "multi-tenant, deploy-anywhere, product-ready" plan
([ADR-0004](adr/ADR-0004-tenancy-and-sqlite.md); contract in API-V2 §3b):

- **Tenants + API keys**: `felucca_sk_…` bearer keys (sha256-at-rest, shown
  once, instant revocation); the legacy configured token is now the admin
  credential — pre-v4 deployments work unchanged. Admin-only CRUD under
  `/api/v1/tenants*`; tenant keys get 404 on infra routes.
- **Enforced scoping**: every sandbox route filters by the key's tenant;
  foreign and unknown ids are indistinguishable (404). Fork children inherit
  the parent's tenant. `tenant_id` never appears on the wire (goldens frozen);
  the agent records it in `meta.json` for P1's nftables isolation.
- **Quotas**: per-tenant max sandboxes/vcpus/mem checked at create+fork → 429.
- **Usage metering**: append-only `usage_events` row per lifecycle transition
  (shape + timestamps) from day one; aggregation lands in P5.
- **SQLite (WAL) storage** behind a new `Store` interface (`go/internal/store`,
  pure-Go `modernc.org/sqlite` — static CGO_ENABLED=0 build preserved):
  whole-snapshot transactions at the same granularity as the old JSON persist,
  plus row-level tenants/keys/usage. One-time `state.json` import (renamed
  `*.imported`) — verified live on the lab. DB file is 0600.
- Conformance grew to **166 checks**: new `feluccad/17-tenancy` (scoping,
  cross-tenant 404s, revocation, quota 429) with per-run-unique tenant names,
  a `req_as` helper in lib.sh, and self-cleaning agent cases (a crashed prior
  run can no longer cascade `AlreadyExists` failures into the next).

Operational notes: kata-lab-0's Lima VM died at the hypervisor level again
(`VZErrorDomain Code=3`, second occurrence, both during the agent suite's
rapid sleep/wake/fork) — with the duplicate-FC bug fixed since v3.1, this now
reads as a macOS Virtualization.framework nested-virt limitation, not a Felucca
bug (kata-lab-1 runs the identical binary clean). Documented as a lab
constraint; production bare-metal workers (v4 P2) are unaffected by vz.
Security review notes: tenant-name length capped; quota check has a benign
one-sandbox TOCTOU burst window (row-level counting will close it);
`usage_events` retention is deferred to P5; P1 must allowlist-sanitize
`tenant_id` before any nft shell-out.

## v4 P1 — cross-tenant network isolation (2026-06-12; lab live — conformance 178/0, verify-v2 21/0)

Second phase of the v4 plan
([ADR-0005](adr/ADR-0005-cross-tenant-network-isolation.md); spec in
PLAN-v4 Phase 1): guest-to-guest traffic on a node now drops across tenants
and flows within one, consuming the `tenant_id` P0 started recording in
`meta.json`. All in the Rust agent (`rust/agent/src/net.rs`,
`rust/agent/src/vm/mod.rs`).

- **br_netfilter**: same-subnet guests on `felucca0` talk via L2 switching,
  which bypasses the ip `forward` hook entirely — the agent loads
  `br_netfilter` and sets `bridge-nf-call-iptables=1` so bridged frames
  traverse netfilter and the forward chain can police them.
- **Single concatenated pair set**: one nft set `tenant_pairs`
  (`ipv4_addr . ipv4_addr`, table `ip felucca`) holds every allowed
  same-tenant (saddr, daddr) pair. Forward chain (policy accept): accept
  established/related, accept anything not `felucca0`→`felucca0`, accept pairs
  in `@tenant_pairs`, drop the remaining bridge-to-bridge. Egress NAT and
  host↔guest are untouched.
- **Flush-and-rebuild from full membership** (the `ensure_nat` idiom):
  `refresh_isolation()` snapshots (tenant, ip) under the state lock, releases
  it, rebuilds under a dedicated `isolation_lock` (concurrent
  create/fork/delete can't interleave nft commands). Runs at end of create
  (pool claim + cold boot), fork success (child's post-re-IP address),
  delete, and at startup after reconcile — nft sets don't survive reboot.
- **Security (closes the P0 review requirement)**: `tenant_id` is never
  interpolated into any nft command — it's only a Rust-side grouping key, so
  there is no injection surface; only agent-allocated IPs reach nft.
  `valid_tenant()` (`[A-Za-z0-9_-]`, 1..=64) is defense-in-depth; a
  missing/invalid tenant joins no pair → isolated from all peers
  (fail-closed). Rejected alternative: per-tenant named sets
  `felucca_t_<tenant>` would have put tenant strings in command lines
  (ADR-0005).
- **Caveat**: tenant networks are node-scoped until the P2 overlay —
  per-node CIDRs are node-local, cross-node same-tenant traffic is not
  bridged in v1.

Evidence: conformance **178/0** (12 new checks in `agent/11-isolation`:
cross-tenant ping FAIL / same-tenant ping PASS / egress-to-gateway OK, driven
via exec), verify-v2 **21/0** (wake 65ms), plus a live repro through the full
control-plane path — two tenants created via the admin API, four sandboxes
created with tenant keys all scheduled onto one node: cross-tenant ping exit 1
(dropped), same-tenant ping exit 0, all deletes 204.

## v4 P2 — WireGuard overlay, node join, TLS, systemd fleet (2026-06-12, Go+Rust)

Workers now join feluccad from anywhere: hub-and-spoke WireGuard overlay with
feluccad as hub. Step commits `bb182f3` (hub: wg config, one-time join tokens
sha256-at-rest TTL 24h, `/api/v1/nodes/join` with consume-LAST semantics so no
failure burns a token), `7ac7ead` (agent `--join`: enroll once, persist
`wg.json`, every boot brings the tunnel up and registers over the overlay),
`fdeb995` (in-binary TLS via autocert on :443/:80 when `tls_domain` is set —
plain listener stays for in-tunnel agents; agent https join via
`curl --config -` so the token never hits argv), `ee54a9f` (conformance
`feluccad/18-join` + live lab overlay), `f2eb5a1`+`80dcc08` (systemd units
validated by running the real fleet under them). Contract: API-V2 §3c; design
+ deferred items: ADR-0006.

Security handled inline (per working agreement, no separate review step):
join tokens single-use + peeked-then-consumed-last under `joinMu`; uniform
401s (no token-state oracle); credential-shape auth checked before overlay-off
503 (no fingerprinting); secrets only on stdin/0600 files, never argv;
enrolled nodes never silently degrade to direct mode (unreadable/corrupt
`wg.json` is fatal — found live when the systemd capability sandbox, which
drops CAP_DAC_OVERRIDE, couldn't read a wrong-owner state file).

systemd migration findings, all unit-encoded now: Firecracker needs `@sandbox`
(it installs its own seccomp(2) filters; died SIGSYS without it) and
`/dev/net/tun`; `ReadWritePaths` entries must pre-exist (226/NAMESPACE);
`StateDirectoryMode=0750` pinned (default re-loosens to 0755 every start);
`modules-load.d/felucca.conf` preloads `br_netfilter`+`wireguard` because
`ProtectKernelModules` forbids the services doing it (load-bearing for P1
isolation). kata-lab-0 vz crash #4 self-healed via unit auto-start on VM
restart — the old re-stage runbook is obsolete.

Evidence: conformance **193/0** (15 new checks in `feluccad/18-join`: mint /
validation ladder / admin-only / uniform 401s / 400-doesn't-burn-token),
verify-v2 **21/0**, both against a mixed fleet running entirely under the
hardened units — kata-lab-1 enrolled as `10.100.0.2` over the tunnel
(sandbox create+exec through it), kata-lab-0 direct. cargo 77/0, go tests
green, static builds intact. 48h soak started 2026-06-12 ~11:20 CEST;
remaining for phase close: soak verdict (MDWE decision) + P2.6 mixed-fleet
acceptance on real external infra (public host, domain, DNS-01) + release
binaries for both arches.

## v4 P3 — sandbox ingress (2026-06-12, Go+Rust)

Services inside sandboxes are now reachable by URL: `https://<name>--<id>.
<domain>` through the new `felucca-gw` reverse proxy. Step commits `ff3cdc3`
(feluccad expose API + worker DNAT) and `653e2e2` (felucca-gw). Contract:
API-V2 §3d; design + deferred items: ADR-0007.

- **Expose API**: POST/DELETE `/api/v1/sandboxes/{id}/expose[/{name}]`,
  multi-service per sandbox; one route row + one worker nft DNAT rule
  (the `ingress` chain — nat hook prerouting — in the existing `ip felucca`
  table, node ports 20000-29999, flush-and-rebuild from meta.json like
  isolation — same nothing-user-controlled-near-nft invariant). Forks re-expose the child
  with fresh node ports.
- **felucca-gw**: route table from feluccad (admin-only `GET /api/v1/routes`,
  cached, outage-tolerant), WebSocket passthrough, 503 wake page for
  sleeping sandboxes, dynamic `8069--<id>` port-in-hostname as a lazy
  expose gated by the per-sandbox `allow_dynamic_ports` opt-in (canonical
  decimal labels only), per-tenant edge toggles + token-bucket rate limits.
  Ships with a hardened systemd unit (felucca-gw.service).
- **Review loop** (pre-commit) caught and fixed: both snapshot stores
  silently dropping all ingress state on feluccad restart (SQLite columns +
  idempotent ALTER migration; state.json loadSandbox fields); a
  concurrent-expose 409 path leaking unaccounted agent DNAT entries and a
  shared-port unexpose TOCTOU (both fixed by serializing ingress mutations
  end-to-end); 30s browser hangs on stale routes (3s connect timeout);
  leading-zero dynamic labels as an unauthenticated feluccad-amplification
  loop (canonical-spelling gate); node-less sandboxes vanishing from the
  route table instead of serving state-aware 503s. Deferred to ADR-0007:
  feluccad↔agent expose reconciliation after failed deletes, dynamic-expose
  GC (P5 idle policies), wildcard TLS (external infra), auto-wake (P5).

Evidence: conformance **227/0** (new `feluccad/19-expose`, +34 checks:
validation ladder, scoping, idempotency/conflict, admin-only route table,
dynamic gate, unexpose), verify-v2 **21/0**, cargo 87/0, go suite green.
Live e2e through the gateway on the systemd fleet: HTTP + a real WebSocket
101-and-frames exchange (gw → kata-lab-0 DNAT → guest python responder);
dynamic-port route served from the **overlay worker** (gw → wg tunnel →
kata-lab-1 DNAT → guest); sleep → wake page → wake → content recovered;
fork child's URLs served the memory-cloned parent httpd on fresh node
ports; deletes flushed every DNAT rule on both workers.

## v4 P4 — templates & bigger guests (2026-06-12, Go+Rust)

Sandboxes now boot from custom rootfs images with bigger shapes (caps
16 vCPU / 32 GiB / 128 GB disk). A template is a catalog row + one image on
feluccad; capture IS the build pipeline; workers pull-and-cache
sha256-addressed. Contract: API-V2 §3e; design + deferred: ADR-0008.

- **Template entity + capture**: `POST /api/v1/templates` captures a
  stopped sandbox's rootfs (streamed agent→feluccad, hashed in flight,
  O_EXCL'd partial as the per-image capture mutex) or registers a
  pre-provisioned image; `disk_gb` is floored at the recorded
  `image_size_gb` so the agent's grow-only resize can never be asked to
  shrink — and disk quota can't be under-counted by big images. Catalog
  is tenant-visible; mutations + raw image downloads are admin/node only.
- **Create-by-template**: image + default shape from the row, explicit
  overrides win; the worker pulls missing/stale images from
  `GET /api/v1/images/{name}` (curl-on-stdin token, per-image locks,
  verified-sidecar-before-image rename order, self-healing no-sidecar
  cache) and grows the rootfs (`truncate` + `e2fsck -fp` + `resize2fs`).
  Per-tenant `max_disk_gb` quota enforced at create/fork.
- **Per-template warm pools**: `PUT /v1/pools` replaces a node's specs;
  the refill loop tops up AND drains (deleted/re-captured templates lose
  their pooled VMs — claims match the full shape INCLUDING sha, so a
  re-capture can never serve a stale rootfs); prewarm failures are
  deleted + backed off 300s. Specs are pushed on template changes and to
  every node right after register (the register response stays the frozen
  `{"id"}`).
- **Pipeline**: `scripts/build-template.sh` (builder boot → base64-chunked
  script upload → nohup+poll run, immune to the 5-min exec cap and the
  guest's noexec /tmp → stop → capture → delete) + provision scripts
  `deploy/templates/docker-base.sh` / `odoo-v18.sh`.
- **Review loop** (pre-commit) caught and fixed: pools that only ever grew
  (teardown was documented but unimplemented — and the executor's drain
  machinery was written + unit-tested yet never wired into the loop:
  release-build dead-code warnings exposed it); pooled VMs served stale
  after template re-capture (sha matching); a 5s prewarm-failure loop
  leaking an Error VM + instance dir per tick; one global download lock
  blocking all creates behind a multi-GB pull; rename-before-sidecar
  pinning a stale image forever after a crash; `stop` returning before FC
  exit (capture could stream a dirty rootfs) + a capture-vs-start race
  (drop-guarded capture registration); create requests cancelled by
  feluccad's 30s timeout leaving records stuck in `creating`
  (spawn-shielded); feluccad's global 60s WriteTimeout killing >60s
  capture/image transfers mid-stream (found live — per-route
  `ResponseController` deadlines); a store error tearing down every pool
  (error-aware spec push); the admin token on build-template.sh's curl
  argv (config-on-stdin); disk_gb=0 meaning "2 GiB" to Go but "image
  size" to Rust (the image_size_gb floor closes the 6× quota bypass).
- **Guest-environment findings** (docker-base e2e, all fixed in the
  provision/pipeline scripts): guest `/tmp` is noexec (run scripts via
  `sh`); the base image ships a 1-byte resolv.conf pointing nowhere
  (rm + rewrite, never `[ -s ] ||`); the FC guest kernel has legacy
  xtables but no nf_tables (pin `iptables-legacy`) and no `raw` table
  (docker bridge networks need `gateway_mode_ipv4=nat-unprotected`; the
  template pre-bakes a `felucca` network and compose project networks
  inherit the mode via `default-network-opts`).
- **Lab churn**: kata-lab-0 vz crashes #5–#7 mid-suite (heavier P4 I/O);
  all healed by `limactl stop -f` + `start` with zero manual staging.
  Post-crash orphan churn surfaced a latent quirk: snapshot-restored
  guests (fork children) don't answer host ARP until their first
  transmit — ping-only, self-healing, exec/vsock unaffected; gratuitous
  ARP from felucca-guest after re-IP is the P5/P6 candidate fix.

Evidence: conformance **266/0** (new `feluccad/20-templates`, +39 checks:
validation, capture incl. running/duplicate/in-progress 409s, catalog,
create-by-template with a marker file proving the child booted from the
captured image after a cross-node pull, disk floors, tenant scoping, disk
quota 429, cleanup), verify-v2 **21/0** (clean-node run; one
crash-recovery + rerun per the runbook), cargo **117/0**, go suite green,
both static binaries link — all on the final binaries. Live repro on the
systemd fleet: `docker-base` built through the public API
(`build-template.sh`: 6 GB builder, in-guest apt provision, stop→capture
streaming past the old 60s limit), then a sandbox created from it ran
`docker run hello-world` (image pulled from Docker Hub through guest NAT)
and a `docker compose` busybox httpd — which was then exposed via the P3
ingress and served through felucca-gw: the miniature Sodoko smoke
(template → compose stack → public URL) passes end-to-end.

## v4 P5 — streaming exec, lifecycle policies, usage, TS SDK (2026-06-13, Go+Rust)

Phase 5 of PLAN-v4: the features that make sandboxes serverless and
integrable, plus the deferrals parked by earlier phases.
Design: [ADR-0009](adr/ADR-0009-streaming-lifecycle-usage.md).

- **Streaming exec** (`?stream=1` → SSE): one wire shape translated at each
  hop — the guest's vsock exec gains `"stream":true` and replies with NDJSON
  frames (`{"stream":"stdout|stderr","data"}` chunks then a terminal
  `{"done":true,...}`); the agent passes them through as chunked
  `application/x-ndjson`; feluccad relays each as one SSE `data:` event,
  flushed immediately. Buffered exec is byte-frozen. Stream cap 16 MiB/stream
  (vs buffered 1 MiB); guest timeouts surface as exit 124; a relay cut yields
  a synthetic `"stream interrupted"` done frame.
- **Lifecycle policies**: per-tenant `default_idle_sleep_s` /
  `default_asleep_delete_s`, per-sandbox `idle_sleep_s` / `asleep_delete_s`
  (0 = inherit, −1 = disabled). A 15s feluccad sweep auto-sleeps idle running
  sandboxes (no exec/ingress/wake) and auto-deletes sleeping ones past their
  TTL. The activity clock is fed by create, fork (parent AND child), wake,
  both exec arms, and gateway ingress reports (`POST /routes/activity`,
  batched per refresh tick).
- **Auto-wake (felucca-gw)**: a request for a sleeping sandbox wakes it and
  waits ≤15s (singleflighted per sandbox id — a burst is one wake, not N) for
  the route to go running; default on, per-tenant `auto_wake` override. Plus
  the ADR-0007 deferral: auto-sleep GCs the sandbox's dynamic (all-digit)
  exposes; auto-wake re-ensures them transparently.
- **Usage**: `GET /api/v1/tenants/{id}/usage?from&to` folds `usage_events`
  into sandbox/vCPU/mem-GiB/disk-GB hours + exec/event counts (admin any
  tenant; a tenant key its own only). Each exec appends an `"exec"` event.
  Retention prunes ONLY `exec` rows hourly (default 90d) — transition events
  are the interval skeleton and are kept.
- **Per-template tenant visibility** (ADR-0008 deferral): `POST /templates`
  takes `"tenant"`; the catalog filters to public + own for tenant keys, and
  foreign create-by-template 404s as "unknown template".
- **Worker image-cache GC** (ADR-0008 deferral): the agent's refill loop
  every ~10 min drops cache images referenced by no VM/pool spec, older than
  1h, re-checked under the per-image download lock.
- **Gratuitous ARP** (P4 deferral): after a fork re-IP the guest fires one
  throwaway UDP datagram at the gateway so snapshot-restored children answer
  host ARP immediately (no more first-transmit ping gap in verify-v2).
- **TS SDK** (`sdk/ts/`, `@felucca/sdk`, zero-dep): create/exec/execStream/
  sleep/wake/fork/expose/templates/tenantUsage; node:test against an
  in-process fake feluccad (8/8). `scripts/felucca-verify.sh <endpoint>` runs
  the conformance suite against any deployment.

Review-loop findings, fixed before commit: (1) the streamed path lossy-decoded
UTF-8 per 8 KiB read, mangling multibyte chars at chunk boundaries — fixed
with a per-stream carry of incomplete sequences (`emit_utf8`, unit-tested with
split `€` and a 12 KiB all-`€` stream); (2) per-tenant `/metrics` gauges would
have leaked the tenant inventory on the unauthenticated metrics endpoint —
removed, per-tenant data stays behind the authed usage endpoint; (3) usage
retention pruned by raw `ts`, which would delete the `created` anchor of a
long-running sandbox and silently zero its hours — now prunes only `exec`
rows; (4) the exec hot path did a synchronous sqlite write under the global
state lock — the event is now built under the lock and appended after
release; (5) gateway auto-wake had no singleflight (wake/route-fetch storm
under a burst) — coalesced per sandbox id; (6) `ListUsage` loaded the tenant's
whole exec history per query — bounded to in-window exec rows.

Gates: cargo (guest UTF-8 + stream tests added), go vet/test green incl. new
`lifecycle_test`/`usage_test`/`exec_stream_test` (17 new server tests), static
feluccad+felucca-gw link; fleet rolled under systemd; conformance **326/0**
(new `feluccad/21-stream-exec`, `22-lifecycle`, `23-template-visibility`; +60
checks over P4's 266), verify-v2 **21/0** (fork-child ping checks pass — the
gratuitous-ARP fix working); live repro: streamed exec of a multi-write command
returned ordered stdout/stderr frames + a `done` frame with the exit code
through feluccad's SSE relay. kata-lab-0 vz crashes #8 and #9 mid-suite (P5
conformance now boots+captures+sleep/wakes VMs across three cases — `20`,
`22`, `23` — much heavier I/O than P4) — each healed by `limactl stop -f` +
`start`, zero staging; every crash-run failure was `000`/empty-body from the
dead hypervisor, never an assertion mismatch, and the recovered re-run was a
clean 326/0. Deferred
(ADR-0009): exec stdin/PTY, `felucca` CLI binary, authenticated metrics,
per-sandbox policy PATCH, SDK npm publish.

## v4 P6 — observability, bench, HA groundwork (2026-08-09, Go+Rust)

Phase 6 of PLAN-v4 — the post-launch operability phase.
Design: [ADR-0010](adr/ADR-0010-observability-and-bench.md).

- **Structured logging**: feluccad + felucca-gw on Go `slog` (text handler,
  stderr, journald-friendly), the agent on Rust `tracing`
  (`tracing-subscriber` fmt, `RUST_LOG` filter, default info). Core
  message phrases preserved (grep for the key word still matches), but exact
  formats changed — colon-suffixed greps like `persist:` must drop the colon
  (migration note in DEPLOYMENT §11); values moved to structured fields with a fixed vocabulary (`err`,
  `sandbox`/`vm`, `tenant`, `node`, `image`, `request_id`, ...). The guest
  deliberately stays on `eprintln!` (serial console, binary size).
- **Request IDs**: feluccad mints `req-<hex>` per `/api/` request, echoes it
  as `X-Felucca-Request-Id`, forwards it on every agent proxy call; the agent
  logs it. Background actors mint `sweep-`/`bg-` ids. Headers only —
  mixed-fleet safe (pre-P6 agents ignore it).
- **Metrics**: three hand-rolled histograms on the open `/metrics` —
  `felucca_{wake,exec,create}_duration_ms` (ms buckets 5..30000,+Inf,
  feluccad wall-clock, always-emitted so scrapes are shape-stable). Legacy
  wake counters kept. Per-tenant gauges return — behind the
  **authenticated** `GET /api/v1/metrics/tenants` (admin token; 404 to
  tenant keys), resolving ADR-0009's leak concern with auth instead of
  feature removal. Grafana dashboard + two-job scrape config in
  `deploy/grafana/`.
- **Bench**: `scripts/bench.sh <endpoint> [token]` — p50/p95/min/max over N
  runs for create-cold, create-claim, exec-buffered, exec-stream
  first-frame, wake (API wall-clock and agent-reported `wake_ms`), fork;
  self-cleaning, per-op failure counts; markdown table output published in
  [BENCHMARKS.md](BENCHMARKS.md).
- **HA groundwork** (docs/HA-GROUNDWORK.md): honest readiness assessment —
  what the Store boundary already buys, the real blockers (in-memory
  working set, snapshot-granularity persistence, singleton wg hub), the
  staged litestream → Postgres → N×feluccad path with effort estimates.
  No code; litestream documented as deployable-today insurance.
- **uffd CoW fork research** (research/uffd-cow-fork.md): the "branch a
  live Odoo system" track — FC uffd-backed snapshot load, per-lineage
  page-fault handler, parent-frozen semantics, bounded prototype scope.
  Implementation deferred until bench data shows fork latency blocking a
  product use.

Conformance grows `feluccad/24-observability` (histogram presence, the
no-tenant-series-on-open-metrics leak guard, the admin-surface auth ladder,
request-id minting — deliberately boots no VMs).

Review-loop findings, fixed before commit: (1) fatal-exit and the
CROSS-TENANT-ISOLATION-NOT-ENFORCED diagnostics were routed through the
RUST_LOG filter and could be silenced — now emitted unconditionally to stderr
(`fatal()` helper, dual-emit for the isolation warning); (2) 401 responses
carried no request id (minted after auth) — minting moved before the auth
gate; (3) the SDK's SSE-frame parse errors lost the request id — threaded;
(4) trace-ID minting deduplicated into `newTraceID`, the wire header name
into `agentclient.RequestIDHeader`; (5) the phrase-preservation claim above
corrected to match reality. Deferred to ADR-0010: context-scoped logger
(request_id on every request-path line), context-based propagation instead
of the string param, bench.sh op dedup.

P6 began with an unplanned validation: the whole lab fleet had been STOPPED
for ~2 months (2026-06-14 → 2026-08-09). On `limactl start` of all three
VMs, every systemd unit self-started, both agents re-registered, and the
fleet reported ready with zero manual staging — the strongest proof yet of
the P2.5 reboot-survivability work. The cold boot also surfaced a known-class
gap worth naming: feluccad's sandbox states (13 `running` rows) had diverged
from the agents' ground truth (`sleeping`/`stopped` after adoption) — feluccad
never re-reconciles against agents after a restart. Left as-is (user
sandboxes among them; wake still works from the agent's real state); the fix
belongs to the backlog's reschedule/reconcile sweep, not P6.

Gates: cargo **121/0 agent** (zero warnings — nothing unwired) + guest
unchanged (41/0 from P5), go vet clean + full suite green (new histogram/
admin-metrics unit tests), static binaries link; fleet
rolled; conformance **342/0** (new `feluccad/24-observability`, +16 checks;
metrics-names golden re-recorded for the histogram series), verify-v2
**21/0** — after a genuinely instructive failure: the first two runs failed
4/21 on host→guest pings because Lima's user-v2 DHCP had SWAPPED the two
workers' 192.168.104.{1,4} addresses across the 2-month stop. The PRODUCT
self-healed (agent advertise auto-detect re-registered the new addrs;
exec/scheduling/conformance never noticed) — only the test harness's
hardcoded addr→VM map broke, silently pinging from the wrong machine.
verify-v2 now resolves workers by registration hostname (lima-<vm>) and
conformance-lab.sh resolves the agent-suite target at run time; nothing in
the lab tooling hardcodes those DHCP addresses anymore. Because the passing
verify run happened to schedule onto kata-lab-0, the full failing sequence
(fresh-boot / post-wake / fork-child / parent-after-fork pings) was then
re-proven directly against kata-lab-1's agent: all four pass (wake 59 ms) —
the node's data path was never broken. kata-lab-1 also
wedged once during diagnosis (its first-ever vz crash — all prior nine were
kata-lab-0) — stop -f + start healed it, zero staging. Live repro: one
request id followed from an API call through feluccad's journal into the
owning agent's journal; bench table in BENCHMARKS.md. Zero vz crashes under the bench's ~90 VM
operations; one kata-lab-1 wedge during the verify diagnosis (above).

## v4 security hardening (2026-08-29, Go+Rust) — successive adversarial rounds

Several review passes over the shipped v4 surface. What makes this entry worth
reading is that **two of them found a vulnerability introduced by the previous
round's remediation**, so the notes below record the ordering constraints, not
just the features.

**Auth, and what a credential reaches**

- Both binaries refuse to start on an empty, placeholder (`REPLACE_WITH…`,
  `felucca-lab-token`) or short token — 32 chars feluccad, 16 agent. An empty
  token authorizes nobody. Open mode is only the named `feluccad
  --insecure-no-auth`, WARN'd at every start and again on a non-loopback bind.
- `bind`'s host part is honoured (it used to be discarded, so a configured
  `127.0.0.1:8080` still served the world). A bind feluccad cannot honour
  literally is fatal; an overlay-unreachable bind is a loud startup WARN.
- `/metrics` is admin-gated. Tenant keys get 404, not 403.
- **Per-node agent credentials, both directions.** Enrollment with a one-time
  join token mints `felucca_nt_…`, keyed on the node's `host:port`. feluccad
  presents it dialing that node — structurally, through a `nodeDial` whose only
  constructor derives the credential from the node's identity, so no call site
  can put `cfg.Token` on the wire (the earlier procedural rule was obeyed at
  nine sites and missed at the two in the lifecycle sweep, shipping the fleet
  admin key to every node on a 15s timer). The agent persists it to
  `<data_dir>/node-token` 0600, refuses to start if it is readable more widely,
  presents it on register/heartbeat, and accepts it inbound alongside the shared
  token — with no fallback back to the shared token, or one forced failure would
  downgrade the agent into handing over the fleet key.
- **A node is its own principal, not a tenant string.** Node credentials reach
  an allowlist of two routes (register, heartbeat) and 404 elsewhere; within
  them they are bound to their own address, hostname and node id and may not
  spend a join token. Modelling a node as a tenant id — where admin is `""` —
  would have put it one typo from being a second admin.
- **Re-join proof of possession**: a join token authorizes *an* enrollment, not
  a *specific node*, and the pubkey is caller-written, so re-joining an enrolled
  pubkey without that node's current `node_token` is a 409. Recovery is a fresh
  WireGuard key.
- Grandfathering is bounded to **once per database** (`node_creds_seeded`), so
  "this node has no credential yet" is not a state an attacker can create by
  registering an address.
- Node authentication costs no store read (in-memory sha256 index); the
  remaining pre-auth store reads are bounded at 4 in flight, because
  `store/sqlite.go` caps the pool at one connection.

**The brute-force guard, and its two regressions**

- Failed credentials are counted per source address on both credential gates.
  Past 10 failures the wait doubles (1s, 2s, 4s…), capped at 60s — or **2s**
  when the key provably stands for many clients, which the shipped
  loopback-behind-Caddy topology always does.
- **A later round's finding, in one line: the credential must be evaluated
  BEFORE the backoff.** Ordering it the other way — the shape an earlier
  remediation had —
  was a global unauthenticated denial of service: `gateAuth` runs on every
  `/api/` path, the shipped config declares no trusted proxy so every client
  shares one `127.0.0.1` key, and each failure refreshed the decay clock. One
  anonymous client sending a bad bearer every ~10s held the entire control
  plane, operators and worker nodes included, in a 429 that never expired. A
  correct token is now always served.
- **And the round after that: a success must NOT clear the record.** A wipe-on-success
  is reachable by anyone sharing the key, so the console's own authenticated
  poll reset whatever a guesser behind the same proxy had accumulated. Only
  quiet time forgives now — one failure per 15s.
- The guard's own eviction is ranked (unpenalized first, then lowest count) so a
  spray across forged sources evicts itself rather than flushing established
  records.

**Forwarded client addresses — closed by configuration, not code**

- `X-Forwarded-For` is honoured only from peers inside `trusted_proxies`
  (default **empty**); the chain is walked right-to-left, bounded to 16 hops and
  never split whole (a ~1 MiB run of commas used to build a ~500k-element slice
  per unauthenticated request); a malformed CIDR is fatal at startup.
- `X-Real-IP` got its own opt-in, `trust_x_real_ip` (default **false**), because
  it carries no chain of custody: feluccad cannot tell a value the proxy wrote
  from one it forwarded verbatim. Setting it without `trusted_proxies` is a
  startup failure.
- `MaxHeaderBytes` dropped from Go's 1 MiB default to 16 KiB — the XFF parse is
  on the pre-auth path.
- **The residual is not closable in code.** feluccad cannot distinguish a header
  its proxy *wrote* from one the proxy *forwarded*. An edge proxy listed in
  `trusted_proxies` must overwrite `X-Forwarded-For`
  (`header_up X-Forwarded-For {remote_host}` / `proxy_set_header
  X-Forwarded-For $remote_addr`); nginx's bare `proxy_pass` forwards the
  client's own header verbatim, and the commonly-copied
  `$proxy_add_x_forwarded_for` appends rather than replaces. DEPLOYMENT.md §7.1
  is now written as a requirement with an operator-runnable check.

**Everything else operator-visible**

- Unguessable ids (104 bits from `crypto/rand`), a 1 MiB body cap ahead of every
  route including the pre-auth join route, bounded display names, API-key
  expiry + listing, agent listener off the wildcard by default, host-side
  guest→host fences the agent refuses to serve without.

Docs: DEPLOYMENT.md §6.2 (throttle), §6.7 (per-node credentials), §7.1
(forwarded addresses), API-V2.md §6, ARCHITECTURE.md "v4 security hardening",
ADR-0004 and ADR-0006 amendments. Three previous doc passes each left a stale
claim behind — most recently "a throttled source is refused even with the
correct token", which survived a sweep that edited the paragraph around it — so
this round re-verified every authentication, throttling and 429 statement in
`docs/` against the source rather than against the previous text.

## Stability wave 0 — worker restarts (2026-11-26, Rust + units)

First wave of the 2026-11 stability audit: the three findings that each took
down **every sandbox on a worker** in one ordinary event. Decisions and the
alternatives that lost: [ADR-0011](adr/ADR-0011-worker-restarts.md).

**Sandboxes outlive the agent (FLU-1, audit F-01)**

- `felucca-agent.service`: `KillMode=process`, `Restart=always`,
  `StartLimitIntervalSec=0`. Restarting, stopping, upgrading or crashing the
  agent no longer kills the Firecrackers in its cgroup, and the agent always
  comes back.
- Start-up takes over what is running instead of assuming nothing is
  (`vm/procscan.rs`, `vm/reconcile.rs`, `Manager::reconcile`):
  - a record keeps its VMM only if the recorded pid is positively that
    instance's Firecracker (owner, argv, canonical socket directory) — not
    merely alive;
  - warm-pool VMs are adopted as pooled (they used to be written off as
    `stopped` with the pid dropped: one leaked Firecracker and tap per pool VM
    per restart once restarts stopped killing them);
  - the Firecracker of a create that never completed is killed and its
    directory removed; a VMM beside a `meta.json` that does not load is left
    running and reported (its network slot stays reserved when the file is
    still valid JSON);
  - a Firecracker beside a record that says asleep or stopped — a wake or
    start that worked and was never written down — is asked: a running guest
    is adopted and the record corrected, anything else is killed. Killing a
    guest that was already awake left `sleeping` over a snapshot its disk had
    outrun;
  - each adopted VMM is asked for its run state (`GET /`) and brought back to
    its record — a sleep or fork cut short used to leave a VM listed `running`
    and frozen, with `resume` a no-op. All are asked at once (two seconds in
    total); one that does not answer, because it is still writing a large
    snapshot, is asked again every five seconds, and `pause`/`resume` do not
    answer "already so" for it meanwhile;
  - taps of adopted pool VMs are re-pinned; a tap that cannot be pinned is
    taken down, not deleted (deleting it cut a live guest's network for good);
    when isolation cannot be established the guest bridge is taken down before
    the agent exits, and nothing raises it again until a start has verified
    the fences (`net::raise_bridge`, one call site).
- `{data_dir}/agent.lock` is taken before anything else at start-up; an agent
  that does not get it exits. A second agent started by hand used to run all
  of start-up against the live agent's sandboxes before failing to bind.
- `meta.json` is replaced by rename, never rewritten in place, and fsynced
  (file and directory). A write that fails or is interrupted is owed and
  repeated from the liveness loop every five seconds.
- Every lifecycle request runs on its own task, so a caller that hangs up no
  longer cuts a sleep, wake, stop, start, pause, resume or delete in the
  middle. `sleep` and `stop` change the record and signal the Firecracker in
  one step; a stop is written to disk (it was not: a stopped sandbox came back
  `sleeping` after a restart whenever an old snapshot was in its directory,
  and `start` then answered 409).
- `delete` ends any Firecracker still serving a socket in the directory before
  it removes it, and fails (409/500, so feluccad keeps its record) rather than
  leave one running where no later start can find it.
- The liveness sweep, `stop` and `sleep` go by what a pid is, and wait for a
  killed Firecracker to have fully exited (every thread), which is when its
  tap is free.
- Template-pool VMs that survived the restart are not drained before feluccad
  has pushed the pool specs to the new agent.
- Also closed by this: FLU-44 (audit F-43, "reconcile adopts any live pid and
  can later kill an unrelated process").
- Removed from the backlog: "pool-orphan reclaim".
- New lab gates: `scripts/verify-restart.sh`, and `scripts/verify-break.sh`
  for the scenarios around it (agent killed every 3 s under load, fifteen
  sandboxes and double restarts, a full disk, every nft table deleted, the
  power-off, a binary rollback under the new unit, a record that cannot be
  written, an agent that dies the instant a create is committed).
  `test/lab/repro-flu15.sh` and `test/lab/repro-flu70.sh` reproduce two open
  issues found on the way.

**One OOM kill takes one sandbox (FLU-2, audit F-19)**

- `felucca-agent.service`: `OOMPolicy=continue` (the default `stop` stopped the
  unit, and with it every sandbox, when the kernel OOM-killed any one
  Firecracker) and `ManagedOOMPreference=omit` (systemd-oomd kills whole
  cgroups).
- The agent sets `oom_score_adj` 300 on every Firecracker it spawns or adopts
  and stays at 0 itself, so the process the kernel takes is a VMM — one
  sandbox — and not the agent or a host daemon. No `OOMScoreAdjust=` on the
  unit, on purpose (ADR-0011); `scripts/verify-units.sh` refuses one.
- New lab gate: `scripts/verify-oom.sh` (runs the unit's cgroup out of memory
  with a `MemoryMax=` drop-in; the same script against the previous unit
  reproduces the outage).

**Warm-pool VMs no longer pile up across restarts (FLU-29, audit F-26)**

- Was: every agent restart left the previous pool VM behind as a `stopped`
  record nothing deletes, and prewarmed a new one. With the changes above a
  pool VM whose Firecracker survived is adopted; one whose Firecracker is
  gone (host reboot) is now removed at start-up — record, slot, tap and
  rootfs copy.
- A pool VM whose Firecracker dies while the agent is running is removed the
  same way (it used to stay as an `error` record nothing deletes), and its
  pool is not refilled for five minutes. Never-claimed pool records found in
  `error` or `stopped` at start-up are removed too.
- Directories that never got a `meta.json` are removed at start-up, and
  `meta.json` is written by rename, so new empty ones cannot appear.
- Deliberately **not** removed: a directory whose `meta.json` exists and does
  not load. It may hold a tenant's disk; the agent reports it and leaves it.
  Empty-`meta.json` directories left by older builds need one manual clean-up.

**One place starts a Firecracker (FLU-57, audit F-54)**

- Create/start, wake and fork each carried their own copy of the spawn, and
  each copy built stdout and stderr from one raw descriptor, so `serial.log`
  was closed twice per VM — in a multi-threaded runtime the second close can
  hit a descriptor another thread was just given. `Manager::spawn_firecracker`
  is now the only spawn site; the two streams each own a descriptor
  (`try_clone`), and the `unsafe` is gone. A structural test keeps it at one.

**A pool teardown no longer deletes a sandbox it was just handed out (FLU-60, found during this wave)**

- Reproduced on the lab with the previous build: remove a template pool
  (`PUT /v1/pools []`) while creates of that shape arrive, and 6 of 6 creates
  came back `201`, listed `running`, with `rootfs.ext4`, `meta.json` and both
  sockets deleted. The drain deleted pool VMs by ids it had listed a moment
  earlier; a VM claimed in between answers to a new id but keeps its pool
  directory, and `delete` of an id nobody has removed the directory of that
  name.
- The drain now confirms under the state lock that a VM is still pooled, and
  makes it unclaimable in the same step, before deleting it. `delete` of an
  id no record has refuses to remove a directory another VM lives in — which
  also covers `DELETE /v1/vms/pool-…` for an id that was claimed earlier.
  `delete` of a pool VM that is still pooled takes it out of the claimable set
  in the critical section that reads it, so a claim cannot land in between.

**Firewall changes no longer touch the agent (FLU-3, audit F-20)**

- `felucca-agent.service`: `Requires=felucca-firewall.service` → `Wants=` +
  `After=` + a start gate of two `ExecStartPre=` steps that look in the kernel
  for the guest fence itself (`iifname "felucca0" drop` in chain `input` of
  `inet felucca_host`), not for a unit that once ran and not for a chain that
  merely exists. Step 1: fence missing → `systemctl reload-or-restart
  felucca-firewall` (after a ruleset flush that unit still says `active`, and
  nothing else would load the rules again). Step 2: fence still missing →
  take `felucca0` down, log why, fail the start. The start is retried every
  5 s and every retry runs step 1, so a repaired rules file is picked up with
  no reload by hand; the agent raises the bridge again once it is up.
  Restarting the firewall unit no longer restarts the agent; a firewall unit
  that fails no longer leaves the agent down with no retry.
- `felucca-firewall.service` loads the rules as one nft transaction (create,
  delete, include the file, require the input chain), identical for `start`,
  `reload` and `restart`. A file that does not load changes nothing.
  Stopping the unit leaves the rules in force. Its one `ExecStop=` acts only
  while the host is shutting down: it then ends the sandboxes, so that none is
  still running when another unit unloads the rules (the distribution's
  `nftables.service` flushes the ruleset when it stops; since `KillMode=process`
  the Firecrackers would otherwise live until systemd's last step). Ordered
  after `nftables.service`. `StandardInput=data` is spelled out.
- `deploy/install.sh`:
  - keeps an existing rules file byte for byte. Only a **worker** run that
    names the control plane (`FELUCCA_CONTROL_PLANE_IP`) writes it again; the
    control-plane role no longer reads the variable at all;
  - takes a **list** of literal addresses, checked before anything is
    installed. Refused, with nothing changed: anything that is not a literal
    address, a `/prefix`, and a value that names nobody (`,`, or loopback
    only) — which used to become a loopback-only allow-list;
  - checks a kept file before applying it again (it must load, and on a
    worker it must have the fence line); dry-runs a new one, creates it
    `root:root 0640`, installs the unit first, replaces by rename, applies
    with `reload`; on a failed apply puts the old file back, applies it again
    and exits non-zero;
  - leaves the rules file root's on both roles (the control-plane ownership
    pass used to hand it to the service user between two steps);
  - exits non-zero when `nft` is missing (was: a warning);
  - never restarts a running agent, and no longer says the rules were applied
    when they were not.
- `scripts/verify-units.sh` asserts the settings above as systemd reads them —
  the last assignment of each, the whole `ExecStartPre=`,
  `StandardInputText=`, `ExecStart=`, `ExecReload=` and `ExecStop=` lists —
  and that `Requires=`/`Requisite=`/`BindsTo=`/`PartOf=`, `OOMScoreAdjust=` and
  `ExecStopPost=` stay gone however they are spelled. It then breaks copies of
  the units in 29 ways and requires its own assertions to reject each one. It
  stubs every `Exec*=` path, not just `ExecStart`. In its staged root it now
  drops system units that are symlinks leaving the unit directory (Ubuntu
  26.04 ships dracut's that way): `systemd-analyze` reported each of them, and
  the CI units job had failed on every pull request since GitHub's
  `ubuntu-latest` became 26.04 (FLU-71).
- `test/installer/allowlist.sh` (CI, under `sudo`): 112 checks, with the
  installer's functions running under the installer's own shell options — 35
  on the address-list parser (injection strings, prefixes, values that name
  nobody), 18 on the generated rulesets and the three spellings of the fence
  (2 of them need `nft`), 4 on relocating `nft` in the units, and 55 that run
  `install_firewall` against a scratch directory with stand-ins for systemd
  and the kernel table (kept, rewritten, refused, apply failed and rolled
  back; these need root). Without root it runs 55 and skips the rest.
- Lab gate `scripts/verify-firewall.sh`: ruleset flushed under a running
  sandbox and brought back by an agent restart; rules file broken, or without
  the fence line → agent refuses, bridge down, sandbox alive, recovery with no
  manual step once the file is right.
- Lab gate `scripts/verify-reboot.sh`: a clean reboot of a worker that holds
  a running, a sleeping and a pool sandbox, with a probe inside one guest that
  logs what it can reach while the host goes down. With
  `WITH_DISTRO_NFTABLES=1` the distribution's `nftables.service` is enabled
  for the run and the shutdown's journal must show the sandboxes ended before
  the ruleset is flushed (FLU-62).

Known limits, each filed separately: one host cannot get both roles' rules
from the installer and `feluccad.service` has no start gate (FLU-64); nothing
re-checks the rules while the agent runs (FLU-8).

**Upgrading an existing worker**

- [ ] **The new unit has to be loaded before the agent is next restarted —
      check before you restart**: `systemctl show felucca-agent -p KillMode`
      must say `process`. Restart with the old unit still loaded and every
      sandbox dies one last time. (`deploy/install.sh worker` copies the
      binary, installs and loads both units, and restarts nothing.)
- [ ] Never roll the binary back without the unit (or the other way round) on
      a node with sandboxes — see DEPLOYMENT.md §10 "Rolling back".
- [ ] `systemctl stop felucca-agent` no longer stops sandboxes. To power a
      node's sandboxes off: `systemctl stop felucca-agent && systemctl kill
      felucca-agent`. Update runbooks and anything that alerts on
      "left-over process" warnings.
- [ ] Re-run `deploy/install.sh worker` (or copy both units and
      `systemctl daemon-reload`). **Do not pass `FELUCCA_CONTROL_PLANE_IP`
      unless you mean to rewrite the allow-list** — without it the installer
      leaves `/etc/felucca/firewall.nft` alone. If you do pass it: literal
      addresses only, no `/prefix` any more (a subnet is a hand edit of the
      file), and a value that names nobody is refused.
- [ ] `systemctl reload-or-restart felucca-firewall` if you installed the
      units by hand (the installer does it). `restart` works too and is now
      harmless.
- [ ] Check the worker's rules file still has the line `iifname "felucca0"
      drop` exactly like that: the agent now refuses to start, and takes the
      guest bridge down, when that rule is not loaded. The installer checks a
      kept file for it and stops if it is missing.
- [ ] A rules file written before this release starts with "regenerated on
      every run, do not hand-edit". Ignore that line (or delete it): the file
      is kept from now on, and hand edits are how subnets and extra addresses
      get in.
- [ ] Host shutdown and reboot: the firewall unit now ends the sandboxes
      itself, right after the agent has stopped and before any unit can unload
      firewall rules. Nothing to do. Do not copy its `ExecStop=` line anywhere
      without the `stopping` test in front: without it, it is "kill every
      sandbox on this worker". A sandbox that was running comes back
      `stopped`, or `sleeping` if it has ever slept — cold-start those
      (`stop`, `start`) rather than waking them, until FLU-4 is fixed.
- [ ] If you edit the rules file from here on, apply it with `reload`.
- [ ] Alerts: a `felucca-agent` that cannot start no longer ends up `failed` —
      it retries every 5 s for good. Alert on the node being `down`, or on
      `NRestarts`, instead; and expect two to three systemd warnings per
      sandbox per attempt in the journal while it loops.
- [ ] Automation that runs the installer: it now exits non-zero when `nft` is
      missing, when `FELUCCA_CONTROL_PLANE_IP` is refused, and when the
      existing rules file does not load or (worker) has no fence line.

## Stability wave 1 — data safety (2026-11-27, Rust + Go)

Second wave of the 2026-11 stability audit: the findings in which Felucca said
something that was not true — a sandbox asleep that was not, a write saved that
was not, a hostname that reached the wrong sandbox — and then acted on it.
Decisions and the alternatives that lost:
[ADR-0012](adr/ADR-0012-data-safety.md). Operator view: DEPLOYMENT.md §10
"Upgrading to stability wave 1" and §12. API: API-V2.md §1, §2a, §3, §3d, §7.

**A dead VM is no longer woken from a stale snapshot (FLU-4)**

- Was: at start-up the agent called a sandbox `sleeping` whenever a
  `vmstate.bin` lay in its directory. A sandbox that had once slept and been
  woken came back `sleeping` after a reboot, and a wake restored old memory over
  a newer disk.
- A sandbox is asleep only when its record says so. A `running` or `paused`
  record whose Firecracker is gone becomes `stopped` at start-up, written to
  `meta.json`. `meta.json` gains the optional `snapshot_valid`, written only by
  a sleep that finished; records of an older agent are kept `sleeping` only
  when both snapshot files exist and `rootfs.ext4` is not newer than
  `vmstate.bin`.
- Stale snapshots are renamed `vmstate.bin.outrun` / `mem.bin.outrun`, not
  deleted; `start`, `stop`, the next sleep and `delete` remove them.
- One lifecycle operation at a time per sandbox: `409 {"error":"busy"}`.
  Same-kind requests wait up to 25 s, a fork never waits, forks of a sleeping
  parent run side by side, stop and delete pre-empt (and answer 409 with
  `"state":"stopped"` if the operation does not let go within 15 s).
- Sleep is accepted from `running`/`paused`, wake from `sleeping`; other states
  answer 409 with a `state` key (they used to answer 500 or act anyway). The
  409s of stop, start, pause and resume carry it too. feluccad records the state
  a worker names and forwards the 409; the sweep does the same for its
  auto-sleep.
- feluccad answers lifecycle verbs on a record still `creating` with `409
  {"error":"InvalidState: creating (the sandbox is still being created)","state":"creating"}`
  (delete stays allowed). A record a stopped or killed feluccad left `creating`
  comes back as `error` after the restart, with a WARN that a VM may exist on
  its worker; its first successful start, wake or resume takes the guest
  address from the worker, so expose and routing work again.
- A delete the worker refuses as busy answers 409 with the worker's body and
  `Retry-After: 2`, the record kept with the state the worker names; any other
  worker refusal stays the 502 `agent delete failed …`. `?force=1` unchanged.
- A wake loads the snapshot without resuming, writes `running`, then resumes,
  and removes `vmstate.bin`. A missing or empty snapshot file makes the sandbox
  `stopped` (409). A resume that got no answer stops the sandbox and sets the
  snapshot aside.
- Snapshot files exist only beside a `sleeping` record: stop, start, start-up
  and a VMM that dies remove them (`mem.bin` is kept while a live VMM maps it).

**A snapshot is complete before anything relies on it (FLU-70)**

- Was: a failed `snapshot/create` left complete-looking files that a later
  wake used, and the guest paused under a record saying `running`.
- Sleep: pause, snapshot to `vmstate.bin.tmp`, fsync the disk image, rename,
  fsync the directory, write `sleeping` + `snapshot_valid`, then kill the VMM.
  Any failure before the kill removes the files and resumes the guest; the
  record is unchanged (`paused` if the resume fails too).
- Fork reserves the child id first (`409 {"error":"AlreadyExists"}` for a
  taken id or directory, before anything is written) and writes the snapshot
  into the child's directory; the parent's directory never gets snapshot
  files. A failed fork leaves no directory, or an `error` child without
  snapshot files.

**A fork child runs on its own disk (FLU-72, found in this wave)**

- Was: the snapshot names the drive's path and the load had no override, so
  every fork child opened its parent's `rootfs.ext4`; the two guests wrote one
  file system. The child's copy was made after the parent resumed.
- The disk is copied (`cp --reflink=auto`) while the parent is paused; the
  child is loaded without resuming, its drive is pointed at its own
  `rootfs.ext4` (`PATCH /drives/rootfs`, checked with `GET /vm/config`), and
  only then resumed.
- A sandbox that does not run on its own disk (forked before this release) is
  not forked — `409 {"error":"InvalidState: this sandbox was forked before this
  release and runs on its parent's disk image, not on its own; it cannot be
  forked","state":"<parent's state>"}`, running, paused or sleeping — and is
  woken only if the image it runs on is unchanged since it slept. DEPLOYMENT.md
  §10 says how to find such sandboxes.
- feluccad relays a fork the worker refuses for good as 409 with the worker's
  body and no `Retry-After` (it was a 502 with the child in `error`), removes
  the child it had reserved, and records the parent's state; nothing is left
  of the child on the worker either, whatever the parent's state. Busy and
  refused forks do not count as activity of the parent.
- A fork now pauses a running parent for the disk copy as well as the
  snapshot.

**A save that fails, fails the request (FLU-5, with FLU-74, FLU-82, FLU-83)**

- Was: every save site logged a failed save and answered success; a database
  that was locked, read-only or full lost acknowledged changes at the next
  restart.
- One save path; the database is written outside the state lock. While the
  store is failing, changes are refused before anything is done: `503
  {"error":"store unavailable"}`, `Retry-After: 5`. A change the worker already
  carried out answers `503 {"error":"applied but not saved","applied":true}`,
  `Retry-After: 5`. A create or fork whose last save fails answers 503 and its
  VM is removed from the worker after the answer. Reads, exec, heartbeats, the
  route table, `/metrics` and `/healthz` keep working; wake is refused; the
  lifecycle sweep pauses.
- A recovery loop retries every 2 s with a fresh writer connection, so a
  database that was read-only when feluccad started recovers without a restart.
  feluccad starts on a database it cannot write (store failing) and still
  refuses to start on one it cannot read, or on a legacy state file that does
  not load.
- `/healthz`: `200 {"ok":true}` or `503 {"ok":false,"store":"failing"}`.
- New metrics: `felucca_persist_failures_total`, `felucca_store_healthy`,
  `felucca_usage_write_failures_total` (failed attempts; the rows stay queued),
  `felucca_usage_rows_dropped_total` (rows lost from the full queue),
  `felucca_store_read_errors_total`.
- `synchronous=FULL`; a second, read-only database handle for reads. Exec
  metering rows are queued (at most 10 000) and written once a second.
- SIGTERM/SIGINT: every listener stops accepting and the requests in flight
  get 10 s in all to finish; then the rest is cut, the exec rows are written
  (retried once a second until just before the 20 s limit; one ERROR says how
  many were lost if that never succeeds), and feluccad exits 0. A second
  signal, or 20 s after the first, exits 1.
- FLU-74: `mem_total_mib` / `mem_free_mib` above 2^40 answer 400; unsigned
  values are clamped when bound. One heartbeat used to make every later save
  fail.
- FLU-82: a tenant and its first key, and a node credential and its join
  token, are each one transaction.
- FLU-83: read errors answer `503 store unavailable` — key lookup (not counted
  by the brute-force guard), quota, template lookups, the tenant row of `POST
  /api/v1/tenants/{id}/keys`, node join — and the sweep skips a tenant whose
  policy it could not read; listings that cannot be read answer 500. They used to mean "no
  limit" or "wrong key".

**A hostname never reaches another sandbox (FLU-6)**

- Was: the agent handed out the lowest free node port, so a deleted sandbox's
  port went to the next expose at once while gateways still routed the old
  hostname to it — indefinitely while feluccad was down.
- Gateway: past `--max-stale` (`FELUCCA_GW_MAX_STALE`, default 600 s, range
  `2 x refresh + 20s` to 600 s, a bare number is seconds), and before its first
  fetch, sandbox requests answer `503 {"error":"route table unavailable"}`,
  `Retry-After: 5`. New `/readyz`. `--refresh` outside 1 to 60 s refuses to
  start. Upstream connections are pooled per sandbox and guest port.
- feluccad quarantines a released node port for 900 s on that worker host,
  sends each expose `avoid` and `prefer`, and checks the answer. New answers:
  `429 {"error":"expose churn limit reached"}` (512 quarantined ports per tenant
  per worker host, 64 distinct node port and guest port pairs per sandbox),
  `503 {"error":"no free node port on this worker"}` (`Retry-After: 30`),
  `503 {"error":"node port conflict, retry"}` (`Retry-After: 5`), and a 503
  naming the expose that holds the port when the worker answers with a port
  this sandbox holds for another guest port. A backward clock step lengthens a
  quarantine by at most the step; `Retry-After` never exceeds 900 s. New gauge
  `felucca_node_ports_quarantined`. Start-up logs an ERROR for each node port
  that rows of two sandboxes name, and for each guest port of a sandbox whose
  rows name several node ports.
- Agent: round-robin allocation from a random start, never a port in `avoid`,
  `prefer` when free, `"avoid_honored":true` in the answer; `500 {"error":"ingress
  rule not installed"}` when the rule did not load; the ingress chain is one
  nft transaction; delete rebuilds the rules before it frees the address.
- The sweep keeps a sleeping sandbox's dynamic exposes for 900 s instead of
  dropping them at once, drops them only on `ready` workers (5 s per call, 5 s
  of worker time per pass), and repeats a drop that got no answer until the
  worker answers. A worker 404 on unexpose means the mapping is gone. An agent
  expose writes `meta.json` only once its rule is in; two overlapping deletes
  free the address once (FLU-40).

**Each wake waiter gets its own service (FLU-7)**

- Was: requests coalesced on one wake all got the first request's route, so a
  sandbox's second service was answered by its first.
- Each waiter resolves its own hostname again after the wake. The gateway reads
  the wake's answer (polls after 2xx, resends after a 503 with `Retry-After`,
  holds off 5 s after 404/409). A `routes/ensure` 429 or 503 is passed on
  instead of becoming 404.

**A Firecracker is ready when its API socket accepts (FLU-86, found in this wave)**

- Was: after starting a Firecracker the agent waited for its API socket's file
  to exist; under load the first request arrived before Firecracker listened,
  and a create failed with `FcError: send request: client error (Connect)`.
- The agent now waits, at most 3000 ms, until the socket accepts a connection,
  for create, start, wake and fork. When it does not: `SocketNeverAppeared:
  the Firecracker API socket did not accept a connection within 3000 ms (its
  file existed)` (or `did not exist`).

**The gateway no longer prints the admin token (FLU-73)**

- `--token` had `$FELUCCA_TOKEN` as its default, which `--help` prints. The
  variable is now read after the flags are parsed.

**Rules are checked in the kernel and put back (FLU-8)**

- Was: the agent built its nft rules at start-up and on membership changes and
  never looked again; after a ruleset flush tenants shared one network,
  `/healthz` still said `"isolation":true`, and only an agent restart helped.
- Every agent chain starts with a marker rule `counter comment
  "felucca:<16 hex>"` (a hash of what was applied). Every 5 s the isolation
  watch looks at the guest bridge, compares the markers in the kernel with what
  the agent last applied, checks the `br_netfilter` and IPv6 switches and the
  anti-spoof chain of every tap with carrier, and rebuilds what drifted,
  including a table set dormant. Tools that hash `nft list` output must use
  `nft -s` (the marker is a counter).
- Before it runs, the agent reads back the chains start-up applied; if it
  cannot read its own markers on this kernel the watch does not start
  (`isolation watch NOT started: …`, ERROR and stderr). Per table, nothing is
  taken down or refused until a marker has been read back there.
- Something is taken down only on positive evidence twice: positively not in
  place, and the repair refused by the kernel or applied and still not there.
  A failed look or a repair with no answer takes nothing down. Then an
  isolation chain, a switch or a table that stays dormant takes the guest
  bridge down (`enforce`, the default) until a tick repairs it; a tap's chain
  takes that tap down; NAT and ingress take nothing down and never enter
  `isolation`. What the watch holds down and something else raises goes down
  again. A route `unreachable <guest net> metric 4294967295` keeps guest
  traffic on the worker while the bridge is down.
- New guests are refused (`IsolationFailed`: create, fork, wake, start, pool
  refill) while a fence is positively not in place or its repair has no
  answer, while the guest bridge is gone, and while start-up could not settle
  the ingress chain — in `enforce` and `report`. While the ingress chain is
  unsettled, `isolation` and `isolation_ok` are false in every mode, `off`
  included.
- A deleted table or flushed chain is back within about 3 to 5 s; in that
  window the fence is open. The watch shortens an exposure to seconds; it does
  not prevent it.
- New setting `isolation_watch` / `FELUCCA_ISOLATION_WATCH` /
  `--isolation-watch <value>` or `--isolation-watch=<value>`: `enforce`
  (default), `report`, `off`; any other value, or the flag without one,
  refuses to start.
- The host fence (`inet felucca_host`) is reported (WARN once a minute,
  `isolation` false), never repaired: it is `felucca-firewall.service`'s. It
  counts only if it was in place when the agent started.
- `/healthz` `isolation` and the heartbeat's `isolation_ok` are the watch's
  verdict: false when a fence, switch or tap pin is unrepaired, the guest
  bridge is gone, the host fence is missing, the last completed tick is older
  than 45 s, or none completed within 45 s of the watch's start.
- Every `nft` and `ip` command is killed after 10 s (`wg`: 60 s) and is not run
  a second time; at most 16 timed-out commands are outstanding. A tick waits
  up to 15 s for the rule lock and works at most 15 s. NAT and the two
  host-side fences are one nft transaction each.
- Slots held after a failed rule rebuild are released once both rebuilds
  succeed again (they used to stay reserved until a restart). A start-up that
  can neither rebuild nor empty the ingress chain places no guest until it is
  rebuilt (retried every 5 s, in every mode).
- Taps start-up could not pin for adopted sandboxes are pinned and raised by
  the watch later (FLU-65), and start-up says which failure happened.
- feluccad keeps each node's `isolation_ok` in memory, logs when it changes,
  and counts it in the new gauge `felucca_nodes_unfenced`.
- Creates avoid a worker that reports its isolation down (FLU-78): they go to
  the ready worker with the fewest sandboxes among those that do not, and fall
  back to all ready workers, with a WARN at most once a minute, only when
  every one reports it down. Such a worker refuses new guests, so it had the
  fewest and drew every create.

**Changed behaviour you may notice**

- Sandboxes that were running at a worker reboot come back `stopped`, never
  `sleeping`. `start` them.
- Stopping a sleeping sandbox discards its snapshot.
- Lifecycle requests can answer `409 {"error":"busy"}`; retry. Lifecycle
  requests on a `creating` sandbox answer 409; a record a stopped feluccad left
  `creating` comes back `error`.
- A fork of a sandbox forked before this release answers 409, and a busy
  delete answers 409 with `Retry-After: 2` (both were 502).
- Stopping feluccad drains requests for up to 10 s. After a feluccad restart
  the workers read `down` until their next heartbeat, and creates answer 503
  until then.
- Creates avoid workers that report their isolation down.
- While feluccad's database cannot be written, every change answers 503 —
  wake included.
- feluccad's `/healthz` can answer 503, and is no longer counted in
  `felucca_api_requests_total`.
- The gateway answers 503 for sandbox URLs after `--max-stale` without a fresh
  table, and before its first fetch.
- `felucca-gw` refuses to start with `--refresh` outside 1 to 60 s.
- Expose can answer 429 (churn limit) and 503 (no free node port, node port
  conflict).
- Dynamic exposes are kept across a sleep.
- A fork pauses its parent for the disk copy as well.
- Every Felucca chain in `nft list` starts with a marker counter rule.
- A worker whose fences the agent cannot restore loses its guest network
  (`enforce`) and refuses new guests (`enforce` and `report`); a create,
  delete or expose behind a stuck `nft` fails after 10 s.
- Parallel forks of a running parent answer `busy`.

**Upgrading**

- [ ] Agents first, then gateways, then feluccad. A new feluccad with an old
      agent keeps the old hostname exposure for that worker, with a warning in
      feluccad's log; see DEPLOYMENT.md §10 for every mixed state. A new
      gateway works against the old feluccad, and a roll-back of feluccad
      needs no database restore.
- [ ] After the agent upgrade, the taps of sleeping and stopped sandboxes keep
      the previous agent's anti-spoof chain (no marker, no final `drop`; policy
      `drop`) until they are woken or started: listings show both forms.

- [ ] No unit changes. Load balancers in front of `felucca-gw`: probe
      `/readyz` instead of `/healthz`.
- [ ] Agents: the isolation watch runs in `enforce` by default and can take a
      worker's guest network down (DEPLOYMENT.md §8.2). Set `isolation_watch`
      to `report` for a first roll-out if you want to see what it would do;
      alert on the lines listed there, `isolation watch NOT started:` first.
- [ ] Alert on `felucca_store_healthy` 0, on feluccad's `/healthz` 503, and on
      `felucca_store_read_errors_total` and `felucca_usage_rows_dropped_total`
      growing; alert on `felucca_nodes_unfenced` together with
      `felucca_nodes_ready`.

- [ ] Look for sandboxes forked before this release (DEPLOYMENT.md §10,
      "Sandboxes forked before this release") and decide which data to keep.
- [ ] Clients: treat `503` with `Retry-After` (`store unavailable`, `applied
      but not saved`, `route table unavailable`, node-port answers) and `409
      busy` as retryable.

**What verified the wave on the lab**

New lab gates, each run against the software of before the wave and against
the final build:

- `scripts/verify-snapshot.sh` (drives `test/lab/snapshot.sh`,
  `test/lab/snapshot-pre.sh` and `test/lab/snapshot-post.sh`; FLU-4, FLU-70,
  FLU-72). Before: 205 passed, 87 failed, 1 skipped. Final build: 301 passed, 0
  failed for every case but the power cut, and 42 passed, 0 failed for the
  power-cut case run by itself (343 in all). In the power-cut case the worker
  VM's power was cut 0.05 s after a sleep was acknowledged; the sandboxes whose
  sleep had been acknowledged woke with their memory intact, and the ones that
  were running came back `stopped`.
- `scripts/verify-persist.sh` (drives `test/lab/persist.sh` with
  `test/lab/dblock.py`; FLU-5, FLU-74). Before: every case has failing checks;
  a create under a database lock answered 201 after 15.7 s and left a VM on
  the worker, `/healthz` answered 200 throughout, and 4 of 10 acknowledged
  changes were gone after a restart. Final build: 94 passed, 0 failed, 3
  skipped; under a write lock the first change was refused after 5.2 s and
  every later one in under a millisecond, and the control plane was healthy
  again 0.3 s after the lock was released.
- `scripts/verify-ingress.sh` (drives `test/lab/ingress.sh`; FLU-6, FLU-7,
  FLU-73). Before: 13 passed, 21 failed; 25 of 852 answers on a deleted
  sandbox's hostname carried another sandbox's body; both hostnames of a woken
  sandbox got the same body in 20 of 20 rounds. Final build: 47 passed, 1
  failed; 0 of 855 such answers (0 of 4290 in a longer run of the case); 20 of
  20 rounds right; with `--max-stale 40` a gateway cut off from the control
  plane gave its last 200 at 38.4 s and its first 503 at 39.5 s. The one
  failed check is the gate's own clean-up of a sandbox on the lab's second
  worker VM, which hung during the run.
- `scripts/verify-isolation.sh` (drives `test/lab/isolation.sh`; FLU-8). An
  agent without the watch: every case from C2 to C8 fails, nothing is put
  back. Final build: 305 passed, 0 failed. A deleted table or flushed chain was
  back in 2.9 to 5.1 s; a repair the kernel refused took the guest bridge down
  in 1.5 s, and it was back 1.6 s after the cause was removed; no false alarm
  in 60 create-and-delete cycles (`/healthz` true in 21 of 21 samples); with
  one of two workers reporting its isolation down, five creates all landed on
  the other. While a table was missing, probes from one tenant's guest to
  another's got through: 6 of 10 with table `ip` deleted, and 5 of 13 with the
  whole ruleset flushed, all within 3.5 s of the break.

The older gates on the final build, on one worker:

- `scripts/verify-break.sh`: 66 passed, 1 failed. The failed check: one of 15
  guests booted at once on 8 CPUs was not ready for exec within the gate's
  limit; it answered later in the same case.
- `scripts/verify-restart.sh`: 229 passed, 0 failed, 5 skipped (two warm-pool
  checks, and three that the open FLU-68 decides on a worker without a pool).
- `scripts/verify-reboot.sh`: passed. `scripts/verify-oom.sh`: 21 passed, 0
  failed.
- `scripts/verify-firewall.sh` with `FELUCCA_LAB_OTHER=none`: 130 passed, 0
  failed, 1 skipped.
- `scripts/conformance-lab.sh`: 423 passed, 0 failed, 2 skipped.
  `scripts/verify-v2.sh`: 20 passed, 1 failed (it expects two ready workers
  and found one).

Changed expectations in the older gates:

- `test/lab/restart.sh`: a sandbox whose Firecracker died while the agent was
  down is `stopped` where the gate expected `sleeping` (FLU-4); rule listings
  are compared with `nft -s`, because every Felucca chain now starts with a
  counter.
- `test/lab/break.sh`: B5 now requires the agent to put its own tables back
  without a restart (FLU-8; observed 2.75 s); B8 expects a wake that cannot
  write its record to fail and leave the sandbox asleep (FLU-4).
- `scripts/verify-firewall.sh`: `FELUCCA_LAB_OTHER=none` leaves out the probe
  from a VM that is not on the allow-list and counts it as skipped.
- The gates hand the admin token over on standard input into a header file
  that only the gate's own user can read (`test/lab/lib.sh`,
  `test/lab/run-on-worker.sh`,
  `scripts/verify-persist.sh`, `scripts/verify-isolation.sh`,
  `scripts/verify-ingress.sh`, `scripts/verify-firewall.sh`);
  `scripts/verify-snapshot.sh`, `scripts/conformance-lab.sh` and
  `scripts/verify-v2.sh` still pass it on a command line (FLU-84).

Measured besides the gates:

- A fork's pause on ext4 (a virtio disk): an image holding 472 MiB of data
  paused the parent 0.2 to 0.7 s, and the whole fork call took 1.0 to 1.4 s;
  an image holding 7.4 GiB paused it 10 to 12 s, and the call took 12 to 14 s.
  That is about 0.6 to 0.75 GiB per second of pause on that disk. The agent
  logs nothing for a fork that succeeds (FLU-89).
- A wake is not slowed by another sandbox's sleep on the same worker: 85 to
  97 ms alone, 89 to 101 ms while a 2 GiB guest with 1500 MiB of memory in use
  was being put to sleep.
- A connection that stalls after the guest bridge has been down and up again:
  DEPLOYMENT.md §8.2 and FLU-92.

Not verified on the lab, because the lab's second worker VM hung during the
last rounds:

- `scripts/verify-ingress.sh` case I13 (one worker's agent stopped) and case
  I9 (a hostname after a sleep longer than the quarantine; 16 minutes) were
  not run.
- `scripts/verify-restart.sh` was not rerun on the worker that has a warm
  pool, so the warm-pool checks did not run on the final build.
- The firewall gate's probe from a VM that is not on the allow-list was left
  out.

Found by the last lab round and left open (filed, not fixed in this wave):
FLU-87 (an expose cut short by a control-plane crash leaves a forwarding rule
on the worker that no route row names), FLU-88 (the gateway has no graceful
stop: a restart cuts off requests in flight), FLU-89 (a fork that succeeds
logs nothing), FLU-92 (the stall above), FLU-93 (the node list does not show a
worker's isolation flag).

## Backlog (v3+, in order)

branch (uffd CoW fork of running VMs) → cross-tenant nftables isolation →
overlayfs root tier → SQLite state → cross-node reschedule → OIDC →
uffd lazy restore → Terraform provider.
