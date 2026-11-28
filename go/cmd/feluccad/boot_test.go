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

// unwritable fails every write of the start-up and of the recovery loop (the
// snapshot save and the start-up's small write) until healed, and counts the
// snapshot saves.
type unwritable struct {
	store.Store
	mu     sync.Mutex
	healed bool
	saves  int
	marks  int
}

func (u *unwritable) SaveSnapshot(snap *state.Snapshot, usage []store.UsageEvent) error {
	u.mu.Lock()
	healed := u.healed
	u.saves++
	u.mu.Unlock()
	if !healed {
		return errors.New("injected: attempt to write a readonly database")
	}
	return u.Store.SaveSnapshot(snap, usage)
}

func (u *unwritable) MarkStarted(now int64) error {
	u.mu.Lock()
	healed := u.healed
	u.marks++
	u.mu.Unlock()
	if !healed {
		return errors.New("injected: attempt to write a readonly database")
	}
	return u.Store.MarkStarted(now)
}

func (u *unwritable) heal() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.healed = true
}

func (u *unwritable) saveCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.saves
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
	t.Cleanup(srv.StopBackground)
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
	u.heal()
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
	srv, err := boot(cfg, u)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(srv.StopBackground)
	if _, err := os.Stat(cfg.StatePath); err != nil {
		t.Fatalf("legacy file renamed although its import was not saved: %v", err)
	}
	// When the recovery loop saves the import, the file is renamed then: left
	// in place, it would be imported again into a recreated database.
	u.heal()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.StatePath + ".imported"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(cfg.StatePath + ".imported"); err != nil {
		t.Errorf("legacy file not renamed after the recovery loop saved its import: %v", err)
	}
	if got := state.New(); db.LoadInto(got) != nil || got.FindSandbox("sb-old") == nil {
		t.Error("the import is not in the database after the recovery")
	}

	cfg2, db2 := bootFixture(t)
	if err := legacy.Persist(cfg2.StatePath); err != nil {
		t.Fatal(err)
	}
	srv2, err := boot(cfg2, db2)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(srv2.StopBackground)
	if _, err := os.Stat(cfg2.StatePath + ".imported"); err != nil {
		t.Errorf("legacy file not renamed after a saved import: %v", err)
	}
	got := state.New()
	if err := db2.LoadInto(got); err != nil || got.FindSandbox("sb-old") == nil {
		t.Errorf("imported state not in the database: err %v", err)
	}
}

// A database that loaded is not rewritten at start-up: the check that it can
// be written is one small row (meta started_at), so whatever the start loaded
// is not made permanent before the first request.
func TestBootDoesNotRewriteTheSnapshotOfALoadedDatabase(t *testing.T) {
	cfg, db := bootFixture(t)
	st := state.New()
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{ID: "sb-1", Name: "a", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 256})
	if err := db.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := &unwritable{Store: db, healed: true}
	srv, err := boot(cfg, u)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(srv.StopBackground)
	if !srv.StoreHealthy() {
		t.Error("store failing after a successful start-up write")
	}
	if n := u.saveCount(); n != 0 {
		t.Errorf("start-up rewrote the snapshot %d time(s), want 0", n)
	}
	u.mu.Lock()
	marks := u.marks
	u.mu.Unlock()
	if marks != 1 {
		t.Errorf("start-up writes of meta started_at: %d, want 1", marks)
	}
}

// unreadable fails the reads boot makes.
type unreadable struct {
	store.Store
	emptyErr, loadErr bool
}

func (u *unreadable) Empty() (bool, error) {
	if u.emptyErr {
		return false, errors.New("injected: disk I/O error")
	}
	return u.Store.Empty()
}

func (u *unreadable) LoadInto(st *state.State) error {
	if u.loadErr {
		return errors.New("injected: disk I/O error")
	}
	return u.Store.LoadInto(st)
}

// A store that cannot be READ ends start-up (P12): serving from a short load
// would destroy the rest of the database at the first save.
func TestBootRefusesAnUnreadableStore(t *testing.T) {
	cfg, db := bootFixture(t)
	if _, err := boot(cfg, &unreadable{Store: db, emptyErr: true}); err == nil {
		t.Error("boot accepted a store whose emptiness could not be read")
	}
	st := state.New()
	if err := db.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := boot(cfg, &unreadable{Store: db, loadErr: true}); err == nil {
		t.Error("boot accepted a store whose working set could not be loaded")
	}
}
