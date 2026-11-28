package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// Both handles carry their pragmas on EVERY connection — including one the pool
// opens later — because they are set in the connection string; and the read
// handle refuses writes.
func TestReadHandleIsReadOnlyAndCarriesThePragmas(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// No idle connections: every query below runs on a freshly opened one,
	// which is exactly the connection a pragma Exec'd once would not reach.
	s.db.SetMaxIdleConns(0)
	s.ro.SetMaxIdleConns(0)

	type check struct {
		pragma string
		want   string
	}
	checks := []check{
		{"synchronous", "2"}, // FULL
		{"busy_timeout", "5000"},
		{"foreign_keys", "1"},
		{"journal_mode", "wal"},
	}
	for _, c := range checks {
		var got string
		if err := s.db.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil || got != c.want {
			t.Errorf("writer %s = %q (err %v), want %q", c.pragma, got, err, c.want)
		}
		if err := s.ro.QueryRow("PRAGMA " + c.pragma).Scan(&got); err != nil || got != c.want {
			t.Errorf("reader %s = %q (err %v), want %q", c.pragma, got, err, c.want)
		}
	}
	var qo string
	if err := s.ro.QueryRow("PRAGMA query_only").Scan(&qo); err != nil || qo != "1" {
		t.Errorf("reader query_only = %q (err %v), want 1", qo, err)
	}

	if _, err := s.ro.Exec(`INSERT INTO meta (k, v) VALUES ('x', 'y')`); err == nil {
		t.Error("the read handle accepted a write")
	} else if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("read handle refused the write with an unexpected error: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO meta (k, v) VALUES ('x', 'y')`); err != nil {
		t.Errorf("the writer refused a write: %v", err)
	}
	var v string
	if err := s.ro.QueryRow(`SELECT v FROM meta WHERE k='x'`).Scan(&v); err != nil || v != "y" {
		t.Errorf("reader does not see the writer's commit: %q err %v", v, err)
	}
}
