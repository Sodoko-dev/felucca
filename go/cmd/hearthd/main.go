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
	"github.com/alpham/infra-saas/hearth/internal/store"
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
	if err := httpSrv.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
}
