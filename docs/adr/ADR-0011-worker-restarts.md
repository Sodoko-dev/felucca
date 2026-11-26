# ADR-0011 — Worker restarts: sandboxes outlive the agent

Status: accepted (stability wave 0, 2026-11-26). Closes three findings of the
2026-11 stability audit that share one root — a worker's sandboxes were only as
alive as the units around them: F-01 (FLU-1), F-19 (FLU-2), F-20 (FLU-3).

Tested on Ubuntu 24.04: systemd 255, nftables 1.0.9, kernel 6.8, Firecracker
1.16. Nothing older was available to test on; the directives used need systemd
≥ 248.

## Context

Every sandbox on a worker is a `firecracker` process spawned by `felucca-agent`,
and all of them live in the agent unit's cgroup. Three ordinary events each
took all of them down at once:

- restarting or crashing the agent (systemd's default `KillMode=control-group`
  kills the whole cgroup);
- the kernel OOM-killing a single Firecracker (the default `OOMPolicy=stop`
  stops the unit, and stopping the unit kills the cgroup);
- restarting the host firewall unit (`Requires=` propagated the restart to the
  agent, and restarting the agent killed the cgroup).

`deploy/install.sh` restarted the firewall unit on every run, so re-running the
installer on a worker — which is how a worker is upgraded — was itself the
third event.

## Decision

### A Firecracker outlives the agent that spawned it (FLU-1)

**`felucca-agent.service` runs with `KillMode=process`.** Stopping the unit
signals the agent's main process and nothing else. `Restart=always` (was
`on-failure`, which leaves an agent that was ended by a plain SIGTERM down) and
`StartLimitIntervalSec=0`: with the sandboxes still running, "gave up starting
the agent" is not an acceptable end state.

Firecrackers stay in the agent unit's cgroup. They are not moved into scopes
of their own (see alternatives).

That one line turns the agent's start-up from "nothing is running, rebuild
from disk" into "everything is running, take it over". The code changes are
what makes that safe:

