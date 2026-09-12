# Research: uffd CoW fork — lazy snapshot loading for "branch a live Odoo system"

- Status: Research track (P6). Not scheduled for implementation. Prototype gated
  on a `scripts/bench.sh` baseline run establishing current fork p95.
- Date: 2026-08-09
- Context: Sodoko's competitive differentiator is forking a live Odoo system
  (branch an Odoo + Postgres + agent stack mid-execution, get an independent
  copy in < 1 s). Today's fork copies `mem.bin` in full — for a 4 GB Odoo guest
  that is ~8 s of disk I/O before the child even starts. This document
  surveys the mechanism to eliminate that copy.
- Related: rust/agent/src/vm/mod.rs (fork path, lines 1171–1373),
  rust/agent/src/fc.rs (snapshot load API client), docs/PLAN-v4.md §P6,
  docs/ARCHITECTURE.md §5b

---

## 1. Today's fork cost

The fork path does three file operations before spawning child Firecracker
(`vm/mod.rs:1203–1216`):

```
copy_rootfs(parent/rootfs.ext4  → child/rootfs.ext4)   // cp --reflink=auto
copy_rootfs(parent/vmstate.bin  → child/vmstate.bin)   // cp --reflink=auto
copy_rootfs(parent/mem.bin      → child/mem.bin)        // cp --reflink=auto
```

`copy_rootfs` (`vm/mod.rs:1818`) calls `cp --reflink=auto src dst` and falls
back to `std::fs::copy` on filesystems that don't support reflinks.

**rootfs:** On btrfs or XFS with reflink support, `rootfs.ext4` is O(1)
metadata (copy-on-write at the block layer). On ext4 (the lab default) it is a
full byte copy of the image — 8–128 GB for Odoo templates. This is being
addressed separately by the overlayfs / image-dedup track (ARCHITECTURE.md §8).

**vmstate.bin:** CPU and device register state. Typically 1–5 MB. Negligible.

**mem.bin:** Guest RAM verbatim. Exactly `mem_mib × 1024 × 1024` bytes. For a
4 GB Odoo guest: 4 GB. At typical ssd throughput (500 MB/s sequential) this
is **~8 s of blocking I/O** on the fork call path. This is the target.

After the copies, FC is spawned and `PUT /snapshot/load` is called with
`mem_backend.backend_type: "File"`. FC reads all pages from the child's
`mem.bin` eagerly before the guest resumes — adding another full-file read on
top of the copy. The total cold-fork latency for a 4 GB guest is in the range
of 10–20 s (estimated; `scripts/bench.sh` does not yet exist).

For comparison: wake latency on the lab is ~70 ms for 256 MB guests (measured).
Fork for those small guests is faster because `mem.bin` is smaller, but the
Odoo workload is the product constraint.

---

## 2. Firecracker's userfaultfd-backed snapshot loading

Linux `userfaultfd(2)` allows a userspace process to intercept page faults on a
memory region and serve the faulting page from any source. Firecracker exposes
this via an alternate `mem_backend` type in its snapshot load API.

### The API hook

Today Felucca sends (`fc.rs:204–233`):

```json
{
  "snapshot_path": "/path/to/vmstate.bin",
  "mem_backend": {
    "backend_path": "/path/to/mem.bin",
    "backend_type": "File"
  },
  "resume_vm": true,
  "network_overrides": [...]
}
```

The Uffd variant substitutes `"Uffd"` for `"File"` and changes `backend_path`
to be the path of a Unix domain socket where an external page-fault handler
process is listening:

```json
{
  "snapshot_path": "/path/to/vmstate.bin",
  "mem_backend": {
    "backend_path": "/path/to/uffd-handler.sock",
    "backend_type": "Uffd"
  },
  "resume_vm": true,
  "network_overrides": [...]
}
```

**Uncertainty note:** The field names `backend_type: "Uffd"` and the UDS
listener/sender convention are inferred from FC's public API design and
prior knowledge of the userfaultfd feature. The lab runs FC 1.16 which is
known to include userfaultfd support. However, I could not fetch
`firecracker/docs/snapshotting/handling-page-faults-on-snapshot-restore.md`
from the FC GitHub repository during this session. **The exact JSON field
names, the UDS wire protocol (who connects to whom, how the uffd fd is
transferred), and the specific `UFFDIO_*` operations FC expects from the
handler must be verified against that document and the FC source before
any prototype code is written.** A reference handler implementation ships in
the FC repository (historically under `tools/` or `tests/`); use it as the
authoritative wire-protocol reference.

### What FC does with the Uffd backend

When `backend_type: "Uffd"` is used:

1. FC registers the guest memory region with `userfaultfd(2)`.
2. FC connects to (or accepts from — see above caveat) the handler UDS and
   transfers the uffd file descriptor.
3. FC starts the guest immediately with all memory pages in an unresolved state.
4. On every page access by the guest that touches an unloaded page, the kernel
   sends a `UFFD_EVENT_PAGEFAULT` notification to the handler.
