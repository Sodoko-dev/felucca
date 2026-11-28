# Felucca — Production Deployment Guide

Felucca is two static Linux binaries (`feluccad` and `felucca-agent`) and a handful
of shell scripts. This guide covers a production install on generic Linux servers.
For the local Lima dev environment see `deploy/config/local-lab.md`.

---

## Table of contents

1. [Topology](#1-topology)
2. [Prerequisites](#2-prerequisites)
3. [Build the binaries](#3-build-the-binaries)
4. [Install — control plane](#4-install--control-plane)
5. [Install — worker nodes](#5-install--worker-nodes)
6. [Tokens, auth, and bind addresses](#6-tokens-auth-and-bind-addresses)
7. [TLS and the reverse proxy (Caddy or nginx)](#7-tls-and-the-reverse-proxy-caddy-or-nginx)
   — including [§7.1 forwarded client addresses](#71-forwarded-client-addresses-required-reading), **required reading**
8. [Host firewall](#8-host-firewall)
9. [How local dev and production differ](#9-how-local-dev-and-production-differ)
10. [Upgrade procedure](#10-upgrade-procedure)
11. [Troubleshooting](#11-troubleshooting)
12. [Running through failures: the store, the route table, node ports, snapshots](#12-running-through-failures-the-store-the-route-table-node-ports-snapshots)

---

## 1. Topology

```
                          HTTPS :443
 Clients ─────────────► Caddy (reverse proxy)
                               │
                               │ HTTP :8080 (loopback / private only)
                               ▼
                           feluccad (control plane)
                           /var/lib/felucca/state.json
                               │
                  ┌────────────┼────────────┐
                  │            │            │
            HTTP :9090   HTTP :9090   HTTP :9090
                  ▼            ▼            ▼
            felucca-agent  felucca-agent  felucca-agent
             (worker 0)   (worker 1)   (worker N)
                  │
          Firecracker × M microVMs
          /dev/kvm, tap, nftables
```

**One control plane, N worker nodes.**

This is the **Caddy-fronted single-node** topology, and it is what the shipped
example configs are tuned for: `feluccad` binds `127.0.0.1:8080` and only Caddy
reaches it. A **WireGuard-overlay fleet** is the other supported shape and it
needs the opposite bind — agents reach feluccad at its overlay address inside the
tunnel, so a loopback bind refuses every registration. Decide which one you are
building before you install: see [§6.3](#63-bind-address-which-topology-are-you-in)
and [§6.4](#64-wireguard-overlay-fleets).

- The control plane runs `feluccad` and holds all state. It does not run
  Firecracker.
- Each worker runs `felucca-agent`, which drives Firecracker processes directly.
- The control plane proxies VM lifecycle calls to the owning agent. If an agent
  is unreachable (heartbeat older than 15 s) it is marked `down` and excluded
  from placement.
- TLS is terminated by a reverse proxy on the control plane host. Agent traffic
  (port 9090) stays on the private network and does not require TLS, though it
  is protected by a bearer token — the node's own per-node credential once it
  has enrolled, the shared fleet token until then (§6.7).

---

## 2. Prerequisites

### Control plane host

| Requirement | Notes |
|---|---|
| Linux x86_64 or aarch64 | Any modern distro (Debian 12+, Ubuntu 22.04+, Fedora 38+, RHEL 9+) |
| Caddy 2 (or nginx) | TLS termination. `trusted_proxies` and the proxy's `X-Forwarded-For` directive are **one decision, not two**: list the proxy only if it **overwrites** the header (`header_up X-Forwarded-For {remote_host}` / `proxy_set_header X-Forwarded-For $remote_addr`). Listing a proxy that forwards the client's own header — nginx's bare `proxy_pass` default — lets any client choose its own throttle key. Leaving `trusted_proxies` empty is the safe fallback. Exact directives and the check: §7.1 |
| Port 443 (or 80) open to clients | Caddy handles ACME |
| Port 8080 NOT exposed publicly | Caddy proxies to it on loopback; the shipped config binds `127.0.0.1:8080` and the §8 rules drop it from anywhere else. On a wg-overlay fleet bind the overlay address instead and widen the rule to match — see §6.3 |

### Worker hosts

| Requirement | Notes |
|---|---|
| Linux x86_64 or aarch64 | Same distro constraint |
| `/dev/kvm` present | Bare-metal or KVM-enabled VM (nested virt on cloud providers) |
| `firecracker` in PATH | Installer fetches the pinned release and verifies its SHA-256 if missing |
| `sha256sum` (coreutils) | Digest verification for the Firecracker and guest-asset downloads |
| `nft` (nftables) | Guest networking and the host firewall — `apt install nftables` / `dnf install nftables`. Required: without it the installer cannot load the host rules, says so, and exits non-zero (§8) |
| `ip` (iproute2) | Bridge and tap management |
| Port 9090 accessible from the control plane | Private network only; not public |
| `squashfs-tools`, `e2fsprogs` | Only needed once to build the base rootfs |

Verify KVM is available on a worker:

```sh
ls -la /dev/kvm          # must exist
sudo dmesg | grep -i kvm # should show KVM enabled
```

---

## 3. Build the binaries

Builds run inside the `infra-saas-lab` Lima VM (Go 1.26 + Rust 1.96 toolchains —
never on the macOS host). See `docs/adr/ADR-0003-go-rust-port.md` for why the
control plane is Go and the agent is Rust.

```sh
REPO=/Users/magdy/projects/github.com/alpham/infra-saas

# feluccad (Go, static, cross-compiles to both arches from one box)
limactl shell infra-saas-lab -- bash -c \
  "export PATH=\$PATH:/usr/local/go/bin && cd $REPO/go && \
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/x86_64/feluccad ./cmd/feluccad && \
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/aarch64/feluccad ./cmd/feluccad"

# felucca-agent (Rust, static musl)
limactl shell infra-saas-lab -- bash -c \
  "source ~/.cargo/env && export CARGO_TARGET_DIR=\$HOME/.cargo-target/felucca-agent && \
   cd $REPO/rust/agent && cargo build --release --target aarch64-unknown-linux-musl && \
   cp \$CARGO_TARGET_DIR/aarch64-unknown-linux-musl/release/felucca-agent \
      $REPO/deploy/release/aarch64/felucca-agent"
```

For x86_64 agents, add the target (`rustup target add x86_64-unknown-linux-musl`
plus a cross linker) or simply build on an x86_64 machine — the agent has no
non-Rust dependencies, so `cargo build --release --target x86_64-unknown-linux-musl`
on the target arch is the path of least resistance.

The installer finds binaries at `deploy/release/<arch>/`.

> **Systemd note**: the units in `deploy/systemd/` are validated under the
> Go/Rust runtimes with Firecracker v1.16 (v4 P2.5): each allow-list entry
> encodes a specific requirement (`@sandbox` for FC's own seccomp(2) filters,
> `/dev/net/tun` for taps, `StateDirectoryMode=0750`), so treat a hardening
> failure as a missing *specific* allowance to add, not a directive to remove
> wholesale. The one remaining experiment is `MemoryDenyWriteExecute=true` in
> `feluccad.service` vs the Go runtime — it survives startup and the
> conformance suite; the 48h soak is the verdict. If you use a non-default
> agent `data_dir`, override `ReadWritePaths` via a drop-in that resets the
> list (see the comment in `felucca-agent.service`).
>
> **Pinned downloads**: the Firecracker version and its SHA-256 live in
> `deploy/install.sh`; the guest kernel and base rootfs objects and their
> SHA-256 values live in `deploy/firecracker-assets.sh` (and are mirrored in
> `infra/setup-node.sh` for the Lima lab). Each script's pin block documents the
> one command that refreshes it. Nothing is installed or extracted before its
> digest matches — "whatever the CDN returned today" is not a version.

---

## 4. Install — control plane

Run on the control plane host as root:

```sh
# Copy the repo (or just the deploy/ subtree) to the server, then:
sudo bash deploy/install.sh control-plane
```

The installer:

1. Creates directories: `/etc/felucca`, `/var/lib/felucca`, `/srv/felucca`, `/usr/share/felucca/ui`.
2. Creates the `felucca` system user (no login shell, no home).
3. Copies `feluccad` to `/usr/local/bin/feluccad`.
4. Installs `deploy/systemd/feluccad.service`.
5. **Generates a real bearer token** (`openssl rand -hex 32`) and writes
   `/etc/felucca/feluccad.json` with that value, mode 0640, owned `felucca:felucca`
   — the example config's `REPLACE_WITH_…` placeholder never reaches `/etc`.
6. Installs `deploy/systemd/felucca-firewall.service`, which applies
   `/etc/felucca/firewall.nft` at boot (§8), writes that file if there is none
   — an existing one is kept as it is, whatever is in the environment — and
   applies it immediately.
7. **Enables but does not start** feluccad, and prints the generated token once.

The installer does not start the service because the config still needs this
host's own values and because a control plane should not answer the network
before you have looked at the firewall rules it just installed.

Record the printed token — it is not shown again, and every worker needs the
same value. Then review the config and start:

```sh
sudo cat /etc/felucca/feluccad.json    # token is already filled in
sudo systemctl start feluccad
sudo systemctl status feluccad

# Smoke test (from the server — port 8080 is loopback-only, see §8)
curl -s http://127.0.0.1:8080/healthz   # {"ok":true} — open, no token
curl -s -H "Authorization: Bearer ${TOKEN}" \
     http://127.0.0.1:8080/metrics      # Prometheus text — admin token required (§6.2)
```

To install with a token you already have (rebuilding a host, or pairing with an
existing fleet), pass it in — the installer validates it and does not generate
one:

```sh
FELUCCA_TOKEN=<existing fleet token> sudo -E bash deploy/install.sh control-plane
```

---

## 5. Install — worker nodes

Run on each worker as root, passing the control plane's token and address:

```sh
FELUCCA_TOKEN=<token printed by the control-plane install> \
FELUCCA_CONTROL_PLANE_IP=10.0.1.10 \
  sudo -E bash deploy/install.sh worker
```

The installer:

1. Checks `FELUCCA_CONTROL_PLANE_IP` and `FELUCCA_TOKEN`. A value it refuses
   ends the run here, before anything on the node has been changed.
2. Checks `/dev/kvm`, installs the **pinned** Firecracker release and verifies
   its SHA-256 before it lands in `/usr/local/bin` (§3 note), checks `nft`.
3. Copies `felucca-agent` to `/usr/local/bin/felucca-agent`.
4. Installs `deploy/systemd/felucca-agent.service`.
5. Writes `/etc/felucca/felucca-agent.json` with the real token (mode 0640) and an
   `/etc/felucca/agent.env` stub.
6. Installs `deploy/systemd/felucca-firewall.service`, writes
   `/etc/felucca/firewall.nft` (or keeps the one that is there), and applies
   it (§8).
7. **Enables but does not start** felucca-agent. On a node where the agent is
   already running (an upgrade) it is **not restarted** either — the installer
   prints the one command left to run (§10).

`FELUCCA_TOKEN` is not optional in practice: without it the worker mints its own
token, which is safe but will not match the control plane, so the node never
registers. `FELUCCA_CONTROL_PLANE_IP` is what opens port 9090 to the control
plane — leave it unset on a first install and 9090 stays loopback-only. It
takes one or more literal addresses (IPv4 or IPv6), separated by commas or
spaces; a control plane that reaches this worker over both its LAN and its
overlay address needs both:

```sh
FELUCCA_CONTROL_PLANE_IP='10.0.1.10, 10.100.0.1' sudo -E bash deploy/install.sh worker
```

The installer refuses, with nothing installed or changed:

- anything that is not a literal address — hostnames included, because the
  rules are loaded at boot before DNS is up;
- an address with a `/prefix`. `192.168.104.1/24` pasted from `ip addr` would
  open the port to the whole /24 without a word; to allow a subnet, edit the
  rules file by hand afterwards (§8);
- a value that names nobody: separators only (`,` — two empty variables joined
  by a comma), or nothing but loopback. The variable is there to open the port
  to someone, and a loopback-only list written over a working one would lock
  the control plane out.

**Re-running the installer without the variable keeps the rules file exactly as
it is**; it only rewrites the allow-list when you pass the variable again (§8).

Then finish the config — `bind` must be this node's management address, never
`0.0.0.0`, because `0.0.0.0` includes the bridge gateway every guest routes
through (§6.3). On an overlay fleet, bind the node's overlay address instead.

Read the installed token into a variable **first**: the config file is about to
be rewritten, and reading it inside the same command that truncates it is a race
that has produced empty tokens (which the agent then refuses to start on).

```sh
# 1. Recover the token the installer wrote.
TOKEN=$(sudo sed -n 's/.*"token": "\([^"]*\)".*/\1/p' /etc/felucca/felucca-agent.json)
[ -n "$TOKEN" ] || { echo "no token found — check /etc/felucca/felucca-agent.json"; exit 1; }

# 2. This node's management address.
ADDR=$(hostname -I | awk '{print $1}')

# 3. Rewrite the config. Dropping "_bind_note" is intentional — it is guidance
#    for the shipped example, ignored by the agent, and you have now chosen.
sudo tee /etc/felucca/felucca-agent.json >/dev/null <<EOF
{
  "bind": "${ADDR}:9090",
  "control_plane": "https://felucca.internal.example.com",
  "advertise_addr": "${ADDR}",
  "data_dir": "/srv/felucca",
  "token": "${TOKEN}",
  "pool_size": 2,
  "net": "on",
  "net_cidr": "10.231.0.0/24"
}
EOF
sudo chmod 0640 /etc/felucca/felucca-agent.json
unset TOKEN
```

Then fetch guest assets (kernel + rootfs) — only needed once per worker. The
kernel and base rootfs are pinned by version and SHA-256 and verified before
either is installed or extracted:

```sh
sudo bash deploy/firecracker-assets.sh --data-dir /srv/felucca
```

Then start the agent:

```sh
sudo systemctl start felucca-agent
sudo systemctl status felucca-agent
```

Verify the worker is registered on the control plane within ~5 s:

```sh
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.internal.example.com/api/v1/nodes
# Should show the worker with status "ready"
```

---

## 6. Tokens, auth, and bind addresses

Felucca uses a single shared bearer token between the control plane and all
agents. Enrolled workers additionally get their own per-node credential, which
replaces the shared token on the control-plane↔agent legs — see **§6.7**. The
shared token is still required on every worker (image pulls need it), so set it
everywhere regardless.

`deploy/install.sh control-plane` generates it for you and prints it once; you
only generate one by hand when pre-seeding a host:

```sh
openssl rand -hex 32
# e.g. a3f8c21b0e4d7f69a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901234ab
```

Set it in:

- `/etc/felucca/feluccad.json` — `"token"` key (or `FELUCCA_TOKEN` env var)
- `/etc/felucca/felucca-agent.json` on every worker — same `"token"` value

**Keep the token out of version control.** Store it in `/etc/felucca/feluccad.env`
or `/etc/felucca/agent.env` (mode 0640, owned by the service user) if you prefer
not to put it in the JSON config:

```sh
# /etc/felucca/feluccad.env
FELUCCA_TOKEN=a3f8c21b...

# /etc/felucca/agent.env
FELUCCA_TOKEN=a3f8c21b...
```

Env vars take precedence over config file values (precedence: flags > env > config > defaults).

### 6.1 Both binaries refuse to start without a real token

There is no "auth off by accident" state any more. A missing token used to mean
*no authentication required*; it now means *do not serve*.

| Condition | `feluccad` | `felucca-agent` |
|---|---|---|
| `token` empty / config failed to load | **exits** | **exits** |
| `token` matches a shipped placeholder (contains `REPLACE_WITH`, or `felucca-lab-token`) | **exits** | **exits** |
| `token` too short | **exits** below 32 chars | **exits** below 16 chars |
| Deliberate open mode | `--insecure-no-auth` | *no such flag — the agent always requires a token* |

```
$ feluccad --db /var/lib/felucca/felucca.db
level=ERROR msg=auth err="no auth token configured: set token / FELUCCA_TOKEN /
  --token, or pass --insecure-no-auth for a loopback-only lab"

$ felucca-agent --config /etc/felucca/felucca-agent.json     # unedited example
fatal: auth token: token is a known placeholder — generate a real one with
  `openssl rand -hex 32`
```

This is why the committed example configs can keep an obvious placeholder in
`"token"`, and why the installer substitutes a real value before the file
reaches `/etc`: a config carrying the repo's literal is a credential anyone can
read off GitHub, and it guards the agent's `POST /v1/vms/:id/exec` (arbitrary
commands inside any tenant's guest) and `GET /v1/vms/:id/rootfs` (any tenant's
whole disk image).

`--insecure-no-auth` exists for a loopback lab and nothing else. It exists on
`feluccad` only, it must be named explicitly, and feluccad logs a WARN at every
start — plus a second, louder one when the bind is not a loopback address:

```
level=WARN msg="INSECURE MODE: --insecure-no-auth is set, the admin API accepts
  every caller unauthenticated — loopback binds and lab use only"
level=WARN msg="INSECURE MODE on a non-loopback address: every host that can
  route here has full admin access"
```

There is no agent equivalent, deliberately: the agent API is root on the node.

The lab scripts (`scripts/verify-v2.sh`, `scripts/conformance-lab.sh`,
`scripts/roll-v3.1.sh`) take the token as `$1` or `FELUCCA_TOKEN`, or read it
from `~/.config/felucca/lab-token` (override with `FELUCCA_TOKEN_FILE`). They fail
with a clear error when none is set rather than falling back to a literal.

### 6.2 What the token guards

- Every `/api/*` request requires `Authorization: Bearer <token>`.
- **Three kinds of credential, three reaches.** The admin token is
  unrestricted. A tenant API key (`felucca_sk_…`) reaches its own sandboxes and
  nothing fleet-wide. A node credential (`felucca_nt_…`, §6.7) reaches exactly
  `POST /api/v1/agents/register` and `POST /api/v1/agents/heartbeat`, bound to
  its own node — everything else answers `404`.
- Agent registration and heartbeat accept either the admin token or the
  registering node's own credential.
- **`/metrics` requires the admin token.** It names every worker in hostname
  labels, reports live fleet counts, and takes the global state lock, so an
  open scrape is both reconnaissance and a lock-contention lever. Point
  Prometheus at it with `bearer_token` (scrape config in `deploy/grafana/`).
- `/healthz` and the static UI files are open — `/healthz` is a fixed
  `{"ok":true}` and discloses nothing.
- Comparison is constant-time (no timing attacks).

**Failed credentials are throttled per source address.** The guard covers both
credential gates: the bearer gate on every `/api/*` path and `/metrics`, and
`POST /api/v1/nodes/join` (which runs ahead of the bearer gate because a joining
worker has no API key yet).

> **The credential is checked first, and a correct token is always served.**
> feluccad authenticates *before* it consults the backoff, so the guard only ever
> shapes the answer to an attempt that had already failed. A source sitting in
> backoff still gets `200` the moment it presents a valid token, and nobody
> else's failed guesses can refuse a request that would have succeeded. If you
> are getting `429` on a request you believe is authenticated, the credential on
> that request is wrong — that is the finding, not the throttle.
>
> This was not always true. An earlier revision evaluated the backoff first, and
> because the shipped topology gives every client one shared `127.0.0.1` key,
> one anonymous client sending a bad bearer every few seconds held the whole
> control plane — operators, console and worker nodes — in a `429` that never
> expired. Any documentation or runbook still saying "a correct token is refused
> while the source is throttled" predates that fix.

How the backoff itself accumulates, per key:

- The first **10** failures are answered `401` and cost nothing. The **11th** is
  also answered `401` but arms the backoff; from then on a failure that arrives
  *inside* the armed window is answered `429 {"error":"too many failed
  attempts"}` with `Retry-After` in whole seconds — the remaining wait rounded
  up to the next second, so it never under-states it, and never below `1`.
- Each further failure doubles the wait — 1s, 2s, 4s … — up to a ceiling that
  depends on whether the key stands for one client (below). Accumulated failures
  stop counting at 18, so the wait never grows past that ceiling.
- **Quiet time is the only thing that forgives**: one accumulated failure per
  15s of silence. A *success does not clear the record* — a wipe-on-success is
  reachable by anyone sharing the key, so the console's own authenticated poll
  would otherwise reset a guesser's counter before it ever bit.
- A client that retries more slowly than the wait it was handed never sees a
  `429` at all; it just keeps getting `401`. The backoff costs an attacker
  request rate, and that is all it is for — what actually makes the admin token
  infeasible to guess is the 32-char minimum enforced at startup (§6.1).
- The table is bounded at 4096 sources so a spray across forged addresses cannot
  exhaust memory. Nothing is evicted until it is full; at that point records
  quiet for over 15 minutes go first, then the least-established ones (nothing
  currently serving a backoff, lowest failure count, oldest) — so a wide spray
  evicts itself rather than flushing the record it was trying to displace.

**The ceiling depends on how well feluccad can tell clients apart.** When the key
provably stands for more than one client, the wait is capped at **2s** instead
of 60s — a shared key must not let one anonymous neighbour spend a full minute
of `429`s on everyone who shares it. feluccad treats a key as shared when, and
only when, the *configuration and peer address* say so — never a header, so a
client cannot opt itself into the softer cap:

| Situation | Key | Cap |
|---|---|---|
| No `trusted_proxies`, peer is loopback or RFC1918/ULA private (the shipped Caddy-on-loopback topology, and overlay fleets) | the peer | **2s** |
| Peer *is* a declared proxy but forwarded nothing attributable | the proxy | **2s** |
| A client address attested by the declared proxy chain | that client | **60s** |
| No `trusted_proxies`, peer is a public address (feluccad exposed directly) | the peer | **60s** |

- The key is a source address. Out of the box that is the **connection peer**,
  and behind a reverse proxy every client arrives as the proxy's own address —
  so one misbehaving client's failures raise everyone else's `401`s to `429`s
  (they do **not** lock anyone out; see the box above). Fixing that means
  configuring both halves: the proxy must **overwrite** `X-Forwarded-For`, and
  feluccad must be told to believe it via `trusted_proxies`. **[§7.1](#71-forwarded-client-addresses-required-reading)
  is required reading before you put anything in front of feluccad** — getting it
  half-right in the wrong direction lets a client pick its own throttle key.
  Either way, put a real rate limiter at the proxy (the commented `rate_limit`
  block in §7) rather than treating this as your only guard.
- `429` means two different things on this API. With `Retry-After` it is this
  auth guard. Without it, it is the tenant quota gate
  (`{"error":"quota exceeded: …"}`) — a different answer with a different fix.

### 6.3 Bind address: which topology are you in?

`bind`'s host part is honoured. It used to be discarded, so `127.0.0.1:8080`
still produced a world-reachable control plane; that is fixed, which means the
value now has consequences. Pick deliberately — the two supported topologies
want opposite answers, and adopting the wrong one costs a fleet-wide outage.

| Topology | `feluccad` `bind` | Why |
|---|---|---|
| **Caddy-fronted single node** (§7, the shipped example) | `127.0.0.1:8080` | Caddy terminates TLS and proxies over loopback; §8 drops 8080 from everywhere else. Nothing else needs to reach the port. |
| **WireGuard-overlay fleet** (§6.4) | the hub's overlay address, e.g. `10.100.0.1:8080` | Enrolled agents speak plain HTTP to feluccad *inside* the tunnel. On a loopback bind **every agent registration is refused** and the fleet never forms. |

For `felucca-agent`, `bind` is always **this node's own management address** (or
its overlay address on an overlay fleet) and never `0.0.0.0`. The wildcard
includes the bridge gateway `10.231.0.1` that every guest has as its default
route, and guest→host packets hit the INPUT hook — which the agent's own
forward-chain tenant isolation never sees. The §8 firewall's
`iifname "felucca0" drop` is the second layer under that; the bind address is the
first.

Both shipped example configs carry a `_bind_note` key spelling this out. It is
ignored by both loaders (verified: feluccad parses the example and listens on the
configured host) and is safe to delete once you have set a real value.

### 6.4 WireGuard overlay fleets

Set `wg_ip` on feluccad to enable the overlay; it is off when empty, which is the
default and what the single-node topology uses. feluccad refuses to start with
the overlay half-configured:

- `wg_ip` must be a CIDR (the hub's overlay address, e.g. `10.100.0.1/24`).
- `wg_endpoint` is required — the **public** `host:port` joining workers dial.
- The overlay refuses to run in open mode: minting join tokens without a
  credential would be unauthenticated kernel network configuration.

Then bind feluccad on the overlay address (§6.3), keep the §8 rule aligned with
whatever you bound, and enroll each worker once:

```sh
# On the control plane — mint a one-time join token:
curl -s -XPOST -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/join-tokens

# On the worker — first boot only; the persisted /srv/felucca/wg.json wins after:
felucca-agent --join https://felucca.example.com --join-token <token>
```

The plain listener on `bind` stays up even when `tls_domain` is set: in-tunnel
agents use it, and removing it would cut every enrolled agent off from the
control plane.

### 6.5 The console and the token

The console holds the bearer token in a module-scoped variable that dies with
the tab. It is **not** read from `localStorage`, `sessionStorage`, or a
`?token=` query parameter, and it is not written to any of them.

- Paste the token into the banner the console shows when it has none. That is
  the only way in, by design.
- A `?token=` in the URL is **stripped and ignored**, not accepted. By the time
  the page runs, that token is already in the browser's history entry and in the
  access log of every proxy in front of feluccad — so it is a token to rotate,
  not a token to use. Never put one in a URL, a bookmark, or a shared link.
- An upgraded console clears any `felucca_token` left in web storage by an older
  build. Rotate that value too; it was readable by any script on the origin.
- With no token the console does **not** poll. Every request would be a counted
  auth failure against the throttle in §6.2, and an unattended tab on a 3s timer
  walks a shared source key past the threshold in about 20 seconds. That no
  longer costs the operator their own access — a correct token is served
  regardless (§6.2) — but it does turn every *unauthenticated* answer on that
  key, including the console's own next attempt and anyone else behind the same
  proxy address, into a `429`, and it buries the real 401 in the journal.
- The console never presents data as live when it is not. A header badge and a
  banner name the state: `LIVE`, `NO TOKEN`, `NO AUTH`, `THROTTLED` (with the
  retry countdown), `STALE`, or `MOCK` — the last meaning the control plane was
  never reached and nothing on screen is real.

### 6.6 Finding and revoking a leaked tenant key

Tenant API keys can now be listed and can carry an expiry, so a leaked key is
recoverable without raw SQL against `felucca.db`.

```sh
# List a tenant's keys — id, prefix, timestamps. Never the secret or its hash.
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/tenants/tn-450c79b1/keys
# {"keys":[{"id":"key-f3a3349e533a","prefix":"felucca_sk_2f75",
#           "created_at":1787982290,"expires_at":0,"revoked_at":null}, …]}

# Match the leaked secret against `prefix` (the first 14 chars), then revoke:
curl -s -XDELETE -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/keys/key-f3a3349e533a

# Mint a replacement that expires on its own (seconds; max one year):
curl -s -XPOST -H "Authorization: Bearer ${TOKEN}" \
  -d '{"expires_in_s":2592000}' \
  https://felucca.example.com/api/v1/tenants/tn-450c79b1/keys
```

`expires_at` is `0` for a key that never expires — the historical default, and
still what `POST /api/v1/tenants` mints alongside a new tenant. Prefer an
explicit `expires_in_s` for anything handed to a person or a CI job.

### 6.7 Per-node agent credentials

feluccad used to present the **cluster admin token** on every call it made to a
worker, and every worker had to present that same token back on register and
heartbeat. A node's address comes from `POST /api/v1/agents/register`, so every
outbound dial was a decision about where to deliver that secret — talking
feluccad into dialing an attacker-controlled host handed over the key to the
whole fleet, and compromising any one worker did the same.

Both halves now land. Enrollment with a one-time join token mints a per-node
bearer `felucca_nt_…`, and that credential is used **in both directions**:

- **feluccad → agent.** feluccad presents that node's credential, and nothing
  else, on every proxy call to it. An address with no stored credential is
  dialed with **no** bearer at all — which is where a forged registration lands.
- **agent → feluccad.** The agent presents it on `POST /api/v1/agents/register`
  and `POST /api/v1/agents/heartbeat` instead of the fleet token.
- **agent inbound.** The agent accepts *either* its own credential or its
  configured `token`, so a rotation takes effect without a restart and a
  grandfathered fleet keeps working.

Two enrollment paths mint it, and both return it exactly once:

| Path | When |
|---|---|
| `POST /api/v1/nodes/join` (the wg overlay path, §6.4) | `--join` + `--join-token` on first agent boot |
| `POST /api/v1/agents/register` carrying `join_token` | non-overlay fleets |

**What a node credential can do, and nothing more.** It reaches exactly two
routes — register and heartbeat — and everything else answers `404`. Within
those two it is bound to its own node: it may only register **its own address**
(`403` otherwise), may not claim a hostname another node holds (`403`), may only
heartbeat **its own node id** (`404` otherwise, the same answer as an unknown
id), and may **not** spend a join token (`403` — enrolling is the operator's
act, not a worker's). A credential harvested off one worker is worth that
worker.

#### Rolling it out

New workers need nothing extra: the agent persists what it is given.

1. Mint a join token: `POST /api/v1/join-tokens` with the admin token.
2. Give it to the worker — `--join-token` (overlay) or `join_token` in the
   registration body.
3. The agent writes the issued credential to **`<data_dir>/node-token`**
   (`/srv/ignis/node-token` by default), mode **0600**, and uses it from then on.

Confirm it took, without printing the secret — the agent says which credential
it is on at every start:

```
level=INFO msg="felucca-agent startup" ... node_cred=own
```

`node_cred=shared` means this node is still on the fleet token. That is a
working state, not a broken one, but it is the state the rollout is meant to
leave behind.

#### Already-enrolled nodes

**Nothing breaks and nothing needs doing urgently.** Workers already in
`felucca.db` when this shipped are grandfathered onto the shared token exactly
once, so they keep authenticating in both directions. That one-time bound is the
security property: if "this node has no credential yet" could mint one on
demand, registering an arbitrary address would still get the control-plane key
delivered to it. feluccad names each grandfathered node in a startup WARN:

```
level=WARN msg="existing nodes are still using the shared control-plane token;
  re-enroll each with a join token so it gets its own agent credential" nodes=3
```

To finish the rollout, re-enroll each node with a fresh join token, one at a
time. Re-enrollment **rotates** the credential (feluccad retires the previous one
in the same operation), and it is also the recovery path for a worker that lost
its copy.

> **On an overlay fleet, re-enroll with a fresh WireGuard key.** A join token
> says *an operator authorized an enrollment*; it does not say *which node*, and
> the pubkey naming the node is written by the caller. So `POST
> /api/v1/nodes/join` under a pubkey feluccad already knows must also carry the
> node's **current** credential in a `node_token` field, or it is refused:
>
> ```
> 409 {"error":"pubkey already enrolled: re-join must present the node's current
>      agent token as node_token, or enroll with a fresh wireguard key"}
> ```
>
> Without that check, anyone holding a valid join token could rotate an existing
> worker's credential out from under it and take that worker off the control
> plane.
>
> **`felucca-agent` does not send `node_token`** — its join request carries only
> `pubkey` and `hostname` (verified against `rust/agent/src/wg.rs`). So the
> supported way to re-enroll a worker is a fresh key: stop the agent, remove
> **both** `<data_dir>/wg.json` and `<data_dir>/wg.key` (plus `node-token` if you
> are recovering a lost credential), and start it with a new `--join-token`.
> `ensure_key` only generates a key when the file is absent, so leaving `wg.key`
> in place is exactly what produces the `409`. The `node_token` field is there
> for a client that can present the credential; today that is a `curl`, not the
> agent.
>
> **What a fresh key costs you.** The new pubkey is a new peer, so it gets a
> **new overlay address** and the node re-registers under it. The node's own
> identity survives — feluccad keys node rows on hostname, so the id and its
> sandboxes stay put and only `addr` moves. But the **old WireGuard peer is not
> removed**: its row and its kernel peer entry stay, and its overlay address
> stays allocated. Nothing in the API retires a peer today, so on a fleet you
> re-enroll often, watch the overlay pool (`wg_ip`'s prefix) and prune stale
> peers by hand with `wg set wg-felucca peer <old-pubkey> remove`.
>
> On a **non-overlay** fleet there is no pubkey and no such check: re-enrolling
> is just another `POST /api/v1/agents/register` with a fresh `join_token`.

#### The fleet token is still required on workers

Per-node credentials narrow the blast radius; they do not remove the shared
token from workers. `felucca-agent` still needs `token` set, still refuses to
start without a real one (§6.1), and still uses it for **template image pulls**
(`GET /api/v1/images/{name}`), which are an admin-only route a node credential
cannot reach. Keep treating a worker compromise as an admin-token compromise
until that path has its own credential.

#### Journal lines worth knowing

```
level=WARN msg="no agent credential for this node: dialing it without one —
  enroll it with a join token" node=10.100.0.2:9090
```
feluccad is dialing a node it has no credential for — a node registered without a
join token, or one whose row was removed. Enroll it.

```
level=WARN msg="node is still on the shared control-plane token (enrolled before
  per-node credentials) — re-enroll it with a join token" node=10.100.0.2:9090
```
One grandfathered node, named once per process (the resolution is memoized
afterwards, so the exec hot path does not drown the journal). This is the list
to work through.

```
level=INFO msg="migrated node credential to its host:port key" node=10.100.0.2:9090 from=10.100.0.2
```
Expected once per node on the first upgrade: credentials are now keyed on
`host:port` rather than the bare host, because two agents on one host are two
nodes. Rows written under the old key are migrated the first time that node is
dialed.

```
fatal: node token: /srv/ignis/node-token: node token is readable beyond its
  owner (mode 0644) — run `chmod 600 /srv/ignis/node-token`, or delete it and
  re-enroll with a fresh join token
```
The agent refuses to start on a credential its host's other users can read —
that credential is already disclosed. A *missing* file is not an error: it means
this node never enrolled, and the shared token carries both directions.

---

## 7. TLS and the reverse proxy (Caddy or nginx)

Felucca does not terminate TLS itself. Run Caddy on the control plane host.
(In-binary autocert is the other option — set `tls_domain`, §6.4. Either way,
if anything at all sits in front of feluccad, read **§7.1**: the proxy decides
what address feluccad throttles on.)

Install Caddy (Debian/Ubuntu):

```sh
apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | tee /etc/apt/sources.list.d/caddy-stable.list
apt-get update && apt-get install -y caddy
```

`/etc/caddy/Caddyfile`:

```caddy
felucca.example.com {
    # Caddy obtains and renews a Let's Encrypt certificate automatically.
    # Replace with your real domain.

    # Reverse-proxy everything to feluccad on loopback.
    #
    # REQUIRED at the edge. header_up makes X-Forwarded-For say exactly one
    # thing — the peer Caddy actually accepted — instead of appending to
    # whatever the client sent. feluccad only reads it if you ALSO set
    # trusted_proxies; see §7.1, and do not set one half without the other:
    # trusted_proxies pointing at a proxy that forwards the client's header
    # lets the client choose its own throttle key.
    reverse_proxy 127.0.0.1:8080 {
        header_up X-Forwarded-For {remote_host}
    }

    # Optional: rate-limit the API to protect against token brute-force.
    # Requires the caddy-ratelimit module.
    # rate_limit {
    #     zone api {
    #         key    {remote_host}
    #         events 200
    #         window 1m
    #     }
    #     match path /api/*
    # }

    # Logging.
    #
    # This access log records the full request URI, so anything in a query
    # string lands on disk and in whatever ships these logs onward. Nothing in
    # Felucca puts a credential there: the API takes the token in the
    # Authorization header (which Caddy does not log), and the console strips
    # and ignores `?token=` rather than accepting it (§6.5). Keep it that way —
    # do not add a token to a URL to "make curl easier".
    log {
        output file /var/log/caddy/felucca-access.log
    }
}
```

If your threat model includes the log itself — a shared host, or logs leaving
the box — Caddy's `format filter` can redact query parameters from the logged
URI (`fields { request>uri query { replace token REDACTED } }`). That block is
not exercised by this repo's tests, so run `caddy validate` against your
Caddyfile after adding it rather than trusting it on sight.

> **If a token ever did reach a URL** — an old bookmark, a copied link, a
> scripted `curl` with `?token=` — treat it as disclosed and rotate it.
> Regenerate with `openssl rand -hex 32`, update `feluccad.json` and every
> worker's `felucca-agent.json`, and restart. It is in browser history and in the
> proxy log whatever the console does with it afterwards.

Reload Caddy:

```sh
systemctl reload caddy
# Or test the config first:
caddy validate --config /etc/caddy/Caddyfile
```

Caddy handles ACME (Let's Encrypt / ZeroSSL) automatically when port 443 and
port 80 (HTTP-01 challenge) are reachable from the internet, or use `tls
internal` for a local CA if the control plane is not internet-facing.

### 7.1 Forwarded client addresses (required reading)

feluccad keys its brute-force throttle (§6.2) on the address it attributes a
request to. Behind a proxy, the connection peer is always the proxy, so without
configuration **every client shares one throttle key** and one client's failed
guesses raise everyone else's `401`s to `429`s. The fix has two halves — a
feluccad setting and a proxy directive — and it is **both or neither**. Doing
only the feluccad half is the one outcome that is *worse* than leaving it alone:
it turns a shared key into a client-chosen one.

> **Why this section is load-bearing and not advisory.** feluccad cannot tell a
> forwarded header its proxy *wrote* from one the proxy *copied through* — both
> arrive as bytes on a connection from the proxy. Nothing feluccad can do in code
> closes that; the only thing that closes it is the proxy overwriting the header
> on every request. So: if you list a proxy in `trusted_proxies`, that proxy
> **must** be configured to overwrite `X-Forwarded-For`. A proxy that forwards
> the client's own header verbatim, listed as trusted, lets any client set
> `X-Forwarded-For: <anything>` and be counted as that address — a fresh throttle
> key per request, and someone else's key on demand.
>
> If you cannot guarantee the proxy overwrites the header, leave
> `trusted_proxies` empty. The shared-key cost is bounded and documented (§6.2
> caps the wait at 2s for a shared key); a forgeable key is not.

**Half 1 — feluccad: `trusted_proxies`.** feluccad honours `X-Forwarded-For`
**only** when the immediate peer's address falls inside one of the CIDRs listed
here (`X-Real-IP` needs one more opt-in — see below). The default is **empty,
which means trust nothing**: no forwarded header is read at all, and the
connection peer is the key. That default is the safe one and it is deliberate —
a client-settable header believed by default lets any caller choose its own
throttle key, which is strictly worse than throttling the proxy as one source.

| Source | Form | Example |
|---|---|---|
| Config file (`/etc/felucca/feluccad.json`) | JSON array of strings | `"trusted_proxies": ["127.0.0.1/32", "::1/128"]` |
| Environment | comma-separated | `FELUCCA_TRUSTED_PROXIES=127.0.0.1/32,::1/128` |
| Flag | comma-separated | `--trusted-proxies=127.0.0.1/32,::1/128` |

- A **bare IP is accepted** and normalised to a single-host CIDR (`127.0.0.1` →
  `127.0.0.1/32`, `::1` → `::1/128`).
- An entry feluccad cannot parse as a CIDR is a **startup failure**, not a
  skipped line: silently dropping it would leave you keying throttles off the
  proxy's own address while believing otherwise, with nothing anywhere saying
  why.

```
level=ERROR msg=trusted-proxies err="trusted proxy \"10.0.0.0/99\" is not a
  CIDR: write e.g. \"127.0.0.1/32\" for a local reverse proxy or \"10.0.0.0/8\"
  for a range"
```

- `--trusted-proxies=""` is an explicit "trust nobody" and is honoured as such.
- List **every** proxy hop, and **only** proxies. feluccad walks the
  `X-Forwarded-For` chain right to left and stops at the first hop that is not
  itself a trusted proxy — that hop is the client. **The left-most entry, the one
  this kind of code usually reaches for, is whatever the original caller wrote
  and is never used.** A hop feluccad cannot parse ends the chain of custody and
  it falls back to the peer; if every hop is one of yours, the peer is as far as
  it goes.
- The walk stops after **16 hops**, and a chain longer than that falls back to
  the peer. The header is caller-sized, so this is a bound on work, not a
  deployment limit: real chains are one or two hops. If you genuinely run more
  than sixteen proxies in front of feluccad, the ones past the sixteenth from the
  right cannot be attributed and every client behind them shares the peer key.
- The whole request head — request line plus all headers — is capped at **16
  KiB** (Go's default is 1 MiB, and this parse runs before authentication).
  Over that, feluccad answers `431 Request Header Fields Too Large`. Generous for
  a bearer, a trace id and a proxy chain; if your proxy injects a large header
  set, that is the limit to check.
- **Never widen this to a range that contains real clients** (and never
  `0.0.0.0/0`). Everything inside these CIDRs is trusted to name whoever it
  likes, so a client inside one can forge its own throttle key — the exact
  problem the empty default exists to prevent.

For the shipped Caddy-fronted single-node topology (§7), the whole of half 1 is:

```jsonc
// /etc/felucca/feluccad.json
"trusted_proxies": ["127.0.0.1/32", "::1/128"]
```

**`X-Real-IP` is a second, separate opt-in: `trust_x_real_ip`.** It defaults to
**false**, and while it is false feluccad ignores `X-Real-IP` completely — from
*any* peer, declared proxy or not. Listing a proxy in `trusted_proxies` does
**not** by itself make `X-Real-IP` believed.

| Source | Form | Example |
|---|---|---|
| Config file | JSON boolean | `"trust_x_real_ip": true` |
| Environment | boolean string | `FELUCCA_TRUST_X_REAL_IP=true` |
| Flag | boolean | `--trust-x-real-ip` |

- The reason it is separate: `X-Forwarded-For` carries a chain feluccad can walk,
  so a client's forged prefix is discarded at the first trusted-proxy boundary.
  `X-Real-IP` is a bare single value with **no chain of custody at all** —
  whatever the last hop wrote, *or forwarded verbatim*, becomes the throttle key.
  Believing it is safe only behind a proxy that rewrites it on every request.
- It is read only when there is no `X-Forwarded-For` on the request, only from a
  declared proxy, and a value that is itself a declared proxy address is ignored.
- **Setting it without `trusted_proxies` is a startup failure**, not a warning:
  with nothing declared, every peer would be believed, which is precisely the
  forgery this setting has to be kept away from.

```
level=ERROR msg=trusted-proxies err="trust_x_real_ip is set but no
  trusted_proxies are declared: X-Real-IP has no chain of custody, so it is only
  ever read from a declared proxy — list the proxy's address in trusted_proxies,
  or drop trust_x_real_ip and have the proxy send X-Forwarded-For"
```

Leave `trust_x_real_ip` alone unless you have a proxy that cannot be made to
send `X-Forwarded-For`. `X-Forwarded-For` from a proxy that overwrites it is
strictly better and is what both the Caddy and nginx blocks below configure.

**Half 2 — the edge proxy must OVERWRITE the header.** Not append to it, not
pass it through. This is a requirement, not a hardening tip: it is the only
thing standing between a listed proxy and a client-chosen throttle key, because
feluccad cannot tell the two apart on the wire.

*Caddy — the shipped topology (§7).* `reverse_proxy` does set `X-Forwarded-For`
by itself, **appending** the connection peer to any value the client supplied.
Since feluccad walks the chain right to left, the appended real peer is the entry
it lands on, so a bare `reverse_proxy` is not exploitable today. Do not rely on
that: it makes your throttle key a property of feluccad's parser rather than a
fact about your proxy, and it changes if either side's chain handling ever does.
**Set the overwrite explicitly** — this line is in the shipped Caddyfile in §7
and is required, not optional:

```caddy
reverse_proxy 127.0.0.1:8080 {
    # REQUIRED at the edge. Replaces the client's value with the peer Caddy
    # actually accepted. Without it the header still carries whatever the
    # client wrote.
    header_up X-Forwarded-For {remote_host}
}
```

Nothing else is required on the Caddy side. Caddy does not need to be told about
`X-Real-IP` at all, because feluccad ignores that header unless you opt in with
`trust_x_real_ip` (half 1) — and you should not.

*nginx as the edge proxy.* nginx sets **nothing** by default: with a bare
`proxy_pass`, whatever `X-Forwarded-For` the client sent is forwarded
**verbatim**. List that nginx in `trusted_proxies` and a client sending
`X-Forwarded-For: 203.0.113.7` is counted as `203.0.113.7` — its own throttle
key, refreshed at will, or someone else's on demand. **This is the failure mode
this whole section exists to prevent.** These lines are **required**, not
optional:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;

    # REQUIRED at the edge. $remote_addr REPLACES any client-supplied value.
    proxy_set_header X-Forwarded-For $remote_addr;

    # Harmless, and correct if you ever set trust_x_real_ip. feluccad ignores
    # X-Real-IP while that stays false (the default), so this line is not
    # what makes the setup safe — the X-Forwarded-For line above is.
    proxy_set_header X-Real-IP       $remote_addr;

    # Not security-relevant, but expected by anything behind a proxy:
    proxy_set_header Host              $host;
    proxy_set_header X-Forwarded-Proto $scheme;

    # SSE (streaming exec, API-V2 §3f) needs buffering off.
    proxy_buffering off;
    proxy_read_timeout 3600s;
}
```

**Do not use `$proxy_add_x_forwarded_for` at the edge.** It is the variable
every nginx snippet on the internet reaches for, and it is the wrong one here:
it **appends** `$remote_addr` to the client's own header rather than replacing
it, so the header still carries whatever the client wrote. feluccad's
right-to-left walk means the appended real peer is what it keys on, so this is
not exploitable today — but it leaves your throttle key depending on a parser
detail instead of on your proxy, and it is one config copy away from the bare
`proxy_pass` above. Use `$remote_addr`.

`$proxy_add_x_forwarded_for` is correct in exactly one place: an **inner** hop
whose own upstream proxies are all listed in `trusted_proxies`. At the edge —
the hop that terminates the client's connection — it is always `$remote_addr`.

**Check it.** A client forging a header must not be able to give itself a fresh
throttle key. Burn one source past the threshold, then send two failing requests
back to back — one plain, one with a forged `X-Forwarded-For`. **Both must come
back with the same status**, because they are the same client:

```sh
URL=https://felucca.example.com/api/v1/nodes
AUTH="Authorization: Bearer wrong"

# Burn the key: 20 rapid failures. The tail must contain 429s.
for i in $(seq 20); do
  curl -so /dev/null -H "$AUTH" -w '%{http_code} ' "$URL"
done; echo

# Immediately, back to back: plain, then forged. Compare the two numbers.
curl -so /dev/null -H "$AUTH" -w 'plain=%{http_code} ' "$URL"
curl -so /dev/null -H "$AUTH" -H "X-Forwarded-For: 203.0.113.9" \
     -w 'forged=%{http_code}\n' "$URL"
```

`plain=429 forged=429` is the pass: the forged header bought nothing. Any result
where **`forged` is `401` while `plain` is `429`** is the failure — the forged
header got a fresh key, meaning a proxy in the path is passing the client's
`X-Forwarded-For` through *and* feluccad is trusting the peer that sent it. Fix
half 2, or clear `trusted_proxies` until you can.

Send the last two requests immediately after the loop and immediately after each
other: the armed window is short (2s for a shared key, up to 60s for an
attributable one — §6.2), and a pause long enough for it to lapse makes both
requests `401` and tells you nothing. Two equal numbers are the signal; a
`plain=401` run is inconclusive, not a pass — re-run it faster.

Run this against a staging control plane. It deliberately drives the source you
run it from into backoff — for a couple of seconds in the shipped shared-key
topology, up to a minute once `trusted_proxies` is configured and your client
address is attributable. It costs you nothing if you hold a valid token (§6.2:
a correct credential is served regardless), but anyone *else* on that key gets
`429` instead of `401` while it lasts.

The Caddy `header_up` line and the nginx block are not exercised by this repo's
tests. Run `caddy validate --config /etc/caddy/Caddyfile` or `nginx -t` after
adding them rather than trusting them on sight.

---

## 8. Host firewall

```
                     Internet
                         │
                   Port 443/80  ← open to clients
                         │
                    [ Caddy ]
                         │
                   Port 8080    ← loopback ONLY (127.0.0.1)
                         │
                   [ feluccad ]
                         │
          Private network (e.g. 10.0.1.0/24)
                         │
                   Port 9090    ← control plane → workers ONLY
                         │
                [ felucca-agent ]
                         │
          Bridge felucca0 (10.231.0.1)  ← guests must NOT reach the host here
                         │
                   /dev/kvm, tap
                         │
                 [ Firecracker ]
```

**These rules are installed, not suggested.** `deploy/install.sh` writes
`/etc/felucca/firewall.nft` and installs `deploy/systemd/felucca-firewall.service`
to apply it. The ruleset lives in its own `inet felucca_host` table, so it
never collides with the agent's `ip felucca` table or with whatever else the
host runs.

**An agent does not start without the guest fence.** The fence is one rule of
the worker ruleset below — `iifname "felucca0" drop` in chain `input` — and
`felucca-agent.service` looks for it in the kernel every time it starts, in two
`ExecStartPre=` steps:

1. **Heal.** If the rule is not loaded, the unit asks systemd to load the rules
   file again (`systemctl reload-or-restart felucca-firewall.service`: a reload
   when that unit is active, a start when it is stopped or failed). This is what
   a ruleset flush needs: after one, the firewall unit still shows `active`, so
   nothing else would run it again.
2. **Gate.** If the rule is still not loaded, the unit takes the guest bridge
   `felucca0` down, logs one line that says why, and fails the start. Sandboxes
   outlive the agent (§10), so they are still running at that point; with the
   bridge down they keep their state but are off the network, instead of sitting
   on one that nothing fences while no agent is there.

A failed start is retried every 5 s, and every retry runs step 1 again. So once
the rules file is right the agent comes back by itself — no `reload`, no
`start` — and when it is up and its own isolation checks have passed it raises
the bridge again. The journal line to look for:

```
felucca-agent: not starting: the guest fence (iifname felucca0 drop) is not
  loaded in table inet felucca_host, so the guest bridge felucca0 was taken down.
```

What this does and does not give you:

- The check is made **when the agent starts**, not while it runs. A ruleset
  flushed under a running agent goes unnoticed until its next start (FLU-8
  tracks checking continuously); from the flush until then the node is not
  fenced.
- The check is for **that rule, spelled that way**. A chain that merely exists
  does not pass, and neither does a rules file whose fence line was removed or
  rewritten — `iifname "felucca0" counter drop` is refused. To count or log
  what the fence drops, add a rule of your own in front of it and leave the line
  alone.
- It looks at the kernel, not at the firewall unit's state. Until ADR-0011 the
  agent had `Requires=felucca-firewall.service` instead, which only attested
  that the unit had run once (flush the table afterwards and the agent still
  started), restarted the agent every time the firewall unit was restarted, and
  left the agent **down with no retry** when that unit failed to start. The
  agent now `Wants=` the firewall unit and orders itself after it, so the rules
  are still loaded first at boot.

The firewall unit loads the file as **one nft transaction**: create the table
if it is absent, delete it, load `/etc/felucca/firewall.nft`, then require that
the file really defined the `input` chain. A file that does not parse, is
empty, or defines no `input` chain commits nothing — the rules that were in
force stay in force — and loading twice never duplicates a rule. `start`,
`reload` and `restart` all run that same transaction. `stop` does **not** remove
the rules: sandboxes keep running when their agent is stopped (§10), so a stop
that deleted the table would expose the host to every guest still up. To take
the rules out on purpose: `nft delete table inet felucca_host`.

The unit is a reviewable file under `deploy/systemd/` rather than a heredoc
inside the installer, because it is part of that enforcement path, and it is
covered by `scripts/verify-units.sh` along with the other three units. The
installer only rewrites the two absolute paths in the units — `/usr/sbin/nft`,
which the agent's two gate lines also name, and the rules file — and only when
`nft` or the config directory is somewhere other than the shipped default.

```bash
bash scripts/verify-units.sh    # systemd-analyze verify over deploy/systemd/*
```

Run it on a systemd host (the lab VM is fine). It stages a throwaway root with
stub binaries at the paths the real `ExecStart` lines name, so the units are
checked exactly as shipped — without it, `systemd-analyze verify` on a checkout
always fails on "Command /usr/local/bin/feluccad is not executable" and on the
unresolved `felucca-firewall.service`, and both of those are artifacts of the
checkout rather than defects. It treats any output as failure: systemd exits 0
on a malformed `RestrictAddressFamilies=` or an unknown directive while only
warning about it, which would otherwise let a hardening regression through.
After the parse it also asserts that the settings the fleet's availability
rests on are the ones **in effect** — the last assignment of each, the whole
ordered list for `ExecStartPre=`, `StandardInputText=`, `ExecStart=`,
`ExecReload=` and `ExecStop=` — and that `Requires=` (or `Requisite=`,
`BindsTo=`, `PartOf=`) on the firewall unit, an `OOMScoreAdjust=` and an
`ExecStopPost=` have not come back, however they are spelled. A unit can parse perfectly and still have lost the
one line that mattered, or still contain it and override it three lines later.
Last, it breaks copies of the units in 25 such ways and requires its own
assertions to reject every one.

The rule that matters most is the worker's `iifname "felucca0" drop`. The agent
listens on the bridge gateway that every guest has as its default route, and
guest→host packets hit the INPUT hook — which the agent's own forward-chain
tenant isolation never sees. Without it, a tenant with root in their own
sandbox (the expected design) reaches the node's root control API and can exec
into every other tenant's guest. Established flows and ICMP stay allowed so
liveness checks still work. It is also the rule the agent's start gate looks
for (above): keep the line exactly as generated.

Worker ruleset (`FELUCCA_CONTROL_PLANE_IP=10.0.1.10`):

```
table inet felucca_host {
    chain input {
        type filter hook input priority filter; policy accept;
        iifname "felucca0" ct state established,related accept
        iifname "felucca0" meta l4proto { icmp, ipv6-icmp } accept
        iifname "felucca0" drop
        tcp dport 9090 ip saddr != { 127.0.0.1, 10.0.1.10 } drop
        tcp dport 9090 ip6 saddr != { ::1 } drop
    }
}
```

Control plane ruleset:

```
table inet felucca_host {
    chain input {
        type filter hook input priority filter; policy accept;
        tcp dport 8080 ip saddr != 127.0.0.1 drop
        tcp dport 8080 ip6 saddr != ::1 drop
    }
}
```

Inspect and manage them with:

```sh
sudo nft list table inet felucca_host        # what is in the kernel — the thing that counts
sudo systemctl status felucca-firewall       # whether the last load of the file worked
sudo systemctl reload felucca-firewall       # apply an edited rules file
sudo systemctl reload-or-restart felucca-firewall   # the same, whatever state the unit is in
```

`reload` restarts nothing: not the agent, not a single sandbox. If the edited
file does not load, the command fails, the unit stays active and the previous
rules stay in force — fix the file and reload again. `restart` does the same
thing to the rules and is equally safe; before ADR-0011 it restarted the agent,
which killed every sandbox on the worker. A plain `reload` is refused when the
unit is stopped or failed; `reload-or-restart` works in every state, and is
what the agent's start gate uses.

What a reload loads is **the file as you wrote it**: the unit includes it
verbatim in its transaction. The generated file defines the one table and
nothing else. A hand-written one that begins with `flush ruleset` wipes every
other table on the node in that same transaction — the agent's isolation
tables included — and the reload still reports success. Do not put it there.

**What the installer does with the rules file on a re-run.**

- It **keeps an existing `/etc/felucca/firewall.nft` byte for byte**. (It used
  to rewrite the file every time, so a re-run without the variable silently
  narrowed port 9090 to loopback and locked the control plane out.)
- The one exception: a **worker** run that names the control plane
  (`FELUCCA_CONTROL_PLANE_IP`, §5) writes the file again from that value. The
  **control-plane** role never reads the variable — a control plane whose 8080
  rule you widened by hand keeps it, whatever is exported in the shell you run
  the installer from.
- A kept file is still checked before it is applied again: it must pass a dry
  run of the unit's transaction, and on a worker it must contain the fence
  line. If it does not, the installer says what is missing, leaves the file
  and the rules in force alone, and exits non-zero. A kept file with no rule
  for the role's port (9090 or 8080) only gets a warning.
- A file it writes is dry-run the same way first, created `root:root 0640`,
  and moved into place by rename; the unit is installed and loaded by systemd
  before that, so nothing that can fail stands between "new file in place" and
  "new file applied". It is applied with `reload`, and the port rules in force
  are printed before and after.
- If applying a new file fails, the previous file is put back **and applied
  again**, and the installer exits non-zero. The rules in force are then the
  ones from before the run.
- The file is `root:root 0640` after every run, kept or written, on both
  roles: root loads it, so nothing else may write it. (The control-plane
  role's ownership pass over `/etc/felucca` skips it.)
- Without `nft` the installer cannot do any of this: it says so and exits
  non-zero. It used to warn and report success.

To get a freshly generated file, delete it and re-run the installer. A rules
file written by an installer from before ADR-0011 still begins with
"regenerated on every run, do not hand-edit". That header is no longer true —
the file is kept, and the installer tells you so when it meets one.

Local additions belong in a separate table of your own. The exceptions are the
allow-lists themselves: a table of your own cannot let through what this one
drops, so to admit a subnet on 9090, or workers that reach feluccad directly on
8080 rather than through Caddy, edit the rule in `/etc/felucca/firewall.nft`
and `reload` — and note that the API is then exposed to whatever you widened it
to.

Known limits (each is tracked as its own issue):

- **`feluccad.service` has no start gate.** It starts whether or not these
  rules are loaded. On a control plane that binds anything but loopback, check
  the kernel yourself before starting it — `sudo nft list chain inet
  felucca_host input` — not `systemctl is-active felucca-firewall`, which only
  says the unit ran once and still says `active` after a flush.
- **One host, both roles.** There is one rules file, and each role's installer
  run generates only its own rules. On a host that is both control plane and
  worker, the second run keeps the first role's file: a worker run then stops
  with "has no guest fence" (and the agent would refuse to start), a
  control-plane run goes through with a warning that nothing covers 8080. Such
  a host needs both sets of rules in the file, written by hand.
- **Host shutdown.** Sandboxes no longer stop when the agent unit stops (§10),
  and a shutdown only stops units. Left at that they would run until
  systemd's final kill pass, after other units have unloaded rules: the
  distribution's `nftables.service` flushes the whole ruleset when it stops
  (Ubuntu 24.04 ships it with `ExecStop=/usr/sbin/nft flush ruleset`, disabled
  by default). `felucca-firewall.service` therefore ends the sandboxes itself
  when — and only when — the host is shutting down: it is stopped after the
  agent and before that loader. Guests are killed, not asked to shut down
  (that was already so). A sandbox still running when the node goes down
  comes back `stopped` (a sandbox that was asleep stays `sleeping`); see §10.

Ports 443/80 for Caddy are yours to open as usual:

```sh
ufw allow 443/tcp
ufw allow 80/tcp
```

### 8.1 The agent's own nft tables (not installer-managed)

`felucca-agent` builds and maintains its own rules at runtime, in tables the
installer never touches. Expect to see them on a worker; do not hand-edit them
(every one is flush-and-rebuilt, so edits are lost on the next lifecycle event).

| Table / chain | Purpose |
|---|---|
| `ip felucca postrouting` | egress masquerade for the guest CIDR |
| `ip felucca forward` | cross-tenant isolation — the `tenant_pairs` set (ADR-0005) |
| `ip felucca input` | guest→host fence: drops everything arriving on `felucca0` except ICMP and replies to host-initiated flows, with the agent's own port named in an explicit drop |
| `ip felucca ingress` | per-expose DNAT to guest ports (ADR-0007) |
| `bridge felucca forward` | accepts only ARP and IPv4 between bridge ports, dropping everything else — closes the pure-L2 path around the IPv4-only tenant rules, link-local IPv6 above all |
| `netdev felucca <tap>` | one chain per tap, `policy drop`: pins that guest's source MAC, ARP sender MAC, ARP sender IP, and IPv4 source address |

Two operational consequences:

- **IPv6 is disabled on `felucca0`** by the agent. Guests get IPv4 from the
  kernel command line; there is no IPv6 guest networking to configure.
- **The agent refuses to start** if networking is on and any of these fences is
  not in force. That is deliberate: a worker without them puts every tenant on
  one flat network, and nothing on the tenant's side would show it while feluccad
  kept scheduling onto it. The fatal names what failed — fix `nft`/permissions,
  or run the agent with `--net off` to serve unnetworked guests on purpose.

  ```
  fatal: cross-tenant isolation is not in force on this node (see the errors
    above) — refusing to serve tenants
  ```

These sit **under** the installer's `inet felucca_host` rules from §8, not
instead of them. Both layers exist because they fail independently: the agent
builds its tables when it starts (and rebuilds forward isolation at every
create, fork and delete, and the ingress chain at every expose, unexpose and
delete), while the installer's are loaded by the firewall unit and are missing
if that unit was never installed.
Neither disappears when its process stops — nft tables live in the kernel, so
the agent's fences stay in force while the agent is down or restarting, which
is what keeps the sandboxes that outlive it (§10) fenced.

What does remove them, both layers at once, is a `nft flush ruleset` — by
hand, by the distribution's own `nftables.service` (its stock config begins
with one, and so does its stop: see "Host shutdown" in §8), or by a
rules file of your own that starts with one. Since stability wave 1 the
agent's **isolation watch** (§8.2) puts its own tables back within one tick
(5 s); it does not put back the host table `inet felucca_host`, which belongs
to `felucca-firewall.service`: reload that unit (`systemctl reload-or-restart
felucca-firewall`), or restart the agent, whose start gate loads it (§8). The
firewall unit is ordered after `nftables.service` so that at boot the
distribution's loader cannot wipe a table that is already loaded.

`felucca-agent` also refuses a `bind` inside the guest CIDR outright, for the
same reason the wildcard is not the default (§6.3):

```
fatal: refusing to listen on 10.231.0.1 — it is inside the guest CIDR
  10.231.0.0/24, which every sandbox can route to
```

If you genuinely front the agent with your own firewall and want the wildcard
back, `bind_any` (`FELUCCA_BIND_ANY`, `--bind-any`) is the named opt-out. It is
off by default; leaving it off, the agent listens on its management address
**and** on `127.0.0.1` — so local health checks and the rollout scripts keep
working, which is why dropping the wildcard costs nothing.

### 8.2 The isolation watch

Every 5 s the agent compares what is in the kernel with what it last applied,
and puts back what is missing. It looks at:

- the guest bridge `felucca0` itself;
- the five fixed chains: `ip felucca` `postrouting` (NAT), `input`, `forward`
  and `ingress`, and `bridge felucca forward` (one `nft -t list table` per
  table per tick);
- the anti-spoof chain `netdev felucca as_<tap>` (for tap `hth-3`:
  `as_hth_3`) of every tap enslaved to `felucca0` that has carrier. A tap no
  Firecracker holds (a sleeping or stopped sandbox) shows `NO-CARRIER`,
  carries nothing and is skipped;
- the switches the agent sets at start-up:
  `/proc/sys/net/bridge/bridge-nf-call-iptables`, and, on a kernel with IPv6,
  `/proc/sys/net/ipv6/conf/felucca0/disable_ipv6`; both must read `1`;
- the host fence `iifname "felucca0" drop` in chain `input` of `inet
  felucca_host` — reported, never repaired (below).

**The marker rule.** Every chain the agent builds starts with a rule

```
counter comment "felucca:<16 hex digits>"
```

whose hex digits are a hash over the whole ruleset the agent applied for that
chain (for `ip felucca forward`, the members of the `tenant_pairs` set
included). It matches every packet and does nothing else. On nftables 1.0.9,
`nft list` shows it at the head of every Felucca chain as `counter packets N
bytes M comment "felucca:…"`, and `nft -s list` as `counter comment
"felucca:…"`. Because it is a counter, its packet and byte counts change all
the time: a tool that hashes or diffs `nft list` output to detect changes must
list with `nft -s` (stateless), or it sees a change on every run. The agent
reads the marker in either form.

**Before it trusts what it reads.** Right after start-up has applied its
chains, the agent reads each of them back once. If a chain it has just applied
is listed without a marker it can read, this build cannot read what this
kernel prints, and a watch would see every chain as missing. The watch then
**does not run in this process** (as with `off`), and the agent logs, as an
ERROR and on stderr, a line beginning `isolation watch NOT started:` that
names the chain and quotes its first rule. **Alert on this line fleet-wide:**
it means the watch is off on that worker without anyone having chosen it.

The same check runs per table (`ip`, `bridge`, `netdev`) while the watch runs:
nothing is taken down, and no new guest is refused, because of what a table
shows until one of the agent's own markers has been read back in it. A table
in which a chain the agent applied lists without a readable marker is left
alone for the rest of the process, with the ERROR `isolation watch: a chain
this agent applied is listed without a marker it can read; nothing in this
table is watched or taken down in this process` (also on stderr, with the
chain and its first rule).

**What counts as drift.** For each chain, the watch compares the marker in the
kernel with the hash the agent last applied. A missing table, a missing chain,
a flushed chain, a chain re-created empty, a chain rebuilt with other content
and a table set `dormant` all show as drift: the watch logs `isolation watch:
drift — the chain is not what this agent last applied; rebuilding it` (with
`table`, `chain` and what it `found`), rebuilds the chain with the same
builder start-up uses and the current membership, looks again, and logs
`isolation watch: chain repaired and confirmed`. A switch that does not read
`1` is set again (`isolation watch: drift — a switch this agent set to 1 is
not; setting it again`, then `isolation watch: switch repaired and
confirmed`). The watch never recomputes what the rules should be from the
sandboxes on the worker: that is the job of the rebuilds that run when
sandboxes come and go. A chain whose last rebuild by the agent itself failed
is rebuilt every tick (ERROR `isolation watch: the chain's last rebuild failed
and rebuilding it failed again; it is retried every tick`, logged again only
when the outcome changes).

A failure is logged once per episode, and again when its text changes; the
`failed_repairs` counter counts episodes. The counters `repairs`,
`failed_repairs` and `skipped_ticks` are logged in one line, `isolation watch
counters`, whenever one changes.

**Taking something down needs positive evidence twice.** A look has three
outcomes: in place, positively not in place, or could not look (the command
failed or timed out). A repair has three too: applied, refused by the kernel
(`nft` exited non-zero with an nft error such as `Error: …` or
`/dev/stdin:L:C-C: Error: …`), or unknown (no answer: timed out, killed, could
not run). Something is taken down only when the first look said positively not
in place **and** the repair was refused, or it was applied and a second look
still says positively not in place. A look that could not be made, or a repair
with no answer, takes nothing down: the tick is incomplete and the next one
tries again. A refusal is not tried again under `sudo -n`; running as root the
agent never uses `sudo -n` at all.

| What positively could not be put back | `enforce` (default) | `report` |
|---|---|---|
| `ip felucca input`, `ip felucca forward`, `bridge felucca forward`, or a switch (also a switch file that is missing because its module was unloaded) | The **guest bridge `felucca0` is taken down** | Left up; ERROR `isolation watch (report mode): a fence or switch could not be put back; the guest bridge is left up — GUESTS ARE NOT FENCED on this node`, at most once a minute |
| A table that stays `dormant` after a rebuild | The guest bridge is taken down; ERROR `isolation watch: the table stays dormant after a rebuild — its chains are unhooked although their rules are in place: run nft delete table <family> felucca, after which the agent rebuilds it` | Left up, same ERROR |
| One tap's `as_<tap>` chain | **That tap is taken down** | Left up, unpinned |
| `ip felucca postrouting` (NAT) or `ip felucca ingress` | Nothing is taken down: their absence closes traffic (no egress; exposed ports do not answer). They never enter `isolation` and never make a tick incomplete | Same |

The log lines that say which case it was: `isolation watch: the repair was
refused` (with `nft`'s first line), `isolation watch: the repair was applied
but the chain is still not there`, and, when nothing is taken down because the
answer is missing, `isolation watch: the repair got no answer (timed out,
killed or could not run); nothing is taken down, it is tried again`.

**With the bridge down**, every guest on the worker keeps running, with its
memory and its processes, but has no network: no egress, no ingress through
the gateway, no traffic between guests. exec keeps working in a running guest
(it goes over vsock, not the bridge). A request from another machine to one of
the worker's exposed node ports fails at once — the `unreachable` route
answers it — rather than timing out. The journal
says `isolation watch: a fence or switch could not be put back — the guest
bridge has been taken down; guests keep running without network until a tick
repairs it` (also on stderr). Every later tick tries again; the first tick
that confirms every fence and switch in place brings the bridge back up
(`isolation watch: every fence is confirmed again — the guest bridge is up
again`). If something else raises the bridge while the fence is still not
back, the watch takes it down again (`isolation watch: the guest bridge held
down for an unrepaired fence was raised by something else; it is taken down
again`). When the kernel refuses a repair, the bridge goes down within one to
two ticks, and it is up again within seconds of the cause being removed.

After the guest bridge has been down and is up again, measured on the lab
(Ubuntu 24.04, kernel 6.8; the bridge down for 6 s during a 100 MB transfer at
4 MB/s):

- Traffic forwarded through the worker (the control plane or another host to
  a guest) moves again 0.3 to 7.1 s after the bridge is up. That is the
  sender's own retransmission back-off.
- One kind of connection stalls: a TCP connection in which a process on the
  worker itself was receiving from a guest and had its receive window closed
  when the bridge went down (a rate-limited download, a slow reader). It moved
  again after 54 to 57 s, and in one run after about 115 s, when a keepalive
  or probe exchange restarted it.
- With the `unreachable` route for the guest network removed, the same
  connection took 117 to 123 s. So the route stays; do not remove it.
  `blackhole` in place of `unreachable` made no difference.
- Not measured: a guest sending to an outside host whose receiver was in that
  state.

This matters where a process on the worker host talks TCP to guests directly,
for example a gateway that runs on the same host as the worker. Tracked as
FLU-92.

A tap taken down logs `isolation watch: the tap's anti-spoof chain could not
be put back — the tap has been taken down until a tick pins it again`, and
comes back with `isolation watch: the tap is pinned again and back up`. A
create, wake, start or fork that pins the tap itself releases it as well. A
held tap raised by something else is lowered again only when its pin is seen
missing on that same tick (`isolation watch: a tap held down for want of its
pin was raised by something else; it is taken down again`). Taps that start-up
could not pin for an adopted sandbox are held the same way, and the watch pins
and raises them later; start-up says which failure happened (`adopted VM's tap
could not be set up … nothing was taken down …`, `adopted VM's tap could not
be pinned — the port has been taken down …`, `adopted VM's tap was pinned but
could not be brought up …`).

So: fix what makes `nft` fail (the ERRORs before these lines name the chain),
and the next tick restores the network. No restart is needed.

**The route for the guest network.** At start-up, before anything can take
the bridge down, the agent installs `ip route replace unreachable <guest net>
metric 4294967295`, and it installs it again before it takes the bridge down
and on every tick while the bridge is down. While the bridge is up, the
bridge's own connected route wins; while it is down, traffic for guest
addresses is answered "unreachable" on the worker instead of following the
default route. If the route cannot be installed: ERROR `the unreachable route
for the guest network could not be installed: while the guest bridge is down,
traffic for guest addresses may follow the default route`.

**A deleted guest bridge** is not rebuilt by the watch: ERROR `isolation
watch: the guest bridge is gone; restart the agent to rebuild it`, `isolation`
false, and new guests are refused until it is back.

**New guests are refused** while a fence the agent owns (the input, forward or
L2 chain, a switch, a table left dormant) is positively not in place and could
not be put back, while such a repair has no answer yet, while the guest bridge
is gone, and while start-up could not settle the ingress chain (below) — in
`enforce` and in `report` alike. Create, fork, wake, start and the warm-pool
refill fail with `IsolationFailed` (the agent's `500
{"error":"IsolationFailed"}`; feluccad answers its usual 502 for that worker
call). Each refusal logs `refused: no new guest on this node now` with a
`reason`; the pool refill logs `pool refill refused: no new guest on this node
now` once, and `pool refill: new guests are allowed again` when it ends. The
host fence, a stale verdict, NAT and ingress never refuse a guest.

**The modes.** `isolation_watch` in the agent's JSON config,
`FELUCCA_ISOLATION_WATCH`, `--isolation-watch <value>` or
`--isolation-watch=<value>` (flag beats environment beats file); the value is
case-insensitive. Anything else, or the flag without a value, makes the agent
refuse to start (`isolation_watch: unknown value "…" (want enforce, report or
off)`, `--isolation-watch needs a value (enforce, report or off)`).

| Mode | Use it when | What it costs |
|---|---|---|
| `enforce` (default) | Production | A fence the agent cannot restore takes that worker's guest network down: guests keep running but are unreachable until it is fixed |
| `report` | A first roll-out, to see what the watch would do before it may take anything down; or a single-tenant worker where reachable beats fenced | Nothing is taken down: while a fence cannot be put back, tenants on that worker may share one network. New guests are still refused |
| `off` | Never in production | Nothing notices rules that vanish while the agent runs; start-up says `isolation watch is off: nothing notices rules that vanish while the agent runs` |

`report` and `off` change only the watch. Start-up still refuses to serve, and
takes the bridge down, when the fences cannot be loaded (§8.1).

**The host fence is reported, not repaired.** `inet felucca_host` belongs to
`felucca-firewall.service` (§8). It counts only if it was in place when the
agent started. If it was not (no firewall unit on this node, say), the agent
warns once `the host fence (iifname "felucca0" drop, chain input of table inet
felucca_host) is not loaded as this agent starts; it is not watched in this
run (no firewall unit?) and does not enter isolation`. If it was and goes
missing, the watch warns, at most once a minute, `isolation watch: the host
fence (iifname "felucca0" drop, chain input of table inet felucca_host) is
missing; it belongs to felucca-firewall.service, which this agent does not
repair — isolation is reported false until it is back`, and reports
`isolation` false. Put it back with `systemctl reload-or-restart
felucca-firewall`. The agent's own `ip felucca input` chain still fences
guest-to-host traffic meanwhile. With the host table deleted, `isolation`
reads false within a second or two, and true again a few seconds after
`systemctl reload felucca-firewall`.

**What `isolation` means now.** The agent's `/healthz`
(`{"ok":true,"isolation":<bool>}`) and the heartbeat's `isolation_ok` carry
the same value. Once a tick has completed, it is true when the last completed
tick left no fence, switch or tap pin unrepaired, no fence chain's latest
rebuild has failed, the guest bridge exists, the host fence is there (if it
was at start), and that tick is at most 45 s old. With the watch on, it is
false when no tick has completed within 45 s of the watch's start. Before
that, and with the watch `off`, it is start-up's verdict and that of the last
rule change. NAT and ingress never count. In every mode, `off` included, it
is also false while start-up could neither rebuild nor empty the ingress chain
(no guest address is handed out then, below), and true again once the retry
has rebuilt it. feluccad shows it in `felucca_nodes_unfenced` and places new
sandboxes elsewhere (§12.5).

**Timing.** A tick runs every 5 s and never overlaps the previous one. It
waits up to 15 s for the agent's rule lock (otherwise it is skipped and
counted: `isolation watch: the rule lock stayed busy; this tick is skipped`),
and works for at most 15 s with the lock held; what it has not finished by
then takes nothing down and leaves the tick incomplete.

**Every command has a limit.** Every `nft` and `ip` command the agent runs is
killed, with its whole process group, after 10 s (`wg` commands: 60 s), and
counts as "no answer": `command outran its time limit and was killed`. A
command that timed out is not run a second time. A create, delete or expose
that used to hang behind a stuck `nft` now fails after 10 s. With 16 such
commands still not reaped, new commands answer "timed out" at once, with the
ERROR `too many commands that outran their time limit are still not reaped;
new commands answer "timed out" at once until they are`.

**Addresses held after a failed rebuild.** When a delete's isolation or
ingress rebuild fails, the old rules may still name the deleted sandbox's
address; its IP slot is held (`delete: a rule rebuild failed, so the old rules
may still name this sandbox's address; its IP slot is held until both rebuilds
succeed again rather than go to the next sandbox`) and released at the first
moment the latest isolation and ingress rebuilds have both succeeded — a later
change, or a watch tick (`both rule rebuilds succeeded: the slots held since a
failed rebuild are free again`).

**The ingress chain at start-up.** If start-up cannot rebuild `ip felucca
ingress`, it empties the chain instead (exposed ports do not answer until a
rebuild succeeds). If it can do neither, the chain may still hold the previous
run's node-port rules for addresses nobody holds now, so **no guest address is
handed out**: creates, forks and pool refills fail (`IsolationFailed`), and so
do wakes and starts, until the chain has been rebuilt. The journal says `the
ingress DNAT chain could be neither rebuilt nor emptied at start-up: …`. The
rebuild is retried every 5 s in every watch mode, `off` included, and its
success logs `the ingress DNAT chain has been rebuilt: guest addresses are
handed out again`.

**What the watch gives, and what it does not.** It shortens an exposure from
"until someone notices" to seconds; it does not prevent one. A deleted table
or a flushed chain is back within about 3 to 5 s, and in that window the fence
is open: on the lab, cross-tenant probes sent every 0.5 s got through 5 or 6
times while the whole `ip felucca` table was deleted, and not at all when a
single chain was deleted or flushed.

**What the watch does not detect.** A rule edited by hand *inside* a chain,
below an intact marker, looks unchanged. A flush of only the `tenant_pairs`
set does not touch the marker either; that closes traffic between a tenant's
own sandboxes (the forward chain drops what the set does not allow), it does
not open it. Do not hand-edit the agent's tables.

**Lines to alert on** (ERROR unless noted):

- `isolation watch NOT started: …` — the watch is not running on that worker
- `isolation watch: a chain this agent applied is listed without a marker it can read; …`
- `isolation watch: a fence or switch could not be put back — the guest bridge has been taken down; …`
- `isolation watch: the tap's anti-spoof chain could not be put back — the tap has been taken down …`
- `isolation watch (report mode): a fence or switch could not be put back; the guest bridge is left up — GUESTS ARE NOT FENCED on this node`
- `isolation watch: the repair was refused`, `isolation watch: the repair was applied but the chain is still not there`, `isolation watch: the repair failed` (a tap)
- `isolation watch: the table stays dormant after a rebuild …`
- `isolation watch: the guest bridge is gone; restart the agent to rebuild it`
- `isolation watch: the switch file is missing (module unloaded or device gone) — it cannot be set` and `isolation watch: the switch could not be set again`
- `the unreachable route for the guest network could not be installed: …`
- `the ingress DNAT chain could be neither rebuilt nor emptied at start-up: …`
- `too many commands that outran their time limit are still not reaped; …`
- `isolation watch: the host fence (…) is missing; …` (WARN, at most once a minute)
- `isolation watch: drift — …` (WARN: something other than the agent changed its rules; find out what)
- `isolation watch: the repair got no answer …` and `command outran its time limit and was killed` (WARN)
- `refused: no new guest on this node now` and `pool refill refused: …` (WARN)

---

## 9. How local dev and production differ

The binaries are identical. Only the config values change.

| Setting | Lima lab | Production |
|---|---|---|
| `control_plane` | `http://192.168.104.3:8080` | `https://felucca.internal.example.com` |
| `advertise_addr` | `192.168.104.x` (user-v2 network) | `10.0.1.x` (private DC IP) |
| `bind` (agent) | the VM's user-v2 address | the node's management address (§6.3) |
| `bind` (feluccad) | the VM's user-v2 address, so the host can reach the UI | `127.0.0.1:8080` behind Caddy, or the overlay address on a wg fleet (§6.3) |
| `token` | 64-hex-char secret (same as prod — the binaries reject empty, short and placeholder tokens everywhere, §6.1) | 64-hex-char secret |
| `/metrics` | admin token required (same as prod) | admin token required (§6.2) |
| TLS | none (L2-isolated Lima network) | Caddy terminates HTTPS |
| `pool_size` | `0` (lab is too small) | `2`–`5` per node |
| `state_path` | `/var/lib/felucca/state.json` | same |
| `data_dir` | `/srv/felucca` | same |
| systemd | workers: `felucca-agent.service` + `felucca-firewall.service`, as in production; control plane: `feluccad.service` only — the lab control plane has **no** host firewall unit, so do not expose that VM | `feluccad.service` / `felucca-agent.service` + `felucca-firewall.service` |

The Lima lab uses the same JSON config structure. See `deploy/config/local-lab.md`
for the exact values used in each Lima VM.

---

## 10. Upgrade procedure

Felucca upgrades are binary-replacement restarts. **Every sandbox survives an
agent restart** — running, paused, sleeping and warm-pool VMs alike. That has
been the claim since v2; since ADR-0011 it is also true under systemd. The
agent unit runs with `KillMode=process`, so stopping, restarting, upgrading or
crashing the agent signals the agent alone. The Firecracker processes keep
running, and the next agent takes them over from
`{data_dir}/instances/*/meta.json`.

### What an agent restart does

While the agent is down (a restart is a few seconds; a crash is `RestartSec=5s`
plus start-up) sandboxes keep running and keep their network: guest traffic
never passed through the agent, and the nft rules that fence the guests live
in the kernel. What stops is everything the agent does — the node API (create,
exec, sleep, wake, fork…), the warm-pool refill, the liveness sweep and the
heartbeat. feluccad lists the node as `down` after 15 s of silence and stops
placing new sandboxes on it; it does not touch the ones already there.

At start-up the agent, before it serves anything:

1. Takes `{data_dir}/agent.lock`. One agent manages a data dir: a second one
   started by hand while the unit is up says who holds the lock and exits at
   once, having changed nothing.
2. Loads every record and **adopts** each VM whose recorded pid really is that
   instance's Firecracker — owned by the agent's user, started as `firecracker
   --api-sock …/instances/<dir>/fc.sock`, with paths compared after resolving
   symlinks. A pid that some other process has since been given is not adopted.
3. Clears away what belongs to no completed operation. It kills the
   Firecracker of a create that was cut short and removes instance directories
   that never got a `meta.json`. It removes warm-pool VMs whose Firecracker is
   gone (after a host reboot, say — they were never handed to anyone and
   cannot be claimed any more). A Firecracker next to a record that says the
   VM is asleep or stopped — a wake or a start that worked and was not written
   down — is asked first: if its guest is running it is adopted and the record
   corrected, otherwise it is killed. Only processes in the agent's own cgroup
   are ever killed.
4. Re-pins the tap of every adopted guest, warm-pool VMs included.
5. Asks each adopted Firecracker what its guest is doing and makes that match
   the record: a VM that a cut-short sleep or fork left paused is resumed; a
   VM that a cut-short resume or pool claim left running is paused again. A
   Firecracker that does not answer within two seconds — one that is still
   writing the snapshot of a large guest answers nothing until it is done — is
   asked again every five seconds until it does.
6. Brings the guest bridge up, once the fences around it are verified. (On an
   ordinary restart it never went down.)

The journal line `reconcile: took over what the previous agent left` carries
the counts (`adopted`, `resumed`, `paused`, `unreachable`, `rolled_forward`,
`orphans_killed`, `dirs_removed`, `dead_pool_removed`, `left_unreadable`,
`taps_down`). `unreachable` are the Firecrackers that did not answer yet; each
gets a line `answers now; its run state is settled` when it does.
`rolled_forward` are the wakes and starts found already running.

Things that follow from VMs outliving the agent:

| You run | What happens |
|---|---|
| `systemctl restart felucca-agent` | Agent restarts; every sandbox keeps running. Operations in flight at that moment fail for their caller. At start-up each is either kept (a wake or start whose guest is already running) or rolled back. |
| `systemctl stop felucca-agent` | Agent stops; **sandboxes keep running**, unmanaged until you start it again. The unit shows `inactive` while its cgroup still lists the Firecrackers. |
| `systemctl kill felucca-agent` | Signals **every** process in the unit: the agent **and every sandbox**. To signal the agent only: `systemctl kill --kill-whom=main felucca-agent` (`--kill-who=` before systemd 252). |
| `systemctl stop felucca-agent && systemctl kill felucca-agent` | The way to power off every sandbox on a node. |
| host reboot / shutdown | The firewall unit ends the sandboxes right after the agent has stopped and before any other unit can unload firewall rules (§8). They are killed, not shut down. After boot each sandbox that was running or paused is `stopped` (never `sleeping`, whatever files are in its directory); one that was asleep stays `sleeping`. Keep `{data_dir}` on a local filesystem. |

Each agent restart makes systemd log three warnings **per running sandbox**:
one `Unit process N (firecracker) remains running after unit stopped.` and two
`Found left-over process N (firecracker) in control group while starting unit.
Ignoring.` (once before the pre-start steps, once before the agent itself; each
is followed by a line starting `This usually indicates unclean termination`).
That is systemd describing exactly the intended state; do not alert on them.

**An agent that cannot start never becomes `failed`.** The unit has
`Restart=always` and no start limit, so a start that keeps failing — the gate
in §8 refusing, a bad config, the agent exiting on its isolation checks — is
retried every 5 s for as long as it takes. The unit shows `activating
(auto-restart)`, `systemctl is-failed` says no, and an alert that waits for a
failed unit never fires: alert on the node being `down` in feluccad, or on
`NRestarts` growing (`systemctl show felucca-agent -p NRestarts`). Every
attempt also repeats those per-sandbox warnings — two per sandbox when the
start gate refuses, three when the agent itself gets to run and exits — so on
a node with many sandboxes a start that fails for hours fills the journal.

Limits to know about (each is tracked as its own issue):

- A sandbox whose Firecracker **died while the agent was down** is not
  reported as `error` when the agent returns: it is listed `stopped`, and the
  record is written that way. The same goes for every sandbox that was running
  at a host reboot or at the power-off command above. Since stability wave 1
  it is never listed `sleeping` because an old snapshot lies in its directory
  (FLU-4): `start` cold-boots it. feluccad learns the state from the first
  sleep, wake, stop, start, pause or resume the worker refuses with a 409 that
  names it; until then it may still list the sandbox as it was.
- The agent has no graceful stop: a create, fork, sleep or wake that is in
  progress when it is restarted fails for the caller (FLU-61). A create or
  fork cut at the wrong moment can leave a running VM that the caller was told
  had failed; deleting the sandbox removes it (FLU-28).
- Since stability wave 1 the agent builds its NAT and ingress (DNAT) chains as
  one `nft -f -` transaction each (FLU-45), so each goes from the old rules to
  the new ones in one step. If start-up cannot rebuild the ingress chain, see
  §8.2.
- A record whose `meta.json` cannot be read is not adopted. If a Firecracker
  is running there the agent leaves it alone and logs an ERROR
  (`left_unreadable`); it is your call whether to repair the file and restart
  the agent, or delete the sandbox — a delete ends that Firecracker before it
  removes the directory. Its network slot stays reserved as long as the file
  is still valid JSON; after a truncated one, do not create sandboxes on that
  node before you have dealt with it (FLU-68).
- If the agent cannot load its isolation rules at start-up it exits (§8.1) and,
  because the sandboxes no longer die with it, **takes the guest bridge down**
  first: guests keep their state but have no network until an agent start gets
  the rules loaded. Nothing raises the bridge in between, however often the
  unit retries. A single tap that cannot be re-pinned is likewise taken down,
  not removed. Since stability wave 1 the isolation watch pins and raises such
  a tap on a later tick (FLU-65, §8.2); with the watch `off` it stays down
  until the next agent start.
- A warm-pool VM whose Firecracker dies is removed, and its pool is not
  refilled for five minutes (or until the agent restarts).
- Helper processes of an operation that was cut short (`cp` of a root
  filesystem, `e2fsck`, `resize2fs`) run to their end after the agent is gone
  (FLU-63).
- The agent recognises its Firecrackers by their command line. The
  `firecracker` on its `PATH` has to be the VMM itself (a symlink is fine); a
  wrapper script that execs a binary of another name is not supported (FLU-67).
- A long-running sandbox keeps the Firecracker binary it was started with. A
  hypervisor upgrade reaches it only when it is next cold-started (FLU-69).

### When a worker runs out of memory

Nothing stops a worker from being promised more guest memory than it has
(memory admission is a separate, open issue), so the kernel OOM killer is the
backstop. What it does now:

- It kills **one process**. The agent unit has `OOMPolicy=continue`, so that
  kill no longer stops the unit — before ADR-0011 the first OOM kill of any
  Firecracker stopped the unit, and stopping the unit killed every sandbox on
  the worker.
- The process it picks is a Firecracker, in practice the one whose guest has
  touched the most memory. The agent gives every Firecracker it spawns or
  adopts `oom_score_adj` 300 and stays at 0 itself, so a VMM is taken before
  the agent, sshd or journald.
- The agent sees that one Firecracker exit and marks that one sandbox `error`
  (`firecracker exited unexpectedly; state -> error` in the journal, next to
  systemd's `A process of this unit has been killed by the OOM killer.`).
  Every other sandbox keeps its pid, its uptime and its state. `start` on the
  `error` sandbox cold-boots it.
- `ManagedOOMPreference=omit` keeps **systemd-oomd**, where it is enabled, away
  from the unit's cgroup: oomd does not pick a process, it kills a whole
  cgroup, and this one holds every sandbox.

Check a node with:

```sh
systemctl show felucca-agent -p OOMPolicy -p ManagedOOMPreference
for p in $(pgrep -x firecracker); do echo "$p $(cat /proc/$p/oom_score_adj)"; done   # 300 each
cat /proc/$(pgrep -x felucca-agent)/oom_score_adj                                    # 0
grep oom_kill /sys/fs/cgroup/system.slice/felucca-agent.service/memory.events
```

Do not add `OOMScoreAdjust=` to the unit: it is inherited by every
Firecracker and sets a floor an unprivileged VMM may lower itself back to.
`scripts/verify-units.sh` refuses it.

### Upgrading from a Hearth install

If this deployment predates the Hearth → Felucca rename, do the one-time
migration first — stopped units, renamed paths (`hearth.db` → `felucca.db`
included), new unit files, and the credential-rotation truth about old
`hearth_sk_`/`hearth_nt_`/`hearth_jt_` prefixes — before following the
regular upgrade steps below. See
[docs/CHANGELOG.md § Rename: Hearth → Felucca](CHANGELOG.md#rename-hearth--felucca-2026-09-12)
for the full checklist.

### Control plane upgrade

```sh
# 1. Build new binaries (see §3 — Go build inside the toolchain VM)

# 2. Copy the new binary to the server
scp deploy/release/x86_64/feluccad root@cp-host:/usr/local/bin/feluccad.new

# 3. Atomic replace + restart (state.json is preserved)
ssh root@cp-host "
  install -m 0755 /usr/local/bin/feluccad.new /usr/local/bin/feluccad
  systemctl restart feluccad
  systemctl status feluccad --no-pager
"
```

### Worker upgrade

**Units first, then the binary, then the restart.** The unit is what keeps the
sandboxes alive across the restart, and `systemctl daemon-reload` applies a new
unit to the running agent at once.

```sh
# 1. Build new felucca-agent binary (see §3 — Rust build inside the toolchain VM)

# 2. Copy the units and the binary to the worker
scp deploy/systemd/felucca-agent.service deploy/systemd/felucca-firewall.service \
    root@worker-host:/etc/systemd/system/
scp deploy/release/x86_64/felucca-agent root@worker-host:/usr/local/bin/felucca-agent.new

# 3. Load the units, check, replace the binary, restart
ssh root@worker-host "
  systemctl daemon-reload
  systemctl show felucca-agent -p KillMode -p Restart     # KillMode=process Restart=always
  systemctl reload-or-restart felucca-firewall
  install -m 0755 /usr/local/bin/felucca-agent.new /usr/local/bin/felucca-agent
  systemctl restart felucca-agent
  systemctl status felucca-agent --no-pager
"
```

`deploy/install.sh worker` gets a node to the same place by its own route: it
copies the binary, installs both units right after (with a `daemon-reload`),
applies the firewall rules with a reload, and **restarts nothing** — the
restart is the one step it leaves to you, and it prints it. Nothing runs the
new binary before that restart, so the order of those two copies does not
matter; what matters, in either procedure, is that the new unit is loaded
before the agent is next restarted. **If `KillMode` does not say `process` when
you restart, every sandbox on the worker dies** — that is what the check is
for. The first restart from an agent that predates ADR-0011 is already safe
once the new unit is loaded: the Firecrackers the old agent started are
adopted by the new one.

The worker re-registers with the control plane within ~5 s after restart.

**Rolling back.** Roll the binary and the unit back together and treat it as a
node drain: with the old unit loaded, the next agent restart or crash kills
every sandbox again. Rolling back the binary alone leaves the new unit in
place, and an agent that predates ADR-0011 does not adopt warm-pool VMs —
every restart then orphans one live Firecracker per pool VM. Empty the pools
first (`pool_size: 0`, and `PUT /v1/pools` with `[]`), or clear the leftovers
with `systemctl kill felucca-agent` once the node is drained.

### Upgrading to stability wave 1

Design and reasons: [ADR-0012](adr/ADR-0012-data-safety.md). No unit file
changes. New settings: the agent's `isolation_watch` (`FELUCCA_ISOLATION_WATCH`,
`--isolation-watch`, default `enforce`, §8.2); the gateway's `--max-stale`
(`FELUCCA_GW_MAX_STALE`, default 600 s) and `/readyz` (§12.2). In the default
mode a worker whose fences the agent cannot restore takes its guest network
down; read §8.2 before you upgrade the agents.

**Order: every agent first, then every gateway, then feluccad.** Each step
works with the components that are still old; what each mixed state gives:

| Mixed state | What you get |
|---|---|
| New agent, old feluccad | The agent allocates node ports round-robin from a random start, so a released port is rarely handed out again at once — rarely, not never: an old feluccad sends no `avoid` and quarantines nothing. Lifecycle refusals carry `state`; an old feluccad forwards stop/start/pause/resume 409s as before and turns sleep/wake 409s into `502 agent sleep failed` / `502 agent wake failed`. A fork the worker refuses (a busy parent, or one forked before this release) answers 502 and leaves the child in `error`. The old feluccad still drops dynamic exposes when it auto-sleeps a sandbox, and still acknowledges changes it could not save |
| New feluccad, old agent | Works. The old agent ignores `avoid` and `prefer` and hands out the lowest free port, so **the hostname exposure of FLU-6 remains for that worker** until its agent is upgraded: feluccad accepts a quarantined port from it and logs `expose: an agent that does not honour avoid returned a quarantined node port; accepted (upgrade the agent)` each time. The old agent answers 500 where the new one answers 409 with `state`, so nothing is recorded from its refusals |
| Old gateway (any feluccad) | No staleness bound: it serves from its last table for as long as feluccad is unreachable. It pools upstream connections per `node:port`, gives every waiter of a wake the first request's route, and answers a `routes/ensure` 429 or 503 with 404. It has no `/readyz` |
| New gateway, old feluccad | Works |

**Rolling feluccad back** needs no database restore: the previous feluccad
starts and works on the database this one has written.


Upgrade the agents with the worker procedure above; nothing else is needed
on a worker. A sandbox that is running keeps running.

What the new agent does with what the old one left, at its first start:

- A record in `running` or `paused` whose Firecracker is gone becomes
  `stopped`, and is written so. Snapshot files beside it are removed
  (`mem.bin` is kept while a live Firecracker of that sandbox maps it).
- A `sleeping` record (the old agent wrote no `snapshot_valid`) stays
  `sleeping` when both `vmstate.bin` and `mem.bin` exist and `rootfs.ext4` was
  not modified after `vmstate.bin`. If the disk is newer, the guest ran after
  that snapshot: the record becomes `stopped` and the two files are set aside
  as `*.outrun` (§12.4), with a warning that names both modification times.
- Only running, paused and pooled sandboxes are re-pinned at agent start. The
  taps of sandboxes that are asleep or stopped keep the previous agent's
  anti-spoof chain — no marker rule, no final `drop` rule; its policy is
  `drop`, so it protects the same way — until the sandbox is woken or started.
  Listing the rules after an upgrade therefore shows both forms.
- Sandboxes forked before this release need attention: see below.

#### Sandboxes forked before this release

Before this release a fork child opened its **parent's** `rootfs.ext4`: the
snapshot records the drive's path, and nothing pointed the child elsewhere.
Every such child that is still running, or that has slept and is woken,
runs on its parent's image. Parent and child have been writing to one file
system from two kernels. The child's own `rootfs.ext4` is the copy made at the
fork — after the parent had been resumed — and holds none of the child's
writes since.

To find them, ask each running sandbox's Firecracker which image its root
drive has open. `GET /vm/config` only reads; still, do not run it while a
lifecycle operation (a sleep, wake, fork, stop) is in progress on that
sandbox, because a Firecracker answers one request at a time and the agent's
call would wait behind yours.

```sh
# On the worker, per instance directory with a running Firecracker:
curl -s --unix-socket {data_dir}/instances/<dir>/fc.sock http://localhost/vm/config \
  | jq -r '.drives[] | select(.drive_id=="rootfs") | .path_on_host'
# Anything other than {data_dir}/instances/<dir>/rootfs.ext4 is such a child.
```

A sleeping one shows itself when it is next slept or woken: the agent logs
`sleep: this sandbox runs on another image than its own rootfs.ext4 (it was
forked before this release); that image is synced` or `wake: this sandbox runs
on another image than its own rootfs.ext4 …` (the image is in the log
line's `image` field).

What the new agent does with them:

- **It does not fork them**, running, paused or sleeping: feluccad answers
  `409 {"error":"InvalidState: this sandbox was forked before this release and
  runs on its parent's disk image, not on its own; it cannot be forked","state":"<its state>"}`,
  and nothing is left of the child, in feluccad or on the worker, whatever the
  parent's state.
- **It wakes one only if the image it runs on has not been modified since its
  `vmstate.bin` was written.** Otherwise the wake stops it, sets its snapshot
  aside and answers 409 with `"state":"stopped"` and `InvalidState: this
  sandbox was forked before this release and runs on its parent's disk image,
  which has changed since it slept; it cannot be woken and is now stopped (use
  start)`.
- A sleep of one fsyncs the image it really runs on.
- A `start` (after a `stop`) cold-boots it from its **own** `rootfs.ext4`, the
  disk as it was at the fork: what it wrote since went into the parent's image.

Felucca cannot separate the two guests' writes. For a child whose data
matters, copy that data out from inside the running guest (with exec, or over
its own network) before you stop it.

#### Rolling the agent back

The previous agent binary ignores `snapshot_valid` and still calls a sandbox
`sleeping` whenever a `vmstate.bin` lies in its directory. The new agent keeps
that file only beside a `sleeping` record (it is removed after a wake, a stop,
a start, at start-up for every other record, and after a fork child's load),
so the previous agent reads every record the new one has touched as it was
meant. Files set aside as `*.outrun` are not snapshots to the previous agent
either; it leaves them alone. Rolled back, the worker has the old behaviour
again: lowest-free node ports, fork children on their parent's disk, and a
failed snapshot that a later wake can take for a good one. Roll back the binary only (the
units did not change in this release), as described in "Rolling back" above.

### Rolling upgrades (multiple workers)

Upgrade workers one at a time. There is no drain command, and feluccad keeps
placing new sandboxes on a node until its heartbeat is 15 s old — so for the
moment the agent is away, the node is still being chosen:

- A create that lands on a node whose agent is not answering fails with
  `502 agent unreachable`, and that sandbox is left in `error`. It is **not**
  retried on another worker: delete it and create again.
- That window is the few seconds of a `systemctl restart`, or up to about 15 s
  after a stop. Once the node shows `down`, new sandboxes go to the other
  workers (`503 no ready node` only when none is left).
- A create that is in progress on the node when its agent stops fails the same
  way for its caller; what the next agent start makes of the half-made VM is
  described under "What an agent restart does" above.
- Sandboxes already on the node keep running throughout.

Do it when few creates are arriving, and expect clients to retry.

---

## 11. Troubleshooting

### Observability (v4 P6)

All three binaries log structured `key=value` text to stderr → journald.
Every `/api/` request gets an `X-Felucca-Request-Id` (also echoed to the
client); to follow one failing call across binaries:

```bash
# On the control plane, find the request:
sudo journalctl -u feluccad | grep req-<id>
# On the owning worker, the same id appears on the agent's handler logs:
sudo journalctl -u felucca-agent | grep req-<id>
```

Agent verbosity: set `RUST_LOG=debug` in `/etc/felucca/agent.env` and restart.
Fatal-exit diagnostics (`fatal: ...`) and the cross-tenant-isolation warning
bypass the filter and always reach the journal — a restrictive `RUST_LOG`
can never hide them.

**Grep migration (pre-P6 → P6):** message key words are unchanged but exact
formats moved to `key=value`; drop colons and inline values from old greps —
`grep 'persist:'` → `grep 'persist failed'`, `grep 'listening on'` →
`grep 'listening'`, `grep 'exec on'` → `grep 'exec failed'`.
Latency histograms live on `/metrics`, which now requires the admin bearer
token (§6.2); per-tenant gauges are on a separate admin surface,
`GET /api/v1/metrics/tenants` (Prometheus scrape config and a ready-made
Grafana dashboard: `deploy/grafana/`). Both scrape jobs need `bearer_token` —
an unauthenticated Prometheus that used to work will now log 401s. Release
ritual: `scripts/bench.sh <endpoint> <token>` and refresh `docs/BENCHMARKS.md`.

| Symptom | Likely cause | Fix |
|---|---|---|
| `systemctl status feluccad` shows `failed` | Binary not found or config parse error | Check `journalctl -u feluccad -n 50`; verify `/usr/local/bin/feluccad` exists and is executable |
| Either binary exits at startup complaining about the token | Token empty, a placeholder (`REPLACE_WITH…`, `felucca-lab-token`), or too short — under 32 characters for `feluccad`, under 16 for `felucca-agent` | Put the fleet token in the JSON config or `FELUCCA_TOKEN`; generate with `openssl rand -hex 32` (64 chars, clears both bars); see §6.1 |
| `feluccad` exits: `msg=trusted-proxies err="… is not a CIDR"` | A malformed entry in `trusted_proxies` — deliberately fatal, never skipped | Write CIDRs (`127.0.0.1/32`, `10.0.0.0/8`); a bare IP is accepted and normalised. See §7.1 |
| `feluccad` exits: `msg=trusted-proxies err="trust_x_real_ip is set but no trusted_proxies are declared"` | `trust_x_real_ip` on with nothing declared would believe `X-Real-IP` from every peer — a header any client can set | Either list the proxy in `trusted_proxies`, or drop `trust_x_real_ip` and have the proxy send `X-Forwarded-For` (preferred). See §7.1 |
| One client's bad token turns everyone else's `401`s into `429`s | Behind a proxy with no forwarded-header config, every client shares the proxy's address as its throttle key (holders of a valid token are unaffected — §6.2) | Configure both halves in §7.1: proxy **overwrites** `X-Forwarded-For`, feluccad lists the proxy in `trusted_proxies`. Never one without the other |
| `felucca-agent` keeps restarting (`activating (auto-restart)`, never `failed`); journal has `felucca-agent: not starting: the guest fence (iifname felucca0 drop) is not loaded …`; guests on that worker have no network | The start gate (§8). The agent already asked systemd to load the rules file again and the rule `iifname "felucca0" drop` is still not in the kernel: the file does not load (syntax error, empty, `nft` missing), or it loads but that line was removed or rewritten — a file generated for the control-plane role looks like that. The gate took `felucca0` down so the sandboxes, which are still running, are not on an unfenced network | `systemctl status felucca-firewall` says why the file does not load; `sudo nft list chain inet felucca_host input` shows what is loaded. Fix `/etc/felucca/firewall.nft` — that is all: the next retry, within 5 s, loads it, the agent starts, and it raises the bridge again. To apply by hand: `systemctl reload-or-restart felucca-firewall` (a plain `reload` is refused when the unit is failed or stopped). No sandbox was stopped |
| The ruleset was flushed (`nft flush ruleset`, or `nftables.service` reloaded) on a worker with running sandboxes | Every table is gone, the agent's and the installer's. The isolation watch rebuilds the agent's tables within 5 s (`isolation watch: drift …`, then `chain repaired and confirmed`); the host table stays missing and `isolation` is reported false (`isolation watch: the host fence … is missing`); `systemctl status felucca-firewall` still says `active` | `systemctl reload-or-restart felucca-firewall` puts the host table back. Sandboxes keep running (§8.1, §8.2) |
| Guests on one worker lost all network; journal has `isolation watch: a fence or switch could not be put back — the guest bridge has been taken down` | `enforce` mode: an isolation chain or switch went missing and the agent could not put it back (§8.2) | Fix what makes `nft` (or the switch) fail — the ERRORs just before say which; the next tick that confirms every fence raises the bridge again. No sandbox was stopped |
| `felucca-agent` exits at start: `isolation_watch: unknown value …` or `--isolation-watch needs a value …` | `isolation_watch` / `FELUCCA_ISOLATION_WATCH` / `--isolation-watch` is not `enforce`, `report` or `off` | Fix the value (§8.2) |
| Journal has `isolation watch NOT started: …` | Start-up applied its chains but cannot read their markers back on this kernel; the watch is off in this process | Report it with the quoted first rule and the `nft --version`; until then the worker runs as with `off` (§8.2) |
| Creates on one worker fail; its journal has `refused: no new guest on this node now` | A fence of the agent is positively not in place (or its repair has no answer), the guest bridge is gone, or the ingress chain was not settled at start-up — the `reason` says which (§8.2) | Fix the cause named in the ERRORs before it; new guests are allowed again by the next tick that confirms the fences |
| `systemctl reload felucca-firewall` fails | The rules file does not parse, is empty, or defines no `input` chain — or the unit is stopped or failed, in which case `reload` is refused whatever the file says | Nothing was changed — the previous rules are still in force. Fix `/etc/felucca/firewall.nft` and `systemctl reload-or-restart felucca-firewall` |
| Worker installer stops with `FELUCCA_CONTROL_PLANE_IP …` | The value is not a list of literal addresses, has a `/prefix`, or names nobody (`,`, or loopback only) | Nothing was installed or changed. Pass literal addresses (`'10.0.1.10, 10.100.0.1'`), or leave the variable unset to keep the rules as they are (§5) |
| Worker installer ends with `The host firewall step did not complete: the existing /etc/felucca/firewall.nft has no guest fence` | The rules file on the node has no `iifname "felucca0" drop` line — hand-edited, or written by a control-plane install on the same host. It was not applied | Put the line back into chain `input`, or let the installer write the file: re-run with `FELUCCA_CONTROL_PLANE_IP=<addresses>` (§8) |
| Control plane cannot reach a worker on 9090 | `FELUCCA_CONTROL_PLANE_IP` was not set at the first install, so 9090 is loopback-only — or the control plane reaches the worker from an address that is not in the list (LAN vs overlay) | Re-run the worker installer with every address it comes from (`FELUCCA_CONTROL_PLANE_IP='10.0.1.10, 10.100.0.1'`), or edit `/etc/felucca/firewall.nft` and `systemctl reload felucca-firewall` |
| Installer aborts: `failed SHA-256 verification` | The pinned Firecracker or guest asset did not match its recorded digest | Do not bypass it. Re-download; if the upstream artifact genuinely changed, refresh the pin per the comment block in the script |
| `GET /healthz` returns `connection refused` from another host | feluccad binds `127.0.0.1:8080` (the shipped default — Caddy fronts it) | Intended. Reach it through Caddy, or widen `bind` *and* the §8 rule if you really need direct access |
| `GET /healthz` returns `connection refused` on the server itself | feluccad not listening | Check `bind` in config; `journalctl -u feluccad -n 50` |
| `GET /healthz` returns `503 {"ok":false,"store":"failing"}`; changes answer `503 store unavailable` | feluccad cannot write its database (locked, full disk, read-only) | Fix the cause; the recovery loop picks it up within 2 s and logs `store recovered` — no restart needed (§12.1) |
| Every sandbox URL answers `503 route table unavailable` | The gateway has had no fresh route table for `--max-stale`, or has not fetched one since it started | Make feluccad reachable from the gateway (URL, token, network); the next fetch ends it (§12.2) |
| `felucca-gw` exits at start: `refresh … is above the maximum` / `max-stale … is below the minimum` | `--refresh` outside 1–60 s, or `--max-stale` outside `2 x refresh + 20s`–600 s | Fix the value (§12.2) |
| Expose answers `429 expose churn limit reached` or `503 no free node port on this worker` | The node-port quarantine (§12.3) | Retry after `Retry-After` |
| A sandbox that was running before a reboot is `stopped`, and wake answers 409 | Intended: a VM whose Firecracker is gone has lost its memory (§12.4) | `start` it |
| `vmstate.bin.outrun` / `mem.bin.outrun` in an instance directory | A snapshot judged stale and set aside (§12.4) | Nothing; `start`, `stop`, the next sleep or `delete` removes them |
| `GET /api/v1/nodes` returns `401` | Token mismatch or missing `Authorization` header | Verify `token` in feluccad config matches the `Bearer` value you're sending |
| A request with a **wrong or missing** token returns `429 too many failed attempts` with a `Retry-After` | The brute-force guard (§6.2) has armed a backoff for **your source address** after >10 failed credentials. Behind a proxy, the failures may be someone else's — you share the key | Fix the credential. A correct token is served immediately regardless of the backoff, so this only ever replaces the `401` you would otherwise get. Then stop whatever is retrying a stale token; the record decays with quiet time (one failure per 15s) |
| A request you believe is **correctly authenticated** returns `429 too many failed attempts` | Not possible from the throttle — feluccad authenticates first (§6.2). The token on that request is not the one feluccad holds | Compare the token actually being sent against `token` in `/etc/felucca/feluccad.json`; check for a stale value in a script, a shell history entry, or a `Bearer ` prefix that got mangled |
| `GET /metrics` returns `401`, Prometheus targets go down after an upgrade | `/metrics` is admin-gated now (§6.2) | Add `bearer_token` to the scrape job; see `deploy/grafana/` |
| Agents never register, `GET /api/v1/nodes` stays empty on an overlay fleet | feluccad bound to `127.0.0.1` while agents dial its overlay IP | Bind the overlay address (§6.3) and align the §8 rule; the shipped example is loopback for the Caddy topology |
| Console shows `MOCK` or `STALE` in the header | It never reached feluccad, or lost it — the rows on screen are not the live fleet | Read the banner; check feluccad and the token. Nothing on a `MOCK` console is real (§6.5) |
| Console shows `NO TOKEN` and is not polling | Deliberate — it will not spend auth attempts without a credential (§6.5) | Paste the token into the banner; click the header badge if you dismissed it |
| Worker shows `status: down` in node list | Agent heartbeat older than 15 s | Check `systemctl status felucca-agent` on the worker; check connectivity on port 9090 |
| Worker never appears in node list | `control_plane` URL wrong or token mismatch | Check agent logs: `journalctl -u felucca-agent -n 50`; verify `control_plane` points to feluccad and token matches |
| Sandbox stays in `creating` | Firecracker spawn failed on worker | Check `journalctl -u felucca-agent -n 100`; check `/dev/kvm` permissions; check `firecracker --version` |
| `POST /sandboxes` returns `503 no ready node` | No workers registered and ready | Fix the worker install and verify it reaches the control plane |
| Guest networking not working (`ip: null`) | `net` is `off` or agent config missing | Set `"net": "on"` and `"net_cidr"` in agent config; verify `nft` and `ip` are installed |
| Bridge `felucca0` missing after agent restart | Idempotent setup should recreate it | Agent recreates the bridge at startup; check `journalctl -u felucca-agent` for errors |
| One sandbox went to `error`; journal has `A process of this unit has been killed by the OOM killer.` | The worker ran out of memory and the kernel took that sandbox's Firecracker (§10). The others were not touched | `start` the sandbox to cold-boot it. If it keeps happening the worker is over-committed: lower what is placed on it |
| Every sandbox on a worker died when the agent was restarted | The unit in force is not the shipped one: `systemctl show felucca-agent -p KillMode` says `control-group` (an old unit, or a drop-in overriding it) | Install `deploy/systemd/felucca-agent.service`, `systemctl daemon-reload`, confirm `KillMode=process` **before** the next restart (§10) |
| `systemctl stop felucca-agent` did not stop the sandboxes | Intended since ADR-0011 | `systemctl stop felucca-agent && systemctl kill felucca-agent` powers off every sandbox on the node (§10) |
| Guests have no network after an agent start; journal has `the guest bridge has been taken down` or `could not be pinned — the port has been taken down` | The agent could not load its nft rules at that start, and takes guest ports down rather than leave them unfenced | Fix `nft` (see the errors just above in the journal) and `systemctl restart felucca-agent`: the next successful start brings the bridge and the taps back up. No sandbox was stopped |
| Journal has `a Firecracker is running in an instance directory whose meta.json does not load` | That sandbox's record is damaged; the agent left the VM running and does not manage it | Repair `{data_dir}/instances/<dir>/meta.json` and restart the agent to adopt it, or kill the process to discard the VM |
| Journal has `could not take the data-dir lock` | Another `felucca-agent` is running on the same `data_dir` | Stop the stray one. Until then this agent does not clean up orphan Firecrackers at start-up |
| `POST /sandboxes/{id}/wake` is slow (> 500 ms) | No warm pool VMs available | Increase `pool_size` in agent config (requires restart); check `felucca_pool_size` metric |
| `POST /sandboxes/{id}/fork` returns `501` | Running v1 binary | Upgrade to v2 binary |
| TLS cert errors | Caddy cannot reach ACME or wrong domain | Check `journalctl -u caddy`; verify DNS and port 80/443 reachability |
| `journalctl -u feluccad` shows `Permission denied` on state file | Wrong ownership of `/var/lib/felucca` | `chown -R felucca:felucca /var/lib/felucca` |

### Useful commands

```sh
# Control plane logs (live)
journalctl -u feluccad -f

# Worker agent logs (live)
journalctl -u felucca-agent -f

# Prometheus metrics (admin token required — §6.2)
curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/metrics | grep felucca_

# List all nodes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/nodes | jq .

# List all sandboxes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/sandboxes | jq .

# Check warm pool gauge on a specific node
curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/metrics | grep felucca_pool_size

# Check a sleeping sandbox's wake latency
curl -s -X POST -H "Authorization: Bearer ${TOKEN}" \
  https://felucca.example.com/api/v1/sandboxes/sb-abc123/wake | jq .wake_ms

# Is feluccad's database keeping up? (200 {"ok":true} / 503 {"ok":false,"store":"failing"})
curl -s -i http://127.0.0.1:8080/healthz

# May the gateway route? (200 ok / 503 route table unavailable)
curl -s -i http://127.0.0.1:8088/readyz
```

---

## 12. Running through failures: the store, the route table, node ports, snapshots

Since stability wave 1 ([ADR-0012](adr/ADR-0012-data-safety.md)) each
component says so when it cannot do what it is asked, instead of answering
success. This section is what that looks like to an operator.

### 12.1 When feluccad's database cannot be written

feluccad answers a change with 2xx only once it is in its SQLite database.
When a write fails — the file is locked by another process, the disk is full,
the file or its mount is read-only — the **store is failing** until a save
succeeds again.

What you see:

- The journal line `store failing: changes are refused until a save succeeds`
  (once per outage, with the error), then `store still failing` every 2 s.
- `GET /healthz` answers `503 {"ok":false,"store":"failing"}`;
  `felucca_store_healthy` is 0; `felucca_persist_failures_total` grows.
- Every request that would change the database is refused before anything is
  done: `503 {"error":"store unavailable"}` with `Retry-After: 5`. That covers
  create, delete, sleep, **wake**, stop, start, pause, resume, fork, expose,
  unexpose, `routes/ensure`, tenants, keys, templates, join tokens and node
  enrolment (API-V2.md §2a has the exact list).
- A change whose worker call had already been made when the save failed
  answers `503 {"error":"applied but not saved","applied":true}`: the worker
  did it, feluccad holds it in memory, and the recovery loop will write it.
  Clients may retry; the worker side is idempotent. A create or fork in that
  position answers `503 store unavailable` instead, and its VM is deleted from
  the worker right after the answer (the sandbox shows `error` until then, and
  stays `error`, deletable, if that delete fails).
- What keeps working: every read, exec, heartbeats and worker registration
  without a join token, the route table the gateways fetch, `/metrics`. **Running
  sandboxes keep serving.** Sleeping ones are not woken, by the API or by the
  gateway, and the lifecycle sweep does nothing (no auto-sleep, no
  auto-delete).
- Exec metering rows wait in memory, at most 10 000; past that the oldest are
  dropped, logged (`usage: exec rows dropped, the queue was full`) and counted
  in `felucca_usage_rows_dropped_total`. Each failed write attempt is counted
  in `felucca_usage_write_failures_total` (those rows are still queued).
- A database that cannot be **read** shows in `felucca_store_read_errors_total`
  (`/healthz` and `felucca_store_healthy` only describe writes); requests that
  need the read answer `503 store unavailable`, listings `500 store error`.

How it ends: a recovery loop retries every 2 s. Each attempt opens a fresh
writer connection, writes the queued exec rows, then everything memory holds;
when both succeed, the journal says `store recovered: the database has
caught up with memory` (with the outage's length) and changes are accepted
again. You do not need to restart feluccad. Fix the cause — free the disk,
stop whatever holds the lock, make the file writable — and wait for the line.

**A database that was read-only when feluccad started.** feluccad does not
refuse to start on a database it cannot write. It logs `STORE NOT WRITABLE at
start-up: serving reads and the route table, refusing changes, retrying the
save until it succeeds`, starts with the store failing, and serves as above.
Because every retry opens a fresh writer connection, making the file writable
is enough; no restart is needed. A database that cannot be **read**, or a
legacy `state.json` that is present but does not load, still ends the process
(`refusing to start`).

**What a restart does and does not do.** A restart does not repair a store
that still cannot be written: feluccad comes back with the store failing.
And it throws away what the recovery loop had not written yet: changes that
were answered `applied but not saved` and the queued exec rows. After such a
restart feluccad's records can be one transition behind the workers (a
sandbox the worker put to sleep still listed `running`, say) until the next
call for that sandbox; the worker's 409 with `state` then corrects it. Nothing
corrects it before that call (FLU-13). So: prefer waiting for the recovery
line to restarting.

**A record left `creating`.** If feluccad stopped (or was killed) during a
create's worker call, the record is set to `error` at the next start, with
the warning `sandbox was being created when the previous control plane
stopped: it is set to error; a VM may exist on its worker — start or delete
settles it`. Look at the sandbox, then `start` it or `delete` it. Its first
successful start, wake or resume takes its guest address from the worker's
list of VMs, so that expose and routing work again; if the worker cannot be
asked, the request still succeeds, a WARN says the address stays unset, and
the next start or wake tries again.

**Stopping feluccad.** On SIGTERM or SIGINT (`stopping: draining the requests
in flight, then flushing the exec usage rows`) every listener stops accepting
at once and the requests in flight get 10 s in all to finish, their saves
included; connections still open after that are cut, and their clients get no
answer and must treat the outcome as unknown. Then the background loops end,
and the queued exec rows are written, retried once a second until just before
the 20 s limit; if that never succeeds, one ERROR says how many were lost
(`usage: final exec flush failed; the queued exec usage rows are lost`).
feluccad then exits with status 0. A second signal (`second signal during the
stop: exiting now`), or 20 s after the first (`stop did not finish in time:
exiting now`), ends it at once with status 1, and systemd starts it again
(`Restart=on-failure`). The unit's `TimeoutStopSec=30s` leaves 10 s beyond
that before systemd sends SIGKILL.

After a restart, the workers read `down` until their next heartbeat, and
creates answer 503 until then. feluccad sets its database file to mode 0600
itself when it opens it.

The database is opened with `synchronous=FULL`: every acknowledged write
survives a power cut. A read failure answers 503 too, and is not counted as a
failed credential by the brute-force guard (§6.2).

### 12.2 The gateway when feluccad is unreachable

`felucca-gw` refreshes its route table every `--refresh` seconds (default 5).
When feluccad cannot be reached it keeps serving from the table it has — for
at most `--max-stale` (default 600 s, environment `FELUCCA_GW_MAX_STALE`). A
table's age counts from the moment its fetch started, and keeps counting while
the host is suspended.

Past that, and between the gateway's start and its first successful fetch,
every request for a sandbox hostname is answered `503 {"error":"route table
unavailable"}` with `Retry-After: 5`. The first successful fetch ends it. The
bound exists because an old table can name a node port that has since gone to
another sandbox.

Point load balancers and health checks at **`GET /readyz`**: 200 `ok` while
the table is fresh enough, the 503 above otherwise. `GET /healthz` is liveness
only and answers 200 as long as the process runs.

Settings:

| Setting | Default | Accepted | Outside the range |
|---|---|---|---|
| `--refresh` (flag only) | `5` (seconds) | 1 to 60 s | the gateway refuses to start |
| `--max-stale` / `FELUCCA_GW_MAX_STALE` | `600` (seconds) | `2 x refresh + 20s` to 600 s | the gateway refuses to start |

`--max-stale` takes whole seconds (`600`) or a duration (`10m`); the flag
beats the environment. The admin token is read from `FELUCCA_TOKEN` (or
`--token`) and no longer appears in `felucca-gw --help`.

### 12.3 Node ports and their quarantine

When a sandbox gives up a node port (unexpose, delete, the sweep), feluccad
keeps that port away from every other sandbox on the same worker host for
**900 s**: long enough for every gateway's table, every call to the worker
already under way and every pooled upstream connection that could still send
the old hostname's traffic to it. The same sandbox, exposing the same guest
port again, gets the port back. `felucca_node_ports_quarantined` shows how many
distinct (worker host, port) pairs are in quarantine.

Each worker has 10 000 node ports (20000–29999), and the quarantine takes from
them. An expose can therefore answer:

| Answer | Why | What to do |
|---|---|---|
| 429 `{"error":"expose churn limit reached"}`, `Retry-After: <s>` | The tenant holds 512 quarantined ports on that worker host, or the sandbox holds 64 distinct (node port, guest port) pairs there (the admin tenant is exempt from the first). `Retry-After` is when the oldest one that counts is released, at most 900 s | Expose less churn-heavily: keep exposes rather than recreate them |
| 503 `{"error":"no free node port on this worker"}`, `Retry-After: 30` | Every node port of the worker is in use or quarantined | Retry after the delay; quarantined ports come free 900 s after their release |
| 503 `{"error":"node port conflict, retry"}`, `Retry-After: 5` | The worker handed out a port feluccad has recorded elsewhere (or a quarantined one); the mapping was removed again | Retry. Each one is logged as an ERROR starting `expose: the worker …` |
| 503 `{"error":"node port conflict: the worker answered node port N, which this sandbox's expose \"<name>\" (guest port G) holds; removing that expose ends it"}`, `Retry-After: 5` | The worker answered with a port this same sandbox holds for another guest port | Unexpose the named expose; retrying alone does not help |

For dynamic (port-in-hostname) exposes the gateway passes these on: 429
`{"error":"too many new dynamic ports for this tenant"}` and 503 `{"error":"dynamic
port temporarily unavailable"}`, each with a `Retry-After`.

A sleeping sandbox keeps its dynamic exposes for 900 s; the sweep drops them
after that (only on `ready` workers, at most 5 s per worker call and 5 s of
worker time per pass), and the next request re-creates them. A drop that got
no answer in time is repeated on later passes until the worker answers.

Quarantines run on the wall clock: a clock stepped backwards lengthens them by
at most the step; `Retry-After` never exceeds 900 s.

At start-up feluccad logs one ERROR `start-up: route rows of different
sandboxes name the same node port; one of them is stale (its hostname may
reach the other sandbox): delete or unexpose the stale one` per worker host
and port that rows of two sandboxes name. Such rows can be left from before
this release. Nothing is repaired automatically: look at the sandboxes the
line names, and unexpose or delete the stale one. (The next expose on that
worker that receives the port also removes the stale row.) It also logs one
ERROR per sandbox and guest port whose rows name several node ports: `start-up:
route rows of one sandbox map one guest port to several node ports; all but one
are stale. The next expose of a new name on that guest port repairs it (or
unexpose the stale name)`.

A worker whose agent predates this release does not read the avoid list: for
that worker, feluccad accepts a quarantined port and logs `expose: an agent
that does not honour avoid returned a quarantined node port; accepted (upgrade
the agent)`. Upgrade that agent.

### 12.4 Snapshots, forks and busy sandboxes

**A sandbox is `sleeping` only if its record says so.** After a reboot, or a
Firecracker that died while the agent was down, the sandbox is `stopped`;
`start` it. Stopping a sleeping sandbox discards its snapshot.

**A sleep that fails leaves the guest running.** Its snapshot files are
removed and the guest is resumed; the API answers an error and the sandbox
stays `running`. (If even the resume fails, it is `paused`.)

**Snapshots set aside as `*.outrun`.** When the agent judges a snapshot stale —
the disk was modified after `vmstate.bin` was written, the image a pre-release
fork runs on was modified since, or a wake's resume got no answer and the guest
may have run — the sandbox becomes `stopped` and the files are renamed
`vmstate.bin.outrun` and `mem.bin.outrun` instead of being deleted, with a
warning `a stale snapshot file was set aside`. Nothing ever wakes from them;
they are kept in case the judgement was wrong (file times lost by a restore,
say). They take as much disk as the guest's memory. `start`, `stop`, the next
sleep and `delete` of that sandbox remove them; agent start-up leaves them.
To keep one, copy it away before any of those.

**A fork pauses a running parent for the snapshot and for the disk copy.** The
copy is `cp --reflink=auto` of the parent's `rootfs.ext4`, made while the
parent is paused so that the child's disk is of the same instant as its
memory. On a file system that supports reflink — XFS made with reflink (the
default of current `mkfs.xfs`) or Btrfs — the copy shares the parent's blocks
and takes little time whatever the image holds. ext4 has no reflink: there
`cp` copies the data, and the pause grows with the amount of data in the
image. Measured on the lab on ext4 (a virtio disk): an image holding 472 MiB
of data paused the parent 0.2 to 0.7 s, and the whole fork call took 1.0 to
1.4 s; an image holding 7.4 GiB of data paused the parent 10 to 12 s, and the
whole fork call took 12 to 14 s. That is about 0.6 to 0.75 GiB per second of
pause on that disk. The agent logs nothing for a fork that succeeds, so the
pause shows only as the latency of the fork call (FLU-89).
Put `{data_dir}` on XFS or Btrfs if forks of large, full images matter
to you. A stop or delete of the parent during the
copy ends it and the fork. A sleeping parent is not paused (it has no VMM);
its files are copied while it is held.

**`409 {"error":"busy"}`.** A sandbox takes one lifecycle operation at a time.
A repeat of the same request waits up to 25 s for the one in progress; any
other answers busy at once. A fork of a parent that is busy answers busy with
`Retry-After: 2` from feluccad; a busy or refused fork does not count as
activity of the parent. Forks of a sleeping parent run side by side;
forks of a running or paused parent do not. Stop and delete end what is in
progress; if it has not let go within 15 s they answer 409 with
`"state":"stopped"` and the sandbox is stopped. feluccad relays that delete
refusal as 409 with `Retry-After: 2` and keeps the record — retry the delete.
Lifecycle requests on a sandbox feluccad still has as `creating` answer
`409 {"error":"InvalidState: creating (the sandbox is still being created)","state":"creating"}`.

### 12.5 Workers that report their isolation down

Each heartbeat carries `isolation_ok`, the agent's isolation verdict (§8.2). feluccad logs `node reports its
isolation down: its sandboxes may not be fenced from each other or from the
host` when a node turns false, and `node reports its isolation in place again`
when it stops. `felucca_nodes_unfenced` counts the ready nodes whose last
heartbeat said false; a node that goes silent drops out of it although its
sandboxes may keep running, so alert on it together with `felucca_nodes_ready`.
**Placement avoids them.** A create goes to the ready worker with the fewest
sandboxes among those that do not report their isolation down (a worker that
never sends `isolation_ok` counts as fine). Only when every ready worker
reports it down does a create go to the one with the fewest, as before, and
feluccad warns, at most once a minute, `creates are being placed on workers
that report their isolation down: no other ready worker exists (such a worker
refuses new guests until its fence is repaired)`. Fork, wake and start stay on
the sandbox's own worker.

