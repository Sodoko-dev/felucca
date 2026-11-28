// Tests for the review findings on FLU-5 (the persistence change): what an
// outage of the store does to requests that were already in flight, to the
// sweep, to the exec rows, and to the recovery itself.
package server

import (
	"errors"
	"math"
	"net/http/httptest"
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

// failStore makes the store fail with one save (the loop keeps failing too).
func (p *persistFixture) failStore(t *testing.T) {
	t.Helper()
	p.fs.failAll()
	if err := p.srv.saveState(); err == nil {
		t.Fatal("save did not fail")
	}
}

// ---- PS-1: an expose that waited behind a failed one ----

// Two identical exposes: the first one's save is held, then fails. The second
// was admitted while the store was healthy and waited on exposeMu; it must not
// answer 200 from the row the first left in memory, which the database does
// not have.
func TestIdenticalExposeBehindAFailedSaveIsRefused(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour
	id := p.create(t, `{"name":"a"}`)
	body := `{"name":"web","port":8080}`

	release := make(chan struct{})
	held := p.fs.holdNextSave(release)
	p.fs.failAfter(0, 1)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- p.do("POST", "/api/v1/sandboxes/"+id+"/expose", body) }()
	<-held
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- p.do("POST", "/api/v1/sandboxes/"+id+"/expose", body) }()
	time.Sleep(100 * time.Millisecond) // past the gate, waiting on exposeMu
	close(release)
	wantApplied(t, <-first, "first expose")
	wantStoreUnavailable(t, <-second, "identical expose that waited behind the failed one")
	if sb := p.savedSandbox(t, id); len(sb.Exposes) != 0 {
		t.Errorf("database exposes: %+v, want none", sb.Exposes)
	}
}

