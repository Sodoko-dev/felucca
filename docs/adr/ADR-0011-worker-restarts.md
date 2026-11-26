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
   record for good (FLU-29).

3. **The record is the truth; the VMM is brought back to it.** Sleep and fork
   pause the guest before they write anything, so an agent that stops in the
   middle of one leaves a VM paused under a record that says `running` — and
   `resume` on a `running` record is a no-op. At start-up the agent asks each
   adopted Firecracker for its state (`GET /`) and resumes or pauses it to
   match the record. An operation that was cut short was never acknowledged
   to anyone, so rolling it back is the answer that surprises nobody.

4. **An orphan sweep that errs towards leaving things alone.** Killed: a
   Firecracker in a directory that never got a `meta.json` (the create was
   never acknowledged), and one whose record did not adopt it (a wake or start
   that never committed, or a duplicate). Left running, with an ERROR: a
   Firecracker beside a `meta.json` that does not load — far more likely a
   healthy tenant VM with a damaged record than garbage. Never considered: a
   process in another cgroup, i.e. not started by this unit.

   The sweep runs only while the agent holds `{data_dir}/agent.lock`. Without
   that, a second agent started by hand on the same data dir would reconcile,
   find the first one's in-flight creates — which have no `meta.json` yet —
   and kill them before failing to bind.

5. **`meta.json` is replaced by rename.** It was truncated and rewritten in
   place; an agent killed between the two left an empty file, and since (4) a
   record that does not load is a VM the agent no longer manages.

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
   succeeds.

8. **Pool specs are not forgotten into a drain.** feluccad pushes template
   pool specs after a node registers; until that has happened in the new
   agent's lifetime an empty list means "not told yet". Only the config
   pool's own excess is drained before the first push.

### The host firewall is checked in the kernel and loaded in one transaction (FLU-3)

**`felucca-agent.service` no longer `Requires=` the firewall unit.** It
`Wants=` it, is ordered `After=` it, and runs

```
ExecStartPre=/usr/sbin/nft list chain inet felucca_host input
```

The property DEPLOYMENT.md §8 rests on is "a worker without the host firewall
does not serve its API". `Requires=` never delivered it: it attests that the
firewall unit *ran once*, not that the table exists. Flush the ruleset (by
hand, or the distribution's `nftables.service` doing it at a reload) and the
agent started regardless. The `ExecStartPre=` check looks at the kernel every
time the agent starts.

Dropping `Requires=` also removes what it did deliver and nobody wanted:

- a restart of the firewall unit restarted the agent;
- a firewall unit that failed to start left the agent's start job failed with
  result `dependency`, which `Restart=` does not retry — the agent stayed down
  until a human started it.

With the check, a missing table is an ordinary start failure: the agent
retries every `RestartSec` and comes up by itself once the table is loaded.

**`felucca-firewall.service` loads the rules as one nft transaction**, the same
one for `start`, `reload` and `restart`. The unit feeds nft this script on
stdin (`StandardInputText=`, `ExecStart=/usr/sbin/nft -f -`):

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
including one written by hand, and the ones already on disk.

A second `nft list chain …` runs after the transaction (`ExecStartPost=`, and
a second `ExecReload=`). On the nft we tested the in-script check is enough;
an nft too old to see its own pending changes would pass it on a stale view,
and the second look turns that into a failed unit instead of a green one.

**The unit has no `ExecStop=`.** Stopping it leaves the rules loaded. Sandboxes
now keep running when their agent is stopped, so a stop that deleted the table
would leave live guests facing an unfiltered host; and a `restart` no longer
has a moment between "deleted" and "loaded".

**The unit is ordered `After=nftables.service`**, so the distribution's loader,
which commonly starts with `flush ruleset`, cannot wipe the table at boot after
it has been loaded.

**The installer does not change policy or disturb the node on a re-run.**

- An existing `/etc/felucca/firewall.nft` is kept unless
  `FELUCCA_CONTROL_PLANE_IP` is given on that run. It used to be regenerated
  every time, so a re-run without the variable narrowed port 9090 to loopback
  and locked the control plane out — applied at once, because the installer
  also restarted the firewall unit.
- `FELUCCA_CONTROL_PLANE_IP` is a list of literal IPv4/IPv6 addresses
  (optionally `/prefix`). It is validated character by character and element
  by element before any of it is written into a ruleset that root loads;
  `1.2.3.4 } accept; tcp dport 9090 accept #` used to be pasted in as it came.
- A generated file is dry-run through the unit's own transaction (`nft -c`)
  before it replaces the old one, replaced by rename, owned `root:root 0640`,
  and applied with `systemctl reload`. If the apply fails the previous file is
  put back and the installer exits non-zero.
- The installer never restarts a running agent. It says which command is left.

## Alternatives considered

- **`KillMode=mixed`.** The agent gets SIGTERM, but whatever is left in the
  cgroup is SIGKILLed after `TimeoutStopSec`. Sandboxes would survive exactly
  until the first slow stop.
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
- **Keep `Requires=` and add the `ExecStartPre=` check.** This was the plan
  before it was challenged. It keeps the restart propagation (harmless once
  sandboxes survive an agent restart) but also keeps the trap: a firewall unit
  that fails to start takes the agent down with no retry.
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
  sandbox. Host shutdown no longer stops them with the unit — they end in
  systemd's final kill pass.
- Every agent restart logs three systemd warnings per running sandbox (one
  "remains running after unit stopped", two "Found left-over process").
- Upgrade order matters once: units, `daemon-reload`, binary, restart. The
  first restart from a pre-ADR-0011 agent is already non-disruptive. Rolling
  back the binary alone under the new unit orphans pool VMs per restart;
  rolling back the unit is a node drain.
- "Agent down" no longer means "sandboxes down". The node shows `down` in
  feluccad while its sandboxes serve traffic; nothing sweeps, refills or
  heartbeats until the agent is back.
- A sandbox keeps the Firecracker binary, seccomp filter and limits it was
  started with for as long as it runs. The unit's device policy stays in force
  for survivors while the unit is stopped (verified: the cgroup and its device
  program remain).
- Not fixed here, and now more visible: a VM whose Firecracker died while the
  agent was down reconciles as `stopped`, or `sleeping` on an old snapshot
  (FLU-4); in-flight operations fail on every restart (no graceful stop); NAT
  and ingress rules are rebuilt non-atomically at each start; helper processes
  (`cp`, `curl`, `e2fsck`) outlive the agent that started them.
- `systemctl reload felucca-firewall` is the way to apply an edited rules
  file. `restart` is equally safe now; neither touches the agent or a sandbox.
- A rules file that does not load is reported by the reload and changes
  nothing. The one way to end up with no table is to remove it on purpose.
- At runtime nothing re-checks the table: a ruleset flushed while the agent is
  running goes unnoticed until the agent next starts, when it refuses to.
  Continuous re-assertion is FLU-8.
- `feluccad.service` has no equivalent check. On the control plane the
  firewall unit is still advisory (filed separately).
- `scripts/verify-units.sh` asserts the lines above are present and that
  `Requires=` on the firewall unit and an `ExecStop=` have not come back.
