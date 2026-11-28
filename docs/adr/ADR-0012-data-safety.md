# ADR-0012 — Data safety: snapshots, saves and hostnames that mean what they say

Status: accepted (stability wave 1, 2026-11-27). Closes the findings of the
2026-11 stability audit in which Felucca told a caller, a gateway or itself
something that was not true, and then acted on it: FLU-4, FLU-70, FLU-72,
FLU-5 (with FLU-74, FLU-82 and FLU-83), FLU-6, FLU-7, FLU-73 and FLU-8, and
two found on the way: FLU-78 (placement onto a worker whose fences are down)
and FLU-86 (a Firecracker taken for ready before its API answered).

Tested on Ubuntu 24.04: systemd 255, nftables 1.0.9, kernel 6.8,
Firecracker 1.16, SQLite through modernc.org/sqlite. The behaviour this ADR
states as measured — exec while the guest bridge is down, the window during
which a deleted table is open, how fast the bridge goes down and comes back,
the host fence's effect on `isolation`, a tap without a Firecracker showing
`NO-CARRIER`, the marker as `nft` lists it, a roll-back of feluccad onto the
database the new one wrote — was measured on that lab and nowhere else.

Terms used below, one meaning each:

- **record**: on a worker, a sandbox's `meta.json` (and the agent's copy of
  it in memory); in feluccad, the sandbox row.
- **VMM**: the Firecracker process of a sandbox.
- **snapshot files**: `vmstate.bin` (device state) and `mem.bin` (guest
  memory) in the sandbox's instance directory.
- **busy**: the sandbox has a lifecycle operation in progress on its agent.
- **store failing**: feluccad's last write to its database did not succeed,
  and no save has succeeded since.
- **node port**: a port in 20000–29999 on a worker that the agent forwards
  (DNAT) to one sandbox's guest port; an expose is a route row naming it.
- **quarantined port**: a node port feluccad has released less than 900 s ago.

## Context

**A dead VM was woken from an old memory snapshot (FLU-4).** At start-up the
agent called a sandbox `sleeping` whenever a `vmstate.bin` lay in its
directory. A sandbox that had slept and been woken once kept that file; when
its VMM later died (a host reboot, an OOM kill), it came back `sleeping`, and a
wake restored the old memory over a disk the guest had written since. The
guest's file system was corrupted without a word. The inferred state was not
even written to `meta.json`.

**A failed snapshot looked like a good one (FLU-70).** When Firecracker's
`snapshot/create` failed (a full disk, say), it left a complete-looking state
file and a full-size, partly written memory file behind, and the guest stayed
paused under a record that said `running`. A later wake took the files for a
snapshot. A sleep also killed the VMM before the record said `sleeping`, so an
agent stopping in between lost a complete snapshot.

**A forked sandbox ran on its parent's disk (FLU-72).** A snapshot records the
absolute path of the root drive, and `snapshot/load` has no override for it.
Every fork child therefore opened its PARENT's `rootfs.ext4`; the copy in the
child's directory was never used while the child ran, and the two guests wrote
to one file system from two kernels. The copy itself was made after the parent
had been resumed, so it did not even match the child's memory.

**Writes were acknowledged that the database never got (FLU-5).** Every save
site logged a failed save and answered success anyway. With the database
locked by another process, read-only or on a full disk, creates, sleeps,
exposes and deletes were acknowledged, and a restart lost them. One heartbeat
carrying a memory figure of 2^63 or more made every later save fail, because
the value cannot be bound as a signed SQLite integer (FLU-74). A tenant and its
first key, and a node credential and the join token it consumed, were two
separate writes that could half-succeed (FLU-82). A database read that failed
was treated as "no quota", "no lifecycle policy" or "wrong key" (FLU-83); the
last one fed the brute-force throttle, so a short read outage locked valid
clients out for longer than it lasted.

**A deleted sandbox's hostname was served by another sandbox (FLU-6).** The
agent handed out the lowest free node port. Delete sandbox A on a worker and
the next expose on that worker — any tenant's — got A's port at once, while
every gateway still routed A's hostname to it until its next refresh, and for
as long as the control plane was unreachable, without limit. Pooled upstream
connections in the gateway were keyed by `node:port` alone, so a connection the
worker still pinned to A could carry a request meant for the new owner. The
agent also freed a deleted sandbox's guest address before it rebuilt the rules
that named it.

**Waking a sandbox sent its second service's requests to its first (FLU-7).**
The gateway coalesces the requests for one sleeping sandbox into one wake, and
handed every waiter the route of the request that started it. A sandbox with
two exposed services, woken by requests for both, answered all of them from
one. The wake's answer was ignored, so a refused wake cost 15 table fetches.

**The gateway printed the admin token (FLU-73).** `--token` took
`$FELUCCA_TOKEN` as its flag default, and `felucca-gw --help` prints defaults.

**Nobody knew a worker had lost its fences (FLU-8).** The agent built its nft
rules at start-up and on membership changes, and never looked at the kernel
again. With its tables gone (`nft flush ruleset`, a distribution
`nftables.service` restart, another tool), another tenant's guest was
reachable at once, egress broke, `/healthz` kept saying `"isolation":true`,
and only an agent restart put things back. The heartbeat carried
`isolation_ok`; feluccad ignored it. An `nft` or `ip` command had no time
limit.

## Decision

### A sandbox is asleep only when its record says so (FLU-4)