// The same for unexpose: the second must not answer from memory either.
func TestIdenticalUnexposeBehindAFailedSaveIsRefused(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour
	id := p.create(t, `{"name":"a"}`)
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"web","port":8080}`); w.Code != 201 {
		t.Fatalf("expose: %d %s", w.Code, w.Body)
	}

	release := make(chan struct{})
	held := p.fs.holdNextSave(release)
	p.fs.failAfter(0, 1)
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- p.do("DELETE", "/api/v1/sandboxes/"+id+"/expose/web", "") }()
	<-held
	second := make(chan *httptest.ResponseRecorder, 1)
	go func() { second <- p.do("DELETE", "/api/v1/sandboxes/"+id+"/expose/web", "") }()
	time.Sleep(100 * time.Millisecond)
	close(release)
	wantApplied(t, <-first, "first unexpose")
	wantStoreUnavailable(t, <-second, "identical unexpose that waited behind the failed one")
}

// ---- PS-3: the exec exemption is the exec route, nothing that ends in /exec ----

func TestGateRefusesTheDeleteOfAnExposeNamedExec(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"exec","port":8080}`); w.Code != 201 {
		t.Fatalf("expose named exec: %d %s", w.Code, w.Body)
	}
	p.failStore(t)
	n := p.fw.count()
	wantStoreUnavailable(t, p.do("DELETE", "/api/v1/sandboxes/"+id+"/expose/exec", ""), "DELETE .../expose/exec while failing")
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("the refused unexpose reached the worker: %v", calls)
	}
	if sb := p.mem(id); len(sb.Exposes) != 1 {
		t.Errorf("memory exposes after the refusal: %+v, want the row kept", sb.Exposes)
	}
	// Exec itself is still served.
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/exec", `{"cmd":["true"]}`); w.Code != 200 {
		t.Errorf("exec while failing: %d %s, want 200", w.Code, w.Body)
	}
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"POST", "/api/v1/sandboxes/sb-1/exec", false},
		{"DELETE", "/api/v1/sandboxes/sb-1/expose/exec", true},
		{"DELETE", "/api/v1/sandboxes/sb-1/exec", true},
		{"PUT", "/api/v1/sandboxes/sb-1/exec", true},
		{"POST", "/api/v1/sandboxes/sb-1/x/exec", true},
	} {
		if got := changesDurableState(c.method, c.path); got != c.want {
			t.Errorf("changesDurableState(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// ---- PS-4: the sweep stops at the first failed save ----

func TestSweepStopsAtTheFirstFailedSave(t *testing.T) {
	t.Run("auto-sleep", func(t *testing.T) {
		p := newPersistFixture(t)
		now := time.Now().Unix()
		for i := 0; i < 2; i++ {
			id := p.create(t, `{"name":"a"}`)
			func() {
				p.srv.st.Lock()
				defer p.srv.st.Unlock()
				sb := p.srv.st.FindSandbox(id)
				sb.IdleSleepS = 5
				sb.LastActivity = now - 60
			}()
		}
		p.fs.failAll()
		n := p.fw.count()
		p.srv.lifecycleSweep(now)
		if calls := p.fw.since(n); len(calls) != 1 {
			t.Errorf("worker calls: %v, want one sleep: the second sandbox was put to sleep after the first save had failed", calls)
		}
	})
	t.Run("auto-delete", func(t *testing.T) {
		p := newPersistFixture(t)
		now := time.Now().Unix()
		for i := 0; i < 2; i++ {
			id := p.create(t, `{"name":"a"}`)
			if w := p.do("POST", "/api/v1/sandboxes/"+id+"/sleep", ""); w.Code != 200 {
				t.Fatalf("sleep: %d", w.Code)
			}
			func() {
				p.srv.st.Lock()
				defer p.srv.st.Unlock()
				sb := p.srv.st.FindSandbox(id)
				sb.AsleepDeleteS = 30
				sb.SleptAt = now - 60
			}()
		}
		p.fs.failAll()
		n := p.fw.count()
		p.srv.lifecycleSweep(now)
		if calls := p.fw.since(n); len(calls) != 1 {
			t.Errorf("worker calls: %v, want one delete: the second sandbox was deleted after the first save had failed", calls)
		}
	})
}

// ---- PS-5: a constraint error is a conflict, not an outage ----

func TestConstraintErrorIsAConflictNotAnOutage(t *testing.T) {
	p := newPersistFixture(t)
	images := filepath.Join(t.TempDir(), "images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}
	p.srv.cfg.ImagesDir = images
	if err := os.WriteFile(filepath.Join(images, "base.ext4"), []byte("rootfs"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two concurrent creates of one template name: both passed the existence
	// check, the other one's insert landed first.
	p.fs.mu.Lock()
	p.fs.templateRace = true
	p.fs.mu.Unlock()
	if w := p.do("POST", "/api/v1/templates", `{"name":"t1","image_file":"base"}`); w.Code != 409 || w.Body.String() != `{"error":"template exists"}` {
		t.Errorf("template create that lost the race: %d %s, want 409 template exists", w.Code, w.Body)
	}
	if p.srv.storeFailing() {
		t.Error("a constraint error marked the store failing")
	}

	// A tenant name taken (the tenant handler used to match the message text).
	if w := p.do("POST", "/api/v1/tenants", `{"name":"acme"}`); w.Code != 201 {
		t.Fatalf("tenant: %d %s", w.Code, w.Body)
	}
	if w := p.do("POST", "/api/v1/tenants", `{"name":"acme"}`); w.Code != 409 || w.Body.String() != `{"error":"tenant name exists"}` {
		t.Errorf("second tenant of one name: %d %s, want 409 tenant name exists", w.Code, w.Body)
	}
	if p.srv.storeFailing() {
		t.Error("a constraint error marked the store failing")
	}
	if w := p.do("GET", "/healthz", ""); w.Code != 200 {
		t.Errorf("healthz after two conflicts: %d, want 200", w.Code)
	}
}

// ---- PS-6: the last save of a create, against a save of another request ----

// Forced interleaving: the create's worker has answered; before the create's
// own save, another save (B) runs; then the create's save fails. B must not
// have stored the create's running row or its "created" event: the caller is
// told the create failed, and a restart before recovery would otherwise bring
// back a running sandbox with an open usage interval.
func TestLastSaveOfACreateCannotBeStoredByAnotherSave(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour // nothing but B writes before the checks
	p.fs.failAfter(2, 1)            // the create's first save, B, then the create's last save fails
	p.fw.set(func(f *fsWorker) { f.onCreate = func() { p.srv.saveMu.Lock() } })

	answer := make(chan *httptest.ResponseRecorder, 1)
	go func() { answer <- p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`) }()
	waitFor(t, "the worker's create", 3*time.Second, func() bool { return hasPrefixCall(p.fw.snapshot(), "POST /v1/vms") })
	// The create has its answer and is on its way to its save, which waits on
	// saveMu (held since the worker answered). Give it the time to change
	// whatever it changes before its save.
	time.Sleep(100 * time.Millisecond)
	// B: another request's save, with saveMu held as save holds it.
	if err := p.srv.saveLocked(nil, nil); err != nil {
		p.srv.saveMu.Unlock()
		t.Fatalf("save B: %v", err)
	}
	p.srv.saveMu.Unlock()
	wantStoreUnavailable(t, <-answer, "create whose last save failed")

	for _, sb := range p.saved(t).Sandboxes {
		if sb.State == model.StateRunning || sb.IP != nil {
			t.Errorf("database holds the failed create's running row: %+v", sb)
		}
	}
	evs, err := p.sqlite.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Event == "created" {
			t.Errorf("database holds the failed create's %q event for %s", e.Event, e.SandboxID)
		}
	}
}

// ---- PS-7: a failed exec flush marks the store failing ----

func TestFailedExecFlushMarksTheStoreFailingUntilBothWritesSucceed(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour // the attempts below are this test's
	id := p.create(t, `{"name":"a"}`)
	p.srv.queueExecUsage(store.UsageEvent{SandboxID: id, Event: "exec", TS: 1})
	p.fs.mu.Lock()
	p.fs.batchErr = true
	p.fs.mu.Unlock()
	if err := p.srv.flushExecUsage(); err == nil {
		t.Fatal("flush against a failing store reported success")
	}
	if !p.srv.storeFailing() {
		t.Fatal("a failed exec flush did not mark the store failing")
	}
	if w := p.do("GET", "/healthz", ""); w.Code != 503 {
		t.Errorf("healthz after a failed flush: %d, want 503", w.Code)
	}
	n := p.fw.count()
	wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes/"+id+"/sleep", ""), "sleep after a failed flush")
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("the refused sleep reached the worker: %v", calls)
	}

	// The snapshot alone is not a recovery.
	if err := p.srv.recoverStore(); err == nil {
		t.Fatal("recovery attempt with the flush still failing reported success")
	}
	if !p.srv.storeFailing() {
		t.Error("store healthy although the exec rows are still not written")
	}
	p.fs.mu.Lock()
	p.fs.batchErr = false
	p.fs.mu.Unlock()
	if err := p.srv.recoverStore(); err != nil {
		t.Fatalf("recovery attempt: %v", err)
	}
	if p.srv.storeFailing() {
		t.Error("store still failing after both writes succeeded")
	}
	evs, _ := p.sqlite.ListUsage("", 0, math.MaxInt32)
	found := false
	for _, e := range evs {
		found = found || (e.SandboxID == id && e.Event == "exec")
	}
	if !found {
		t.Error("the exec row was not written by the recovery")
	}
}

// ---- PS-8: one failure per attempt ----

func TestUsageWriteFailuresCountAttemptsNotRows(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour
	id := p.create(t, `{"name":"a"}`)
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		sb := p.srv.st.FindSandbox(id)
		for i := 0; i < 3; i++ {
			p.srv.queueUsageLocked(sb, "paused")
		}
	}()
	before := usageWriteFailures.Load()
	p.fs.failAll()
	if err := p.srv.saveState(); err == nil {
		t.Fatal("save did not fail")
	}
	if got := usageWriteFailures.Load() - before; got != 1 {
		t.Errorf("a failed snapshot save carrying 3 lifecycle events counted %d, want 1", got)
	}
	// A request's save while failing is not an attempt.
	if err := p.srv.saveState(); !errors.Is(err, errStoreFailing) {
		t.Fatalf("save while failing: %v, want errStoreFailing", err)
	}
	if err := p.srv.recoverStore(); err == nil {
		t.Fatal("recovery attempt did not fail")
	}
	if got := usageWriteFailures.Load() - before; got != 2 {
		t.Errorf("after a refused save and a failed retry: %d, want 2", got)
	}
	// A failed save that carries no usage rows counts nothing.
	p.fs.heal()
	if err := p.srv.recoverStore(); err != nil {
		t.Fatal(err)
	}
	p.fs.failAfter(0, 1)
	if err := p.srv.saveState(); err == nil {
		t.Fatal("save did not fail")
	}
	if got := usageWriteFailures.Load() - before; got != 2 {
		t.Errorf("a failed save without usage rows was counted: %d, want 2", got)
	}
}

// ---- PS-9 / P16: a request's save while failing does not wait ----

func TestRequestSaveWhileFailingDoesNotAskTheDatabase(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour
	p.failStore(t)
	calls := p.fs.calls()

	mutated, undone := false, false
	err := p.srv.save(func() { mutated = true }, func() { undone = true })
	if !errors.Is(err, errStoreFailing) {
		t.Fatalf("save while failing: %v, want errStoreFailing", err)
	}
	if !mutated || !undone {
		t.Errorf("refused save: mutate ran %v, undo ran %v, want both (as for a failed write)", mutated, undone)
	}
	// A plain registration is answered 200 and does not even try.
	if w := p.do("POST", "/api/v1/agents/register", `{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`); w.Code != 200 {
		t.Errorf("register while failing: %d %s", w.Code, w.Body)
	}
	if got := p.fs.calls(); got != calls {
		t.Errorf("snapshot writes while failing: %d, want none — only the recovery loop retries", got-calls)
	}
	// The loop's attempt does ask, and writes the registration.
	p.fs.heal()
	if err := p.srv.recoverStore(); err != nil {
		t.Fatal(err)
	}
	if l := len(p.saved(t).Nodes); l != 2 {
		t.Errorf("saved nodes after recovery: %d, want 2", l)
	}
}

// ---- P5' (D-15a): the clean-up after a record its owner moved on ----

// The create's last save failed and its caller was told so; meanwhile its
// owner started the record (answered 200). When the worker delete succeeds, the
// record is set back to error with a "stopped" event, not left running with
// no VM.
func TestCleanupSetsARecordMovedOnBackToError(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	n := p.fw.count()
	p.srv.removeUnsavedVM(p.srv.st.Nodes[0].Addr, id, "req-test")
	if calls := p.fw.since(n); len(calls) != 1 || calls[0] != "DELETE /v1/vms/"+id {
		t.Fatalf("worker calls: %v, want the delete", calls)
	}
	sb := p.mem(id)
	if sb == nil || sb.State != model.StateError {
		t.Fatalf("memory after the clean-up: %+v, want the record in error", sb)
	}
	if saved := p.savedSandbox(t, id); saved == nil || saved.State != model.StateError {
		t.Errorf("database after the clean-up: %+v, want error", saved)
	}
	if evs := p.events(t, id); !equalStrings(evs, []string{"created", "stopped"}) {
		t.Errorf("events: %v, want [created stopped]", evs)
	}
}

// ---- P5' : a fork the worker refuses as busy ----

func TestForkOfABusyParentRemovesTheChildAndSaysBusy(t *testing.T) {
	p := newPersistFixture(t)
	parent := p.create(t, `{"name":"parent"}`)
	p.fw.set(func(f *fsWorker) { f.forkBusy = true })
	w := p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`)
	if w.Code != 409 || w.Body.String() != `{"error":"busy"}` || w.Header().Get("Retry-After") != "2" {
		t.Errorf("fork of a busy parent: %d %s Retry-After=%q, want 409 busy with Retry-After 2", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if l := len(p.srv.st.Snapshot().Sandboxes); l != 1 {
		t.Errorf("memory holds %d sandboxes, want only the parent", l)
	}
	if l := len(p.saved(t).Sandboxes); l != 1 {
		t.Errorf("database holds %d sandboxes, want only the parent", l)
	}
	// Any other refusal keeps the child in error and answers 502, as before.
	p.fw.set(func(f *fsWorker) { f.forkBusy, f.failFork = false, true })
	if w := p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`); w.Code != 502 {
		t.Errorf("fork the worker failed: %d %s, want 502", w.Code, w.Body)
	}
}

// ---- 17: a fork refused at its first save gives the parent its clocks back ----

func TestForkRefusedAtItsFirstSaveRestoresTheParentsClocks(t *testing.T) {
	p := newPersistFixture(t)
	parent := p.create(t, `{"name":"parent"}`)
	if w := p.do("POST", "/api/v1/sandboxes/"+parent+"/sleep", ""); w.Code != 200 {
		t.Fatalf("sleep: %d", w.Code)
	}
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		sb := p.srv.st.FindSandbox(parent)
		sb.LastActivity, sb.SleptAt = 1000, 2000
	}()
	p.fs.failAll()
	wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`), "fork, first save failing")
	if sb := p.mem(parent); sb.LastActivity != 1000 || sb.SleptAt != 2000 {
		t.Errorf("parent clocks after a fork that never happened: last_activity %d slept_at %d, want 1000 and 2000", sb.LastActivity, sb.SleptAt)
	}
}

// ---- PS-2: a database that was read-only when it was opened ----

// With a REAL file: SQLite opens a file it cannot write read-only, and the
// writer pool keeps that connection. The recovery loop must open a fresh one,
// or the store stays failing after the file is writable, until a restart.
func TestRecoveryLoopReopensAWriterThatWasOpenedReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0444 file is writable for root, so the database cannot be made read-only here")
	}
	tmp := t.TempDir()
	path := filepath.Join(tmp, "felucca.db")
	db, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(state.New().Snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenSQLite(path)
	if err != nil {
		t.Fatalf("open a read-only database file: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// OpenSQLite sets the file to 0600; the connection it opened is read-only
	// already. Keep the file read-only until the operator's fix below.
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Token: "admin-tok", UIDir: tmp, StatePath: filepath.Join(tmp, "state.json"), DBPath: path}
	srv := New(cfg, state.New(), db)
	srv.retryInterval = 20 * time.Millisecond
	t.Cleanup(srv.StopBackground)
	srv.st.RegisterNode("worker-1", "http://127.0.0.1:1", 4, 8192, time.Now().Unix())
	if err := srv.saveState(); err == nil {
		t.Fatal("save to a read-only database succeeded: the test did not make it read-only")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the recovery loop to write once the file is writable", 3*time.Second, func() bool { return !srv.storeFailing() })
	got := state.New()
	if err := db.LoadInto(got); err != nil || len(got.Nodes) != 1 {
		t.Errorf("database after recovery: %d nodes, err %v; want the registered node", len(got.Nodes), err)
	}
}

// ---- PS-15: what was not covered ----

// A row-level write that fails marks the store failing, and the recovery loop
// heals it (the loop's snapshot save writes whatever memory holds).
func TestRowLevelWriteFailureMarksTheStoreFailingAndRecovers(t *testing.T) {
	p := newPersistFixture(t)
	tenantID, _ := mintTenant(t, p.h, "acme")
	p.fs.mu.Lock()
	p.fs.createKeyErr = true
	p.fs.mu.Unlock()
	if w := p.do("POST", "/api/v1/tenants/"+tenantID+"/keys", ""); w.Code != 500 {
		t.Errorf("key create whose insert failed: %d %s, want 500", w.Code, w.Body)
	}
	if !p.srv.storeFailing() {
		t.Fatal("a failed row-level write did not mark the store failing")
	}
	wantStoreUnavailable(t, p.do("POST", "/api/v1/tenants/"+tenantID+"/keys", ""), "key create while failing")
	p.fs.mu.Lock()
	p.fs.createKeyErr = false
	p.fs.mu.Unlock()
	waitFor(t, "the recovery loop", 3*time.Second, func() bool { return !p.srv.storeFailing() })
	if w := p.do("POST", "/api/v1/tenants/"+tenantID+"/keys", ""); w.Code != 201 {
		t.Errorf("key create after recovery: %d %s", w.Code, w.Body)
	}
}

// The gate for a tenant key and for a node credential.
func TestStoreGateForTenantAndNodePrincipals(t *testing.T) {
	p := newPersistFixture(t)
	tenantID, key := mintTenant(t, p.h, "acme")
	own := p.create(t, `{"name":"own"}`)
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		p.srv.st.FindSandbox(own).TenantID = tenantID
	}()
	host, port := nodeHostOf(t, p.srv)
	const nodeTok = "felucca_nt_gate"
	if err := putNodeCred(p.srv, host, port, nodeTok, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	node := p.srv.st.Snapshot().Nodes[0]
	jt := mintJoinToken(t, p.srv)
	p.failStore(t)

	tenant := "Bearer " + key
	n := p.fw.count()
	wantStoreUnavailable(t, doRequest(p.h, "POST", "/api/v1/sandboxes", `{"name":"b"}`, tenant), "tenant create while failing")
	// A sleep shows the gate itself: past it, the worker would be called and
	// the answer would be "applied but not saved".
	wantStoreUnavailable(t, doRequest(p.h, "POST", "/api/v1/sandboxes/"+own+"/sleep", "", tenant), "tenant sleep while failing")
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("a tenant's refused requests reached the worker: %v", calls)
	}
	if w := doRequest(p.h, "GET", "/api/v1/sandboxes", "", tenant); w.Code != 200 {
		t.Errorf("tenant list while failing: %d", w.Code)
	}

	nodeAuth := "Bearer " + nodeTok
	if w := doRequest(p.h, "POST", "/api/v1/agents/heartbeat", `{"id":"`+node.ID+`","mem_free_mib":100}`, nodeAuth); w.Code != 200 {
		t.Errorf("node heartbeat while failing: %d %s", w.Code, w.Body)
	}
	reg := `{"hostname":"` + node.Hostname + `","addr":"` + node.Addr + `","cpus":4,"mem_total_mib":8192`
	if w := doRequest(p.h, "POST", "/api/v1/agents/register", reg+`}`, nodeAuth); w.Code != 200 {
		t.Errorf("node re-registration while failing: %d %s", w.Code, w.Body)
	}
	// A node presenting a join token gets the store's 503 before the 403 it
	// would get otherwise (listed as not changing).
	wantStoreUnavailable(t, doRequest(p.h, "POST", "/api/v1/agents/register", reg+`,"join_token":"`+jt+`"}`, nodeAuth), "node spending a join token while failing")
}

// UsageFlushLoop flushes once more when it is stopped: a row queued just
// before the stop is written although no tick came.
func TestUsageFlushLoopFlushesAtTheStop(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.queueExecUsage(store.UsageEvent{SandboxID: "sb-final", Event: "exec", TS: 1})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.srv.UsageFlushLoop(stop); close(done) }()
	close(stop) // before the first tick (execUsageFlushInterval)
	<-done
	evs, err := p.sqlite.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].SandboxID != "sb-final" {
		t.Errorf("rows after the stop: %+v, want the queued exec row", evs)
	}
}

func TestMetricsSayTheStoreIsHealthy(t *testing.T) {
	p := newPersistFixture(t)
	w := p.do("GET", "/metrics", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "felucca_store_healthy 1\n") {
		t.Errorf("/metrics with a healthy store: %d, want felucca_store_healthy 1", w.Code)
	}
}
