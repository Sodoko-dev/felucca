# ADR-0002 — No Kubernetes control plane: a single Zig binary + SQLite

- Status: Accepted
- Date: 2026-06-10
- Context: supersedes the k3s + CRDs + operator + Postgres control plane in ADR-0000 (baseline).
- Related: ADR-0001 (Zig backend), `docs/PLAN.md`.

## Decision

The Hearth lab control plane is a **single Zig binary (`hearthd`) backed by SQLite (WAL mode)**. It is
**not** Kubernetes. There are no CRDs, no operator/controller-runtime, no k3s on the control path, and
no etcd/Postgres in the lab. `hearthd` owns the REST API, node registry, scheduler, a periodic
reconcile loop, and static-UI serving. k3s already running on the worker VMs is **ignored** by Hearth.

## Decision drivers

1. **No Zig Kubernetes ecosystem.** There is no Zig client-go, no controller-runtime, no CRD codegen.
   Building an operator in Zig means reimplementing the Kubernetes machinery itself. The reconcile
   pattern, by contrast, is ~200 lines of Zig (diff desired vs actual rows in SQLite, call agents).
2. **"Sandboxes are not pods" — strengthened.** The baseline's core thesis (ADR-0000 §1) is that a node
   agent drives Firecracker directly on the hot path, *bypassing* the kube-scheduler. With no k8s at
   all, there is no scheduler to bypass and no slow declarative path to fight — the design is simpler
   and the hot path (PLAN §3) is unobstructed by construction.
3. **Operational weight vs lab scale.** k3s + CRDs + operator + Cilium + Postgres is six stateful
   distributed systems to operate before one microVM boots. The lab is two worker VMs and one control
   VM; SQLite on a single control node is correct-by-scale. The relational queries the baseline wanted
   Postgres for (quotas, billing, audit) are fully served by SQLite at this size.
4. **Single static binary deployment** (see ADR-0001). `hearthd` is one file + one SQLite file. Backup
   is `cp hearth.db`. No cluster to bootstrap, upgrade, or babysit.
5. **Firecracker control does not need k8s.** Everything Hearth does — boot, snapshot, restore, fork,
   tap networking, warm pool — is the node agent talking to Firecracker over UDS. Kubernetes/Kata
   RuntimeClass would *abstract away* exactly the snapshot/fork control the product depends on.

## Alternatives considered

- **k3s + CRDs + operator + Postgres (baseline).** Stable declarative surface for `kubectl` and a
  future Terraform provider; one-command agent join via k3s. **Rejected for the lab:** no Zig operator
  ecosystem; heavy multi-service operation before any value; abstracts away Firecracker snapshot/fork
  control; overkill for a 3-VM lab. It remains the *scale-out target's* reference, not the lab's.
- **Nomad.** Lighter than k8s, pluggable task drivers. **Rejected:** still an external orchestrator
  with no Zig integration; a custom Firecracker driver is as much work as the direct agent, with less
  control over the hot path.
- **etcd-only / raw KV.** **Rejected:** no relational queries for quotas/audit; another distributed
  system to run; SQLite is simpler and sufficient at lab scale.

## Consequences

**Positive:** minimal moving parts; hot path unobstructed; trivial deploy/backup; relational state
without a DB server; the lab can boot a microVM end-to-end (PLAN Phase 1) in days, not after standing
up six subsystems.

**Negative / risks:**
- **Single control node = SPOF in the lab.** Acceptable for a lab; HA is a deliberate scale-out swap.
- **Reconcile loop is bespoke code we own and test** — we trade the operator's batteries-included
  reconciliation for code we maintain. Kept small and covered by Phase-2+ acceptance tests.
- **No `kubectl`/CRD surface for ecosystem tooling.** The REST API (PLAN §6) is the only surface;
  a Terraform provider targets it directly. Acceptable and arguably cleaner.

## Scale-out path (how this grows without rearchitecting)

State lives in a DB behind an interface, and the control plane is otherwise stateless:

1. **SQLite → Postgres:** swap the DB driver; schema is already relational.
2. **Single `hearthd` → HA:** run N `hearthd` behind a load balancer once on Postgres; no other change.
3. **Dynamic fleet:** the node registry already supports one-command join (PLAN Phase 7) — bare-metal
   hosts become capacity by running `hearth-agent`. No CRDs needed for this.
4. **If a declarative/`kubectl` surface is ever required**, it can be layered as a thin adapter over the
   stable REST API rather than rebuilt as the control plane's foundation.

The lab deliberately chooses the simplest control plane that boots a microVM today, with each shortcut
defined as an additive swap-point rather than a dead end.