5. The handler reads the requested page (8-byte address + length) from
   `parent/mem.bin` at the corresponding offset and calls `ioctl(uffd_fd,
   UFFDIO_COPY, ...)` to inject the page into FC's anonymous guest memory.
6. Once injected, the page lives in FC's anonymous mmap. Guest writes are
   private to that FC process — no further handler involvement.

The Rust `userfaultfd` crate (`crates.io/crates/userfaultfd`) provides safe
wrappers around these ioctls and is the natural choice for the handler binary.

### Net effect on fork latency

With the Uffd backend:

- `copy_rootfs(parent_mem, child_mem)` is **skipped** — the child has no
  `mem.bin` of its own; the handler serves pages from the parent's file.
- FC spawns and `PUT /snapshot/load` returns within milliseconds (no file I/O
  at load time).
- The guest begins executing from the snapshot moment immediately.
- Pages are faulted in lazily as the guest's working set is accessed.

For a 4 GB Odoo guest whose active working set at fork time is ~200 MB (the
running Odoo + Postgres processes + hot pages), only those ~200 MB are ever
faulted in. The remaining 3.8 GB stay on disk and are never read.

---

## 3. Design sketch for Felucca

### Handler topology

One handler process per fork lineage (one parent, N direct children). The
handler:

- Opens `instances/<parent_id>/mem.bin` with `O_RDONLY` on startup. This file
  becomes immutable for the lifetime of the handler.
- Creates one UDS per child at `instances/<child_id>/uffd.sock`.
- Accepts a connection from each child's FC process on that socket.
- Receives the uffd fd from FC (or sends it — to be confirmed against FC docs).
- Runs a poll/epoll loop over all active uffd fds.
- On each fault: `pread(mem_fd, page_buf, PAGE_SIZE, fault_offset)` →
  `ioctl(uffd_fd, UFFDIO_COPY, {dst: fault_addr, src: page_buf, len: PAGE_SIZE})`.

The handler is a standalone Rust binary in `rust/uffd-handler/` (not part of
the agent binary, to allow independent restart and simpler capability
boundaries). The agent spawns it as a child process and tracks its pid.

### Modified fork sequence

```
Today:
  1. copy_rootfs(parent_mem → child_mem)    [~8 s for 4 GB]
  2. Spawn child FC
  3. PUT /snapshot/load backend_type=File   [FC reads all pages, ~4 s]

With uffd (--enable-uffd-fork flag):
  1. Spawn uffd-handler (background), wait for uffd.sock to appear
  2. Spawn child FC
  3. PUT /snapshot/load backend_type=Uffd, backend_path=child/uffd.sock
     [FC connects to handler, starts in <10 ms]
  4. Guest resumes; pages fault in lazily from parent/mem.bin
```

The `vmstate.bin` copy (`copy_rootfs(parent_vmstate → child_vmstate)`) is kept
— it is small (1–5 MB) and required for FC to load the CPU/device state.

### Memory accounting

The parent's `mem.bin` is a shared read-only source. From the OS perspective:

- Handler reads pages via `pread` → OS page cache holds the data.
- Multiple children faulting the same offset → the same cache pages are
  returned to all of them via `UFFDIO_COPY` (data is memcpy'd into each child
  FC's anonymous memory region, not shared at the physical-page level by this
  mechanism).
- After `UFFDIO_COPY`, each child owns a private anonymous copy of each
  populated page. Guest writes modify that private copy.

**True physical-page sharing** (one physical page backing N guests until one
writes) is not provided by the `UFFDIO_COPY` mechanism. The handler could
attempt it via `MAP_SHARED` mmap of `parent/mem.bin` as the copy source, which
may result in shared physical pages via the page cache for clean pages — but
this is kernel-version-dependent, not guaranteed, and should not be relied on
for capacity planning.

**Overcommit:** With N children, total RSS in the worst case is
N × mem_mib (each child touches all pages). In the Odoo-fork scenario, children
are expected to diverge slowly (each running its own user session), so real
working-set overlap should be high. Measure before drawing conclusions.

### What "CoW" means here

The `UFFDIO_COPY` model is **lazy copy-in, private thereafter**:

- A page is "shared" only in the sense that the handler serves it from one
  source (parent/mem.bin) to multiple children on demand.
- Once copied into a child's address space, it is fully private. The child
  modifies it freely; there is no further coordination with the handler or
  other children.
- This is NOT copy-on-write in the mmap(MAP_PRIVATE) sense (where a physical
  page is shared until a write triggers a kernel copy). The `UFFDIO_COPY` path
  always produces a private copy at first access.

The practical benefit is not physical sharing but **time**: avoiding the
upfront memcpy and letting FC start before any page is read.

### Interaction with parent lifecycle

**Parent DELETE while children exist:** Must be refused (409 InvalidState) for
as long as any uffd handler referencing `parent/mem.bin` is alive. If the
parent is deleted, the handler loses its file descriptor; subsequent child
faults receive an error from `pread` and the child hangs or crashes. The agent
must track a `uffd_child_count` per parent VM and reject DELETE until it
reaches zero.

