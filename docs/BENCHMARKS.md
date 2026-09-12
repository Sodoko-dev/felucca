# Felucca benchmarks

Numbers from `scripts/bench.sh` (wall-clock at the API — what a caller
experiences, agent round-trip included). Refreshed per release; a release
with stale numbers here is not done. Methodology: N iterations per op,
p50/p95 over sorted samples, self-cleaning; see the script header.

## Lab fleet (aarch64 Lima/vz, nested virt — NOT bare metal)

> The lab numbers are a lower bound for marketing use: Lima's vz nested
> virtualization taxes VM boot and snapshot I/O heavily. The bare-metal
> datapoint for the comparison table lands with P2.6's real worker.

<!-- BENCH:BEGIN — paste the bench.sh markdown table below this line -->

**Run: 2026-08-09**, lab fleet (2 workers), N=10 per op, git `9f83b59`+P6 tree.

| Operation | p50 (ms) | p95 (ms) | min (ms) | max (ms) | n | fails |
|-----------|----------|----------|----------|----------|---|-------|
| `create-cold` | 170 | 487 | 151 | 487 | 10 | 0 |
| `create-claim` | 73 | 362 | 41 | 362 | 10 | 0 |
| `exec-buffered` | 41 | 66 | 14 | 66 | 10 | 0 |
| `exec-stream-first-frame` | 9 | 54 | 6 | 54 | 10 | 0 |
| `wake (wall-clock)` | 75 | 79 | 74 | 79 | 10 | 0 |
| `wake (api wake_ms)` | 62 | 68 | 61 | 68 | 10 | 0 |
| `fork` | 1122 | 1911 | 996 | 1911 | 10 | 0 |

Notes: `create-claim`'s p95 is a pool miss (pool depth 1 — every other claim
cold-boots); wake wall-clock vs agent-reported `wake_ms` differ by the HTTP
round-trip (~13 ms); `fork`'s ~1.1 s is dominated by the mem.bin copy — the
baseline the uffd CoW research (research/uffd-cow-fork.md) exists to beat.

<!-- BENCH:END -->

Historical reference points (measured earlier, different methodology —
single observations, not percentiles): wake 67–85 ms (v2, Zig and Go+Rust
both); exec round-trip "instant" (<200 ms perceived) on cold and woken
guests (v3.1).
