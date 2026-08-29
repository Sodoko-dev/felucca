# ADR-0005 — Cross-tenant network isolation via a single nft pair set

- Status: Accepted, **extended 2026-08-29** (see the amendment below)
- Date: 2026-06-12
- Context: second phase (P1) of the v4 "multi-tenant, deploy-anywhere,
  product-ready" plan. Consumes the `tenant_id` that P0 records in the agent's
  `meta.json` (ADR-0004 §5) and discharges the P0 security-review requirement
  that tenant ids be allowlist-sanitized before any nft shell-out. Related:
  ADR-0004 (tenancy), `docs/PLAN-v4.md` Phase 1.

## Decision

1. **br_netfilter, because the bridge bypasses the `forward` hook.** Same-subnet
   guests on `hearth0` talk via L2 switching, which never traverses the ip
   `forward` hook — an nft forward chain alone would see nothing. The agent
   loads `br_netfilter` and sets `net.bridge.bridge-nf-call-iptables=1`
   (`ensure_bridge_netfilter`, `rust/agent/src/net.rs`) so bridged IPv4 frames
   are routed through netfilter and the forward chain can police guest-to-guest
   traffic.
2. **One concatenated pair set, not per-tenant sets.** A single nft set
   `tenant_pairs` (`type ipv4_addr . ipv4_addr`) in the existing table
   `ip hearth` holds every allowed ordered (source IP, destination IP) pair —
   i.e. all same-tenant pairs. The forward chain (policy **accept**) is, in
   order: accept `ct state established,related`; accept anything whose
   in-interface is not `hearth0`; accept anything whose out-interface is not
   `hearth0`; accept `ip saddr . ip daddr @tenant_pairs`; **drop** the
   remaining `hearth0`↔`hearth0` traffic. Only intra-bridge guest-to-guest is
   policed — egress NAT and host↔guest are accepted before the pair check.
