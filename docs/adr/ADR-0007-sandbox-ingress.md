# ADR-0007 — Sandbox ingress: named exposes, worker DNAT, felucca-gw

- Status: Accepted
- Date: 2026-06-12
- Context: fourth phase (P3, "the Sodoko unlock") of the v4 plan. Services
  inside sandboxes (Odoo, Chatwoot, ...) must be reachable from the internet
  by URL, multi-service per sandbox, across the mixed fleet — including
  workers reachable only over the P2 WireGuard overlay. Related: ADR-0005
  (guest networking + nft table), ADR-0006 (overlay), `docs/PLAN-v4.md`
  Phase 3. Delivered in step commits `ff3cdc3` (expose API + worker DNAT)
  and `653e2e2` (felucca-gw).

## Decision

1. **One hostname label per service: `<name>--<sandbox-id>`.** Single-label
   (never nested subdomains — a wildcard cert covers one DNS label), split
   on the FIRST `--`. Names are `[a-z0-9-]`, 1..=32, no leading/trailing/
   double dash — so `--` stays unambiguous — and never all digits: the
   all-digit namespace is reserved for dynamic port-in-hostname labels
   (`8069--<id>`). Sandbox ids contain only single dashes by construction.

   > **Amended 2026-08-29 (v4 security hardening).** The label budget is now
   > load-bearing for a security property, not just for DNS. The gateway
   > authenticates **nothing** on an inbound ingress request, so the hostname
   > label *is* the capability that reaches a tenant's service — and the old id
   > carried a monotonic `seq` tail, which disclosed every other tenant's
   > position in the issuance stream and spent label budget that entropy needed
   > more. Ids are now `"<prefix>-" + 26 hex chars` from `crypto/rand`
   > (`state.IDRandomBytes = 13`, 104 bits), and the seq counter — still
   > persisted, since it is part of the state format — is out of the id. The
   > arithmetic is exact and leaves nothing spare: 32 (longest expose name) + 2
   > (`--`) + 29 (`sb-` + 26 hex) = 63, the DNS single-label maximum. Widening
   > either bound requires re-checking it.
2. **An expose is a route row plus one worker DNAT rule.** feluccad stores
   `{name, guest_port, node_port}` on the sandbox (snapshot-persisted in
   both stores — the SQLite table gained two columns with an idempotent
   ALTER migration). The owning worker allocates the node port (lowest free
   in 20000..=29999 across all VMs on the node) and installs
   `tcp dport <node_port> dnat to <guest_ip>:<guest_port>` in the `ingress`
   chain (nat hook prerouting, priority -100) of the existing `ip felucca`
   nft table — flush-and-rebuild from full
   membership, exactly like the ADR-0005 isolation chain, rebuilt at agent
   startup from `meta.json`. The same injection invariant holds: nothing
   user-controlled ever reaches an nft argv (IPs are re-parsed as
   `Ipv4Addr`, ports are `u16` by construction).
3. **The gateway is a separate small binary (`felucca-gw`).** Wildcard DNS
   `*.<domain>` points at it; it maps Host labels to `node_host:node_port`
   using feluccad's admin-only `GET /api/v1/routes` (cached, refreshed every
   5s; a feluccad outage keeps the last table — routes must not die with the
   control plane). It dials workers exactly like feluccad does (direct or
   overlay address), so a sandbox on a NAT'd home-lab worker is publicly
   reachable — proven in the lab over the wg tunnel. WebSocket/longpolling
   pass through `httputil.ReverseProxy` natively; the inbound Host is
   preserved (vhost-aware guests); connect timeout is 3s so stale routes
   fail fast instead of hanging browsers.
4. **Dynamic ports are lazy exposes, off by default.** `8069--<id>` hosts
   trigger `POST /api/v1/routes/ensure`, which creates a normal expose named
   after the port — gated by the per-sandbox `allow_dynamic_ports` create
   flag (explicit allowlist is the multi-tenant default; agent-built guests
   have unintended listeners). Only canonical decimal labels are honored
   (leading-zero spellings would ensure a port whose label never matches —
   an unauthenticated request-amplification loop against feluccad).