1. **Identity, not liveness.** A record keeps its VMM only when the recorded
   pid is owned by the agent's uid, was started as `firecracker --api-sock
   <P>`, and `<P>` is `fc.sock` in that instance's own directory — compared
   after canonicalisation, so a `data_dir` spelled differently from the day
   the VMM was started is still the same directory. `kill(pid, 0)` only says
   that *some* process has the number. The same check replaces it in the
   liveness sweep and before every signal: an adopted VMM is not the agent's
   child, so its pid is free for reuse the moment it dies.

   The check has three outcomes. A lookup that fails for a reason other than
   "no such process" is `Unknown`, and `Unknown` is treated as "still there":
   a VM declared dead on a failed read would be marked `error` while its VMM
   runs on, and the next start-up would kill that VMM as an orphan.

2. **Pooled VMs are adopted as pooled.** They used to become `stopped` with
   the pid dropped — harmless while every restart killed them, one leaked
   Firecracker and tap per restart once it does not (measured: five orphans
   after nine restarts). Their taps are re-pinned with everyone else's; the
   claim path pins nothing, so start-up is the only place it happens before a
   pool VM is handed to a tenant. A pooled record whose VMM is gone (a host
   reboot) is removed at start-up, record, tap and directory: it was never
   handed to anyone, cannot be claimed, and used to stay behind as a `stopped`
   record for good (FLU-29). The same happens to a pool VM whose VMM dies
   while the agent is running — it is removed, not left as an `error` record
   that nothing ever deletes — and its pool is then not refilled for five
   minutes: whatever killed it is most likely the node running out of memory.

3. **The record is the truth; the VMM is brought back to it.** Sleep and fork
   pause the guest before they write anything, so an agent that stops in the
   middle of one leaves a VM paused under a record that says `running` — and
   `resume` on a `running` record is a no-op. At start-up the agent asks each
   adopted Firecracker for its state (`GET /`) and resumes or pauses it to
   match the record. An operation that was cut short was never acknowledged
   to anyone, so rolling it back is the answer that surprises nobody.

   A Firecracker serves one API request at a time, and one that is still
   writing the snapshot of a large guest answers nothing until it has
   finished — seconds to minutes (`mem.bin` is as large as the guest's
   memory). Start-up asks all adopted VMMs at once and waits two seconds in
   total. One that has not answered is not given up on: it is asked again
   every five seconds until it does, and until then `pause` and `resume` on
   that VM go to the VMM instead of answering "already so" from the record.

4. **An orphan sweep that errs towards leaving things alone.** Killed: a
   Firecracker in a directory that never got a `meta.json` (the create was
   never acknowledged), and a second Firecracker in a directory whose record
   adopted another. Left running, with an ERROR: a Firecracker beside a
   `meta.json` that does not load — far more likely a healthy tenant VM with a
   damaged record than garbage. Never considered: a process in another cgroup,
   i.e. not started by this unit.

   **Asked first:** a Firecracker beside a record that holds none and says the
   VM is asleep, stopped or in error. That is what a wake or a start leaves
   when the agent goes away after the guest is running and before the record
   says so. If its guest is running, the record is what is behind: the guest
   has been writing to its disk, and killing it would leave `sleeping` on top
   of a snapshot the disk has outrun. The VMM is adopted and the record
   brought up to it. In every other case — never started, paused, no answer —
   nothing has run since the state on disk was taken, and the VMM is killed.

   **One agent per data dir.** The agent takes `{data_dir}/agent.lock` before
   it does anything else at start-up and exits without it. A second agent
   started by hand while the unit is up would otherwise run the whole of
   start-up against the first one's sandboxes, from records that are stale by
   the time it acts: kill its in-flight creates (no `meta.json` yet), pause a
   pool VM handed to a tenant a moment ago, rebuild the node's rulesets
   without the VMs created since — and only then fail to bind.

5. **`meta.json` is replaced by rename, and is on disk when the write
   returns.** It was truncated and rewritten in place; an agent killed between
   the two left an empty file, and since (4) a record that does not load is a
   VM the agent no longer manages. The temp file and the directory are
   fsynced: "no `meta.json`" now means "remove the directory", so an
   acknowledged create must not lose its record to a power cut. A write that
   fails (a full disk) or whose caller goes away is owed, and repeated every
   five seconds until it lands — otherwise the retried request is answered
   from memory and the stale file is what the next start believes.

6. **A killed VMM is waited for until every thread of it is gone.** `waitpid`
   returns at once for a process that is not a child, and a process loses its
   command line when it *enters* exit — before it unmaps the guest's memory
   and closes the tap. Sleep followed by wake on an adopted VM would find the
   tap still held.

7. **Failing closed without destroying.** A tap that cannot be re-pinned used
   to be deleted; with a live guest attached that leaves Firecracker holding a
   dead fd, and no later start can give the guest its network back. It is now
   taken down and kept. Likewise, when start-up ends in "isolation is not in
   force — refusing to serve", the agent takes the guest bridge down before
   it exits: that exit used to kill the guests, and they must not stay on a
   network the agent could not fence. Both are undone by the next start that
   succeeds — and only by that one. The bridge is brought up in exactly one
   place, after the fences have been checked; setting the host up no longer
   raises it, or every retry of a failing start would put the guests back on
   an unfenced bridge for as long as the attempt lasts.

8. **Pool specs are not forgotten into a drain.** feluccad pushes template
   pool specs after a node registers; until that has happened in the new
   agent's lifetime an empty list means "not told yet". Only the config
   pool's own excess is drained before the first push.

9. **An operation runs to its end, whoever is still listening.** Every
   lifecycle request (sleep, wake, pause, resume, stop, start, delete — create
   and fork already were) runs on its own task. A caller that hangs up, as
   feluccad does after 30 seconds, used to cut the operation wherever it was.
   Inside the operations, the step that makes a record stop naming a VMM and
   the signal to that VMM are one step with nothing in between: sleep records
   `sleeping`, drops the pid and kills; stop records `stopped`, drops the pid
   and signals. A stop is also written to disk now. It was not, so a stopped
   VM read as `running` with a dead pid at the next start — and from there as
   `sleeping` whenever an old snapshot was lying in its directory, a state
   `start` refuses.

10. **A directory is never removed from under a running Firecracker.** Before
    `delete` removes an instance directory it ends whatever VMM still serves a
    socket in it — the one case being a VM whose record did not load, which
    start-up left running and which the control plane can still delete by id.
    A VMM whose directory is gone cannot be found by any later start. If the
    process cannot be ended, the delete fails and feluccad keeps its record. A
    delete of a pool VM takes it out of the claimable set in the same critical
    section that reads it, so a create cannot be handed a VM that is being
    removed (FLU-60).

### One out-of-memory kill takes one sandbox (FLU-2)

**`OOMPolicy=continue`.** With the default (`stop`), the kernel OOM-killing any
process of the unit stops the unit. `KillMode=process` alone already keeps the
other Firecrackers alive through such a stop, but the agent would still be
taken down and restarted for nothing; with `continue` the unit carries on, the
agent sees one VMM exit, and one sandbox goes to `error`.

**`ManagedOOMPreference=omit`.** systemd-oomd does not choose a process, it
kills a whole cgroup — and this one holds every sandbox on the worker.

**Every Firecracker gets `oom_score_adj` 300; the agent stays at 0.** The
guests are what fills a worker's memory, so the process to lose is a VMM — one
sandbox — rather than the agent, sshd or journald. The agent writes the value
itself: in the one function that spawns a Firecracker (before the child is
handed to its reaper, so the pid cannot have been recycled), and again for
every VMM it adopts at start-up, which is what covers VMMs started by an agent
that predates this.

There is deliberately **no `OOMScoreAdjust=` on the unit**, although a strongly
negative one for the agent was the original suggestion. A unit-level value is
inherited by every Firecracker, and the kernel remembers it as the floor a
process without CAP_SYS_RESOURCE may lower itself back to. A compromised VMM
could undo the agent's `300` and make itself the last process on the node to
die. With the unit at 0, the lowest a VMM can go is level with the agent.

### The host firewall is checked in the kernel and loaded in one transaction (FLU-3)

**`felucca-agent.service` no longer `Requires=` the firewall unit.** It
`Wants=` it, is ordered `After=` it, and has a start gate of two
`ExecStartPre=` steps. Both test the same thing — is the guest fence, the rule
`iifname "felucca0" drop`, in chain `input` of table `inet felucca_host` in the
kernel:

```
ExecStartPre=-+/bin/sh -c '<fence loaded> || systemctl reload-or-restart felucca-firewall.service'
ExecStartPre=/bin/sh -c '<fence loaded> && exit 0; ip link set dev felucca0 down 2>/dev/null; echo "<why>" >&2; exit 1'

