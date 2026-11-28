package store_test

// FLU-5 store tests: writes that belong together are one transaction, unsigned
// values cannot make a save fail, and reads do not wait for a writer.

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// openAt opens a store at a known path, so a test can reach the same file
// through a raw handle as well.
func openAt(t *testing.T) (*store.SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// rawExec runs statements on a separate, plain handle on the same file — how a
// test plants a trigger that makes one insert fail inside a transaction.
func rawExec(t *testing.T, path string, stmts ...string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	for _, s := range stmts {
		if _, err := raw.Exec(s); err != nil {
			t.Fatalf("raw %q: %v", s, err)
		}
	}
}

func abortInsertsOn(t *testing.T, path, table string) {
	rawExec(t, path, `CREATE TRIGGER boom_`+table+` BEFORE INSERT ON `+table+` BEGIN SELECT RAISE(ABORT, 'injected'); END`)
}

func allowInsertsOn(t *testing.T, path, table string) {
	rawExec(t, path, `DROP TRIGGER boom_`+table)
}

func runningSnapshot(sbState model.SandboxState) *state.Snapshot {
	return &state.Snapshot{
		Seq: 1,
		Sandboxes: []model.Sandbox{{
			ID: "sb-1", Name: "one", Namespace: "default", State: sbState, VCPUs: 1, MemMiB: 256, CreatedAt: 1,
		}},
	}
}

// The snapshot and the usage events describing it are ONE transaction: an
// event that fails to insert takes the state change with it, and the previous
// snapshot stays.
func TestSaveSnapshotWritesStateAndUsageInOneTransaction(t *testing.T) {
	db, path := openAt(t)
	if err := db.SaveSnapshot(runningSnapshot(model.StateRunning), []store.UsageEvent{
		{SandboxID: "sb-1", Event: "created", TS: 1},
	}); err != nil {
		t.Fatalf("first save: %v", err)
	}

	abortInsertsOn(t, path, "usage_events")
	err := db.SaveSnapshot(runningSnapshot(model.StateSleeping), []store.UsageEvent{
		{SandboxID: "sb-1", Event: "slept", TS: 2},
	})
	if err == nil {
		t.Fatal("save with a failing usage insert reported success")
	}
	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Sandboxes) != 1 || got.Sandboxes[0].State != model.StateRunning {
		t.Fatalf("state after a failed save: %+v, want the previous snapshot (running) — the state change landed without its event", got.Sandboxes)
	}
	if evs := eventNames(t, db, ""); len(evs) != 1 || evs[0] != "created" {
		t.Fatalf("events after a failed save: %v, want [created]", evs)
	}

	allowInsertsOn(t, path, "usage_events")
	if err := db.SaveSnapshot(runningSnapshot(model.StateSleeping), []store.UsageEvent{
		{SandboxID: "sb-1", Event: "slept", TS: 2},
	}); err != nil {
		t.Fatalf("save after the trigger is gone: %v", err)
	}
	got = state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Sandboxes[0].State != model.StateSleeping {
		t.Errorf("state: %q, want sleeping", got.Sandboxes[0].State)
	}
	if evs := eventNames(t, db, ""); len(evs) != 2 || evs[1] != "slept" {
		t.Errorf("events: %v, want [created slept]", evs)
	}
}

func eventNames(t *testing.T, db *store.SQLite, tenant string) []string {
	t.Helper()
	evs, err := db.ListUsage(tenant, 0, math.MaxInt32)
	if err != nil {
		t.Fatalf("list usage: %v", err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Event)
	}
	return out
}

// database/sql refuses a uint64 with the high bit set. Every unsigned 64-bit
// column is clamped when bound, so no value in memory can make a save fail.
func TestUnsignedValuesAreClampedWhenBound(t *testing.T) {
	db := open(t)
	huge := uint64(1) << 63
	snap := &state.Snapshot{
		Nodes: []model.Node{{ID: "node-1", Hostname: "h", Addr: "1.2.3.4:9090", MemTotalMiB: huge, MemFreeMiB: math.MaxUint64}},
		Sandboxes: []model.Sandbox{{
			ID: "sb-1", Name: "one", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: huge, CreatedAt: 1,
		}},
	}
	if err := db.SaveSnapshot(snap, []store.UsageEvent{{SandboxID: "sb-1", Event: "created", MemMiB: huge, TS: 1}}); err != nil {
		t.Fatalf("save with 2^63 values: %v", err)
	}
	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatalf("load: %v", err)
	}
	if n := got.Nodes[0]; n.MemTotalMiB != math.MaxInt64 || n.MemFreeMiB != math.MaxInt64 {
		t.Errorf("node mem: total=%d free=%d, want both clamped to MaxInt64", n.MemTotalMiB, n.MemFreeMiB)
	}
	if got.Sandboxes[0].MemMiB != math.MaxInt64 {
		t.Errorf("sandbox mem: %d, want MaxInt64", got.Sandboxes[0].MemMiB)
	}
	if err := db.AppendUsage(store.UsageEvent{SandboxID: "sb-1", Event: "exec", MemMiB: huge, TS: 2}); err != nil {
		t.Errorf("append usage with 2^63: %v", err)
	}
	if err := db.AppendUsageBatch([]store.UsageEvent{{SandboxID: "sb-1", Event: "exec", MemMiB: huge, TS: 3}}); err != nil {
		t.Errorf("append usage batch with 2^63: %v", err)
	}
	if err := db.CreateTemplate(&store.Template{ID: "tpl-1", Name: "t", Image: "i", MemMiB: huge}); err != nil {
		t.Errorf("create template with 2^63: %v", err)
	}
}

