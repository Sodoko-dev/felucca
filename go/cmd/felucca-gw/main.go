// felucca-gw — the sandbox ingress gateway (v4 P3). Deployable anywhere that
// can reach the workers (typically beside feluccad, over the wg overlay for
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
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/gateway"
	"github.com/alpham/infra-saas/felucca/internal/ingress"
)

// Listener bounds for the ingress server. This process is the INTERNET-FACING
// tenant ingress: everything below is what an unauthenticated client can make
// felucca-gw spend before it has proved anything. feluccad's three listeners carry
// the same discipline (cmd/feluccad/main.go), and main_test.go here is the
// structural guard, mirroring TestEveryListenerIsBounded there.
//
// Why these and not feluccad's set: felucca-gw is a reverse proxy in front of
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
	// split here. 16 KiB matches feluccad and leaves generous room for cookies and
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

// options are felucca-gw's command-line settings.
type options struct {
	listen, feluccad, token, domain, limitsPath string
	refresh                                     uint
	maxStale                                    time.Duration
	autoWake                                    bool
}

// defineFlags registers the flags on fs. A flag whose value can be a secret
// takes NO default from the environment: flag prints defaults in --help, and
// the admin token would be printed with them. Those are read from the
// environment in resolveEnv, after parsing. --max-stale is resolved there too,
// so that a malformed FELUCCA_GW_MAX_STALE is an error rather than a panic.
func defineFlags(fs *flag.FlagSet) *options {
	o := &options{}
	fs.StringVar(&o.listen, "listen", envOr("FELUCCA_GW_LISTEN", "0.0.0.0:8088"), "address to serve ingress on")
	fs.StringVar(&o.feluccad, "feluccad", envOr("FELUCCA_GW_FELUCCAD", "http://127.0.0.1:8080"), "feluccad base URL (route table source)")
	fs.StringVar(&o.token, "token", "", "feluccad admin token (routes are admin-only); default: $FELUCCA_TOKEN")
	fs.StringVar(&o.domain, "domain", envOr("FELUCCA_GW_DOMAIN", ""), "wildcard ingress zone, e.g. sb.example.com")
	fs.UintVar(&o.refresh, "refresh", 5, "route table refresh interval (seconds, 1 to 60)")
	fs.DurationVar(&o.maxStale, "max-stale", ingress.GatewayMaxStaleCeiling, "oldest route table to serve from; older answers 503 (2 x refresh + 20s to 600s; env FELUCCA_GW_MAX_STALE)")
	fs.StringVar(&o.limitsPath, "tenant-limits", envOr("FELUCCA_GW_TENANT_LIMITS", ""), "optional JSON file: {\"<tenant-id>\":{\"enabled\":true,\"rps\":10,\"auto_wake\":true}}")
	fs.BoolVar(&o.autoWake, "auto-wake", envOr("FELUCCA_GW_AUTO_WAKE", "1") != "0", "wake sleeping sandboxes on request instead of serving the 503 page")
	return o
}

// resolveEnv fills, after parsing, the flags that were not given on the
// command line from the environment (flag beats env; env beats default), and
// checks the timings.
func (o *options) resolveEnv(fs *flag.FlagSet) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if !given["token"] {
		o.token = os.Getenv("FELUCCA_TOKEN")
	}
	if v := os.Getenv("FELUCCA_GW_MAX_STALE"); v != "" && !given["max-stale"] {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("FELUCCA_GW_MAX_STALE: %v", err)
		}
		o.maxStale = d
	}
	return gateway.ValidateTimings(time.Duration(o.refresh)*time.Second, o.maxStale)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	o := defineFlags(flag.CommandLine)
	flag.Parse()
	if err := o.resolveEnv(flag.CommandLine); err != nil {
		slog.Error("felucca-gw: " + err.Error())
		os.Exit(1)
	}

	if o.domain == "" {
		slog.Error("felucca-gw: --domain is required (the wildcard zone ingress hostnames live under)")
		os.Exit(1)
	}
	if o.token == "" {
		slog.Error("felucca-gw: --token is required (the route table is admin-only)")
		os.Exit(1)
	}

	limits := map[string]gateway.TenantLimit{}
	if o.limitsPath != "" {
		data, err := os.ReadFile(o.limitsPath)
		if err != nil {
			slog.Error("felucca-gw: tenant limits", "err", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(data, &limits); err != nil {
			slog.Error("felucca-gw: tenant limits", "path", o.limitsPath, "err", err)
			os.Exit(1)
		}
	}

	gw := gateway.New(gateway.Config{
		Domain:       o.domain,
		FeluccadURL:  o.feluccad,
		Token:        o.token,
		Refresh:      time.Duration(o.refresh) * time.Second,
		MaxStale:     o.maxStale,
		TenantLimits: limits,
		AutoWake:     o.autoWake,
	})

	stop := make(chan struct{})
	go gw.Run(stop)

	srv := &http.Server{
		Addr:              o.listen,
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
	slog.Info("felucca-gw listening", "addr", o.listen, "domain", o.domain, "feluccad", o.feluccad)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