**The agent never derives `sleeping` from files.** At start-up a record in
`running` or `paused` whose VMM is gone becomes `stopped`, whatever files lie
beside it, and that state is written to `meta.json`. The guest's memory went
with its VMM; no file can bring it back.

**`meta.json` gains one optional field, `snapshot_valid`.** Only a sleep that
finished writes it, as `true`, after the snapshot is complete and on disk.
Every transition out of `sleeping` clears it. It is serialized only when true,
as the last key, so every other record keeps the bytes it had, and an older
agent, which ignores keys it does not know, reads the file as before.

**Records written by an older agent are judged by their disk.** A `sleeping`
record without the field is kept when both snapshot files exist and
`rootfs.ext4` was not modified after `vmstate.bin`. A disk modified later
(strictly, at the file system's own precision) means the guest ran after that
snapshot: the record becomes `stopped`, and the snapshot is set aside (below).
A record whose files cannot be examined is left as it is for that start, and
the wake makes the same judgement again before it runs.

**Stale snapshots are set aside, not deleted.** The verdict rests on file times
(or, for a wake, on an answer that never came), and a wrong one — after a
restore that lost the times, say — must not destroy every old snapshot on a
node at once. The two files are renamed `vmstate.bin.outrun` and
`mem.bin.outrun`. Nothing takes them for a snapshot. `start`, `stop`, the next
sleep and `delete` (with the directory) remove them; start-up leaves them.

**One lifecycle operation at a time per sandbox.** Sleep, wake, start, pause,
resume and fork mark the sandbox busy for their whole duration; the mark is
released on every exit path. A second request of the same kind waits up to
25 s for the one in progress and then answers from the state it finds (a
retried sleep finds the sandbox asleep and says so); 25 s is below feluccad's
30 s worker-call timeout, so a waiter never outlives its caller. A request of
a different kind answers `409 {"error":"busy"}` at once. A fork never waits: a
fork queued behind another operation would act on a state its caller never
saw. Forks of a sleeping parent only read its files, so any number of them run
side by side. Stop and delete do not queue: they mark the sandbox, kill every
Firecracker of that instance directory (which makes the call in progress
fail), and wait up to 15 s for the operation to let go. If it does not, they
answer 409 with `"state":"stopped"` — the stop has already written that
record. An operation that finds, when it comes to commit, that its sandbox is
being stopped or deleted or no longer holds its VMM, does not commit. exec,
expose and unexpose are not lifecycle operations in this sense.

**Each verb accepts only the states it can act on**, and a refusal names the
state: sleep from `running` or `paused` (`sleeping` is an idempotent success);
wake from `sleeping` (`running` is an idempotent success); every other state
answers `409 {"error":"InvalidState: …","state":"<state>"}`. The `state` key is
a contract: feluccad records the state a worker names in a refusal, if its own
record still shows the state the request started from, queues the matching
usage event, and forwards the 409. Without that, a record the worker no longer
agrees with — every sandbox that was running at a worker reboot — stays wrong
for ever. The same holds for a fork and a delete the worker refuses: feluccad
records the state the worker names. A refused fork is relayed as 409 (below).
A delete the worker refuses for now — its answer begins `busy`, an operation
in progress has not let go of the VM — is relayed as 409 with `Retry-After: 2`
and the record is kept; any other delete refusal stays the 502 "agent delete
failed", with the worker's text logged rather than relayed, because it can
name worker internals such as a process id.

**A record still `creating` takes no lifecycle verb.** feluccad answers sleep,
wake, stop, start, pause and resume of it with 409 and `"state":"creating"`,
without calling the worker: the create's last save would overwrite whatever the
verb recorded. Delete stays allowed. A record that a stopped or killed
feluccad left `creating` comes back as `error` at the next start, with a
warning that a VM may exist on its worker: no create of the old process runs
in the new one, and in `creating` the record would refuse every verb for
ever, while `error` lets its owner `start` or `delete` it. Such a record may
have no guest address: its first successful start, wake or resume (or a
refusal that records it `running`) takes the address from the worker's list
of VMs, since without it expose and routing do not work. When the worker
cannot be asked, the request still succeeds, with a warning, and the next
start or wake tries again.

**A wake makes the record say `running`, on disk, before the guest runs.** It
checks that both snapshot files exist and are not empty (a missing one makes
the record `stopped` and answers 409); loads the snapshot without resuming;
for a record without `snapshot_valid`, judges it as start-up does; writes
`running`; resumes; removes `vmstate.bin`. A failed load or a failed record
write kills the new VMM and leaves the record `sleeping` with its files. A
resume that Firecracker refuses while it still reports the VM paused does the
same: the guest never ran. A resume that got no answer at all (the VMM died,
or the reply was lost) may have run the guest: the VMM is killed, the record
becomes `stopped`, and the snapshot is set aside.

**Snapshot files exist only beside a `sleeping` record.** `stop` removes them
once the VMM is gone (stopping a sleeping sandbox discards its snapshot),
`start` before its cold boot, and a VMM that dies under the agent takes them
with it. Start-up removes `vmstate.bin.tmp` always, and the snapshot files of
every record that is not `sleeping`. `mem.bin` is never removed while a live
VMM of that sandbox maps it (checked by device and inode in
`/proc/<pid>/maps`; "cannot tell" means keep). This is also what makes a
rollback to the previous agent binary safe: it still infers `sleeping` from
`vmstate.bin`, and that inference is now true for every record the new agent
has touched.

### A snapshot is complete, durable and marked before anything relies on it (FLU-70)

**The order of a sleep:** pause; `snapshot/create` with the state file under
the temporary name `vmstate.bin.tmp`; fsync the disk image the VMM has open as
its root drive; rename the state file to `vmstate.bin`; fsync the directory;
write `sleeping` with `snapshot_valid` to `meta.json`; only then kill the VMM.
Firecracker fsyncs both snapshot files itself; the disk and the directory
entry are what it does not. So a `sleeping` record on disk means the snapshot
beside it is complete and durable, and an acknowledged sleep survives a power
cut.

**Any failure before the kill is undone with the guest still alive:** the
snapshot files are removed (`mem.bin` only when no live VMM maps it), the
guest is resumed if this sleep paused it, the record is unchanged, and the
answer is the error. If the resume fails too, the record becomes `paused`. A
guest resumed after a failed `snapshot/create` works at once; left paused under
a record that says `running`, nothing would ever resume it.

**Fork reserves the child first.** The child id is inserted as a `creating`
record and its directory created exclusively, before anything is written; an
id or a directory that exists answers `409 {"error":"AlreadyExists"}`. The
snapshot of a running or paused parent is written straight into the child's
directory, under the temporary name, so the parent's directory never holds
snapshot files and two forks never write over one pair of files. A failure
before the child has a VMM removes the reservation, the directory and the
network slot; a failure after leaves the child `error`, without its snapshot
files.

### A fork child runs on its own disk (FLU-72)

**The disk is copied while the parent is paused,** at the same instant as the
memory: pause (if running); `snapshot/create` into the child's directory;
`cp --reflink=auto` of `rootfs.ext4` into the child's directory, as a child
process a stop or delete of the parent can end; resume. A sleeping parent has
no VMM to pause; its three files are copied while it is held.

**The child's VMM is given its own disk before it runs:** spawn; load the
snapshot without resuming; check with `GET /vm/config` that the loaded state
names the parent's own `rootfs.ext4` (the same file by device and inode);
`PATCH /drives/rootfs` to the child's `rootfs.ext4`; check with `GET
/vm/config` that the drive is now the child's; write the record; resume. Any
failure kills the child's VMM and leaves it `error`. The child is never resumed
on a drive that is not its own.

**A sandbox that does not run on its own disk is not forked.** Sandboxes
forked before this release run on their parent's image, and their own
`rootfs.ext4` is a copy from the moment of their fork; pairing their memory
with it would hand the child a disk that does not match. A running or paused
parent is asked for its root drive before it is paused; a sleeping parent is
caught by the child's check after the load. Either way the answer is 409
`InvalidState` with the parent's `state`, and feluccad relays it and removes
the child it had reserved: a refusal for good, so no `Retry-After`. Nothing is
left of the child on the worker either, whatever the parent's state: feluccad
forgets the id, so an `error` record kept there would never be deleted, and
each retry would leave another full disk copy.
 The text
names no path and no other sandbox, because it reaches the tenant word for
word; the log has the image. A wake does the same check: when the loaded state
names another image, the wake goes on only if that image has not been modified
since `vmstate.bin` was written; otherwise the sandbox is stopped, its snapshot
set aside, and the answer is 409 with `"state":"stopped"`. A sleep of such a
sandbox fsyncs the image it really runs on.

**A refused fork is no activity.** A fork that the worker refuses, busy or for
good, gives the parent back the idle and sleep clocks the fork had bumped: a
fork that did not happen must not keep a sandbox from its auto-sleep.

### A Firecracker is ready when its API socket accepts (FLU-86)

After starting a Firecracker — for a create, start, wake or fork — the agent
waits until its API socket accepts a connection, at most 3000 ms, where it
used to wait for the socket's file to exist: the file exists as soon as
Firecracker has bound the socket, and under load the first request arrived
before it listened and failed the whole create (`FcError: send request:
client error (Connect)`). When the wait runs out the answer is `SocketNeverAppeared: the
Firecracker API socket did not accept a connection within 3000 ms (its file
existed)` (or `did not exist`).

### A save that fails, fails the request (FLU-5, FLU-74, FLU-82, FLU-83)

**One save path.** Every change reaches the database through one function. It
takes a save mutex, then the state lock; deep-copies the working set and takes
the queued usage events; releases the state lock; and writes snapshot and
events in one transaction. The state lock is not held while the database is
busy, so a save that waits out SQLite's 5 s busy timeout no longer holds every
request behind it. A caller may pass an undo, which runs before the save mutex
is released, so no other save can store the state being undone.

**Usage events are queued with the change they describe**, in the same state
lock section, and written by the save that stores that change. Exec rows are
queued separately (at most 10 000; the oldest are dropped and counted in
`felucca_usage_rows_dropped_total`) and written by a background flusher once a
second: exec no longer waits on the database. Rows lost and write attempts
that failed (the rows stay queued) are separate counters, so an alert can tell
"retrying" from "lost".

**Store health.** A failed write marks the store failing and starts a recovery
loop that, every 2 s, opens a fresh writer connection, flushes the queued exec
rows and then saves what memory holds. Only an attempt that wrote both marks
the store healthy again, so "healthy" means the database has caught up with
memory. The exec flush runs outside the save mutex, so it does not hold
requests behind it. While the store is failing, a request's save fails as soon
as it holds the save mutex — after at most one snapshot attempt — instead of
waiting out its own busy timeout: the recovery loop is the one that retries.

**While the store is failing, every request that changes durable state is
refused before it does anything:** `503 {"error":"store unavailable"}` with
`Retry-After: 5`. Reads, exec, heartbeats, registration without a join token, the
route table, the gateway's activity report, `/metrics` and `/healthz` are not
refused. The lifecycle sweep does nothing. **Wake is refused too:** waking
anyway and recording it late can leave a running VM that the control plane
lists as asleep after a restart, with nothing to correct it until FLU-13.

**What each save site does when its save fails.** The order stays "worker
first, then save" for every verb. Create and fork save their record before the
worker is called; if that fails, the record is dropped and the worker is never
called. If the last save of a create or fork fails, the VM exists but the
caller is told 503: the record goes to `error` and, after the answer, the VM is
deleted on the worker. Delete, sleep, wake, stop, start, pause, resume, expose
and unexpose keep in memory what the worker did and answer `503
{"error":"applied but not saved","applied":true}`. Nothing is undone on the
worker: an undo of an expose could remove a rule another saved name depends
on, and every one of these verbs is idempotent on the worker, so a retry is
safe.

**No value in memory can make a save fail.** `mem_total_mib` and
`mem_free_mib` above 2^40 are refused with 400 at registration and heartbeat,
and the store clamps unsigned values to the signed range when it binds them.

**Writes that belong together are one transaction:** a tenant and its first
key; a node credential and the consumption of the join token that buys it.

**A read that fails is not an answer.** Reads go through a second, read-only
database handle. A read error during key authentication answers 503 and is not
counted by the failed-credential throttle; a read error in the quota lookup, a
template lookup, the tenant row before a key is minted, or the node join
answers 503; a listing that cannot be read answers 500; the sweep skips the
sandboxes of a tenant whose policy it could not read. Every read error is
counted (`felucca_store_read_errors_total`): a read outage leaves the write
health at 1 and `/healthz` at 200, and would otherwise be invisible.

**The database is opened with `synchronous=FULL`,** and every pragma is set in
the connection string so a replaced connection has them too. In WAL mode
`NORMAL` can lose the last commits on a power cut, and feluccad acknowledges a
write only after its commit.

**Start-up.** A database that cannot be opened or read ends the process, as
before; so does a legacy state file that is present but does not load (it used
to be a warning, after which the empty state became permanent). A database
that cannot be written does not: feluccad starts with the store failing,
serves reads and the route table, refuses changes, and keeps retrying. Every
running sandbox's ingress depends on feluccad answering the gateway, and a
write outage is no reason to take that away. A database that was read-only
when feluccad opened it becomes writable to feluccad without a restart, because
each retry opens a fresh writer connection.

**`/healthz` says whether the database has caught up:** `200 {"ok":true}`, or
`503 {"ok":false,"store":"failing"}`. It is answered before anything that
takes the state lock.

**SIGTERM and SIGINT stop feluccad with a drain.** Every listener stops
accepting at once, and the requests in flight get 10 s in all to finish, their
saves included: a create cut between the worker's answer and its last save
would leave a VM the database does not know. What is left after 10 s is cut.
Then the background loops end, the queued exec rows get a final flush (after
the drain, so the rows of the requests that finished are in it), retried once
a second until just before the 20 s limit, with one ERROR that says how many
rows were lost if it never succeeds; the exit status is 0. A second signal, or 20 s after the first, exits at once with
status 1: a flush that hangs on a locked database must not keep the process
until systemd's SIGKILL (the unit's `TimeoutStopSec=30s`).

### A hostname never reaches another sandbox (FLU-6)

**The gateway fails closed on an old table.** It stamps a table with the time
its fetch started, and never replaces a table with one whose fetch started
earlier. When the table is older than `--max-stale` (default 600 s), and
before the first successful fetch, every sandbox request answers `503
{"error":"route table unavailable"}` with `Retry-After: 5`. The age is the
larger of the monotonic and the wall-clock difference, so a suspended host
does not make an old table young. `/healthz` stays liveness; a new `/readyz`
says whether the gateway may route.

**feluccad quarantines every node port it releases.** When the last route row
of a sandbox that maps a guest port to a node port is removed — unexpose,
delete, the sweep, the undo of a create or fork — the port is recorded with
the worker host, the sandbox, the guest port, the tenant and the time, and kept
away from every other sandbox on that worker host for 900 s. The entries are
part of the saved snapshot, so a row's removal and its port's quarantine reach
the database in one transaction.

**The worker is told what to avoid.** Every expose request to an agent carries
`avoid` — every node port feluccad has recorded for that worker host (the rows
of all sandboxes on it and the quarantined ports), except the ones this same
sandbox holds for this same guest port — and, when this sandbox last held a
still-quarantined port for this guest port, `prefer`. The agent never returns
a port in `avoid`, returns `prefer` when no sandbox on the worker holds it, and
says `"avoid_honored":true`. A sleep and wake of a sandbox therefore gets its
old port back and releases nothing.

**The agent allocates round-robin** from a cursor that starts at a random port
when the agent starts, skipping ports in use and ports in `avoid`. Nothing of
the allocator is kept on disk.

**feluccad checks the worker's answer.** A port that another sandbox's row on
that worker host names means that row is stale (the worker is the authority on
who holds a port): it is removed and quarantined, the new mapping is removed
from the worker, and the caller gets 503. So is a quarantined port that another
sandbox (or this one for another guest port) released, from an agent that
said `avoid_honored`; a port this sandbox itself released for this guest port
is its own and is taken back. A port this sandbox holds for another guest port
is refused with a 503 that names the expose holding it, because only removing
that expose ends it. From an older agent, which does not say `avoid_honored`,
a quarantined port is accepted with a warning: that is the old exposure, until
the agent is upgraded. When the worker maps a guest port to one node port and
the sandbox's rows name others, the rows follow the worker and the others are
quarantined; at start-up feluccad logs an ERROR for each such split, and for
each node port that rows of two sandboxes name.

**The quarantine runs on the wall clock, and errs long.** A clock stepped
backwards lengthens a quarantine by at most the step; the remaining time a
caller is shown never exceeds 900 s. Rewriting the release time instead would
have been permanent: a later forward correction could then end a quarantine
while gateways still routed the old name. A forward step shortens every
quarantine; entries are dropped for good only 900 s plus a 60 s margin after
their release.

**Churn is capped,** because a quarantined port is lost to everybody on the
worker for 900 s: at most 512 quarantined ports per tenant per worker host (the
admin tenant is unmetered) and 64 distinct (node port, guest port) pairs per
sandbox on that host. Beyond a cap, an expose that
needs a fresh port answers 429 with `Retry-After` set to the time until the
oldest counting entry expires.

**The sweep keeps a sleeping sandbox's dynamic exposes.** It used to drop them
when it put the sandbox to sleep, releasing their ports, so a sandbox that
woke got fresh ports and quarantined more. They are now dropped once the
sandbox has been asleep for at least 900 s, only on `ready` workers, with 5 s
per worker call and 5 s of worker time per pass: the sweep holds the global
expose lock across the call, and a worker that does not answer must not stall
every expose in the fleet. A drop that got no answer in time is repeated on
later passes, also after the sandbox woke, until the worker answers: the rule
may be gone on the worker, and a row that names a dead port is worse than
none (the gateway re-creates a missing route on the next request).

**The numbers are tied together in one place** (`go/internal/ingress`):
quarantine (900 s) ≥ gateway `--max-stale` ceiling (600 s) + `--refresh`
ceiling (60 s) + feluccad's worker-call timeout (30 s) + the gateway's idle
upstream connection timeout (90 s) + a 60 s margin. A test fails when they
stop adding up. The gateway refuses to start with `--refresh` outside 1 to
60 s, or `--max-stale` outside `2 x refresh + 20s` to 600 s.

**The gateway's upstream connections are pooled per sandbox, guest port, node
host and node port,** never per `node:port` alone.

**The agent's delete rebuilds the rules before it frees the address:** the
sandbox leaves the membership, isolation and ingress rules are rebuilt, the tap
goes, and only then is the IP slot freed. If a rebuild fails, the slot is
held, and released by the first moment at which the latest isolation and
ingress rebuilds have both succeeded (a later change, or a tick of the
isolation watch below). The ingress chain is now rebuilt as one `nft -f -`
transaction, and an expose answers success only once its rule is in the
kernel. If start-up can neither rebuild nor empty the ingress chain, it may
still hold the previous run's node-port rules for addresses nobody holds now;
then no guest is placed or brought back (creates, forks, pool refills, wakes
and starts fail with `IsolationFailed`) until the chain has been rebuilt; the
rebuild is retried every 5 s in every watch mode. An expose writes `meta.json`
only once its rule is in the kernel, and two overlapping deletes of one
sandbox free its address once. The agent's expose and unexpose handlers run to
their end when the caller hangs up.

### Each wake waiter resolves its own route (FLU-7)

One wake per sandbox stays. When it is done, every waiter, the first one too,
resolves its own label again from the top, with the checks of a fresh request.
The gateway reads the wake's answer: after a 2xx it fetches the table (at most
15 times, 1 s apart) until any route of the sandbox shows it running; after a
503 with `Retry-After` it sends the wake again after that delay (at least
250 ms, at most 2 s) while 15 s last; after a 404 or a 409 it fetches the table
once and does not send another wake for that sandbox for 5 s. The shared call
is completed in a `defer`, so a panic cannot park later requests.

### The token is not a flag default (FLU-73)

`--token` has no default; `$FELUCCA_TOKEN` is read after the flags are parsed,
when the flag was not given.

### feluccad shows which workers report their isolation down (FLU-8, control-plane part)

feluccad keeps the `isolation_ok` of each node's last heartbeat in memory (not
in the database: it is only true until the next heartbeat; absent counts as
fine), logs a warning when a node starts reporting `false` and a line when it
stops, and exposes `felucca_nodes_unfenced`.

**Creates avoid a worker that reports its isolation down (FLU-78).** A create
goes to the ready worker with the fewest sandboxes among those that do not
report `isolation_ok` false (a worker that never sends it counts as fine).
Such a worker refuses new guests, so it gains none and stays the least
loaded: placement by load alone sent every create in the fleet to it, where
each failed for as long as its fence was down. Only when every ready worker
reports it down does a create go to the least loaded of all, as before, with
a warning at most once a minute; a fleet that reports nothing keeps the
placement it had. Fork, wake and start stay on the sandbox's own worker.

### Rules are checked in the kernel and put back (FLU-8, agent part)

**Every chain the agent builds starts with a marker rule,** `counter comment
"felucca:<16 hex digits>"`, the hash (FNV-1a, 64 bit) over the whole ruleset
the agent applied for that chain — for the cross-tenant forward chain, the
members of the `tenant_pairs` set included. It goes in as the chain's first
rule, so a flushed chain, a chain re-created empty, a missing chain or table,
and a chain rebuilt with other content all list without the expected marker.
The agent reads the marker with the counter's values (`counter packets N bytes
M comment "…"`, as `nft list` prints it on nftables 1.0.9) and without them
(`nft -s`). NAT, the guest-to-host input chain and the L2 chain are now built,
like the others, as one `nft -f -` transaction each.

**The watch compares, it does not recompute.** The agent remembers, per chain,
the hash it last applied (under the lock that serializes rule changes). Every
5 s the watch looks at the guest bridge, lists each table once (`nft -t list
table` of `ip felucca`, `bridge felucca`, `netdev felucca`, and `inet
felucca_host`) and compares the marker in the kernel with that hash. A
difference is drift — somebody else changed the kernel: a warning that names
table and chain, a rebuild with the current membership and the builder
start-up uses, a second look to confirm, a count. A table set `dormant` (its
chains unhooked) is drift too, and the rebuild clears the flag. The watch does
not work out what the rules should be from the membership: that is the
rebuilds' job when membership changes, so a change that has not happened yet
is not drift. A chain whose last rebuild failed is rebuilt every tick. A
failure is logged once per episode, and again when its text changes.

**What it also checks.** The switches the agent sets at start-up
(`/proc/sys/net/bridge/bridge-nf-call-iptables`, and on a kernel with IPv6
`/proc/sys/net/ipv6/conf/felucca0/disable_ipv6`) must read `1` and are set
again otherwise; a switch file that is missing (module unloaded) is broken. The
anti-spoof chain of every tap enslaved to the guest bridge that has carrier is
checked against the pin derived from its slot; a tap without carrier (no
Firecracker holds it: it shows `NO-CARRIER`) carries no traffic and is
skipped. A deleted guest bridge is not rebuilt by the watch; it is reported,
makes `isolation` false and refuses new guests until an agent restart builds
it again.

**Something is taken down only on positive evidence twice.** A look has three
outcomes — in place, positively not in place, could not look — and so does a
repair: applied; refused by the kernel (`nft` exited non-zero and its first
error line is nft's own, `Error: …` or `/dev/stdin:L:C-C: Error: …`); or
unknown (it timed out, was killed, or could not run). Something goes down only
when the first look said positively not in place **and** the repair was
refused, or it was applied and a second look still says positively not in
place. A failed look or a repair with no answer is not evidence: a command
that timed out may have committed, and a slow netlink is not a missing fence.
Such a tick is incomplete and takes nothing down; the next one tries again.
A refusal is not retried under `sudo -n` (that used to replace the kernel's
answer by sudo's own failure), and as root nothing runs under `sudo -n`.

What goes down then, in the default mode: for the input, forward or L2 chain,
a switch, or a table that stays dormant after a rebuild, the guest bridge,
until a tick confirms every fence again and raises it; for one tap's chain,
that tap, until it is pinned again (by a tick, or by a create, wake, start or
fork that pins it). NAT and ingress take nothing down, because their absence
closes traffic rather than opening it; they never enter `isolation` and never
make a tick incomplete. What the watch holds down and something else raises is
taken down again — a tap only on a look that shows its pin missing that same
tick. Guests keep running without network. Taps that start-up could not pin for
adopted sandboxes are held down the same way and pinned and raised by the
watch.

**It must first prove it can read its own markers.** Right after start-up has
applied its chains, the agent reads them back. If a chain it has just applied
is listed without a marker it can read, this build cannot read what this
kernel prints, and a watch would see every chain as missing and take the
bridge and every tap down. The watch then does not run in that process (as
with `off`), and the agent says so, as an ERROR and on stderr, in a line
beginning `isolation watch NOT started:` that names the chain and quotes its
first rule. While the watch runs, the same holds per table (`ip`, `bridge`,
`netdev`): nothing is taken down and nothing refused because of a table until
one of the agent's markers has been read back there, and a table in which an
applied chain lists without a readable marker is left alone for the rest of
the process.

**New guests are refused on a worker whose fences are not known to be in
place.** While a fence the agent owns is positively not in place and could not
be put back, while such a repair has no answer, and while the guest bridge is
gone, create, fork, wake, start and the warm-pool refill answer
`IsolationFailed` — in `enforce` and in `report` alike. Taking the bridge down
protects the guests already there; refusing new ones keeps the control plane
from placing more tenants onto an unfenced network, whatever the mode. The host
fence, a stale verdict, NAT and ingress do not refuse.

**The guest network gets an `unreachable` route,** `ip route replace
unreachable <guest net> metric 4294967295`, installed at start-up before
anything can take the bridge down, again before the watch takes it down, and
on every tick while it is down. While the bridge is up its connected route
wins; while it is down, traffic for guest addresses is answered "unreachable"
on the worker instead of following the default route out of it.

**Three modes,** `isolation_watch` / `FELUCCA_ISOLATION_WATCH` /
`--isolation-watch`: `enforce` (default), `report` (check, repair and refuse
new guests; never take anything down), `off`. An unknown value, or the flag
without one, refuses to start. `report` and `off` change the watch only:
start-up still refuses to serve, and takes the bridge down, when the fences
cannot be loaded. The default is `enforce` because a fence the agent cannot
restore should cost one worker's guest network rather than leave its tenants
on one flat network; `report` is the mode for a first roll-out (see the
alternatives).

**The host fence is reported, not repaired.** `inet felucca_host` belongs to
`felucca-firewall.service`, and ADR-0011 declined to let the agent drive
systemd. It enters `isolation` only if it was in place when the agent started
— a node without the firewall unit gets one warning, not a permanent `false`.
A fence that was there and goes missing is a warning at most once a minute and
`isolation` false; the agent's own input chain still fences guest-to-host
traffic.

**`isolation` means the watch's verdict.** `/healthz` keeps its shape
`{"ok":true,"isolation":<bool>}`, and the heartbeat's `isolation_ok` is the
same value: true when the last completed tick left no fence, switch or tap pin
unrepaired, no fence chain's latest rebuild failed, the guest bridge exists,
the host fence is there (if it was at start), and that tick is at most 45 s
old. With the watch on, no tick completed within 45 s of its start is `false`.
Before that, and with the watch off, it is start-up's verdict and that of the
last rule change. In every mode, `off` included, it is `false` while start-up
could neither rebuild nor empty the ingress chain: such a worker hands out no
guest address, and reporting it fine would draw every create to it.

**Every command has a limit, and a tick has a budget.** Every `nft` and `ip`
child the agent runs is killed, with its process group, after 10 s (`wg`
commands, which may resolve a name, after 60 s), and its answer is "unknown";
a command that timed out is not run a second time. One `nft` stuck on a
netlink lock used to hold the rule lock — and with it every create, delete,
expose and the watch — for as long as it hung. With 16 timed-out commands not
yet reaped, new ones answer "timed out" at once instead of piling up. A tick
waits up to 15 s for the rule lock (otherwise it is skipped and counted) and
works at most 15 s with it held; ticks never overlap. The verdict goes stale
after 45 s: with a shorter limit (20 s, first), a burst of queued rule changes
made a healthy worker's ticks skip until it reported `false`.

**What the watch gives.** It shortens an exposure from "until someone notices"
to seconds; it does not prevent one. A deleted table or a flushed chain is
back within about 3 to 5 s (the watch looks every 5 s), and in that window the
fence is open: on the lab, cross-tenant probes sent every 0.5 s got through 5
or 6 times while the whole `ip felucca` table was deleted, and not at all for
the deletion of a single chain. When the kernel refuses a repair, the bridge
went down within one to two ticks (1.5 s in the test) and came back within
seconds of the cause being removed. With the bridge down, exec keeps working
in a running guest (vsock), and a request to an exposed node port from another
machine fails at once on the `unreachable` route instead of timing out.

**What the watch knowingly does not detect.** A rule edited by hand inside a
chain, below an intact marker. A flush of only the `tenant_pairs` set, which
leaves the marker in place; it closes traffic between a tenant's own
sandboxes, it does not open traffic between tenants.

## Alternatives considered

- **A write probe before every irreversible worker action** (ask the database
  whether it can be written, then call the worker). A probe that succeeds says
  nothing about the save a moment later; the outage begins between the two as
  easily as before them. The gate plus "worker first, then save, answer what
  happened" covers every order of events with one rule.
- **Save the intended state before acting**, and correct it after. Then the
  database says `sleeping` for a sandbox the worker never put to sleep whenever
  the worker call fails, and a restart in between believes it. Without the
  control plane correcting its records from the workers (FLU-13) nothing would
  repair that. Create and fork do save first, because there the record that is
  saved is a reservation (`creating`) that claims nothing about the worker.
- **Keep the port quarantine on the worker, in a file.** The worker does not
  know which hostnames the gateways still route to its ports, and it would be
  one more file per worker that a reinstall loses. feluccad already holds the
  route rows; the quarantine lives beside them, in the same transaction.
- **Require the `conntrack` tool** on workers, so that a deleted sandbox's
  tracked connections can be flushed when its guest address is reused. It is
  not installed on the workers today, so the existing flush does nothing; the
  question is filed on its own (FLU-76) rather than made a new hard
  dependency here.
- **Let the agent restart the host firewall unit** when the host table is
  missing at runtime. ADR-0011 declined to let the agent drive systemd from a
  unit that is deliberately limited to `CAP_NET_ADMIN`/`CAP_NET_RAW`; that
  still holds (FLU-77).
- **Never placing a create on a worker that reports its isolation down.** A
  fleet in which every worker reports false would then accept no create at
  all, and such a worker refuses new guests anyway; the preference with a
  logged fallback keeps that fleet's placement as it was.
- **Delete stale snapshots instead of setting them aside.** The verdict rests
  on file times. A restore, a copy or a clock problem that disturbs them would
  make start-up delete every old snapshot on a node at once, with no way back.
  Set aside, they cost disk until the next `start`, `stop`, sleep or delete.
- **A watch that recomputes the expected rules from the membership** each
  tick (the first design). A membership change whose rebuild has not run
  yet would read as drift, and the watch would duplicate the rebuilds'
  work; comparing with what was last applied leaves that to the rebuilds.
- **Taking something down on one piece of evidence** — a look that fails, a
  repair that times out, or a missing marker alone. A failed `nft list` is not
  a missing fence, a timed-out `nft` may have committed, and a build that
  cannot read this kernel's listing would see every marker missing; each would
  turn a slow netlink or a parser mismatch into a worker-wide outage. Hence two
  positive pieces of evidence, and the self-check and per-table calibration
  before either counts.
- **`report` as the default mode.** It costs nothing in availability: nothing
  is ever taken down. What it costs is the fence: while a repair keeps
  failing, the guests already on that worker may reach each other or the host,
  and only the log and `isolation` false say so. `enforce` costs that worker's
  guest network until the fence is back, and nothing else. The default is
  `enforce`; `report` is the mode for a first roll-out, to see what the watch
  would do on a fleet before it may take anything down. In both, new guests
  are refused.
- **Retrying a refused repair under `sudo -n`.** Under the unit, sudo's own
  failure replaced the kernel's refusal, so no repair was ever "refused" and
  nothing could be taken down on evidence; the kernel's answer decides.
- **Relaying every worker refusal of a delete.** Only a refusal that ends by
  waiting (`busy…`) is relayed; any other can name worker internals and does
  not end by retrying, so it stays the 502 and goes to the log.
- **Stopping feluccad without a drain.** A create cut between the worker's
  answer and its last save leaves a VM the database does not know; 10 s of
  drain lets it finish, and the hard limit of 20 s keeps the stop inside the
  unit's 30 s.
- **A longer wait for same-kind requests** (120 s was the first value). A
  waiter that outlives feluccad's 30 s call carries the operation out for
  nobody.
- **A smaller gateway `--max-stale` default** (120 s). A control-plane outage
  shorter than ten minutes then takes ingress down; with 600 s it does not, and
  the quarantine is sized for it.

## Consequences

- Creates avoid workers that report their isolation down while another ready
  worker does not.


- Sandboxes that were running at a worker reboot, or whose VMM died while the
  agent was down, come back `stopped`, never `sleeping`. `start` cold-boots
  them. Stopping a sleeping sandbox discards its snapshot.
- Lifecycle requests can answer `409 {"error":"busy"}`. A same-kind repeat
  waits up to 25 s first; a fork answers busy at once, and feluccad passes it on
  with `Retry-After: 2`. Parallel forks of a sleeping parent run side by side;
  parallel forks of a running parent answer busy.
- Sleep and wake from a state they cannot act on answer 409 with the `state`
  key, and feluccad records that state.
- A fork pauses its parent for the snapshot and the disk copy. On a file system
  with reflink (XFS made with reflink, the default of current `mkfs.xfs`, or
  Btrfs) the copy shares the parent's blocks and is quick; ext4 has none, `cp`
  copies the data, and the pause grows with what the image holds. On ext4 on
  the lab it was 0.2 to 0.7 s for an image with 472 MiB of data and 10 to 12 s
  for one with 7.4 GiB; the agent does not log it (FLU-89).
- Sandboxes forked before this release cannot be forked (409 with the parent's
  `state`), and are woken only if their parent's image is unchanged since they
  slept. A refused fork leaves no child record in feluccad.
- Lifecycle verbs on a record still `creating` answer 409 with
  `"state":"creating"`; a record a stopped feluccad left `creating` comes back
  `error`. A delete the worker refuses as busy answers 409 with
  `Retry-After: 2`.
- Stopping feluccad drains requests for 10 s; a second signal or 20 s exits
  with status 1.
- While the store is failing, every change is refused with 503, wake included,
  and `/healthz` answers 503. Running sandboxes keep serving. A change that
  reached the worker before the save failed answers 503 with `"applied":true`.
- `/healthz` of feluccad is no longer counted in `felucca_api_requests_total`.
- Every Felucca chain in `nft list` output starts with a marker rule. It is a
  counter: tools that hash or diff the listing must use `nft -s`.
- In the default `enforce` mode, a worker whose isolation chains or switches
  the agent cannot restore loses its guest network (the bridge, or one tap)
  until a tick repairs it, and in every mode new guests are refused on it.
  `/healthz` `isolation` and `isolation_ok` go false when a fence is
  unrepaired, when the guest bridge is gone, when a host fence that was there
  at start is missing, and when the watch has not completed a tick for 45 s.
- A worker whose agent cannot read its own markers runs without a watch and
  says so once, `isolation watch NOT started: …`: alert on that line.
- A create, delete or expose that waited on a stuck `nft` now fails after
  10 s.
- New metrics: `felucca_persist_failures_total`, `felucca_store_healthy`,
  `felucca_usage_write_failures_total`, `felucca_usage_rows_dropped_total`,
  `felucca_store_read_errors_total`, `felucca_node_ports_quarantined`,
  `felucca_nodes_unfenced`.
- The gateway answers 503 for every sandbox URL once its table is older than
  `--max-stale`, and before its first fetch. Load balancers should probe
  `/readyz`.
- Expose can answer 429 (churn cap) and 503 (no free node port, or a node port
  conflict). Dynamic exposes are kept across a sleep.
- A worker's 10 000 node ports are shared with the quarantine: ports a tenant
  releases stay unusable for 900 s.
- Upgrade order: agents, then gateways, then feluccad. A new feluccad with an
  old agent keeps the old hostname exposure for that worker, with a warning in
  feluccad's log at each occurrence.

## Left open

- After a feluccad restart during a store outage, its records can be one
  transition behind the workers until the next call for that sandbox; the
  control plane correcting its records from the workers is FLU-13.
- A worker's node ports are reachable directly, without the gateway (FLU-75).
- Tracked connections of a deleted sandbox when its guest address is reused
  (FLU-76).
- The host firewall table (`inet felucca_host`) is not re-asserted at runtime
  (FLU-77).
- The gateway's route-table fetch is capped at 8 MiB (FLU-79).
- Route rows and quarantine entries after a database restore (FLU-80).
- Firecracker's drive cache mode (FLU-81).
- A failed create can leak a network slot (FLU-12).
- exec after a plain pause and resume, with no snapshot in between (FLU-15).
- Whether a fork should pause its parent for the disk copy at all is still
  open with the project owner.
