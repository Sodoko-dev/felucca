// hearthd — Hearth control plane (Go port).
// Listens on 0.0.0.0:<port>, exposes the REST API, schedules sandboxes onto
// agents, serves the static UI, and persists in-memory state to a JSON file.
package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/server"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
	"github.com/alpham/infra-saas/hearth/internal/wg"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}

	// Ensure the db directory exists (best effort).
	if err := state.EnsureStateDir(cfg.DBPath); err != nil {
		slog.Warn("could not create db dir", "err", err)
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
		slog.Warn("could not load state", "path", cfg.DBPath, "err", err)
	}

	srv := server.New(cfg, st, db)

	// WireGuard overlay (v4 P2): bring up wg-hearth and re-add enrolled peers.
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

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	slog.Info("hearthd listening", "addr", addr, "ui_dir", cfg.UIDir, "db", cfg.DBPath,
		"auth", map[bool]string{true: "on", false: "off"}[cfg.Token != ""])

	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
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
			Addr:         ":443",
			Handler:      srv.Handler(),
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
			TLSConfig:    m.TLSConfig(),
		}
		go func() {
			// Cert/key paths empty: certificates come from autocert.
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil {
				slog.Error("tls listen :443", "err", err)
				os.Exit(1)
			}
		}()

		// :80 — ACME HTTP-01 challenges + redirect everything else to https.
		go func() {
			if err := http.ListenAndServe(":80", m.HTTPHandler(nil)); err != nil {
				slog.Error("tls listen :80", "err", err)
				os.Exit(1)
			}
		}()
	}

	// The plain cfg.Port listener stays up UNCONDITIONALLY, even with TLS on:
	// overlay-joined agents speak plain HTTP to hearthd's overlay IP inside the
	// wg tunnel (the tunnel provides the encryption). Removing this listener
	// would cut every enrolled agent off from the control plane.
	if err := httpSrv.ListenAndServe(); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
