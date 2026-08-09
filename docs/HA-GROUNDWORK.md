# P6.4 HA Groundwork — Readiness Assessment

- Status: Research / not a design commitment
- Date: 2026-08-09
- Context: Audit of what it would cost to make hearthd highly available today,
  and what the cheapest credible first step is. This is not a decision to pursue
  HA — it is an honest answer to "could we, and at what price?"
- Related: ADR-0002 §Scale-out (the sanctioned path), ADR-0004 (Store interface),
  go/internal/store/store.go, go/internal/state/state.go

---

## 1. What is already HA-ready

### Store interface boundary

`go/internal/store/store.go` defines the `Store` interface with `SaveSnapshot` /
`LoadInto` for the whole working set plus row-level operations for tenants, API
keys, join tokens, WireGuard peers, templates, and usage events. The SQLite
implementation sits entirely behind this boundary. Swapping to a Postgres
implementation is a driver change; no handler code changes are required.

### Transactional SaveSnapshot

Every hearthd mutation goes through a single SQLite transaction wrapping the
serialised `State`. WAL mode means concurrent reads never block. There is no
risk of partial-state corruption from a crash mid-write: the SQLite journal
either commits the whole snapshot or rolls it back.

### Stateless gateway

`hearth-gw` holds no durable state — it is a reverse proxy that pulls its
route table from hearthd's API (cached, event-refreshed). Any number of gateways
can run against the same hearthd instance; none of them need to fail over.

### Agent self-recovery

Agents re-register on start (`POST /api/v1/agents/register` is idempotent by
hostname) and reconcile `instances/*/meta.json` at startup. A hearthd failover
that completes within the 15-second heartbeat timeout is invisible to agents:
they simply resume heartbeating to the new address. Sleeping sandboxes survive
the gap intact — the agent owns their on-disk state.

---

## 2. The actual blockers (honest inventory)

### In-memory working set + single global lock

`State` in `go/internal/state/state.go` holds all sandboxes and nodes in Go
slices under a single `sync.Mutex`. Every read and write serialises through
this lock. This is correct for one process. It makes two concurrent hearthd
instances sharing the same database impossible without external coordination:
they would each load `State` into their own address space, apply mutations
independently, and race on `SaveSnapshot`.

### Snapshot-granularity persistence

`SaveSnapshot` serialises the entire `State` struct as one SQLite transaction.
Two hearthd writers racing on this call would produce last-writer-wins semantics:
the first writer's committed changes are silently overwritten by the second.
Until mutations become row-level `INSERT`/`UPDATE` operations on individual
sandbox and node rows, two concurrent hearthd processes cannot share a database
safely.

### IP/port allocators in memory

Sequence counters (`seq`, used for ID generation) and the node registry live in
`State` and are subject to the same snapshot-race described above. (Slot
allocation — `claim_slot` / `free_slot` — is agent-local and is not hearthd's
concern, but a second hearthd assigning the same sandbox ID would corrupt state.)

### WireGuard hub is a singleton

hearthd IS the WireGuard hub: it holds the overlay private key, manages peer
registrations via the join-token flow, and configures the `wg` interface at
startup. Two hearthd instances cannot both act as hub without either
active-passive failover (one holds the key and interface at a time) or a
redesign of the overlay topology (external WG hub, or a floating VIP). This
is the stickiest structural constraint.

### Join tokens and images on local disk

WireGuard state is persisted in `wg.json` on the hearthd host's filesystem.
Template images live in `images/` on the same host. A standby hearthd on a
different machine has neither: it must either share a network filesystem or
have an out-of-band sync. Image distribution is already solved for the
agent→agent case (ADR-0008 pull-and-cache), but not for control-plane
failover.

---

## 3. The staged path

### Stage 1 — Litestream replication (zero code, deployable today)

Litestream is a SQLite replication sidecar that streams WAL frames to an
object store (S3, R2, GCS, or a local replica path). No hearthd code changes
are required.

| Property | Value |
|---|---|
| RPO | Seconds (WAL frame streaming interval, configurable) |
| RTO | Minutes (manual restore + start on a standby host) |
| Code delta | Zero |
| Effort | ~1 day (config + restore runbook + smoke test) |
| What it does NOT give you | Automatic failover, zero-downtime, second writer |

This does not make hearthd HA. It makes the database recoverable with a small
data-loss window. For most early-customer SLOs (99.9% ≈ 8 h downtime/year)
a practised manual failover from a litestream replica is competitive.

#### Concrete config

`/etc/litestream/litestream.yml`:

