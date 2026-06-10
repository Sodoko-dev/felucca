#!/usr/bin/env bash
# Re-record conformance goldens from a live REFERENCE implementation
# (the Zig v2 stack until the ports land). Same env as run.sh.
RECORD=1 exec bash "$(cd "$(dirname "$0")" && pwd)/run.sh" "$@"
