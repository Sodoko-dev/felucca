# ADR-0006 — WireGuard hub-and-spoke overlay and one-time node join

- Status: Accepted
- Date: 2026-06-12
- Context: third phase (P2, "deploy anywhere") of the v4 plan. Workers must
  join a hearthd that is not on their L2 segment — across clouds and behind
  NAT — without hand-distributing credentials or VPN config. Related:
  ADR-0002 (no Kubernetes; flat process model), ADR-0005 (isolation stays
  node-scoped until the overlay extends it), `docs/PLAN-v4.md` Phase 2.
  Delivered across step commits `bb182f3` (hub + join API), `7ac7ead`
  (agent `--join`), `fdeb995` (TLS), `ee54a9f` (conformance + lab e2e),
  `f2eb5a1`/`80dcc08` (systemd units validated live).

## Decision

1. **Hub-and-spoke WireGuard, hearthd is the hub.** hearthd owns interface
   `wg-hearth` (`--wg-ip`, e.g. `10.100.0.1/24`; `--wg-endpoint` is its
   public `host:port`). Each joined worker is one peer with a /32 inside the
   overlay network, allocated by the hub (`wg.AllocateOverlayIP`: lowest free
   host, skipping network/broadcast/server/taken — pure function, kept
   deliberately store-non-transactional for the single-hearthd model,
   serialized by `joinMu`). Spokes talk to the hub only; the control plane's
   node↔hearthd traffic is exactly hub↔spoke, so spoke↔spoke routing is not
   needed yet (see Deferred).
2. **One-time join tokens, consumed last.** `POST /api/v1/join-tokens`
   (admin-only) mints `hearth_jt_<secret>`; only the sha256 is stored
   (`hashSecret`, the single credential-hashing site), TTL 24h
   (`store.JoinTokenTTL`). `POST /api/v1/nodes/join` authenticates by the
   join token itself (routed before the bearer gate), validates the body
   (44-char base64 pubkey), then: peek token (`CheckJoinToken`) → persist
   peer (`CreateWgPeer`, upsert by pubkey keeps the overlay IP, so re-joins
   are idempotent while inside the configured subnet) → install into the
   kernel (`srv.addPeer`) → **only then** `ConsumeJoinToken`. Any failure
   before the last step leaves the token usable — a half-failed join never
   strands an operator with a burned token.
3. **The agent enrolls once, then the file is the identity.** `hearth-agent
   --join <url> --join-token <tok>` generates a keypair only if none exists
   (`ensure_key` creates on NotFound only — never rotates silently), calls
   the join API (30s timeout), and persists the grant to
   `{data_dir}/wg.json` (0600, temp+rename). Failure to persist after a
   successful join is fatal on the spot (the token is already consumed;
   failing weeks later on reboot would be undebuggable). On every boot a
   present `wg.json` wins over `--join` (warn), brings the tunnel up
   (`ensure_interface`, idempotent), and routes the control plane over the
   overlay: `control_plane = http://<server_overlay_ip>:<api_port>`,
   `advertise_addr = <own overlay ip>`. An unreadable or corrupt `wg.json`
   is **fatal**, never a silent fall-back to direct mode — the file's
   presence proves enrollment, and starting un-enrolled registers a wrong
   (often loopback) address with the hub. NAT'd spokes use
   `persistent-keepalive` (hub-configured, default 25s).
4. **Secrets never touch argv.** On both sides, private keys reach `wg set`
   as a 0600 file path and `wg pubkey` via stdin; the https join shells out
   to `curl --config -` with the token inside the config on stdin. Join
   tokens exist in plaintext only in the mint response and the enrolling
   agent's memory.
5. **TLS terminates beside the join, not inside the overlay.** With
   `--tls-domain`, hearthd adds an autocert listener on :443 (+ :80 for
   HTTP-01); the plain `--port` listener stays unconditionally because
   overlay agents speak plain HTTP *inside* the tunnel (WireGuard is the
   transport security). First enrollment against an http:// hub is TOFU
   (see Deferred).
6. **systemd units are part of the contract.** The deploy units were
   validated by running the real fleet under them (P2.5): Firecracker needs
   `@sandbox` in `SystemCallFilter` (it installs its own seccomp filters via
   `seccomp(2)`) and `/dev/net/tun` in `DeviceAllow`; the agent's capability
   bounding set drops `CAP_DAC_OVERRIDE`, which is why wrong-owner state
   files are loud fatals rather than silent fallbacks (Decision 3);
   `modules-load.d/hearth.conf` preloads `br_netfilter` + `wireguard`
   because `ProtectKernelModules=true` forbids the services loading them
   (load-bearing for ADR-0005's isolation).

## Rejected alternatives

- **Mesh (every node peers with every node).** No control-plane need for
  spoke↔spoke today; a mesh multiplies key distribution and churn handling
  for zero current benefit. Revisit when cross-node tenant networks (P3+)
  need direct east-west paths.
- **Reusable bearer token for join.** A leaked long-lived join credential
  enrolls arbitrary attacker nodes into the fleet. One-time + 24h TTL +
  sha256-at-rest bounds the blast radius to one node per mint, briefly.
- **Token consumed at validation time.** Burns the token on transient
  failures (kernel install, DB write), forcing a re-mint round-trip with the
  operator mid-bootstrap. Consume-last is strictly better and still
  single-use (`joinMu` serializes the check-to-consume window).
- **wg-quick / external VPN tooling.** Another config file format and
  service to install; the needed subset (one interface, one peer per side)
  is a few `ip`/`wg` invocations the binaries already know how to make,
  with the same bare-then-`sudo -n` idiom as the rest of the stack.
- **TLS for intra-overlay agent traffic.** Double encryption with real
  operational cost (cert distribution to every worker) and no threat-model
  gain over WireGuard's transport security.

## Consequences

- Mixed fleets are first-class: the lab runs one direct worker and one
  overlay worker simultaneously; `verify-v2` maps overlay IPs to nodes.
- The hub is a single point of failure for *new* tunnels and control-plane
  traffic; existing data-plane sandboxes keep running if hearthd dies
  (unchanged from pre-overlay).
- Overlay state lives in two places by design — hub DB (`wg_peers`) and the
  kernel — reconciled at hearthd startup by a batch `AddPeers` re-add
  (warn-don't-die).
- Tenant isolation (ADR-0005) remains node-scoped; the overlay moves
  control traffic, not tenant networks.

## Deferred (recorded during P2 review loops)

- **Store-transactional overlay IP allocation** — needed only for HA/multi-
  hearthd (ADR-0002's Postgres path); today `joinMu` is sufficient.
- **Hub `ip_forward` for spoke↔spoke** — P3, when a gateway is not beside
  hearthd.
- **Go wg package client-side shape** — P3 may want the hub to also dial
  agents; the package is hub-only today.
- **Same-host hub+worker collision** — both want `wg-hearth`; an agent
  beside hearthd on one host would clobber the hub's interface. Documented
  constraint, not auto-detected.
- **TOFU bootstrap** — first join over plain http trusts the network once;
  use the https join (P2.3) wherever a domain exists. P2.6 verifies the
  https path live.
- **Release binaries for both arches** in `deploy/release/<arch>/` —
  install.sh expects them; not yet built/published.
