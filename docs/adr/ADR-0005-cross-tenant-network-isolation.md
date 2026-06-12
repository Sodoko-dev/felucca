# ADR-0005 — Cross-tenant network isolation via a single nft pair set

- Status: Accepted
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