5. **Sandbox lifecycle maps onto routes.** Sleeping → the gateway serves a
   503 wake page with Retry-After (the DNAT rule survives sleep — the guest
   IP persists, so wake needs no rule churn). Node-less sandboxes keep
   route rows with an empty node host so the gateway answers with the
   sandbox state instead of a generic 404. Forks re-expose the parent's
   services on the child with fresh node ports (child URLs work; the agent
   clears inherited exposes so the parent's rules are never clobbered).
6. **Per-tenant ingress policy lives at the ingress.** `felucca-gw` takes a
   `--tenant-limits` JSON map (enabled toggle + token-bucket rps) — not
   feluccad store state. Rationale: limits are an edge concern, vary per
   gateway deployment, and need no API round-trip on the hot path.
7. **Ingress mutations are serialized in feluccad** (`exposeMu`, outer to the
   state lock): the expose flow spans check → agent call → row update, and
   two racing exposes of one name otherwise leak an unaccounted agent DNAT
   entry on the 409 path (review-loop finding), while racing unexposes of
   names sharing a guest port could both skip the agent removal.

## Rejected alternatives

- **Routing in feluccad itself.** Couples the data plane to the control
  plane's availability and load profile; a gateway is deployable N× behind
  DNS/LB and beside the traffic.
- **Per-request route lookups against feluccad.** An API round-trip per HTTP
  request; the cache + periodic refresh bounds staleness at seconds and
  keeps serving through control-plane restarts (the roll gate restarts
  feluccad routinely).
- **Worker-side reverse proxy instead of DNAT.** A userspace hop per packet
  on every worker plus per-worker TLS/config; nft DNAT is kernel-path,
  already in the agent's vocabulary (ADR-0005), and rebuilds from meta.json
  like everything else.
- **Dynamic ports on by default (pure E2B-style).** Agent-built guests run
  listeners the tenant never meant to publish (postgres, the guest agent);
  explicit named exposes are the multi-tenant default, the flag covers
  dev/debug.
- **Hostname scheme `name.id.sb.domain`.** Nested labels need per-sandbox
  certs or a multi-level wildcard (not a thing); `name--id` keeps one
  wildcard label.

## Consequences

- Sodoko's shape works: four exposes on one sandbox = four URLs; fork a
  live Odoo and the child's URLs serve immediately (proven live: cloned
  httpd answered on the child's fresh node port).
- The node-port range caps exposes at 10k per worker; allocation is
  node-local so a sandbox reschedule (P6 HA) re-allocates and the route
  table follows automatically.
- Route staleness is bounded by the refresh interval: a just-slept sandbox
  can 502 (fast, 3s) for up to one interval before the wake page appears.
- The gateway holds the feluccad admin token (the route table is admin-only)
  — its env file is root:felucca 0640 like feluccad's own config.

## Deferred (recorded during the P3 review loop)

- **feluccad↔agent expose reconciliation**: a best-effort sandbox delete that
  never reaches the agent leaves the guest + DNAT rule running with no
  route row (publicly reachable at node_host:node_port, bypassing gateway
  policy). Needs the agent-side unknown-VM sweep that cross-node reschedule
  (P6) brings anyway.
- **Dynamic-expose GC**: lazily ensured digit-label exposes live until
  sandbox deletion; P5 idle/TTL policies own expiry (they own last-activity
  tracking).
- **Wildcard TLS at the gateway** (ACME DNS-01): needs the real domain —
  lands with the external-infra acceptance alongside P2.6.
- **Auto-wake on request** (the 503 page wakes the sandbox): needs idle
  policies' activity plumbing (P5); the page + Retry-After is the contract
  until then.