<fence loaded> = /usr/sbin/nft list chain inet felucca_host input 2>/dev/null | grep -q "iifname .felucca0. drop"
```

The property DEPLOYMENT.md §8 rests on is "a worker without the host firewall
does not serve its API". `Requires=` never delivered it: it attests that the
firewall unit *ran once*, not that the table exists. Flush the ruleset (by
hand, or the distribution's `nftables.service` doing it at a reload) and the
agent started regardless. The gate looks at the kernel every time the agent
starts.

Dropping `Requires=` also removes what it did deliver and nobody wanted:

- a restart of the firewall unit restarted the agent;
- a firewall unit that failed to start left the agent's start job failed with
  result `dependency`, which `Restart=` does not retry — the agent stayed down
  until a human started it.

**Step 1 heals, step 2 refuses and quarantines.** The first version of this
gate was the second step alone, testing only that the chain existed. A second
review found what that does once sandboxes outlive the agent:

- After a ruleset flush the firewall unit is still `active (exited)`. `Wants=`
  does not start an active unit and nothing else runs it, so the gate failed
  every 5 s for good — and the sandboxes that had survived the restart sat on
  a bridge with no fence at all, their agent locked out of the only code that
  rebuilds the rules. Before `KillMode=process` the same restart would have
  killed those guests. Step 1 is the missing half: when the fence is not
  loaded, ask systemd to load the rules file again (`reload-or-restart`: a
  reload when the unit is active, a start when it is stopped or failed). It is
  best effort (`-`), it does nothing at all when the fence is loaded, and it
  runs on every retry — so a rules file that someone repairs is picked up by
  the next attempt with no manual reload.
- When the fence still cannot be had — the rules file does not load, or loads
  without the line — the guests must not stay on the network while no agent is
  there. Step 2 takes `felucca0` down before it fails the start. The guests
  keep running; the agent raises the bridge again itself, after its own
  isolation checks have passed.
- A chain called `input` is not the fence. A rules file generated for the
  control-plane role, a flushed chain, or a hand edit that drops the line all
  have the chain. The gate greps the listing for the rule itself.

`+` on step 1 runs it outside the unit's sandbox. Measured on systemd 255 it is
not needed — `systemctl reload-or-restart` works from a root process holding
only `CAP_NET_ADMIN`/`CAP_NET_RAW`, and `ip link set … down` and `nft list`
work inside the sandbox — but whether systemd accepts such a caller is not
something the unit should depend on. The same run showed no deadlock, with the
firewall unit ordered `Before=` the agent, for a manual `systemctl restart`, an
automatic restart after `SIGKILL`, and a cold start of both units. (It was
made with copies of the two shipped units, names swapped for a scratch table
and bridge, on a host with no sandboxes; the lab gate repeats it on a worker
with one running.)

With the gate, a missing fence is an ordinary start failure: the agent retries
every `RestartSec`, reloads the rules file on each attempt, and comes up by
itself once the fence loads.

**`felucca-firewall.service` loads the rules as one nft transaction**, the same
one for `start`, `reload` and `restart`. The unit feeds nft this script on
stdin (`StandardInput=data`, `StandardInputText=`, `ExecStart=/usr/sbin/nft -f
-`; systemd 255 infers the first from the second, the unit states it anyway,
because an nft that was handed `/dev/null` instead exits 0 having loaded
nothing):

```
table inet felucca_host
delete table inet felucca_host
include "/etc/felucca/firewall.nft"
list chain inet felucca_host input
```

The first line makes the delete safe on a first load; the last makes the
transaction fail unless the file really defined the input chain. A rules file
that does not parse, is empty, or defines no input chain commits nothing, and
the rules that were in force stay in force. Loading twice does not duplicate
rules (loading the plain file twice did). Because the four lines live in the
unit and not in the rules file, every rules file gets this treatment —
including one written by hand, and the ones already on disk. The other side of
including the file verbatim: the transaction does whatever the file says. One
that begins with `flush ruleset` wipes every other table on the node, the
agent's among them, and the load succeeds. The generated file defines one
table and nothing else.

A second `nft list chain …` runs after the transaction (`ExecStartPost=`, and
a second `ExecReload=`). On the nft we tested the in-script check is enough;
an nft too old to see its own pending changes would pass it on a stale view,
and the second look turns that into a failed unit instead of a green one.

**Stopping the unit leaves the rules loaded.** Sandboxes now keep running when
their agent is stopped, so a stop that deleted the table would leave live
guests facing an unfiltered host; and a `restart` no longer has a moment
between "deleted" and "loaded".

**At host shutdown the unit ends the sandboxes.** This is the one thing its
`ExecStop=` does, and only while `systemctl is-system-running` says
`stopping`. `KillMode=process` means a shutdown, which only stops units,
leaves every Firecracker running until systemd's final kill pass — after other
units have unloaded rules. The distribution's `nftables.service` runs
`nft flush ruleset` when it stops; with it enabled the lab measured the flush
0.2 s before the last Firecracker was killed, on an idle worker. For that long
guests ran with no host fence and no tenant isolation. The firewall unit is
the right place for the step because of its ordering — after the
distribution's loader, before the agent, so stopped between the two at
shutdown — and because it is active whether or not the agent is running. An
`ExecStopPost=` on the agent's unit would not run when the agent is in its
retry loop.

**The unit is ordered `After=nftables.service`**, so the distribution's loader,
which commonly starts with `flush ruleset`, cannot wipe the table at boot after
it has been loaded.

**The installer does not change policy or disturb the node on a re-run.**

- An existing `/etc/felucca/firewall.nft` is kept, byte for byte. It used to
  be regenerated every time, so a re-run without the variable narrowed port
  9090 to loopback and locked the control plane out — applied at once, because
  the installer also restarted the firewall unit.
- The one thing that rewrites it is a **worker** run that names the control
  plane with `FELUCCA_CONTROL_PLANE_IP`. The decision is taken on what the
  value parses to and on the role, not on the variable being non-empty: the
  first cut regenerated whenever it was set, so a control-plane re-run from a
  shell that still had it exported replaced a hand-widened 8080 rule with the
  stock loopback-only one, and a worker value of `,` wrote a loopback-only
  allow-list over a working one. The control-plane role no longer reads the
  variable; a worker value that names nobody is an error.
- `FELUCCA_CONTROL_PLANE_IP` is a list of literal IPv4/IPv6 addresses. It is
  validated character by character and element by element before any of it is
  written into a ruleset that root loads; `1.2.3.4 } accept; tcp dport 9090
  accept #` used to be pasted in as it came. No `/prefix`: `192.168.104.1/24`
  pasted from `ip addr`, or a `/0`, would widen the port without a word, and a
  subnet is a hand edit that later runs keep.
