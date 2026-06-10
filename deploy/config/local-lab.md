# Local Lima-lab config equivalents

The same `hearthd` and `hearth-agent` binaries run identically in the Lima dev
lab and on production servers. The only difference is the config file values.

## Lima lab topology

| VM | Address | Role | Config file |
|---|---|---|---|
| `infra-saas-lab` | `192.168.104.3` | control plane | `hearthd-lab.json` below |
| `kata-lab-0` | `192.168.104.1` | worker 0 | `hearth-agent-lab.json` below |
| `kata-lab-1` | `192.168.104.4` | worker 1 | same template, different `advertise_addr` |

## hearthd — Lima lab (`/etc/hearth/hearthd.json` inside `infra-saas-lab`)

```json
{
  "bind": "0.0.0.0:8080",
  "state_path": "/var/lib/hearth/state.json",
  "ui_dir": "/usr/share/hearth/ui"
}
```

`token` is omitted in the lab (no auth). In production, set it to the output of
`openssl rand -hex 32` and repeat the value in every agent config.

The macOS host reaches the UI at `http://127.0.0.1:8080` via Lima's automatic
port-forward for guest port 8080.

## hearth-agent — Lima lab (`/etc/hearth/hearth-agent.json` inside `kata-lab-0`)

```json
{
  "bind": "0.0.0.0:9090",
  "control_plane": "http://192.168.104.3:8080",
  "advertise_addr": "192.168.104.1",
  "data_dir": "/srv/hearth",
  "pool_size": 0,
  "net": "on",
  "net_cidr": "10.231.0.0/24"
}
```

For `kata-lab-1` change `advertise_addr` to `192.168.104.4`.

`advertise_addr` can be omitted if the agent can auto-detect the correct source
IP toward the control plane (API-V2.md §6). In the Lima user-v2 network the
auto-detection works reliably; on multi-homed production nodes, set it
explicitly.

## What differs between lab and production

| Setting | Lima lab | Production |
|---|---|---|
| `control_plane` | `http://192.168.104.3:8080` | `https://hearth.internal.example.com` |
| `advertise_addr` | `192.168.104.x` | `10.0.1.x` (private datacenter IP) |
| `token` | omitted | 64-hex-char secret |
| TLS | none (plain HTTP, L2-isolated) | Caddy reverse proxy terminates TLS |
| `pool_size` | `0` (single-VM lab) | `2`–`5` per node |
| `data_dir` | `/srv/hearth` | `/srv/hearth` (same) |

The binary paths, systemd units, config file locations, and all API semantics
are identical between lab and production.
