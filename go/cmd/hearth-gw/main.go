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

// Listener bounds for the ingress server. This process is the INTERNET-FACING
// tenant ingress: everything below is what an unauthenticated client can make
// hearth-gw spend before it has proved anything. hearthd's three listeners carry
// the same discipline (cmd/hearthd/main.go), and main_test.go here is the
// structural guard, mirroring TestEveryListenerIsBounded there.
//
// Why these and not hearthd's set: hearth-gw is a reverse proxy in front of
// tenant services, and ReverseProxy carries WebSocket upgrades and streaming
// responses natively. A ReadTimeout would kill a long-lived upload or an upgraded
// connection mid-flight, and a WriteTimeout would cut every stream at the
// deadline — so those two stay off, deliberately, and the slow-client bounds are
// carried by the header and idle timeouts instead, which apply only while the
// connection is NOT in the middle of a request.
const (
	// maxHeaderBytes caps the request head (request line + all headers). Go's
	// default is 1 MiB PER CONNECTION, three orders of magnitude past anything a
	// proxied request needs, and it is attacker-controlled work available to any
	// client that can open a socket: every request's Host header is parsed and
	// split here. 16 KiB matches hearthd and leaves generous room for cookies and
	// a proxy chain.
	maxHeaderBytes = 16 << 10

	// readHeaderTimeout bounds header dribbling — the slow-loris case — without
	// touching the body, so it is safe on a streaming proxy.
	readHeaderTimeout = 10 * time.Second

	// idleTimeout bounds an established keep-alive connection that is NOT in the
	// middle of a request. Without it a client can hold connections (and their
	// buffers) open indefinitely at no cost, which is the cheap half of the
	// slow-loris family that ReadHeaderTimeout does not cover. It never applies
	// to an in-flight request, so a long-lived proxied stream is unaffected.
	idleTimeout = 120 * time.Second
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
		MaxHeaderBytes:    maxHeaderBytes,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// ReadTimeout and WriteTimeout are DELIBERATELY unset on this listener,
		// and the zeros are written out rather than omitted so the decision is
		// visible here and the structural guard in main_test.go can insist the
		// choice keeps being made. They are whole-request deadlines: this is a
		// reverse proxy for tenant services, so a ReadTimeout would cut a long
		// upload and a WriteTimeout would cut every WebSocket upgrade, SSE
		// stream and long-poll at the deadline. The slow-client bounds live in
		// ReadHeaderTimeout and IdleTimeout above, neither of which can fire
		// during an in-flight request.
		ReadTimeout:  0,
		WriteTimeout: 0,
	}
	slog.Info("hearth-gw listening", "addr", *listen, "domain", *domain, "hearthd", *hearthd)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
