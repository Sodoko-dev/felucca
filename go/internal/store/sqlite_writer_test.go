package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/alpham/infra-saas/felucca/internal/state"
)

// A file that was read-only when the writer opened it: SQLite opened it
// read-only without an error, and the one writer connection stays read-only
// after the file is made writable. ResetWriter makes the next write open a
// fresh connection.
func TestResetWriterRecoversAConnectionOpenedReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0444 file is writable for root, so the database cannot be made read-only here")
	}
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSnapshot(state.New().Snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("open a read-only file: %v", err)
	}
	defer s.Close()
	if err := os.Chmod(path, 0o444); err != nil { // OpenSQLite set 0600
		t.Fatal(err)
	}
	if err := s.MarkStarted(1); err == nil {
		t.Fatal("write to a read-only file succeeded: the test did not make it read-only")
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// The connection is still the read-only one: this is the outage that
	// lasted until a restart.
	if err := s.MarkStarted(2); err == nil {
		t.Error("the writer recovered without ResetWriter: the connection was not the read-only one this test is about")
	}
	s.ResetWriter()
	if err := s.MarkStarted(3); err != nil {
		t.Fatalf("write after ResetWriter on a writable file: %v", err)
	}
}

func TestMarkStartedWritesOneMetaRowAndNoSnapshot(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, now := range []int64{10, 20} {
		if err := s.MarkStarted(now); err != nil {
			t.Fatalf("mark started: %v", err)
		}
	}
	var v string
	if err := s.ro.QueryRow(`SELECT v FROM meta WHERE k='started_at'`).Scan(&v); err != nil || v != "20" {
		t.Errorf("started_at: %q err %v, want 20", v, err)
	}
	if empty, err := s.Empty(); err != nil || !empty {
		t.Errorf("Empty after MarkStarted: %v err %v, want true (no snapshot was written)", empty, err)
	}
}

func TestIsConstraintClassifiesByResultCode(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tn := &Tenant{ID: "tn-1", Name: "acme", CreatedAt: 1}
	if err := s.CreateTenant(tn); err != nil {
		t.Fatal(err)
	}
	dup := &Tenant{ID: "tn-2", Name: "acme", CreatedAt: 2}
	err = s.CreateTenant(dup)
	if err == nil {
		t.Fatal("a second tenant of one name was accepted")
	}
	if !IsConstraint(err) {
		t.Errorf("UNIQUE violation not classified as a constraint: %v", err)
	}
	if IsConstraint(errors.New("UNIQUE constraint failed: tenants.name")) {
		t.Error("a plain error whose text says UNIQUE was classified as a constraint: classify by code, not text")
	}
	if IsConstraint(nil) {
		t.Error("nil classified as a constraint")
	}
}

func TestOpenSQLiteRefusesAnInMemoryDatabase(t *testing.T) {
	for _, p := range []string{":memory:", "file::memory:?cache=shared", "file:x?mode=memory"} {
		if s, err := OpenSQLite(p); err == nil {
			s.Close()
			t.Errorf("OpenSQLite(%q) succeeded: its writer and read handle would be two different databases", p)
		}
	}
}
