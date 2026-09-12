# felucca-agent (Rust)

Felucca node agent: REST API on `:9090`, drives Firecracker over per-VM unix
sockets, snapshot sleep/wake, fork, warm pool, tap/bridge/nftables guest
networking, register/heartbeat with feluccad. tokio + axum + serde; static musl.

The wire/config contract is `docs/API-V2.md`; `test/conformance/` enforces it.
On-disk `meta.json`/`vmstate.bin`/`mem.bin` formats are unchanged from v2 —
the agent adopts any v2-era `data_dir` on startup (reconcile in `src/vm/reconcile.rs`).
Port history: `docs/adr/ADR-0003-go-rust-port.md`.

## Build & test (inside the infra-saas-lab Lima VM — never on the macOS host)

```sh
REPO=/Users/magdy/projects/github.com/alpham/infra-saas
limactl shell infra-saas-lab -- bash -c \
  "source ~/.cargo/env && export CARGO_TARGET_DIR=\$HOME/.cargo-target/felucca-agent && \
   cd $REPO/rust/agent && cargo test --target aarch64-unknown-linux-musl && \
   cargo build --release --target aarch64-unknown-linux-musl"
```

For x86_64, build on an x86_64 machine (or set up a musl cross linker).

## Run

```sh
FELUCCA_TOKEN=<bearer-token> felucca-agent \
  --control-plane http://<feluccad-host>:8080 --data-dir /srv/ignis \
  --net on --net-cidr 10.231.0.0/24 --pool-size 1
# advertise_addr auto-detects via the route to the control plane when unset.
# Precedence: flags > FELUCCA_* env > --config JSON > defaults.
```

## Layout

- `src/config.rs` — layered config loader (parity with feluccad's, incl. `net on|off`)
- `src/server.rs` — axum routes + bearer middleware (constant-time)
- `src/fc.rs` — Firecracker UDS client (boot-source/drives/snapshot bodies byte-matched to v2)
- `src/vm/` — manager, `meta.json` (`Option` fields serialize as explicit null), pool, startup reconcile
- `src/net.rs` — bridge `felucca0`, `hth-<slot>` taps, nftables masquerade (shells out to ip/nft)
- `src/ipalloc.rs` — slot↔IP allocator over `net_cidr`
- `src/registration.rs` — register-until-id, 5s heartbeats, /proc readers

Firecracker children are spawned `kill_on_drop(false)` and reaped by detached
wait tasks (improvement over v2, which leaked zombies).