**Parent sleep (new snapshot) while children exist:** Sleep calls
`PUT /snapshot/create` which overwrites `vmstate.bin` and `mem.bin`. Overwriting
`mem.bin` while the handler is reading it produces corrupted pages for in-flight
faults. The agent must refuse parent sleep (409) while `uffd_child_count > 0`.
Wake (reading the existing snapshot) is safe — it is read-only.

**Child DELETE:** Agent kills the child FC process, decrements
`uffd_child_count` on the parent. When count reaches zero, the handler process
may be terminated and the parent is free to sleep or be deleted again.

---

## 4. Risks and unknowns to prototype

| Risk | Severity | Notes |
|---|---|---|
| FC uffd UDS wire protocol unverified | Blocking | Read `firecracker/docs/snapshotting/handling-page-faults-on-snapshot-restore.md` first; do not guess |
| Handler crash = child hangs on next fault | High | Handler must be supervised (tokio restart); children must be killed if handler exits uncleanly; set a fault-response timeout |
| `mem.bin` mutation guard | High | Parent sleep/snapshot must be refused while handler is live; needs atomic reference count check in agent |
| Parent DELETE race | High | Same reference-count guard; must be enforced under the inner lock |
| Fork-of-fork chains | Medium | A child that sleeps gets its own `mem.bin`; if it is then forked, it would need its own handler referencing its own file. Two-level chains work structurally but increase handler count. Forbid for the prototype. |
| Balloon device deflation | Medium | If the guest has a virtio-balloon device that has deflated pages, those page offsets may not be present in `mem.bin`. The handler would serve stale data. Felucca's guests do not currently use balloon devices; verify before enabling on custom templates. |
| NUMA effects | Low | Pages served by the handler are allocated on the handler's NUMA node; on NUMA hosts this adds remote-memory latency on every first fault. Non-issue in the current flat-memory lab. |
| Overcommit exhaustion | Medium | Operator must understand that N uffd children of a 4 GB parent can consume up to N × 4 GB RAM in the worst case. Need an overcommit limit or monitoring metric. |
| bench.sh does not yet exist | Blocking | Cannot claim improvement without a before number. |

**Target:** fork p95 < 100 ms for a 4 GB guest (vs. current estimated ~10–20 s,
unmeasured). The 100 ms target is chosen to match wake latency order-of-magnitude
and to be sub-perceptual for the Sodoko "branch this Odoo" UX.

---

## 5. Recommended prototype scope

**Constraints for the prototype:**

- Single-level fork only (children may not themselves be uffd-forked).
- Parent is kept in `sleeping` state for the lifetime of any uffd child
  (simplest correctness model: parent's `mem.bin` is stable because parent
  is sleeping; no snapshot-overwrite race).
- Gated behind `--enable-uffd-fork` agent config flag, default false.
- No changes to the feluccad control-plane API — the fork endpoint is unchanged.

**Deliverables in order:**

1. **`scripts/bench.sh`** — establish p50/p95 baselines for create, wake, and
   fork with the current File backend across 256 MB and 4 GB guest shapes.
   This is the before number; nothing else proceeds without it.

2. **`rust/uffd-handler/`** — standalone Rust binary:
   - Opens `parent/mem.bin` as a read-only mmap.
   - Creates a UDS at a path passed on the command line.
   - Waits for FC to connect and transfer the uffd fd (exact protocol per FC
     docs).
   - Runs an epoll loop; serves `UFFDIO_COPY` responses from the mmap.
   - Exits cleanly when the UDS is closed (child FC died); exit code propagated
     to the agent.

3. **Agent changes in `rust/agent/src/vm/mod.rs`:**
   - When `enable_uffd_fork` config is set:
     - Skip `copy_rootfs` for `mem.bin`.
     - Spawn `uffd-handler` with parent mem path and child uffd.sock path;
       wait for socket to appear (same poll loop as fc.sock).
     - Send `backend_type: "Uffd"` and `backend_path: child/uffd.sock` in the
       snapshot load request (new branch in `fc::build_snapshot_load`).
   - Add `uffd_children: usize` field to the `Vm` struct in the inner state.
   - Increment on child creation; decrement on child deletion.
   - Refuse parent `sleep` and `DELETE` with 409 while `uffd_children > 0`.
   - Kill handler process when `uffd_children` drops to zero.

4. **Conformance additions:**
   - Fork with uffd flag; verify child executes correctly (exec round-trip).
   - Verify parent DELETE with live uffd child returns 409.
   - Verify parent sleep with live uffd child returns 409.
   - Verify child DELETE decrements count; parent operations succeed after.

5. **`scripts/bench.sh` re-run** after implementation to record the after
   number for the comparison table.

**Out of scope for the prototype:**

- Multi-level fork chains.
- Handler auto-restart / fault-response timeout (add in follow-on once basic
  flow works).
- Memory accounting / overcommit limits.
- Production validation on NUMA or balloon-equipped hosts.
- Removing the `--enable-uffd-fork` flag (promote to default only after
  soak-verified on bare metal with the Odoo template).