3. **Flush-and-rebuild from full membership.** `rebuild_isolation(members)`
   takes `(tenant_id, guest_ip)` for every VM that currently holds an IP,
   ensures table/set/chain exist, flushes the parts it owns, and repopulates —
   idempotent, mirroring the `ensure_nat` flush-and-re-add idiom.
   `Manager::refresh_isolation()` (`rust/agent/src/vm/mod.rs`) snapshots
   membership under the state lock, releases it, then rebuilds; rebuilds are
   serialized by a dedicated `isolation_lock` so concurrent create/fork/delete
   can't interleave nft commands (each rebuild snapshots at its start, so the
   last to run leaves the freshest state). Call sites: end of create (both the
   warm-pool claim and cold-create paths), end of the fork success path (the
   child's post-re-IP address), end of delete, and at agent startup after
   reconcile (`rust/agent/src/main.rs`) — nft sets don't survive a host reboot.
4. **`tenant_id` never reaches an nft command — that is the security
   property.** The tenant id is only a Rust-side `HashMap` grouping key inside
   `rebuild_isolation`; the only strings interpolated into nft commands are
   agent-allocated IP addresses. `valid_tenant()` (`[A-Za-z0-9_-]`, length
   1..=64) is defense-in-depth, not the primary injection guard. A missing or
   invalid tenant id is treated as "no tenant": the VM joins no pair and is
   therefore isolated from **all** peers — the failure mode is fail-closed.

## Amendment — 2026-08-29 (v4 security hardening): three more fences

The decision above stands unchanged, but the security review found it was not
the whole fence. The `ip` `forward` chain polices exactly one path — IPv4,
guest-to-guest, across the bridge — and three other paths reached around it.
All three are now closed in `rust/agent/src/net.rs`, and `netfilter_ready()`
returns true only when **every** one of them is confirmed; with networking on
and any of them missing, `hearth-agent` **refuses to serve** rather than accept
tenant placements onto a node with no isolation.

1. **Guest → host (`ip hearth input`, `ensure_guest_input_filter`).** Guests
   route to the world through the bridge gateway, so traffic they aim *at the
   host itself* lands on the INPUT hook, which the forward chain never sees —
   every host service on a wildcard bind was one curl from inside any sandbox,
   the agent's own root API included. The chain accepts anything arriving on a
   different interface, then explicitly drops `tcp dport <agent port>` from
   `hearth0`, then permits only ICMP (gateway pings, path-MTU discovery) and
   `ct state established,related` before a trailing `iifname hearth0 drop`.
   Chain policy stays `accept` deliberately — this is a shared hook, and a drop
   policy would cut the host's own SSH; the trailing drop fails closed for guest
   traffic only. No DHCP or DNS holes: guests are addressed from the kernel
   command line and resolve through NAT'd egress.
2. **Non-IPv4 between guests (`bridge hearth forward`, `ensure_bridge_l2_filter`).**
   The pair set is `ipv4_addr . ipv4_addr` in the `ip` family, so it only ever
   sees IPv4. Two guests on one bridge exchange every *other* ethertype by pure
   L2 switching — most importantly IPv6, which any guest kernel autoconfigures
   as a link-local address on eth0 and which no tenant rule covered. The bridge-
   family chain (priority -200) accepts ARP and IPv4 between `hearth0` ports and
   drops the rest; IPv6 is separately disabled on the bridge
   (`ensure_ipv6_disabled`).
3. **Source-address spoofing (`netdev hearth <tap>`, `ensure_tap_antispoof`).**
   Membership is keyed on the guest's IP, so a guest that simply *claims* another
   tenant's address rides that tenant's allow pair. A per-tap ingress chain with
   `policy drop` pins the guest's `ether saddr`, its `arp saddr ether` **and**
   `arp saddr ip`, and its `ip saddr`. The ARP sender-MAC pin is not decoration:
   the bridge learns from that field, so without it a guest can move a victim's
   address onto its own port in the FDB and receive the victim's traffic. If the
   ruleset cannot be rendered (tap, IP or MAC failing validation) nothing is
   pinned and the call reports failure rather than leaving the tap open.

This partially reverses the "ebtables / L2 filtering on the bridge" rejection
below. What was rejected there — expressing *tenant pair semantics* in L2 — is
still rejected, and the pair set remains the tenant boundary. What the L2 chain
does is narrower and complementary: it confines guest-to-guest traffic to the
ethertypes the `ip` chain can actually adjudicate. It also introduces no second
toolchain, which was half the original objection: it is nft, in the `bridge`
family, loaded through the same `nft -f -` path as everything else.

## Alternatives considered

- **Per-tenant named sets `hearth_t_<tenant>`** (the plan's original sketch):
  rejected — set names would embed tenant strings in nft command lines, making
  string sanitization the actual security boundary (exactly the injection
  surface the P0 review flagged). It also needs one set + one rule per tenant
  and create/teardown bookkeeping; the pair set is one set + one rule, ever.
- **Per-VM chains or per-VM rules**: O(VMs) rules with ordering churn on every
  lifecycle event; rejected — the pair set keeps the ruleset constant-shaped.
- **ebtables / L2 filtering on the bridge**: would avoid br_netfilter, but
  introduces a second (legacy) toolchain beside the existing nft NAT and can't
  express same-tenant IP-pair semantics as a single set lookup; rejected.
- **Incremental element add/remove instead of full rebuild**: smaller writes,
  but drift-prone — one missed removal is a stale cross-tenant allow.
  Rejected — the full rebuild is idempotent, self-healing after partial
  failures, matches the `ensure_nat` idiom, and is cheap at node scale.

## Consequences

- On a node: cross-tenant guest-to-guest traffic drops, same-tenant flows,
  egress NAT and host↔guest are unaffected. VMs with no/invalid tenant are
  isolated from every peer.
- **Node-scoped (caveat)**: tenant networks are node-scoped until the P2
  overlay — per-node CIDRs are node-local in v1, so cross-node same-tenant
  traffic is not bridged. The overlay phase extends them.
- The pair set is O(Σ per-tenant-count²) elements — fine at per-node VM
  counts; revisit (e.g. nft maps / marks) if a single node ever hosts very
  large same-tenant fleets.
- `bridge-nf-call-iptables=1` is host-global: all bridged IPv4 on the host now
  traverses netfilter. On a dedicated worker `hearth0` is the only bridge, so
  the blast radius is the chain we installed (early accepts keep everything
  else untouched).
- Every create/fork/delete pays one serialized rebuild (a handful of nft
  execs). Sleep/wake don't change membership and pay nothing — the
  latency-critical wake path is untouched.
