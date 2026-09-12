// feluccad — Felucca control plane (Go port).
// Listens on the configured bind address, exposes the REST API, schedules
// sandboxes onto agents, serves the static UI, and persists in-memory state to
// a JSON file.
package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/server"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
	"github.com/alpham/infra-saas/felucca/internal/wg"
)

// maxHeaderBytes caps the request head (request line + all headers) on every
// listener. Go's default is 1 MiB, which is three orders of magnitude more than
// any request feluccad serves needs and is per-request attacker-controlled work:
// X-Forwarded-For is parsed on the auth path, so a ~1 MiB header was a ~1 MiB
// allocation an unauthenticated client could ask for at will. 16 KiB leaves
// generous room for a bearer, a trace id and a proxy chain.
const maxHeaderBytes = 16 << 10

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// A bind feluccad cannot honour literally is fatal: the listener must be
	// the one the operator wrote, never a wildcard feluccad substituted for a
	// host it failed to parse.
	if err := cfg.ValidateBind(); err != nil {
		slog.Error("bind", "err", err)
		os.Exit(1)
	}

	// Malformed trusted-proxy CIDRs are fatal for the same reason: the
	// operator believes forwarded client addresses are honoured, and silently
	// dropping the entry changes who the throttle counts.
	if err := cfg.ValidateTrustedProxies(); err != nil {
		slog.Error("trusted-proxies", "err", err)
		os.Exit(1)
	}

	// The admin API is remote root on every worker in the fleet, so refuse to
	// listen without a usable token — same reasoning as the wg-overlay guard
	// below, applied to every deployment. Open mode exists only when the
	// operator names it.
	if err := cfg.ValidateAuth(); err != nil {
		slog.Error("auth", "err", err)
		os.Exit(1)
	}
	if cfg.InsecureNoAuth {
		slog.Warn("INSECURE MODE: --insecure-no-auth is set, the admin API accepts every caller unauthenticated — loopback binds and lab use only",
			"bind", cfg.Bind, "addr", cfg.ListenAddr())
		// Worth saying twice: off-box reachability turns the lab escape hatch
		// into an open control plane, which is remote root on every worker.
		if host, _, err := net.SplitHostPort(cfg.ListenAddr()); err == nil {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				slog.Warn("INSECURE MODE on a non-loopback address: every host that can route here has full admin access", "addr", cfg.ListenAddr())
			}
		}
	}

	// A bind that cannot be reached at feluccad's overlay address takes the
	// whole enrolled fleet offline without a single error on this side: agents
	// just get connection refused. Cheap to say here, expensive to find later.
	if problem := cfg.OverlayBindProblem(); problem != "" {
		slog.Warn("OVERLAY UNREACHABLE: "+problem, "bind", cfg.Bind, "addr", cfg.ListenAddr(), "wg_ip", cfg.WgIP)
	}

	// Ensure the db directory exists (best effort).
	if err := state.EnsureStateDir(cfg.DBPath); err != nil {
		slog.Warn("could not create db dir", "err", err)
	}

	// A pre-rename install keeps its database under the old product name.
	// Opening a fresh felucca.db beside it would look like a clean boot while
	// every tenant, sandbox and node silently vanished — and a regenerated
	// wg.key would strand every enrolled worker. Refuse and say what to move.
	if _, statErr := os.Stat(cfg.DBPath); os.IsNotExist(statErr) {
		if legacy := legacyDBPath(cfg.DBPath); legacy != "" {
			slog.Error("pre-rename database found and the configured one is absent: refusing to start with empty state",
				"legacy", legacy, "expected", cfg.DBPath,
				"fix", "move the state directory as described in docs/CHANGELOG.md (Rename: Hearth → Felucca)")
			os.Exit(1)
		}
	}

	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	st := state.New()
	empty, err := db.Empty()
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	if empty {
		// One-time migration: import the legacy JSON state if present.
		if _, statErr := os.Stat(cfg.StatePath); statErr == nil {
			if err := st.Load(cfg.StatePath); err != nil {
				slog.Warn("could not import legacy state", "path", cfg.StatePath, "err", err)
			} else if err := db.SaveSnapshot(st); err != nil {
				slog.Warn("could not save imported state", "err", err)
			} else {
				if err := os.Rename(cfg.StatePath, cfg.StatePath+".imported"); err != nil {
					slog.Warn("could not rename legacy state file", "err", err)
				}
				slog.Info("migrated legacy state", "from", cfg.StatePath, "to", cfg.DBPath)
			}
		} else if err := db.SaveSnapshot(st); err != nil {
			slog.Warn("could not initialize store", "err", err)
		}
	} else if err := db.LoadInto(st); err != nil {
		// Fatal, not a warning. LoadInto appends as it scans and does not
		// clear what it already appended, so a failed load leaves a partially
		// populated working set — and the first mutation calls persist(), whose
		// SaveSnapshot deletes every row before reinserting the ones in memory.
		// Serving from a short load therefore destroys the rest of the database
		// on the next write. Refusing to start is recoverable by an operator;
		// a truncated sandboxes table is not.
		slog.Error("could not load state: refusing to start rather than serve a partial working set the first write would make permanent",
			"path", cfg.DBPath, "err", err)
		os.Exit(1)
	}

	srv := server.New(cfg, st, db)

	// WireGuard overlay (v4 P2): bring up wg-felucca and re-add enrolled peers.
	// Empty wg_ip means the overlay is disabled (single-network deployments).
	if cfg.WgIP != "" {
		// Fail fast on config the join flow depends on: a worker that joins
		// against a misconfigured hub burns operator time (and, pre-fix,
		// tokens). The overlay also must never run in open mode — minting
		// join tokens would be unauthenticated kernel network config.
		if cfg.Token == "" {
			slog.Error("wg overlay requires an auth token (--token): refusing open-mode overlay")
			os.Exit(1)
		}
		if cfg.WgEndpoint == "" {
			slog.Error("wg overlay requires --wg-endpoint (public host:port workers dial)")
			os.Exit(1)
		}
		if _, _, err := net.ParseCIDR(cfg.WgIP); err != nil {
			slog.Error("wg overlay: bad --wg-ip", "ip", cfg.WgIP, "err", err)
			os.Exit(1)
		}
		pub, err := wg.EnsureKey(cfg.WgKeyPath)
		if err != nil {
			slog.Error("wg key", "err", err)
			os.Exit(1)
		}
		if err := wg.EnsureInterface(cfg.WgIP, cfg.WgPort, cfg.WgKeyPath); err != nil {
			slog.Error("wg interface", "err", err)
			os.Exit(1)
		}
		peers, err := db.ListWgPeers()
		if err != nil {
			slog.Error("wg peers", "err", err)
			os.Exit(1)
		}
		batch := make([]wg.Peer, 0, len(peers))
		for _, p := range peers {
			batch = append(batch, wg.Peer{PubKey: p.PubKey, OverlayIP: p.OverlayIP})
		}
		if err := wg.AddPeers(batch); err != nil {
			// Peers stay persisted; joins re-install live ones. Warn, don't die.
			slog.Warn("wg re-add peers", "count", len(batch), "err", err)
		}
		srv.SetWgPubKey(pub)
		slog.Info("wg overlay up", "ip", cfg.WgIP, "iface", wg.InterfaceName, "port", cfg.WgPort, "peers", len(peers))
	}

	// Lifecycle sweep (v4 P5.2): idle auto-sleep, asleep-TTL auto-delete,
	// hourly usage-event retention. Runs for the process lifetime.
	go srv.LifecycleLoop(make(chan struct{}))

	addr := cfg.ListenAddr()
	slog.Info("feluccad listening", "addr", addr, "ui_dir", cfg.UIDir, "db", cfg.DBPath,
		"auth", map[bool]string{true: "off (--insecure-no-auth)", false: "on"}[cfg.InsecureNoAuth])

	httpSrv := &http.Server{
		Addr:           addr,
		Handler:        srv.Handler(),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}

	// Optional Let's Encrypt TLS (v4 P2.3). Empty tls_domain means plain
	// HTTP only — exactly the pre-TLS behavior.
	if cfg.TLSDomain != "" {
		if err := os.MkdirAll(cfg.TLSCacheDir, 0o700); err != nil {
			slog.Error("tls: could not create autocert cache dir", "path", cfg.TLSCacheDir, "err", err)
			os.Exit(1)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),
			Cache:      autocert.DirCache(cfg.TLSCacheDir),
		}
		slog.Info("tls enabled", "domain", cfg.TLSDomain, "autocert_cache", cfg.TLSCacheDir)

		// :443 — public TLS endpoint, same handler/timeouts as the plain server.
		tlsSrv := &http.Server{
			Addr:           ":443",
			Handler:        srv.Handler(),
			ReadTimeout:    30 * time.Second,
			WriteTimeout:   60 * time.Second,
			MaxHeaderBytes: maxHeaderBytes,
			TLSConfig:      m.TLSConfig(),
		}
		go func() {
			// Cert/key paths empty: certificates come from autocert.
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil {
				slog.Error("tls listen :443", "err", err)
				os.Exit(1)
			}
		}()

		// :80 — ACME HTTP-01 challenges + redirect everything else to https.
		// Same bounds as the other two listeners: this one is public whenever
		// tls_domain is set, and it used to be a bare http.ListenAndServe with
		// Go's 1 MiB header default and no timeouts at all — the one listener
		// the maxHeaderBytes invariant did not actually cover.
		acmeSrv := &http.Server{
			Addr:           ":80",
			Handler:        m.HTTPHandler(nil),
			ReadTimeout:    30 * time.Second,
			WriteTimeout:   60 * time.Second,
			MaxHeaderBytes: maxHeaderBytes,
		}
		go func() {
			if err := acmeSrv.ListenAndServe(); err != nil {
				slog.Error("tls listen :80", "err", err)
				os.Exit(1)
			}
		}()
	}

	// The plain cfg.Port listener stays up UNCONDITIONALLY, even with TLS on:
	// overlay-joined agents speak plain HTTP to feluccad's overlay IP inside the
	// wg tunnel (the tunnel provides the encryption). Removing this listener
	// would cut every enrolled agent off from the control plane.
	if err := httpSrv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}

// legacyDBPath returns where a pre-rename (Hearth) install would have kept the
// database that cfg now expects at db, if a file still exists there; "" when
// the path has no product name in it or nothing is there.
func legacyDBPath(db string) string {
	old := strings.ReplaceAll(db, "felucca", "hearth")
	if old == db {
		return ""
	}
	if _, err := os.Stat(old); err == nil {
		return old
	}
	return ""
}
