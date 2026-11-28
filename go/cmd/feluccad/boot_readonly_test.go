package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// boot against a database FILE that is really read-only (not a fake): it
// starts with the store failing, serves reads, and recovers without a restart
// once the file is writable.
func TestBootOnAReadOnlyDatabaseFileStartsFailingAndRecovers(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0444 file is writable for root, so the database cannot be made read-only here")
	}
	tmp := t.TempDir()
	cfg := &config.Config{Token: "admin-tok", UIDir: tmp, StatePath: filepath.Join(tmp, "state.json"), DBPath: filepath.Join(tmp, "felucca.db")}
	seed, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{ID: "sb-1", Name: "a", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 256})
	if err := seed.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	seed.Close()
	if err := os.Chmod(cfg.DBPath, 0o444); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open the read-only file: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := os.Chmod(cfg.DBPath, 0o444); err != nil { // OpenSQLite set 0600
		t.Fatal(err)
	}

	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot on a read-only database: %v — it must start, failing", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })
	if srv.StoreHealthy() {
		t.Fatal("store healthy after a start-up write to a read-only file")
	}
	h := srv.Handler()
	if w := req(h, "GET", "/api/v1/sandboxes", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "sb-1") {
		t.Errorf("read while failing: %d %s", w.Code, w.Body)
	}
	if w := req(h, "POST", "/api/v1/sandboxes", `{"name":"b"}`); w.Code != 503 {
		t.Errorf("create while failing: %d, want 503", w.Code)
	}
	if err := os.Chmod(cfg.DBPath, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for !srv.StoreHealthy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !srv.StoreHealthy() {
		t.Fatal("store did not recover after the file became writable")
	}
}
