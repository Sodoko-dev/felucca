// hearthd — Hearth control plane (Go port).
// Listens on 0.0.0.0:<port>, exposes the REST API, schedules sandboxes onto
// agents, serves the static UI, and persists in-memory state to a JSON file.
package main

import (
	"fmt"
	"log"
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
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Ensure the db directory exists (best effort).
	if err := state.EnsureStateDir(cfg.DBPath); err != nil {
		log.Printf("could not create db dir: %v", err)
	}

	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	st := state.New()
	empty, err := db.Empty()
	if err != nil {
		fmt.Fprintf(os.Stderr, "store: %v\n", err)
		os.Exit(1)
	}
	if empty {
		// One-time migration: import the legacy JSON state if present.
		if _, statErr := os.Stat(cfg.StatePath); statErr == nil {
			if err := st.Load(cfg.StatePath); err != nil {
				log.Printf("could not import legacy state from %s: %v", cfg.StatePath, err)
			} else if err := db.SaveSnapshot(st); err != nil {
				log.Printf("could not save imported state: %v", err)
			} else {
				if err := os.Rename(cfg.StatePath, cfg.StatePath+".imported"); err != nil {
					log.Printf("could not rename legacy state file: %v", err)
				}
				log.Printf("migrated legacy state %s into %s", cfg.StatePath, cfg.DBPath)
			}
		} else if err := db.SaveSnapshot(st); err != nil {
			log.Printf("could not initialize store: %v", err)
		}
	} else if err := db.LoadInto(st); err != nil {
		log.Printf("could not load state from %s: %v", cfg.DBPath, err)
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
			fmt.Fprintln(os.Stderr, "wg overlay requires an auth token (--token): refusing open-mode overlay")
			os.Exit(1)
		}
		if cfg.WgEndpoint == "" {
			fmt.Fprintln(os.Stderr, "wg overlay requires --wg-endpoint (public host:port workers dial)")
			os.Exit(1)
		}
		if _, _, err := net.ParseCIDR(cfg.WgIP); err != nil {
			fmt.Fprintf(os.Stderr, "wg overlay: bad --wg-ip %q: %v\n", cfg.WgIP, err)
			os.Exit(1)
		}
		pub, err := wg.EnsureKey(cfg.WgKeyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "wg key: %v\n", err)
			os.Exit(1)
		}
		if err := wg.EnsureInterface(cfg.WgIP, cfg.WgPort, cfg.WgKeyPath); err != nil {
			fmt.Fprintf(os.Stderr, "wg interface: %v\n", err)
			os.Exit(1)
		}
		peers, err := db.ListWgPeers()
		if err != nil {
			fmt.Fprintf(os.Stderr, "wg peers: %v\n", err)
			os.Exit(1)
		}
		batch := make([]wg.Peer, 0, len(peers))
		for _, p := range peers {
			batch = append(batch, wg.Peer{PubKey: p.PubKey, OverlayIP: p.OverlayIP})
		}
		if err := wg.AddPeers(batch); err != nil {
			// Peers stay persisted; joins re-install live ones. Warn, don't die.
			log.Printf("wg re-add %d peers: %v", len(batch), err)
		}
		srv.SetWgPubKey(pub)
		log.Printf("wg overlay up: %s on %s port %d (%d peers)",
			cfg.WgIP, wg.InterfaceName, cfg.WgPort, len(peers))
	}

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	log.Printf("hearthd listening on %s (ui_dir=%s, db=%s, auth=%s)",
		addr, cfg.UIDir, cfg.DBPath,
		map[bool]string{true: "on", false: "off"}[cfg.Token != ""])

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
			log.Fatalf("tls: could not create autocert cache dir %s: %v", cfg.TLSCacheDir, err)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),
			Cache:      autocert.DirCache(cfg.TLSCacheDir),
		}
		log.Printf("tls enabled: domain=%s autocert_cache=%s", cfg.TLSDomain, cfg.TLSCacheDir)

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
				log.Fatalf("tls listen :443: %v", err)
			}
		}()

		// :80 — ACME HTTP-01 challenges + redirect everything else to https.
		go func() {
			if err := http.ListenAndServe(":80", m.HTTPHandler(nil)); err != nil {
				log.Fatalf("tls listen :80: %v", err)
			}
		}()
	}

	// The plain cfg.Port listener stays up UNCONDITIONALLY, even with TLS on:
	// overlay-joined agents speak plain HTTP to hearthd's overlay IP inside the
	// wg tunnel (the tunnel provides the encryption). Removing this listener
	// would cut every enrolled agent off from the control plane.
	if err := httpSrv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
