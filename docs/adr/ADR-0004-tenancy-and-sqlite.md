# ADR-0004 — Tenancy via API keys + enforced namespaces; SQLite storage layer

- Status: Accepted
- Date: 2026-06-12
- Context: first phase (P0) of the v4 "multi-tenant, deploy-anywhere, product-ready"
  plan. Fulfills the SQLite design point ADR-0002 named. Related: ADR-0002
  (no-Kubernetes control plane), ADR-0003 (Go/Rust port).

## Decision

1. **Tenant is a first-class entity; API keys are the v1 credential.** A tenant
   carries quotas (max sandboxes/vcpus/mem/disk; 0 = unlimited). Credentials are
   bearer keys `hearth_sk_<48 hex>`; the server stores only the SHA-256. The
   pre-existing single configured token becomes the **admin** credential
   (unrestricted, unmetered) — every pre-v4 deployment keeps working unchanged.
   OIDC is deliberately deferred: machine consumers (the first product is a
   machine) need keys, humans need SSO; both will resolve to the same tenant
   entity later, so the boundary outlives the auth method.
2. **Tenancy is enforced, not advertised.** All sandbox routes resolve key →
   tenant and scope every read/action; foreign and unknown ids are
   indistinguishable (404). Infrastructure routes (nodes, agent registration,
   tenant admin) are admin-only and 404 for tenant keys. `tenant_id` is
   **excluded from all JSON responses** (`json:"-"`): the v2 wire contract is
   frozen by the conformance goldens, and tenancy is visible only through
   scoping behavior — itself pinned by new conformance cases (hearthd/17).
3. **Durable state is SQLite (WAL) via the pure-Go driver** (`modernc.org/sqlite`)
   behind a `Store` interface (`go/internal/store`). hearthd keeps its in-memory
   working set (internal/state) and writes through with **whole-snapshot
   transactions** — the exact granularity the JSON file had, so the swap is
   behaviorally invisible; row-level writes are a later optimization, and the
   interface is the future Postgres/HA seam (ADR-0002 §scale-out). Tenants,
   API keys, and **usage_events** (append-only metering rows per lifecycle
   transition — billing can be computed retroactively from day one) are
   row-level and live only in the store. CGO stays disabled: the static-binary
   deployment story is load-bearing for the deploy-anywhere positioning.
4. **One-time migration, no flag day.** On first boot with an empty store,
   hearthd imports a legacy `state.json` if present and renames it to
   `.imported`; otherwise it initializes empty. `--state` remains only as the
   migration source; `--db` (HEARTH_DB, `db_path`) is the new operative config.
5. **The agent stays tenancy-dumb but tenancy-aware.** `POST /v1/vms` accepts an
   optional `tenant_id`, persisted in `meta.json` (optional field, old metas
   parse unchanged; the 7-key `/v1/vms` list shape is untouched). The agent does
   not authorize by tenant — hearthd is the policy point — but the recorded
   tenant feeds the next phase: per-tenant nftables isolation on the node.

## Alternatives considered

- **Full OIDC now**: weeks of issuer/JWKS/session work before the first machine
  consumer can integrate; rejected — keys now, OIDC later on the same tenant model.
- **403 for foreign sandboxes**: leaks existence; rejected for 404.
- **Row-level write-through from day one**: more code on every handler for zero
  observable benefit at current scale; rejected — snapshot writes match the
  proven JSON pattern, and the Store interface hides the upgrade.
- **mattn/go-sqlite3 (CGO)**: faster, but breaks the static cross-compiled
  single-binary deployment; rejected.
- **tenant_id in sandbox JSON**: breaks every recorded golden and v2 client;
  rejected — scoping behavior, not payload, carries tenancy.

## Consequences

- Pre-v4 single-token deployments work unchanged (admin context); the
  conformance suite passes unmodified against a v4 control plane.
- Quota checks are O(sandboxes) under the state lock — fine at lab scale,
  revisit with row-level queries when sandbox counts grow.
- Snapshot-per-mutation writes the whole working set per change (as JSON did);
  WAL keeps this cheap; row-level writes are the known upgrade path.
- Key revocation is immediate (per-request hash lookup); key *rotation* is
  manual (mint new, revoke old). Usage events exist from the first deployment.