- All of that is checked **before anything is installed**. A refusal used to
  come after the new binary and unit were in place, left a temp file behind,
  and on a first install skipped the one-time print of the minted token.
- A kept file is checked before it is applied again: it must pass a dry run
  of the unit's own transaction (`nft -c`), and on a worker it must contain the
  fence line — applying a file without it would take a loaded fence away, and
  the agent would refuse to start afterwards anyway. Either failure leaves the
  file and the kernel alone and makes the installer exit non-zero.
- A generated file is dry-run the same way, created `root:root 0640`, and
  replaces the old one by rename. The firewall unit is installed and loaded by
  systemd before that, so nothing that can fail sits between the rename and
  the `systemctl reload` that applies it. If the apply fails the previous file
  is put back and applied again, and the installer exits non-zero.
- The rules file is root's on both roles, always: the control-plane role's
  ownership pass over `/etc/felucca` skips it instead of handing it to the
  service user until a later step takes it back.
- The installer never restarts a running agent. It says which command is left.
- Without `nft` it exits non-zero. That used to be a warning.

## Alternatives considered

- **`KillMode=mixed`.** The agent gets SIGTERM, and whatever is left in the
  cgroup is SIGKILLed as soon as the agent has exited — it does not wait for
  `TimeoutStopSec` (measured on systemd 255: `systemctl stop` returned after
  12 ms with the second process of the unit already gone). No sandbox would
  survive a single stop.
