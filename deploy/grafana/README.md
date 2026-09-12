# Felucca Grafana dashboard (v4 P6)

`felucca-dashboard.json` — import into Grafana (>= 10) and point it at a
Prometheus datasource. Two scrape jobs feed it:

```yaml
scrape_configs:
  # Open fleet metrics: tenant-anonymous only (see ADR-0009 — tenant-labeled
  # series were deliberately kept OFF this unauthenticated endpoint).
  - job_name: felucca
    metrics_path: /metrics
    static_configs: [{ targets: ["feluccad-host:8080"] }]

  # Per-tenant gauges: admin-authenticated surface (v4 P6). 404s without the
  # admin token, like every other admin route.
  - job_name: felucca-tenants
    metrics_path: /api/v1/metrics/tenants
    authorization:
      type: Bearer
      credentials: "<feluccad admin token>"   # or credentials_file
    static_configs: [{ targets: ["feluccad-host:8080"] }]
```

Histogram series (`felucca_{wake,exec,create}_duration_ms_*`) are in-memory
only — they reset on feluccad restart, which is normal Prometheus practice
(`rate()`/`histogram_quantile()` handle counter resets). The legacy
`felucca_wake_ms_{last,sum}`/`felucca_wake_total` counters remain for
back-compat with pre-P6 dashboards.
