# Hearth — Production Deployment Guide

Hearth is two static Linux binaries (`hearthd` and `hearth-agent`) and a handful
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

---

## 1. Topology

```
                          HTTPS :443
 Clients ─────────────► Caddy (reverse proxy)
                               │
                               │ HTTP :8080 (loopback / private only)
                               ▼
                         hearthd (control plane)
                         /var/lib/hearth/state.json
                               │
                  ┌────────────┼────────────┐
                  │            │            │
            HTTP :9090   HTTP :9090   HTTP :9090
                  ▼            ▼            ▼
           hearth-agent  hearth-agent  hearth-agent
           (worker 0)    (worker 1)    (worker N)
                  │
          Firecracker × M microVMs
          /dev/kvm, tap, nftables
```

**One control plane, N worker nodes.**

This is the **Caddy-fronted single-node** topology, and it is what the shipped
example configs are tuned for: `hearthd` binds `127.0.0.1:8080` and only Caddy
reaches it. A **WireGuard-overlay fleet** is the other supported shape and it
needs the opposite bind — agents reach hearthd at its overlay address inside the
tunnel, so a loopback bind refuses every registration. Decide which one you are
building before you install: see [§6.3](#63-bind-address-which-topology-are-you-in)
and [§6.4](#64-wireguard-overlay-fleets).

- The control plane runs `hearthd` and holds all state. It does not run
  Firecracker.
- Each worker runs `hearth-agent`, which drives Firecracker processes directly.
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
| `nft` (nftables) | Guest networking — `apt install nftables` / `dnf install nftables` |
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

# hearthd (Go, static, cross-compiles to both arches from one box)
limactl shell infra-saas-lab -- bash -c \
  "export PATH=\$PATH:/usr/local/go/bin && cd $REPO/go && \
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/x86_64/hearthd ./cmd/hearthd && \
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' \
     -o $REPO/deploy/release/aarch64/hearthd ./cmd/hearthd"

# hearth-agent (Rust, static musl)
limactl shell infra-saas-lab -- bash -c \
  "source ~/.cargo/env && export CARGO_TARGET_DIR=\$HOME/.cargo-target/hearth-agent && \
   cd $REPO/rust/agent && cargo build --release --target aarch64-unknown-linux-musl && \
   cp \$CARGO_TARGET_DIR/aarch64-unknown-linux-musl/release/hearth-agent \
      $REPO/deploy/release/aarch64/hearth-agent"
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
> `hearthd.service` vs the Go runtime — it survives startup and the
> conformance suite; the 48h soak is the verdict. If you use a non-default
> agent `data_dir`, override `ReadWritePaths` via a drop-in that resets the
> list (see the comment in `hearth-agent.service`).
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

1. Creates directories: `/etc/hearth`, `/var/lib/hearth`, `/srv/hearth`, `/usr/share/hearth/ui`.
2. Creates the `hearth` system user (no login shell, no home).
3. Copies `hearthd` to `/usr/local/bin/hearthd`.
4. Installs `deploy/systemd/hearthd.service`.
5. **Generates a real bearer token** (`openssl rand -hex 32`) and writes
   `/etc/hearth/hearthd.json` with that value, mode 0640, owned `hearth:hearth`
   — the example config's `REPLACE_WITH_…` placeholder never reaches `/etc`.
6. Writes `/etc/hearth/firewall.nft` and installs
   `deploy/systemd/hearth-firewall.service`, which applies it at boot (§8),
   then applies it immediately.
7. **Enables but does not start** hearthd, and prints the generated token once.

The installer does not start the service because the config still needs this
host's own values and because a control plane should not answer the network
before you have looked at the firewall rules it just installed.

Record the printed token — it is not shown again, and every worker needs the
same value. Then review the config and start:

```sh
sudo cat /etc/hearth/hearthd.json    # token is already filled in
sudo systemctl start hearthd
sudo systemctl status hearthd

# Smoke test (from the server — port 8080 is loopback-only, see §8)
curl -s http://127.0.0.1:8080/healthz   # {"ok":true} — open, no token
curl -s -H "Authorization: Bearer ${TOKEN}" \
     http://127.0.0.1:8080/metrics      # Prometheus text — admin token required (§6.2)
```

To install with a token you already have (rebuilding a host, or pairing with an
existing fleet), pass it in — the installer validates it and does not generate
one:

```sh
HEARTH_TOKEN=<existing fleet token> sudo -E bash deploy/install.sh control-plane
```

---

## 5. Install — worker nodes

Run on each worker as root, passing the control plane's token and address:

```sh
HEARTH_TOKEN=<token printed by the control-plane install> \
HEARTH_CONTROL_PLANE_IP=10.0.1.10 \
  sudo -E bash deploy/install.sh worker
```

The installer:

1. Checks `/dev/kvm`, installs the **pinned** Firecracker release and verifies
   its SHA-256 before it lands in `/usr/local/bin` (§3 note), checks `nft`.
2. Copies `hearth-agent` to `/usr/local/bin/hearth-agent`.
3. Installs `deploy/systemd/hearth-agent.service`.
4. Writes `/etc/hearth/hearth-agent.json` with the real token (mode 0640) and an
   `/etc/hearth/agent.env` stub.
5. Writes `/etc/hearth/firewall.nft` and installs
   `deploy/systemd/hearth-firewall.service`, then applies them (§8).
6. **Enables but does not start** hearth-agent.

`HEARTH_TOKEN` is not optional in practice: without it the worker mints its own
token, which is safe but will not match the control plane, so the node never
registers. `HEARTH_CONTROL_PLANE_IP` is what opens port 9090 to the control
plane — leave it unset and 9090 stays loopback-only.

Then finish the config — `bind` must be this node's management address, never
`0.0.0.0`, because `0.0.0.0` includes the bridge gateway every guest routes
through (§6.3). On an overlay fleet, bind the node's overlay address instead.

Read the installed token into a variable **first**: the config file is about to
be rewritten, and reading it inside the same command that truncates it is a race
that has produced empty tokens (which the agent then refuses to start on).

```sh
# 1. Recover the token the installer wrote.
TOKEN=$(sudo sed -n 's/.*"token": "\([^"]*\)".*/\1/p' /etc/hearth/hearth-agent.json)
[ -n "$TOKEN" ] || { echo "no token found — check /etc/hearth/hearth-agent.json"; exit 1; }

# 2. This node's management address.
ADDR=$(hostname -I | awk '{print $1}')

# 3. Rewrite the config. Dropping "_bind_note" is intentional — it is guidance
#    for the shipped example, ignored by the agent, and you have now chosen.
sudo tee /etc/hearth/hearth-agent.json >/dev/null <<EOF
{
  "bind": "${ADDR}:9090",
  "control_plane": "https://hearth.internal.example.com",
  "advertise_addr": "${ADDR}",
  "data_dir": "/srv/hearth",
  "token": "${TOKEN}",
  "pool_size": 2,
  "net": "on",
  "net_cidr": "10.231.0.0/24"
}
EOF
sudo chmod 0640 /etc/hearth/hearth-agent.json
unset TOKEN
```

Then fetch guest assets (kernel + rootfs) — only needed once per worker. The
kernel and base rootfs are pinned by version and SHA-256 and verified before
either is installed or extracted:

```sh
sudo bash deploy/firecracker-assets.sh --data-dir /srv/hearth
```

Then start the agent:

```sh
sudo systemctl start hearth-agent
sudo systemctl status hearth-agent
```

Verify the worker is registered on the control plane within ~5 s:

```sh
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.internal.example.com/api/v1/nodes
# Should show the worker with status "ready"
```

---

## 6. Tokens, auth, and bind addresses

Hearth uses a single shared bearer token between the control plane and all
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

- `/etc/hearth/hearthd.json` — `"token"` key (or `HEARTH_TOKEN` env var)
- `/etc/hearth/hearth-agent.json` on every worker — same `"token"` value

**Keep the token out of version control.** Store it in `/etc/hearth/hearthd.env`
or `/etc/hearth/agent.env` (mode 0640, owned by the service user) if you prefer
not to put it in the JSON config:

```sh
# /etc/hearth/hearthd.env
HEARTH_TOKEN=a3f8c21b...

# /etc/hearth/agent.env
HEARTH_TOKEN=a3f8c21b...
```

Env vars take precedence over config file values (precedence: flags > env > config > defaults).

### 6.1 Both binaries refuse to start without a real token

There is no "auth off by accident" state any more. A missing token used to mean
*no authentication required*; it now means *do not serve*.

| Condition | `hearthd` | `hearth-agent` |
|---|---|---|
| `token` empty / config failed to load | **exits** | **exits** |
| `token` matches a shipped placeholder (contains `REPLACE_WITH`, or `hearth-lab-token`) | **exits** | **exits** |
| `token` too short | **exits** below 32 chars | **exits** below 16 chars |
| Deliberate open mode | `--insecure-no-auth` | *no such flag — the agent always requires a token* |

```
$ hearthd --db /var/lib/hearth/hearth.db
level=ERROR msg=auth err="no auth token configured: set token / HEARTH_TOKEN /
  --token, or pass --insecure-no-auth for a loopback-only lab"

$ hearth-agent --config /etc/hearth/hearth-agent.json     # unedited example
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
`hearthd` only, it must be named explicitly, and hearthd logs a WARN at every
start — plus a second, louder one when the bind is not a loopback address:

```
level=WARN msg="INSECURE MODE: --insecure-no-auth is set, the admin API accepts
  every caller unauthenticated — loopback binds and lab use only"
level=WARN msg="INSECURE MODE on a non-loopback address: every host that can
  route here has full admin access"
```

There is no agent equivalent, deliberately: the agent API is root on the node.

The lab scripts (`scripts/verify-v2.sh`, `scripts/conformance-lab.sh`,
`scripts/roll-v3.1.sh`) take the token as `$1` or `HEARTH_TOKEN`, or read it
from `~/.config/hearth/lab-token` (override with `HEARTH_TOKEN_FILE`). They fail
with a clear error when none is set rather than falling back to a literal.

### 6.2 What the token guards

- Every `/api/*` request requires `Authorization: Bearer <token>`.
- **Three kinds of credential, three reaches.** The admin token is
  unrestricted. A tenant API key (`hearth_sk_…`) reaches its own sandboxes and
  nothing fleet-wide. A node credential (`hearth_nt_…`, §6.7) reaches exactly
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
> hearthd authenticates *before* it consults the backoff, so the guard only ever
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

**The ceiling depends on how well hearthd can tell clients apart.** When the key
provably stands for more than one client, the wait is capped at **2s** instead
of 60s — a shared key must not let one anonymous neighbour spend a full minute
of `429`s on everyone who shares it. hearthd treats a key as shared when, and
only when, the *configuration and peer address* say so — never a header, so a
client cannot opt itself into the softer cap:

| Situation | Key | Cap |
|---|---|---|
| No `trusted_proxies`, peer is loopback or RFC1918/ULA private (the shipped Caddy-on-loopback topology, and overlay fleets) | the peer | **2s** |
| Peer *is* a declared proxy but forwarded nothing attributable | the proxy | **2s** |
| A client address attested by the declared proxy chain | that client | **60s** |
| No `trusted_proxies`, peer is a public address (hearthd exposed directly) | the peer | **60s** |

- The key is a source address. Out of the box that is the **connection peer**,
  and behind a reverse proxy every client arrives as the proxy's own address —
  so one misbehaving client's failures raise everyone else's `401`s to `429`s
  (they do **not** lock anyone out; see the box above). Fixing that means
  configuring both halves: the proxy must **overwrite** `X-Forwarded-For`, and
  hearthd must be told to believe it via `trusted_proxies`. **[§7.1](#71-forwarded-client-addresses-required-reading)
  is required reading before you put anything in front of hearthd** — getting it
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

| Topology | `hearthd` `bind` | Why |
|---|---|---|
| **Caddy-fronted single node** (§7, the shipped example) | `127.0.0.1:8080` | Caddy terminates TLS and proxies over loopback; §8 drops 8080 from everywhere else. Nothing else needs to reach the port. |
| **WireGuard-overlay fleet** (§6.4) | the hub's overlay address, e.g. `10.100.0.1:8080` | Enrolled agents speak plain HTTP to hearthd *inside* the tunnel. On a loopback bind **every agent registration is refused** and the fleet never forms. |

For `hearth-agent`, `bind` is always **this node's own management address** (or
its overlay address on an overlay fleet) and never `0.0.0.0`. The wildcard
includes the bridge gateway `10.231.0.1` that every guest has as its default
route, and guest→host packets hit the INPUT hook — which the agent's own
forward-chain tenant isolation never sees. The §8 firewall's
`iifname "hearth0" drop` is the second layer under that; the bind address is the
first.

Both shipped example configs carry a `_bind_note` key spelling this out. It is
ignored by both loaders (verified: hearthd parses the example and listens on the
configured host) and is safe to delete once you have set a real value.

### 6.4 WireGuard overlay fleets

Set `wg_ip` on hearthd to enable the overlay; it is off when empty, which is the
default and what the single-node topology uses. hearthd refuses to start with
the overlay half-configured:

- `wg_ip` must be a CIDR (the hub's overlay address, e.g. `10.100.0.1/24`).
- `wg_endpoint` is required — the **public** `host:port` joining workers dial.
- The overlay refuses to run in open mode: minting join tokens without a
  credential would be unauthenticated kernel network configuration.

Then bind hearthd on the overlay address (§6.3), keep the §8 rule aligned with
whatever you bound, and enroll each worker once:

```sh
# On the control plane — mint a one-time join token:
curl -s -XPOST -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/join-tokens

# On the worker — first boot only; the persisted /srv/hearth/wg.json wins after:
hearth-agent --join https://hearth.example.com --join-token <token>
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
  access log of every proxy in front of hearthd — so it is a token to rotate,
  not a token to use. Never put one in a URL, a bookmark, or a shared link.
- An upgraded console clears any `hearth_token` left in web storage by an older
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
recoverable without raw SQL against `hearth.db`.

```sh
# List a tenant's keys — id, prefix, timestamps. Never the secret or its hash.
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/tenants/tn-450c79b1/keys
# {"keys":[{"id":"key-f3a3349e533a","prefix":"hearth_sk_2f75",
#           "created_at":1787982290,"expires_at":0,"revoked_at":null}, …]}

# Match the leaked secret against `prefix` (the first 14 chars), then revoke:
curl -s -XDELETE -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/keys/key-f3a3349e533a

# Mint a replacement that expires on its own (seconds; max one year):
curl -s -XPOST -H "Authorization: Bearer ${TOKEN}" \
  -d '{"expires_in_s":2592000}' \
  https://hearth.example.com/api/v1/tenants/tn-450c79b1/keys
```

`expires_at` is `0` for a key that never expires — the historical default, and
still what `POST /api/v1/tenants` mints alongside a new tenant. Prefer an
explicit `expires_in_s` for anything handed to a person or a CI job.

### 6.7 Per-node agent credentials

hearthd used to present the **cluster admin token** on every call it made to a
worker, and every worker had to present that same token back on register and
heartbeat. A node's address comes from `POST /api/v1/agents/register`, so every
outbound dial was a decision about where to deliver that secret — talking
hearthd into dialing an attacker-controlled host handed over the key to the
whole fleet, and compromising any one worker did the same.

Both halves now land. Enrollment with a one-time join token mints a per-node
bearer `hearth_nt_…`, and that credential is used **in both directions**:

- **hearthd → agent.** hearthd presents that node's credential, and nothing
  else, on every proxy call to it. An address with no stored credential is
  dialed with **no** bearer at all — which is where a forged registration lands.
- **agent → hearthd.** The agent presents it on `POST /api/v1/agents/register`
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
level=INFO msg="hearth-agent startup" ... node_cred=own
```

`node_cred=shared` means this node is still on the fleet token. That is a
working state, not a broken one, but it is the state the rollout is meant to
leave behind.

#### Already-enrolled nodes

**Nothing breaks and nothing needs doing urgently.** Workers already in
`hearth.db` when this shipped are grandfathered onto the shared token exactly
once, so they keep authenticating in both directions. That one-time bound is the
security property: if "this node has no credential yet" could mint one on
demand, registering an arbitrary address would still get the control-plane key
delivered to it. hearthd names each grandfathered node in a startup WARN:

```
level=WARN msg="existing nodes are still using the shared control-plane token;
  re-enroll each with a join token so it gets its own agent credential" nodes=3
```

To finish the rollout, re-enroll each node with a fresh join token, one at a
time. Re-enrollment **rotates** the credential (hearthd retires the previous one
in the same operation), and it is also the recovery path for a worker that lost
its copy.

> **On an overlay fleet, re-enroll with a fresh WireGuard key.** A join token
> says *an operator authorized an enrollment*; it does not say *which node*, and
> the pubkey naming the node is written by the caller. So `POST
> /api/v1/nodes/join` under a pubkey hearthd already knows must also carry the
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
> **`hearth-agent` does not send `node_token`** — its join request carries only
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
> identity survives — hearthd keys node rows on hostname, so the id and its
> sandboxes stay put and only `addr` moves. But the **old WireGuard peer is not
> removed**: its row and its kernel peer entry stay, and its overlay address
> stays allocated. Nothing in the API retires a peer today, so on a fleet you
> re-enroll often, watch the overlay pool (`wg_ip`'s prefix) and prune stale
> peers by hand with `wg set wg-hearth peer <old-pubkey> remove`.
>
> On a **non-overlay** fleet there is no pubkey and no such check: re-enrolling
> is just another `POST /api/v1/agents/register` with a fresh `join_token`.

#### The fleet token is still required on workers

Per-node credentials narrow the blast radius; they do not remove the shared
token from workers. `hearth-agent` still needs `token` set, still refuses to
start without a real one (§6.1), and still uses it for **template image pulls**
(`GET /api/v1/images/{name}`), which are an admin-only route a node credential
cannot reach. Keep treating a worker compromise as an admin-token compromise
until that path has its own credential.

#### Journal lines worth knowing

```
level=WARN msg="no agent credential for this node: dialing it without one —
  enroll it with a join token" node=10.100.0.2:9090
```
hearthd is dialing a node it has no credential for — a node registered without a
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

Hearth does not terminate TLS itself. Run Caddy on the control plane host.
(In-binary autocert is the other option — set `tls_domain`, §6.4. Either way,
if anything at all sits in front of hearthd, read **§7.1**: the proxy decides
what address hearthd throttles on.)

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
hearth.example.com {
    # Caddy obtains and renews a Let's Encrypt certificate automatically.
    # Replace with your real domain.

    # Reverse-proxy everything to hearthd on loopback.
    #
    # REQUIRED at the edge. header_up makes X-Forwarded-For say exactly one
    # thing — the peer Caddy actually accepted — instead of appending to
    # whatever the client sent. hearthd only reads it if you ALSO set
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
    # Hearth puts a credential there: the API takes the token in the
    # Authorization header (which Caddy does not log), and the console strips
    # and ignores `?token=` rather than accepting it (§6.5). Keep it that way —
    # do not add a token to a URL to "make curl easier".
    log {
        output file /var/log/caddy/hearth-access.log
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
> Regenerate with `openssl rand -hex 32`, update `hearthd.json` and every
> worker's `hearth-agent.json`, and restart. It is in browser history and in the
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

hearthd keys its brute-force throttle (§6.2) on the address it attributes a
request to. Behind a proxy, the connection peer is always the proxy, so without
configuration **every client shares one throttle key** and one client's failed
guesses raise everyone else's `401`s to `429`s. The fix has two halves — a
hearthd setting and a proxy directive — and it is **both or neither**. Doing
only the hearthd half is the one outcome that is *worse* than leaving it alone:
it turns a shared key into a client-chosen one.

> **Why this section is load-bearing and not advisory.** hearthd cannot tell a
> forwarded header its proxy *wrote* from one the proxy *copied through* — both
> arrive as bytes on a connection from the proxy. Nothing hearthd can do in code
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

**Half 1 — hearthd: `trusted_proxies`.** hearthd honours `X-Forwarded-For`
**only** when the immediate peer's address falls inside one of the CIDRs listed
here (`X-Real-IP` needs one more opt-in — see below). The default is **empty,
which means trust nothing**: no forwarded header is read at all, and the
connection peer is the key. That default is the safe one and it is deliberate —
a client-settable header believed by default lets any caller choose its own
throttle key, which is strictly worse than throttling the proxy as one source.

| Source | Form | Example |
|---|---|---|
| Config file (`/etc/hearth/hearthd.json`) | JSON array of strings | `"trusted_proxies": ["127.0.0.1/32", "::1/128"]` |
| Environment | comma-separated | `HEARTH_TRUSTED_PROXIES=127.0.0.1/32,::1/128` |
| Flag | comma-separated | `--trusted-proxies=127.0.0.1/32,::1/128` |

- A **bare IP is accepted** and normalised to a single-host CIDR (`127.0.0.1` →
  `127.0.0.1/32`, `::1` → `::1/128`).
- An entry hearthd cannot parse as a CIDR is a **startup failure**, not a
  skipped line: silently dropping it would leave you keying throttles off the
  proxy's own address while believing otherwise, with nothing anywhere saying
  why.

```
level=ERROR msg=trusted-proxies err="trusted proxy \"10.0.0.0/99\" is not a
  CIDR: write e.g. \"127.0.0.1/32\" for a local reverse proxy or \"10.0.0.0/8\"
  for a range"
```

- `--trusted-proxies=""` is an explicit "trust nobody" and is honoured as such.
- List **every** proxy hop, and **only** proxies. hearthd walks the
  `X-Forwarded-For` chain right to left and stops at the first hop that is not
  itself a trusted proxy — that hop is the client. **The left-most entry, the one
  this kind of code usually reaches for, is whatever the original caller wrote
  and is never used.** A hop hearthd cannot parse ends the chain of custody and
  it falls back to the peer; if every hop is one of yours, the peer is as far as
  it goes.
- The walk stops after **16 hops**, and a chain longer than that falls back to
  the peer. The header is caller-sized, so this is a bound on work, not a
  deployment limit: real chains are one or two hops. If you genuinely run more
  than sixteen proxies in front of hearthd, the ones past the sixteenth from the
  right cannot be attributed and every client behind them shares the peer key.
- The whole request head — request line plus all headers — is capped at **16
  KiB** (Go's default is 1 MiB, and this parse runs before authentication).
  Over that, hearthd answers `431 Request Header Fields Too Large`. Generous for
  a bearer, a trace id and a proxy chain; if your proxy injects a large header
  set, that is the limit to check.
- **Never widen this to a range that contains real clients** (and never
  `0.0.0.0/0`). Everything inside these CIDRs is trusted to name whoever it
  likes, so a client inside one can forge its own throttle key — the exact
  problem the empty default exists to prevent.

For the shipped Caddy-fronted single-node topology (§7), the whole of half 1 is:

```jsonc
// /etc/hearth/hearthd.json
"trusted_proxies": ["127.0.0.1/32", "::1/128"]
```

**`X-Real-IP` is a second, separate opt-in: `trust_x_real_ip`.** It defaults to
**false**, and while it is false hearthd ignores `X-Real-IP` completely — from
*any* peer, declared proxy or not. Listing a proxy in `trusted_proxies` does
**not** by itself make `X-Real-IP` believed.

| Source | Form | Example |
|---|---|---|
| Config file | JSON boolean | `"trust_x_real_ip": true` |
| Environment | boolean string | `HEARTH_TRUST_X_REAL_IP=true` |
| Flag | boolean | `--trust-x-real-ip` |

- The reason it is separate: `X-Forwarded-For` carries a chain hearthd can walk,
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
hearthd cannot tell the two apart on the wire.

*Caddy — the shipped topology (§7).* `reverse_proxy` does set `X-Forwarded-For`
by itself, **appending** the connection peer to any value the client supplied.
Since hearthd walks the chain right to left, the appended real peer is the entry
it lands on, so a bare `reverse_proxy` is not exploitable today. Do not rely on
that: it makes your throttle key a property of hearthd's parser rather than a
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
`X-Real-IP` at all, because hearthd ignores that header unless you opt in with
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

    # Harmless, and correct if you ever set trust_x_real_ip. hearthd ignores
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
it, so the header still carries whatever the client wrote. hearthd's
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
URL=https://hearth.example.com/api/v1/nodes
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
`X-Forwarded-For` through *and* hearthd is trusting the peer that sent it. Fix
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
                   [ hearthd ]
                         │
          Private network (e.g. 10.0.1.0/24)
                         │
                   Port 9090    ← control plane → workers ONLY
                         │
                [ hearth-agent ]
                         │
          Bridge hearth0 (10.231.0.1)  ← guests must NOT reach the host here
                         │
                   /dev/kvm, tap
                         │
                 [ Firecracker ]
```

**These rules are installed, not suggested.** `deploy/install.sh` writes
`/etc/hearth/firewall.nft` and installs `deploy/systemd/hearth-firewall.service`
to apply it, and `hearth-agent.service` has `Requires=hearth-firewall.service` —
a worker whose rules are missing does not serve its API at all. The ruleset
lives in its own `inet hearth_host` table, so it never collides with the agent's
`ip hearth` table or with whatever else the host runs.

The unit is a reviewable file under `deploy/systemd/` rather than a heredoc
inside the installer, because it is part of that enforcement path: it is what
the agent's `Requires=` resolves to, and it is covered by
`scripts/verify-units.sh` along with the other three units. The installer only
rewrites the two absolute paths in it, and only when `nft` or the config
directory is somewhere other than the shipped default.

```bash
bash scripts/verify-units.sh    # systemd-analyze verify over deploy/systemd/*
```

Run it on a systemd host (the lab VM is fine). It stages a throwaway root with
stub binaries at the paths the real `ExecStart` lines name, so the units are
checked exactly as shipped — without it, `systemd-analyze verify` on a checkout
always fails on "Command /usr/local/bin/hearthd is not executable" and on the
unresolved `hearth-firewall.service`, and both of those are artifacts of the
checkout rather than defects. It treats any output as failure: systemd exits 0
on a malformed `RestrictAddressFamilies=` or an unknown directive while only
warning about it, which would otherwise let a hardening regression through.

The rule that matters most is the worker's `iifname "hearth0" drop`. The agent
listens on the bridge gateway that every guest has as its default route, and
guest→host packets hit the INPUT hook — which the agent's own forward-chain
tenant isolation never sees. Without it, a tenant with root in their own
sandbox (the expected design) reaches the node's root control API and can exec
into every other tenant's guest. Established flows and ICMP stay allowed so
liveness checks still work.

Worker ruleset (`HEARTH_CONTROL_PLANE_IP=10.0.1.10`):

```
table inet hearth_host {
    chain input {
        type filter hook input priority filter; policy accept;
        iifname "hearth0" ct state established,related accept
        iifname "hearth0" meta l4proto { icmp, ipv6-icmp } accept
        iifname "hearth0" drop
        tcp dport 9090 ip saddr != { 127.0.0.1, 10.0.1.10 } drop
        tcp dport 9090 ip6 saddr != { ::1 } drop
    }
}
```

Control plane ruleset:

```
table inet hearth_host {
    chain input {
        type filter hook input priority filter; policy accept;
        tcp dport 8080 ip saddr != 127.0.0.1 drop
        tcp dport 8080 ip6 saddr != ::1 drop
    }
}
```

Inspect and manage them with:

```sh
sudo nft list table inet hearth_host
sudo systemctl status hearth-firewall
sudo systemctl restart hearth-firewall     # re-apply after editing the file
```

The installer regenerates `/etc/hearth/firewall.nft` on every run, so local
additions belong in a separate table of your own. If workers reach hearthd
directly on 8080 rather than through Caddy, widen the control-plane rule there —
and note that the API is then exposed to whatever you widen it to.

Ports 443/80 for Caddy are yours to open as usual:

```sh
ufw allow 443/tcp
ufw allow 80/tcp
```

### 8.1 The agent's own nft tables (not installer-managed)

`hearth-agent` builds and maintains its own rules at runtime, in tables the
installer never touches. Expect to see them on a worker; do not hand-edit them
(every one is flush-and-rebuilt, so edits are lost on the next lifecycle event).

| Table / chain | Purpose |
|---|---|
| `ip hearth postrouting` | egress masquerade for the guest CIDR |
| `ip hearth forward` | cross-tenant isolation — the `tenant_pairs` set (ADR-0005) |
| `ip hearth input` | guest→host fence: drops everything arriving on `hearth0` except ICMP and replies to host-initiated flows, with the agent's own port named in an explicit drop |
| `ip hearth ingress` | per-expose DNAT to guest ports (ADR-0007) |
| `bridge hearth forward` | accepts only ARP and IPv4 between bridge ports, dropping everything else — closes the pure-L2 path around the IPv4-only tenant rules, link-local IPv6 above all |
| `netdev hearth <tap>` | one chain per tap, `policy drop`: pins that guest's source MAC, ARP sender MAC, ARP sender IP, and IPv4 source address |

Two operational consequences:

- **IPv6 is disabled on `hearth0`** by the agent. Guests get IPv4 from the
  kernel command line; there is no IPv6 guest networking to configure.
- **The agent refuses to start** if networking is on and any of these fences is
  not in force. That is deliberate: a worker without them puts every tenant on
  one flat network, and nothing on the tenant's side would show it while hearthd
  kept scheduling onto it. The fatal names what failed — fix `nft`/permissions,
  or run the agent with `--net off` to serve unnetworked guests on purpose.

  ```
  fatal: cross-tenant isolation is not in force on this node (see the errors
    above) — refusing to serve tenants
  ```

These sit **under** the installer's `inet hearth_host` rules from §8, not
instead of them. Both layers exist because they fail independently: the agent's
own fence is gone if the agent is not running, and the installer's is gone if
the firewall unit was never installed.

`hearth-agent` also refuses a `bind` inside the guest CIDR outright, for the
same reason the wildcard is not the default (§6.3):

```
fatal: refusing to listen on 10.231.0.1 — it is inside the guest CIDR
  10.231.0.0/24, which every sandbox can route to
```

If you genuinely front the agent with your own firewall and want the wildcard
back, `bind_any` (`HEARTH_BIND_ANY`, `--bind-any`) is the named opt-out. It is
off by default; leaving it off, the agent listens on its management address
**and** on `127.0.0.1` — so local health checks and the rollout scripts keep
working, which is why dropping the wildcard costs nothing.

---

## 9. How local dev and production differ

The binaries are identical. Only the config values change.

| Setting | Lima lab | Production |
|---|---|---|
| `control_plane` | `http://192.168.104.3:8080` | `https://hearth.internal.example.com` |
| `advertise_addr` | `192.168.104.x` (user-v2 network) | `10.0.1.x` (private DC IP) |
| `bind` (agent) | the VM's user-v2 address | the node's management address (§6.3) |
| `bind` (hearthd) | the VM's user-v2 address, so the host can reach the UI | `127.0.0.1:8080` behind Caddy, or the overlay address on a wg fleet (§6.3) |
| `token` | 64-hex-char secret (same as prod — the binaries reject empty, short and placeholder tokens everywhere, §6.1) | 64-hex-char secret |
| `/metrics` | admin token required (same as prod) | admin token required (§6.2) |
| TLS | none (L2-isolated Lima network) | Caddy terminates HTTPS |
| `pool_size` | `0` (lab is too small) | `2`–`5` per node |
| `state_path` | `/var/lib/hearth/state.json` | same |
| `data_dir` | `/srv/hearth` | same |
| systemd | not used in lab (so the host firewall unit is not installed either — do not expose a lab VM) | `hearthd.service` / `hearth-agent.service` + `hearth-firewall.service` |

The Lima lab uses the same JSON config structure. See `deploy/config/local-lab.md`
for the exact values used in each Lima VM.

---

## 10. Upgrade procedure

Hearth upgrades are binary-replacement restarts. **Sleeping VMs survive agent
restarts** (the Firecracker process is already stopped when a VM is sleeping;
the snapshot on disk is untouched by a restart).

### Control plane upgrade

```sh
# 1. Build new binaries (see §3 — Go build inside the toolchain VM)

# 2. Copy the new binary to the server
scp deploy/release/x86_64/hearthd root@cp-host:/usr/local/bin/hearthd.new

# 3. Atomic replace + restart (state.json is preserved)
ssh root@cp-host "
  install -m 0755 /usr/local/bin/hearthd.new /usr/local/bin/hearthd
  systemctl restart hearthd
  systemctl status hearthd --no-pager
"
```

### Worker upgrade

```sh
# 1. Build new hearth-agent binary (see §3 — Rust build inside the toolchain VM)

# 2. Copy to worker
scp deploy/release/x86_64/hearth-agent root@worker-host:/usr/local/bin/hearth-agent.new

# 3. Atomic replace + restart
#    Running VMs are unaffected — Firecracker children are independent processes.
#    Sleeping VMs are safe — their snapshot is on disk; the agent re-discovers
#    them from meta.json on restart (this is the contract from API-V2.md §6).
ssh root@worker-host "
  install -m 0755 /usr/local/bin/hearth-agent.new /usr/local/bin/hearth-agent
  systemctl restart hearth-agent
  systemctl status hearth-agent --no-pager
"
```

The worker re-registers with the control plane within ~5 s after restart.
Running sandboxes remain in the `running` state because their Firecracker
processes are independent of the agent process.

### Rolling upgrades (multiple workers)

Upgrade workers one at a time. Before restarting an agent, optionally drain it:

```sh
# The scheduler will not place new VMs on a down node.
# No built-in drain command in v2 — simply stop the agent,
# upgrade, restart; in-flight creates will fail with 503 and
# the client should retry (the control plane will choose another
# ready worker).
```

---

## 11. Troubleshooting

### Observability (v4 P6)

All three binaries log structured `key=value` text to stderr → journald.
Every `/api/` request gets an `X-Hearth-Request-Id` (also echoed to the
client); to follow one failing call across binaries:

```bash
# On the control plane, find the request:
sudo journalctl -u hearthd | grep req-<id>
# On the owning worker, the same id appears on the agent's handler logs:
sudo journalctl -u hearth-agent | grep req-<id>
```

Agent verbosity: set `RUST_LOG=debug` in `/etc/hearth/agent.env` and restart.
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
| `systemctl status hearthd` shows `failed` | Binary not found or config parse error | Check `journalctl -u hearthd -n 50`; verify `/usr/local/bin/hearthd` exists and is executable |
| Either binary exits at startup complaining about the token | Token empty, a placeholder (`REPLACE_WITH…`, `hearth-lab-token`), or too short — under 32 characters for `hearthd`, under 16 for `hearth-agent` | Put the fleet token in the JSON config or `HEARTH_TOKEN`; generate with `openssl rand -hex 32` (64 chars, clears both bars); see §6.1 |
| `hearthd` exits: `msg=trusted-proxies err="… is not a CIDR"` | A malformed entry in `trusted_proxies` — deliberately fatal, never skipped | Write CIDRs (`127.0.0.1/32`, `10.0.0.0/8`); a bare IP is accepted and normalised. See §7.1 |
| `hearthd` exits: `msg=trusted-proxies err="trust_x_real_ip is set but no trusted_proxies are declared"` | `trust_x_real_ip` on with nothing declared would believe `X-Real-IP` from every peer — a header any client can set | Either list the proxy in `trusted_proxies`, or drop `trust_x_real_ip` and have the proxy send `X-Forwarded-For` (preferred). See §7.1 |
| One client's bad token turns everyone else's `401`s into `429`s | Behind a proxy with no forwarded-header config, every client shares the proxy's address as its throttle key (holders of a valid token are unaffected — §6.2) | Configure both halves in §7.1: proxy **overwrites** `X-Forwarded-For`, hearthd lists the proxy in `trusted_proxies`. Never one without the other |
| `hearth-agent` will not start: `Unit hearth-firewall.service not found` | Worker installed without the host firewall (or `nft` was missing when the installer ran) | Install nftables and re-run `deploy/install.sh worker`; this is deliberate — see §8 |
| Control plane cannot reach a worker on 9090 | `HEARTH_CONTROL_PLANE_IP` was not set at install, so 9090 is loopback-only | Re-run the worker installer with it set, or edit `/etc/hearth/firewall.nft` and `systemctl restart hearth-firewall` |
| Installer aborts: `failed SHA-256 verification` | The pinned Firecracker or guest asset did not match its recorded digest | Do not bypass it. Re-download; if the upstream artifact genuinely changed, refresh the pin per the comment block in the script |
| `GET /healthz` returns `connection refused` from another host | hearthd binds `127.0.0.1:8080` (the shipped default — Caddy fronts it) | Intended. Reach it through Caddy, or widen `bind` *and* the §8 rule if you really need direct access |
| `GET /healthz` returns `connection refused` on the server itself | hearthd not listening | Check `bind` in config; `journalctl -u hearthd -n 50` |
| `GET /api/v1/nodes` returns `401` | Token mismatch or missing `Authorization` header | Verify `token` in hearthd config matches the `Bearer` value you're sending |
| A request with a **wrong or missing** token returns `429 too many failed attempts` with a `Retry-After` | The brute-force guard (§6.2) has armed a backoff for **your source address** after >10 failed credentials. Behind a proxy, the failures may be someone else's — you share the key | Fix the credential. A correct token is served immediately regardless of the backoff, so this only ever replaces the `401` you would otherwise get. Then stop whatever is retrying a stale token; the record decays with quiet time (one failure per 15s) |
| A request you believe is **correctly authenticated** returns `429 too many failed attempts` | Not possible from the throttle — hearthd authenticates first (§6.2). The token on that request is not the one hearthd holds | Compare the token actually being sent against `token` in `/etc/hearth/hearthd.json`; check for a stale value in a script, a shell history entry, or a `Bearer ` prefix that got mangled |
| `GET /metrics` returns `401`, Prometheus targets go down after an upgrade | `/metrics` is admin-gated now (§6.2) | Add `bearer_token` to the scrape job; see `deploy/grafana/` |
| Agents never register, `GET /api/v1/nodes` stays empty on an overlay fleet | hearthd bound to `127.0.0.1` while agents dial its overlay IP | Bind the overlay address (§6.3) and align the §8 rule; the shipped example is loopback for the Caddy topology |
| Console shows `MOCK` or `STALE` in the header | It never reached hearthd, or lost it — the rows on screen are not the live fleet | Read the banner; check hearthd and the token. Nothing on a `MOCK` console is real (§6.5) |
| Console shows `NO TOKEN` and is not polling | Deliberate — it will not spend auth attempts without a credential (§6.5) | Paste the token into the banner; click the header badge if you dismissed it |
| Worker shows `status: down` in node list | Agent heartbeat older than 15 s | Check `systemctl status hearth-agent` on the worker; check connectivity on port 9090 |
| Worker never appears in node list | `control_plane` URL wrong or token mismatch | Check agent logs: `journalctl -u hearth-agent -n 50`; verify `control_plane` points to hearthd and token matches |
| Sandbox stays in `creating` | Firecracker spawn failed on worker | Check `journalctl -u hearth-agent -n 100`; check `/dev/kvm` permissions; check `firecracker --version` |
| `POST /sandboxes` returns `503 no ready node` | No workers registered and ready | Fix the worker install and verify it reaches the control plane |
| Guest networking not working (`ip: null`) | `net` is `off` or agent config missing | Set `"net": "on"` and `"net_cidr"` in agent config; verify `nft` and `ip` are installed |
| Bridge `hearth0` missing after agent restart | Idempotent setup should recreate it | Agent recreates the bridge at startup; check `journalctl -u hearth-agent` for errors |
| `POST /sandboxes/{id}/wake` is slow (> 500 ms) | No warm pool VMs available | Increase `pool_size` in agent config (requires restart); check `hearth_pool_size` metric |
| `POST /sandboxes/{id}/fork` returns `501` | Running v1 binary | Upgrade to v2 binary |
| TLS cert errors | Caddy cannot reach ACME or wrong domain | Check `journalctl -u caddy`; verify DNS and port 80/443 reachability |
| `journalctl -u hearthd` shows `Permission denied` on state file | Wrong ownership of `/var/lib/hearth` | `chown -R hearth:hearth /var/lib/hearth` |

### Useful commands

```sh
# Control plane logs (live)
journalctl -u hearthd -f

# Worker agent logs (live)
journalctl -u hearth-agent -f

# Prometheus metrics (admin token required — §6.2)
curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/metrics | grep hearth_

# List all nodes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/nodes | jq .

# List all sandboxes
curl -s -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/sandboxes | jq .

# Check warm pool gauge on a specific node
curl -s -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/metrics | grep hearth_pool_size

# Check a sleeping sandbox's wake latency
curl -s -X POST -H "Authorization: Bearer ${TOKEN}" \
  https://hearth.example.com/api/v1/sandboxes/sb-abc123/wake | jq .wake_ms
```