// A tenant never exists without the key it was created for.
func TestCreateTenantWithKeyIsAtomic(t *testing.T) {
	db, path := openAt(t)
	tn := &store.Tenant{ID: "tn-1", Name: "acme", CreatedAt: 1}
	key := &store.APIKey{ID: "key-1", TenantID: "tn-1", KeyHash: "h1", Prefix: "p", CreatedAt: 1}

	abortInsertsOn(t, path, "api_keys")
	if err := db.CreateTenantWithKey(tn, key); err == nil {
		t.Fatal("tenant+key with a failing key insert reported success")
	}
	if got, err := db.GetTenant("tn-1"); err != nil || got != nil {
		t.Fatalf("tenant after a failed key insert: %+v err %v, want none — the name is taken by a tenant nobody holds a key for", got, err)
	}

	allowInsertsOn(t, path, "api_keys")
	if err := db.CreateTenantWithKey(tn, key); err != nil {
		t.Fatalf("tenant+key: %v", err)
	}
	if id, err := db.LookupKeyByHash("h1"); err != nil || id != "tn-1" {
		t.Errorf("key lookup: %q err %v, want tn-1", id, err)
	}
}

// A join token is consumed exactly when the credential it buys is stored.
func TestEnrollNodeCredIsAtomic(t *testing.T) {
	db, path := openAt(t)
	if err := db.CreateJoinToken(&store.JoinToken{ID: "jt-1", TokenHash: "tok", CreatedAt: 100}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	cred := &store.NodeCred{Host: "10.0.0.2:9090", Token: "felucca_nt_x", CreatedAt: 100}

	// A token that is not redeemable buys nothing.
	if ok, err := db.EnrollNodeCred(cred, "unknown", 100); err != nil || ok {
		t.Fatalf("enroll with an unknown token: ok=%v err=%v, want false, nil", ok, err)
	}
	if c, _ := db.GetNodeCred(cred.Host); c != nil {
		t.Fatalf("credential written for an unknown token: %+v", c)
	}

	// The credential write fails: the token is not burned.
	abortInsertsOn(t, path, "node_creds")
	if ok, err := db.EnrollNodeCred(cred, "tok", 100); err == nil || ok {
		t.Fatalf("enroll with a failing credential write: ok=%v err=%v, want an error", ok, err)
	}
	if ok, err := db.CheckJoinToken("tok", 101); err != nil || !ok {
		t.Fatalf("token after a failed credential write: redeemable=%v err=%v, want still redeemable", ok, err)
	}

	allowInsertsOn(t, path, "node_creds")
	if ok, err := db.EnrollNodeCred(cred, "tok", 100); err != nil || !ok {
		t.Fatalf("enroll: ok=%v err=%v", ok, err)
	}
	if c, err := db.GetNodeCred(cred.Host); err != nil || c == nil || c.Token != "felucca_nt_x" {
		t.Errorf("credential: %+v err %v", c, err)
	}
	if ok, _ := db.CheckJoinToken("tok", 101); ok {
		t.Error("token still redeemable after a successful enrollment")
	}
}

// Queued exec rows are written all or none.
func TestAppendUsageBatchIsAllOrNothing(t *testing.T) {
	db, path := openAt(t)
	batch := []store.UsageEvent{{SandboxID: "a", Event: "exec", TS: 1}, {SandboxID: "b", Event: "exec", TS: 2}}

	rawExec(t, path, `CREATE TRIGGER boom_b BEFORE INSERT ON usage_events WHEN NEW.sandbox_id = 'b' BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	if err := db.AppendUsageBatch(batch); err == nil {
		t.Fatal("batch with a failing row reported success")
	}
	if evs := eventNames(t, db, ""); len(evs) != 0 {
		t.Fatalf("rows after a failed batch: %v, want none", evs)
	}
	rawExec(t, path, `DROP TRIGGER boom_b`)
	if err := db.AppendUsageBatch(batch); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if evs := eventNames(t, db, ""); len(evs) != 2 {
		t.Errorf("rows: %v, want 2", evs)
	}
}

// Reads go through their own handle: while another process holds the write
// lock (and a save waits on it), key lookups and listings still answer.
func TestReadsProceedWhileAnotherHandleHoldsTheWriteLock(t *testing.T) {
	db, path := openAt(t)
	if err := db.CreateTenantWithKey(&store.Tenant{ID: "tn-1", Name: "acme", CreatedAt: 1},
		&store.APIKey{ID: "key-1", TenantID: "tn-1", KeyHash: "h1", Prefix: "p", CreatedAt: 1}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A third handle, standing in for another process, takes the write lock.
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	defer other.Close()
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}

	saved := make(chan error, 1)
	go func() { saved <- db.SaveSnapshot(runningSnapshot(model.StateRunning), nil) }()
	time.Sleep(50 * time.Millisecond) // let the save reach the busy wait

	start := time.Now()
	if id, err := db.LookupKeyByHash("h1"); err != nil || id != "tn-1" {
		t.Fatalf("key lookup under a held write lock: %q err %v", id, err)
	}
	if ts, err := db.ListTenants(); err != nil || len(ts) != 1 {
		t.Fatalf("list tenants under a held write lock: %v err %v", ts, err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("reads took %v while another handle held the write lock, want well under 500ms", took)
	}
	select {
	case err := <-saved:
		t.Fatalf("the save returned (%v) before the lock was released: the reads above did not run against a held lock", err)
	default:
	}

	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if err := <-saved; err != nil {
		t.Errorf("save after the lock was released: %v", err)
	}
}
