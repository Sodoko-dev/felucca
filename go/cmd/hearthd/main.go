// hearthd — Hearth control plane (Go port).
// Listens on 0.0.0.0:<port>, exposes the REST API, schedules sandboxes onto
// agents, serves the static UI, and persists in-memory state to a JSON file.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/server"
	"github.com/alpham/infra-saas/hearth/internal/state"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	st := state.New()
	if err := st.Load(cfg.StatePath); err != nil {
		log.Printf("could not load state from %s: %v", cfg.StatePath, err)
	}

	// Ensure the state directory exists (best effort).
	if err := state.EnsureStateDir(cfg.StatePath); err != nil {
		log.Printf("could not create state dir: %v", err)
	}

	srv := server.New(cfg, st)

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	log.Printf("hearthd listening on %s (ui_dir=%s, state=%s, auth=%s)",
		addr, cfg.UIDir, cfg.StatePath,
		map[bool]string{true: "on", false: "off"}[cfg.Token != ""])

	httpSrv := &http.Server{
		Addr:         addr,
		Handler:      srv.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}
	if err := httpSrv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
