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