- **One systemd scope per VM**, so the agent unit could keep the default kill
  mode and each VM had its own cgroup (and OOM accounting). The right shape
  eventually, and not a one-line change: starting a transient unit needs
  CAP_SYS_ADMIN or polkit from inside a unit that is deliberately limited to
  CAP_NET_ADMIN/CAP_NET_RAW.
- **Identity by pid start time recorded in `meta.json`, or by pidfd.** Both
  add state the previous agent has to have written; argv + owner + socket
  directory can be checked against VMMs that any earlier agent started, which
  is what makes the first upgrade non-disruptive.
- **Roll an interrupted operation forward** (a sleeping record with a live,
  resumed VMM is "really" awake). It would avoid waking a snapshot that the
  guest has since outrun, but that hazard exists independently of this ADR
  (FLU-4) and needs its own fix; start-up should not grow a second copy of
  wake's post-conditions.
- **Graceful stop** (finish in-flight operations on SIGTERM). Worth having and
  filed separately. It cannot replace the start-up handling, which also has
  to cover a crash.
- **Different OOM scores for pool VMs and tenant VMs** (pool VMs first: they
  hold nobody's data). Sensible, but a pool VM that dies is prewarmed again on
  the next tick with no backoff, and its dead record is not cleaned up until
  the agent restarts — under sustained pressure that is a boot loop that eats
  slots. It needs a refill backoff first; filed separately.
- **A memory limit per VM** (`MemoryMax` on a cgroup of its own) would turn a
  host-wide OOM into "that guest hit its own limit". It needs the per-VM
  scopes above.
- **Keep `Requires=` and add the `ExecStartPre=` check.** This was the plan
  before it was challenged. It keeps the restart propagation (harmless once
  sandboxes survive an agent restart) but also keeps the trap: a firewall unit
  that fails to start takes the agent down with no retry.
- **A gate that only refuses** (one `ExecStartPre=` that fails when the table
  is missing). This shipped in the first cut and is described above: after a
  flush it never passes again by itself, and meanwhile the guests are unfenced.
- **A gate that looks for the chain rather than the rule.** Also the first
  cut. It passes for any file that defines `chain input`.
- **Have the firewall unit add the fence itself**, as a fifth line of its
  script, so that the rule exists in one canonical spelling whatever the rules
  file says and no hand edit can make the gate's text match fail. Not done:
  the unit is shared with the control-plane role, the generated worker file
  would carry the rule twice, and "the file is the whole policy" is worth
  keeping. The price is the rule in "Consequences": the fence line must stay
  exactly as generated.
- **Restart back-off for a start that keeps failing** (`RestartSteps=`,
  `RestartMaxDelaySec=`, systemd ≥ 254) to bound the journal volume of a
  crash loop. Not used: systemd does not reset the step counter when a start
  succeeds (measured on systemd 255: three failed starts, one that stayed up,
  then a kill — the next start waited the full `RestartMaxDelaySec`). A
  long-lived unit would drift to the maximum delay and stay there, and a slow
  return of the agent costs more than the log lines do.
- **Put the delete/re-add preamble in the generated rules file.** Then the
  safety of a reload depends on what is in the file. A hand-written file, or
  one generated by an older installer, would append duplicate rules on every
  reload — two `!= { … } drop` rules for port 9090 with different allow-lists
  drop both lists.
- **Persist `FELUCCA_CONTROL_PLANE_IP` in an env file** and regenerate from it.
  The rules file already is the persisted state; a second copy can disagree
  with it, and a file that root sources is one more thing to get wrong.
- **`flush table` instead of `delete table`.** A flush keeps chains (and their
  hook, type and priority) that the new file no longer declares.
- **`destroy table`.** Needs nft 1.0.7 and kernel 6.3; Ubuntu 22.04 and
  Debian 12 ship older.

## Consequences

- `systemctl stop felucca-agent` leaves sandboxes running. Powering a node's
  sandboxes off is `systemctl stop felucca-agent && systemctl kill
  felucca-agent`; `systemctl kill felucca-agent` on a running unit hits every
  sandbox. A host shutdown no longer stops them with the agent's unit
  either; the firewall unit ends them, right after the agent has stopped and
  before any unit can unload firewall rules (above; FLU-62). They are killed,
  not shut down, as before.
- Every agent restart logs three systemd warnings per running sandbox (one
  "remains running after unit stopped", two "Found left-over process").
- A start that keeps failing is retried every `RestartSec` for good and the
  unit never becomes `failed`: it shows `activating (auto-restart)`, and an
  alert on failed units does not fire. Each attempt repeats two (the gate
  refused) or three (the agent ran and exited) of those warnings per sandbox.
- What must be in place before an agent is next restarted is the new unit,
  loaded (`daemon-reload`). By hand: units, `daemon-reload`, check
  `KillMode=process`, binary, restart. `install.sh worker` copies the binary
  first and the units right after, and restarts nothing — the order of the
  two copies does not matter, since nothing runs the new binary until the
  restart. The first restart from a pre-ADR-0011 agent is already
  non-disruptive. Rolling back the binary alone under the new unit orphans
  pool VMs per restart; rolling back the unit is a node drain.
- "Agent down" no longer means "sandboxes down". The node shows `down` in
  feluccad while its sandboxes serve traffic; nothing sweeps, refills or
  heartbeats until the agent is back.
- A sandbox keeps the Firecracker binary, seccomp filter and limits it was
  started with for as long as it runs. The unit's device policy stays in force
  for survivors while the unit is stopped (verified: the cgroup and its device
  program remain).
- Not fixed here, and now more visible: a VM whose Firecracker died while the
  agent was down reconciles as `stopped`, or `sleeping` on an old snapshot
  (FLU-4); in-flight operations fail on every restart (no graceful stop,
  FLU-61); NAT and ingress rules are rebuilt non-atomically at each start
  (FLU-45); helper processes (`cp`, `curl`, `e2fsck`, `resize2fs`) outlive
  the agent that started them (FLU-63); a create that was committed but never
  answered leaves a running VM the caller was told had failed (FLU-28).
- A worker can still be promised more memory than it has (admission is
  FLU-24). What changes is the price: the sandbox whose guest grew the most
  dies, instead of all of them. A badly over-committed worker loses sandboxes
  one after another until what is left fits.
- An OOM kill is no longer visible as a failed or restarted unit. The signals
  are the sandbox going to `error`, the journal line, and `oom_kill` in the
  unit cgroup's `memory.events`. The control plane is not told (FLU-13).
- `systemctl reload felucca-firewall` is the way to apply an edited rules
  file. `restart` is equally safe now; neither touches the agent or a sandbox.
  `reload` is refused while the unit is stopped or failed;
  `reload-or-restart` works in every state.
- A rules file that does not load is reported by the reload and changes
  nothing. What does take the rules away: deleting the table, a `nft flush
  ruleset` by hand, the distribution's `nftables.service` (its stock config
  starts with one, and its stop is one), or a rules file that itself begins
  with `flush ruleset`.
