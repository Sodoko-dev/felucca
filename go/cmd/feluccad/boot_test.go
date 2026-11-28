package main

// Start-up (FLU-5 P12): what boot — the part of main that loads the working set
// and makes the start-up save — refuses, and what it starts through.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// unwritable fails every snapshot save until healed.
type unwritable struct {
	store.Store
	mu     sync.Mutex
	healed bool
}

func (u *unwritable) SaveSnapshot(snap *state.Snapshot, usage []store.UsageEvent) error {
	u.mu.Lock()
	healed := u.healed
	u.mu.Unlock()
	if !healed {
		return errors.New("injected: attempt to write a readonly database")
	}
	return u.Store.SaveSnapshot(snap, usage)
}

func bootFixture(t *testing.T) (*config.Config, *store.SQLite) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
	}
	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return cfg, db
}

func req(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer admin-tok")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A database that cannot be written does not stop feluccad: it starts with the
// store failing, serves reads, refuses changes, and recovers by itself.
func TestBootWithAnUnwritableStoreStartsFailing(t *testing.T) {
	cfg, db := bootFixture(t)
	st := state.New()
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{ID: "sb-1", Name: "a", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 256})
	if err := db.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := &unwritable{Store: db}

	srv, err := boot(cfg, u)
	if err != nil {
		t.Fatalf("boot with an unwritable store: %v — it must start, failing", err)
	}
	if srv.StoreHealthy() {
		t.Fatal("store reported healthy after a failed start-up save")
	}
	h := srv.Handler()
	if w := req(h, "GET", "/api/v1/sandboxes", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "sb-1") {
		t.Errorf("read while failing: %d %s", w.Code, w.Body)
	}
	if w := req(h, "GET", "/api/v1/routes", ""); w.Code != 200 {
		t.Errorf("route table while failing: %d", w.Code)
	}
	if w := req(h, "POST", "/api/v1/sandboxes", `{"name":"b"}`); w.Code != 503 {
		t.Errorf("create while failing: %d, want 503", w.Code)
	}
	if w := req(h, "GET", "/healthz", ""); w.Code != 503 {
		t.Errorf("healthz while failing: %d, want 503", w.Code)
	}

	// The recovery loop heals it without a request.
	u.mu.Lock()
	u.healed = true
	u.mu.Unlock()
	deadline := time.Now().Add(6 * time.Second)
	for !srv.StoreHealthy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !srv.StoreHealthy() {
		t.Fatal("store did not recover after the database became writable")
	}
}

// A legacy state file that is present but does not load ends start-up: the
// alternative made the empty state permanent at the first change.
func TestBootRefusesAnUnloadableLegacyStateFile(t *testing.T) {
	cfg, db := bootFixture(t)
	if err := os.WriteFile(cfg.StatePath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := boot(cfg, db); err == nil {
		t.Fatal("boot accepted an unloadable legacy state file")
	}
	if empty, _ := db.Empty(); !empty {
		t.Error("the database was initialized although the legacy import failed")
	}
}

// The legacy file is renamed only after its import was saved.
func TestBootRenamesTheLegacyFileOnlyAfterItsImportIsSaved(t *testing.T) {
	cfg, db := bootFixture(t)
	legacy := state.New()
	legacy.Sandboxes = append(legacy.Sandboxes, &model.Sandbox{ID: "sb-old", Name: "old", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 256})
	if err := legacy.Persist(cfg.StatePath); err != nil {
		t.Fatal(err)
	}

	u := &unwritable{Store: db}
	if _, err := boot(cfg, u); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if _, err := os.Stat(cfg.StatePath); err != nil {
		t.Fatalf("legacy file renamed although its import was not saved: %v", err)
	}
	// Let that server's recovery loop finish before the next boot.
	u.mu.Lock()
	u.healed = true
	u.mu.Unlock()

	cfg2, db2 := bootFixture(t)
	if err := legacy.Persist(cfg2.StatePath); err != nil {
		t.Fatal(err)
	}
	if _, err := boot(cfg2, db2); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if _, err := os.Stat(cfg2.StatePath + ".imported"); err != nil {
		t.Errorf("legacy file not renamed after a saved import: %v", err)
	}
	got := state.New()
	if err := db2.LoadInto(got); err != nil || got.FindSandbox("sb-old") == nil {
		t.Errorf("imported state not in the database: err %v", err)
	}
}
