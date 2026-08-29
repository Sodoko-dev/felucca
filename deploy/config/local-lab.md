# Local Lima-lab config equivalents

The same `hearthd` and `hearth-agent` binaries run identically in the Lima dev
lab and on production servers. The only difference is the config file values.

## Lima lab topology

| VM | Address | Role | Config file |
|---|---|---|---|
| `infra-saas-lab` | `192.168.104.3` | control plane | `hearthd-lab.json` below |
| `kata-lab-0` | `192.168.104.1` | worker 0 | `hearth-agent-lab.json` below |
| `kata-lab-1` | `192.168.104.4` | worker 1 | same template, different `advertise_addr` |

## The lab token

The lab runs with a real token, same as production. Both binaries **refuse to
start** on an empty token, a known placeholder (`hearth-lab-token`, anything
containing `REPLACE_WITH`), or a short one — under 32 characters for `hearthd`,
under 16 for `hearth-agent`. An unauthenticated agent API is root-level command
execution inside every guest on the node, so this is a startup failure and not a
warning. Generate a token and keep it outside the repo — the lab scripts read it
from there:

```sh
mkdir -p ~/.config/hearth
openssl rand -hex 32 > ~/.config/hearth/lab-token
chmod 0600 ~/.config/hearth/lab-token
```

`scripts/verify-v2.sh`, `scripts/conformance-lab.sh` and `scripts/roll-v3.1.sh`
take it as `$1`/`TOKEN`/`HEARTH_TOKEN` or fall back to that file
(`HEARTH_TOKEN_FILE` overrides the path), and fail loudly when none is set.

Two things follow for lab work:

- **`/metrics` needs the token now.** `curl -s localhost:8080/metrics` returns
  `401`; add `-H "Authorization: Bearer $(cat ~/.config/hearth/lab-token)"`.
- **The console will not poll without a token.** Open
  `http://127.0.0.1:8080/`, paste the lab token into the banner. It is held in
  memory for that tab only — no `localStorage`, and `?token=` in the URL is
  stripped and ignored, not accepted. A header badge says `NO TOKEN` / `MOCK` /
  `STALE` whenever what you are looking at is not the live lab.

For a throwaway single-VM lab with nothing else on the network, `hearthd
--insecure-no-auth` skips the token entirely. It is loopback-and-lab only, it
must be named explicitly, hearthd logs a WARN every start, and there is no
`hearth-agent` equivalent — so an agent still needs a real token.

## hearthd — Lima lab (`/etc/hearth/hearthd.json` inside `infra-saas-lab`)

```json
{
  "bind": "0.0.0.0:8080",
  "state_path": "/var/lib/hearth/state.json",
  "ui_dir": "/usr/share/hearth/ui",
  "token": "<contents of ~/.config/hearth/lab-token>"
}
```

The macOS host reaches the UI at `http://127.0.0.1:8080` via Lima's automatic
port-forward for guest port 8080.

`bind` is `0.0.0.0` here and **that is deliberate for the lab only.** hearthd
honours the host part of `bind` now, and two things in this lab need the port
off-loopback: Lima's automatic forward (which does not pick up a
guest-loopback-only listener) and the worker VMs, which reach the control plane
at `192.168.104.3:8080`. Production is the opposite — `127.0.0.1:8080` behind
Caddy, or the overlay address on a wg fleet (DEPLOYMENT.md §6.3). The lab runs
no systemd units, so it has no `hearth-firewall` rules either: the L2-isolated
Lima network is the whole boundary. Do not expose a lab VM.

## hearth-agent — Lima lab (`/etc/hearth/hearth-agent.json` inside `kata-lab-0`)

```json
{
  "bind": "192.168.104.1:9090",
  "control_plane": "http://192.168.104.3:8080",
  "advertise_addr": "192.168.104.1",
  "data_dir": "/srv/hearth",
  "token": "<same value as hearthd>",
  "pool_size": 0,
  "net": "on",
  "net_cidr": "10.231.0.0/24"
}
```

For `kata-lab-1` change `bind` and `advertise_addr` to `192.168.104.4`.

`bind` is the VM's user-v2 address rather than `0.0.0.0` on purpose: `0.0.0.0`
includes the bridge gateway `10.231.0.1`, which every guest has as its default
route, so the agent's root API would answer sandboxes directly. The lab does not
run the systemd units, so it does not get the `hearth-firewall` rules that block
that path on a production worker (see DEPLOYMENT.md §8) — the bind address is
the whole defence here. Do not expose a lab VM beyond the host.

`advertise_addr` can be omitted if the agent can auto-detect the correct source
IP toward the control plane (API-V2.md §6). In the Lima user-v2 network the
auto-detection works reliably; on multi-homed production nodes, set it
explicitly.

## What differs between lab and production

| Setting | Lima lab | Production |
|---|---|---|
| `control_plane` | `http://192.168.104.3:8080` | `https://hearth.internal.example.com` |
| `advertise_addr` | `192.168.104.x` | `10.0.1.x` (private datacenter IP) |
| `bind` (agent) | `192.168.104.x:9090` | the node's management address |
| `token` | 64-hex-char secret from `~/.config/hearth/lab-token` | 64-hex-char secret |
| TLS | none (plain HTTP, L2-isolated) | Caddy reverse proxy terminates TLS |
| `pool_size` | `0` (single-VM lab) | `2`–`5` per node |
| `data_dir` | `/srv/hearth` | `/srv/hearth` (same) |
| host firewall | not installed (no systemd in the lab) | `hearth-firewall.service`, required by `hearth-agent.service` |

The binary paths, systemd units, config file locations, and all API semantics
are identical between lab and production.