- At runtime nothing re-checks the rules: a ruleset flushed while the agent is
  running goes unnoticed, and the node unfenced, until the agent next starts —
  when its gate loads the host rules again and the agent rebuilds its own.
  Continuous re-assertion is FLU-8.
- The gate matches the fence by its text. The line `iifname "felucca0" drop`
  has to stay exactly as generated: a hand edit that rewrites it (adding
  `counter`, say) loads fine and then stops the agent, and its guests'
  network, at the next agent start. The generated file and the installer's
  check on a kept file both say so; nothing checks at `reload` time.
- One host cannot be given both roles by the installer: there is one rules
  file and each role's run writes only its own rules. A worker run that finds
  a control-plane file stops with "no guest fence" (FLU-64).
- `feluccad.service` has no equivalent check. On the control plane the
  firewall unit is still advisory (FLU-64).
- `scripts/verify-units.sh` asserts that the settings above are the ones in
  effect — last assignment, whole `ExecStartPre=`/`StandardInputText=`/
  `ExecStart=`/`ExecReload=` lists — that `Requires=`/`Requisite=`/`BindsTo=`/
  `PartOf=` on the firewall unit, `OOMScoreAdjust=` and an `ExecStopPost=` have
  not come back in any spelling, that the firewall unit's one `ExecStop=` is
  the shutdown step word for word, and that it rejects 29 deliberately broken
  copies of the units. `test/installer/allowlist.sh` runs the installer's
  firewall code against a scratch directory and stand-ins for systemd and the
  kernel table.
