# ADR-0001 — Backend in Zig 0.16.0 (replacing Go)

- Status: Accepted
- Date: 2026-06-10
- Context: supersedes the Go node-agent + gRPC choice in ADR-0000 (baseline).
- Related: ADR-0002 (no Kubernetes control plane), `docs/PLAN.md`.

## Decision

Both backend components — the control plane (`feluccad`) and the node agent (`felucca-agent`) — are
implemented in **Zig 0.16.0**, communicating over **REST/JSON on HTTP and Unix domain sockets**.
No gRPC. The baseline's Go + `firecracker-go-sdk` + gRPC stack is replaced.

## Decision drivers

1. **User mandate.** Zig 0.16.0 is a hard, non-negotiable constraint. It is already installed and
   verified at `/usr/local/bin/zig` (`0.16.0`) on the `infra-saas-lab` build VM.
2. **The transport Firecracker actually uses is REST over a Unix domain socket.** Driving Firecracker
   is `PUT`/`GET` JSON against `/run/felucca/<id>.sock` (`/machine-config`, `/boot-source`, `/drives`,
   `/network-interfaces`, `/actions`, `/snapshot/create`, `/snapshot/load`). Zig std (`std.net`,
   `std.http`, `std.json`) maps onto this directly — UDS client + JSON is a few hundred lines, no SDK.
3. **No Zig gRPC ecosystem.** There is no mature gRPC stack for Zig. Forcing gRPC would mean writing a
   protobuf/HTTP-2 implementation. REST/JSON over `std.http` (with chunked responses / SSE for log and
   exec streaming, vsock for PTY) covers every transport need without that cost.
4. **Static binaries, trivial deployment.** Zig produces a single statically-linked binary per
   component. "Add a node" becomes "copy one file and run it" — exactly the one-command-join property
   the scale-out path (PLAN §10) and Phase-7 acceptance depend on. No runtime, no interpreter, no
   container required on a worker.
5. **No GC pauses on the hot path.** The wake/fork/snapshot path (PLAN §3) is the product's only
   defensible advantage; tail latency is the metric. Zig has manual, explicit memory management and no
   garbage collector, so there are no stop-the-world pauses to perturb wake p99. This matters most in
   `felucca-agent`'s warm-pool hand-out and CoW fork loops.
6. **Small, auditable surface for untrusted-workload infrastructure.** A platform whose whole purpose
   is isolating untrusted code benefits from a backend with no hidden runtime and a minimal dependency
   tree. Zig's explicitness aids review of the privileged agent code.

## Alternatives considered

- **Go (baseline).** Mature `firecracker-go-sdk` and gRPC story; fast iteration. **Rejected:** violates
  the user mandate; GC introduces hot-path tail-latency variance; gRPC is unnecessary given Firecracker
  is REST-over-UDS.
- **Rust.** Pairs naturally with Firecracker (same language) and is GC-free. **Rejected:** still
  violates the Zig mandate; the baseline already noted Rust only for a future hot-path rewrite.
- **gRPC transport in any language.** **Rejected:** no Zig ecosystem; Firecracker's own API is REST/JSON,
  so gRPC adds a translation layer with no benefit at lab scale. Streaming needs are met by HTTP
  chunked/SSE and vsock.

## Consequences

**Positive:** single static binary per component; clean fit to Firecracker's UDS REST API; no GC tail
latency on wake/fork; minimal dependencies; trivial node onboarding.

**Negative / risks:**
- `std.http.Server` in Zig 0.16 is young; expect to hand-roll chunked transfer, SSE framing, and
  connection lifecycle. Mitigation: the API surface is small (PLAN §6) and the UDS↔Firecracker side is
  de-risked by Firecracker's simple, well-documented REST API.
- Smaller library ecosystem than Go/Rust — SQLite access, JSON, and HTTP are first-party or thin
  bindings; anything exotic is hand-written.
- Zig 0.16 language churn: pin the toolchain to 0.16.0 (already installed) and avoid pre-release std
  APIs where alternatives exist.

## Follow-ups

- Pin `zig 0.16.0` in `build.zig`/CI; document the SQLite binding choice (C lib via Zig `@cImport` vs
  pure-Zig) in a later ADR if it proves load-bearing.
- Revisit a Rust hot-path module only if profiling on bare metal shows Zig is not the bottleneck (it
  is not expected to be).