```yaml
dbs:
  - path: /var/lib/hearth/hearth.db
    replicas:
      - type: s3
        bucket: my-hearth-backups
        path: hearth/db
        region: eu-central-1
        # For a simpler single-standby topology, use type: file
        # with a shared NFS or DRBD mount instead:
        # type: file
        # path: /mnt/hearth-standby/hearth.db
```

`/etc/systemd/system/litestream.service`:

```ini
[Unit]
Description=Litestream SQLite replication for hearthd
After=network.target
Before=hearthd.service

[Service]
ExecStart=/usr/local/bin/litestream replicate \
    -config /etc/litestream/litestream.yml
Restart=always
User=hearth
Group=hearth

[Install]
WantedBy=multi-user.target
```

Make hearthd depend on litestream so the replica is running before writes begin:

```ini
# In /etc/systemd/system/hearthd.service [Unit] section:
Requires=litestream.service
After=litestream.service
```

**Restore runbook (on failover host):**

```sh
# 1. Install litestream and copy /etc/litestream/litestream.yml.
# 2. Restore the latest replica:
litestream restore \
    -config /etc/litestream/litestream.yml \
    /var/lib/hearth/hearth.db

# 3. Copy wg.json and the images/ directory from the failed host
#    (or from the backup that wraps both) — these are NOT in the DB.
rsync -a failed-host:/var/lib/hearth/wg.json /var/lib/hearth/
rsync -a failed-host:/srv/ignis/images/      /srv/ignis/images/

# 4. Point DNS / load-balancer at the standby host.
# 5. Start hearthd; agents re-register within 5 s.
systemctl start litestream hearthd
```

### Stage 2 — Postgres swap (the big refactor)

Replace `modernc/sqlite` with `pgx/v5` (pure-Go, preserves the static-binary
story) behind the `Store` interface. Replace `SaveSnapshot` / `LoadInto` with
row-level `INSERT ON CONFLICT DO UPDATE` and `UPDATE` on individual sandbox and
node rows, one per handler mutation.

| Property | Value |
|---|---|
| Unlocks | Two hearthd processes can share the DB without racing on a snapshot transaction |
| Effort | ~2 weeks (schema migration, row-level writes on every handler path, pgx connection pool, integration tests against real Postgres) |
| What it still does NOT give you | Leadership for reconcile sweeps, WG hub coordination, shared images |
| Code delta | Large — every handler that today calls `SaveSnapshot` gets two or three targeted DB calls instead |

This is the load-bearing step that ADR-0002 always pointed at. Nothing in Stage
3 is possible without it.

### Stage 3 — N×hearthd active-active (requires Stage 2)

Two or more hearthd processes behind a load balancer.

Additional work beyond Stage 2:

- **Reconcile sweep coordination.** The idle-sleep and asleep-delete sweeps
  must fire once per interval, not N times. Options: distributed lock (Postgres
  advisory lock, Redis), designated leader via lease row, or partition sandboxes
  by tenant across instances.
- **WG hub redesign.** Options: a single floating WG VIP managed by keepalived;
  move hub duties to a separate lightweight process; or accept active-passive
  (one hearthd holds the WG interface, others proxy).
- **Shared images store.** Replace the local `images/` directory with an object
  store (or NFS) so any hearthd instance can serve image pull requests.
- **ID generation.** Replace the in-memory `seq` counter with a Postgres
  sequence or UUID generation to avoid duplicate IDs across instances.

| Property | Value |
|---|---|
| Effort | ~2–3 weeks on top of Stage 2 |
| Prerequisites | Stage 2 complete, Postgres provisioned, object store for images |

---

## 4. Recommendation

Stay single-hearthd until a customer SLO demands otherwise. This is the
deliberate design (ADR-0002) and it has held through all v4 phases without
incident.

**Deploy Litestream today.** It is operationally invisible (a sidecar), requires
zero code changes, and gives you a point-in-time-recoverable database for the
cost of one config file and an S3 bucket. The restore runbook above is the
entire disaster recovery procedure. Do this before the first paying customer.

**Start Stage 2 when one of the following is true:**

1. A paying customer requires an explicit HA SLO that manual failover cannot
   satisfy (e.g., RTO < 60 s).
2. Sandbox count exceeds ~10,000 active, at which point the O(N) quota scan
   under the state lock becomes measurable in request latency.
3. The engineering team has a week to dedicate to the Store refactor without
   disrupting ongoing product work.

Stage 3 follows naturally from Stage 2 when traffic volume justifies it; no
architectural dead ends exist between here and there.
