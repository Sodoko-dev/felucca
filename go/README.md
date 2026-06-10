# hearthd (Go)

Hearth control plane: REST API on `:8080`, scheduler, node registry, JSON state
persistence, static UI serving, Prometheus metrics. Stdlib only, `CGO_ENABLED=0`.

The wire/config contract is `docs/API-V2.md`; `test/conformance/` enforces it.
Port history and behavior guarantees: `docs/adr/ADR-0003-go-rust-port.md`.

## Build & test (inside the infra-saas-lab Lima VM — never on the macOS host)

```sh
REPO=/Users/magdy/projects/github.com/alpham/infra-saas
limactl shell infra-saas-lab -- bash -c \
  "export PATH=\$PATH:/usr/local/go/bin GOCACHE=\$HOME/.cache/go-build && cd $REPO/go && \
   go vet ./... && go test ./... && \
   CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o /tmp/hearthd ./cmd/hearthd"
```

`GOARCH=amd64` cross-compiles the x86_64 production binary from the same box.

## Run

```sh
hearthd --bind 0.0.0.0:8080 --state /var/lib/hearth/state.json \
        --ui-dir /usr/share/hearth/ui --token <bearer-token>
# or HEARTH_BIND / HEARTH_STATE / HEARTH_UI_DIR / HEARTH_TOKEN env vars,
# or --config /etc/hearth/hearthd.json. Precedence: flags > env > config > defaults.
```

## Layout

- `cmd/hearthd` — wiring: config → state load → routes → serve
- `internal/config` — layered config loader (incl. the bind→port quirk)
- `internal/model` — Sandbox/Node JSON shapes (pointer fields = explicit nulls)
- `internal/state` — store, `sb-`/`node-` id generation, atomic persist, adoption-compatible loader
- `internal/server` — routes, bearer middleware (`crypto/subtle`), metrics text, static UI
- `internal/agentclient` — proxy calls to agents
- `internal/sched` — ready node with lowest vm_count
