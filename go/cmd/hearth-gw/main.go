// hearth-gw — the sandbox ingress gateway (v4 P3). Deployable anywhere that
// can reach the workers (typically beside hearthd, over the wg overlay for
// NAT'd nodes): wildcard DNS *.<domain> points here, and Host headers of the
// form <name>--<sandbox-id>.<domain> proxy to the exposed guest service.
//
// TLS: terminate a wildcard cert (ACME DNS-01) in front of or inside this
// process — the in-binary autocert path needs the real domain and lands with
// the mixed-fleet acceptance (P2.6-style external infra). The lab runs plain
// HTTP with explicit Host headers.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/gateway"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	listen := flag.String("listen", envOr("HEARTH_GW_LISTEN", "0.0.0.0:8088"), "address to serve ingress on")
	hearthd := flag.String("hearthd", envOr("HEARTH_GW_HEARTHD", "http://127.0.0.1:8080"), "hearthd base URL (route table source)")
	token := flag.String("token", envOr("HEARTH_TOKEN", ""), "hearthd admin token (routes are admin-only)")
	domain := flag.String("domain", envOr("HEARTH_GW_DOMAIN", ""), "wildcard ingress zone, e.g. sb.example.com")
	refresh := flag.Uint("refresh", 5, "route table refresh interval (seconds)")
	limitsPath := flag.String("tenant-limits", envOr("HEARTH_GW_TENANT_LIMITS", ""), "optional JSON file: {\"<tenant-id>\":{\"enabled\":true,\"rps\":10,\"auto_wake\":true}}")
	autoWake := flag.Bool("auto-wake", envOr("HEARTH_GW_AUTO_WAKE", "1") != "0", "wake sleeping sandboxes on request instead of serving the 503 page")
	flag.Parse()

	if *domain == "" {
		slog.Error("hearth-gw: --domain is required (the wildcard zone ingress hostnames live under)")
		os.Exit(1)
	}
	if *token == "" {
		slog.Error("hearth-gw: --token is required (the route table is admin-only)")
		os.Exit(1)
	}

	limits := map[string]gateway.TenantLimit{}
	if *limitsPath != "" {
		data, err := os.ReadFile(*limitsPath)
		if err != nil {
			slog.Error("hearth-gw: tenant limits", "err", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &limits); err != nil {
			slog.Error("hearth-gw: tenant limits", "path", *limitsPath, "err", err)
			os.Exit(1)
		}
	}

	gw := gateway.New(gateway.Config{
		Domain:       *domain,
		HearthdURL:   *hearthd,
		Token:        *token,
		Refresh:      time.Duration(*refresh) * time.Second,
		TenantLimits: limits,
		AutoWake:     *autoWake,
	})

	stop := make(chan struct{})
	go gw.Run(stop)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: WebSocket/longpolling connections are long-lived.
	}
	slog.Info("hearth-gw listening", "addr", *listen, "domain", *domain, "hearthd", *hearthd)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
