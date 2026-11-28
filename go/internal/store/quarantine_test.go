package store_test

// FLU-6: the quarantined node ports are saved with the snapshot, in its
// transaction, and the table is created on a database that predates it.

import (
	"reflect"
	"testing"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

func exposedSnapshot(exposes []model.Expose, q []model.ReleasedPort) *state.Snapshot {
	return &state.Snapshot{
		Seq: 1,
		Sandboxes: []model.Sandbox{{
			ID: "sb-1", Name: "one", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 256, CreatedAt: 1,
			Exposes: exposes,
		}},
		Quarantine: q,
	}
}

func load(t *testing.T, db *store.SQLite) *state.State {
	t.Helper()
	st := state.New()
	if err := db.LoadInto(st); err != nil {
		t.Fatalf("load: %v", err)
	}
	return st
}

// "Route row removed" and "its port quarantined" are one transaction: a save
// that fails writes neither, the next successful one writes both.
func TestQuarantineIsSavedInTheSnapshotTransaction(t *testing.T) {
	db, path := openAt(t)
	row := []model.Expose{{Name: "web", GuestPort: 80, NodePort: 20001}}
	if err := db.SaveSnapshot(exposedSnapshot(row, nil), nil); err != nil {
		t.Fatalf("first save: %v", err)
	}
	q := []model.ReleasedPort{
		{Host: "10.0.0.1", Port: 20001, SandboxID: "sb-1", GuestPort: 80, TenantID: "tn-a", ReleasedAt: 1234},
		{Host: "10.0.0.1", Port: 20001, SandboxID: "sb-0", GuestPort: 81, TenantID: "", ReleasedAt: 1000},
	}

	abortInsertsOn(t, path, "released_ports")
	if err := db.SaveSnapshot(exposedSnapshot(nil, q), nil); err == nil {
		t.Fatal("save with a failing quarantine insert reported success")
	}
	got := load(t, db)
	if len(got.Sandboxes[0].Exposes) != 1 || len(got.Quarantine) != 0 {
		t.Fatalf("after a failed save: rows %+v quarantine %+v, want the row and no entry", got.Sandboxes[0].Exposes, got.Quarantine)
	}

	allowInsertsOn(t, path, "released_ports")
	if err := db.SaveSnapshot(exposedSnapshot(nil, q), nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	got = load(t, db)
	if len(got.Sandboxes[0].Exposes) != 0 {
		t.Errorf("rows after the save: %+v, want none", got.Sandboxes[0].Exposes)
	}
	if !reflect.DeepEqual(got.Quarantine, q) {
		t.Errorf("quarantine after reload: %+v, want %+v", got.Quarantine, q)
	}

	// The table is rewritten with the snapshot, not appended to.
	if err := db.SaveSnapshot(exposedSnapshot(nil, q[:1]), nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := load(t, db); len(got.Quarantine) != 1 {
		t.Errorf("quarantine after a save with one entry: %+v", got.Quarantine)
	}
}

// A database written before the table existed gets it on open.
func TestReleasedPortsTableIsCreatedOnAnExistingDatabase(t *testing.T) {
	db, path := openAt(t)
	if err := db.SaveSnapshot(exposedSnapshot(nil, nil), nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	db.Close()
	rawExec(t, path, `DROP TABLE released_ports`)

	db2, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	q := []model.ReleasedPort{{Host: "h", Port: 20001, SandboxID: "sb-1", GuestPort: 80, ReleasedAt: 1}}
	if err := db2.SaveSnapshot(exposedSnapshot(nil, q), nil); err != nil {
		t.Fatalf("save on the migrated database: %v", err)
	}
	if got := load(t, db2); len(got.Sandboxes) != 1 || !reflect.DeepEqual(got.Quarantine, q) {
		t.Errorf("after reopen: sandboxes %d quarantine %+v", len(got.Sandboxes), got.Quarantine)
	}
}
