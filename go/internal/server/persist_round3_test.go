// Tests for the third review of the save path: what feluccad answers when the
// worker refuses, verbs on a record still being created, a registration while
// the store is failing, the read-error and dropped-row counters, and the
// recovery attempt's hold on saveMu.
package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// ---- B1: a fork the worker refuses for good ----

func TestForkRefusedForGoodRemovesTheChildAndRecordsTheParent(t *testing.T) {
	fork := func(p *persistFixture, parent string) (int, string, string) {
		w := p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`)
		return w.Code, w.Body.String(), w.Header().Get("Retry-After")
	}
	t.Run("a state that differs is recorded", func(t *testing.T) {
		p := newPersistFixture(t)
		parent := p.create(t, `{"name":"parent"}`)
		refusal := `{"error":"InvalidState: stopped (a stopped sandbox cannot be forked)","state":"stopped"}`
		p.fw.set(func(f *fsWorker) { f.forkRefusal = refusal })
		code, body, retry := fork(p, parent)
		if code != 409 || body != refusal || retry != "" {
			t.Errorf("fork refused for good: %d %s Retry-After=%q, want 409 with the worker's body and no Retry-After", code, body, retry)
		}
		if l := len(p.srv.st.Snapshot().Sandboxes); l != 1 {
			t.Errorf("memory holds %d sandboxes, want only the parent (the reserved child removed)", l)
		}
		if l := len(p.saved(t).Sandboxes); l != 1 {
			t.Errorf("database holds %d sandboxes, want only the parent", l)
		}
		if sb := p.mem(parent); sb.State != model.StateStopped {
			t.Errorf("parent: %s, want stopped (the state the worker holds)", sb.State)
		}
		if evs := p.events(t, parent); !equalStrings(evs, []string{"created", "stopped"}) {
			t.Errorf("parent events: %v, want [created stopped]", evs)
		}
	})
	t.Run("the same state changes nothing", func(t *testing.T) {
		p := newPersistFixture(t)
		parent := p.create(t, `{"name":"parent"}`)
		refusal := `{"error":"InvalidState: forked before this release (runs on its parent's image)","state":"running"}`
		p.fw.set(func(f *fsWorker) { f.forkRefusal = refusal })
		if code, body, _ := fork(p, parent); code != 409 || body != refusal {
			t.Errorf("fork refused for good: %d %s, want 409 relayed", code, body)
		}
		if l := len(p.srv.st.Snapshot().Sandboxes); l != 1 {
			t.Errorf("memory holds %d sandboxes, want only the parent", l)
		}
		if sb := p.mem(parent); sb.State != model.StateRunning {
			t.Errorf("parent: %s, want running", sb.State)
		}
		if evs := p.events(t, parent); !equalStrings(evs, []string{"created"}) {
			t.Errorf("parent events: %v, want [created]", evs)
		}
	})
	t.Run("the removal's save fails", func(t *testing.T) {
		p := newPersistFixture(t)
		parent := p.create(t, `{"name":"parent"}`)
		refusal := `{"error":"InvalidState: running","state":"running"}`
		p.fw.set(func(f *fsWorker) { f.forkRefusal = refusal })
		p.fs.failAfter(1, 1) // the child's record saved, its removal's save fails
		if code, body, _ := fork(p, parent); code != 409 || body != refusal {
			t.Errorf("fork refused, removal not saved: %d %s, want the 409 unchanged", code, body)
		}
		waitFor(t, "the recovery loop", 3*time.Second, func() bool { return !p.srv.storeFailing() })
		if l := len(p.saved(t).Sandboxes); l != 1 {
			t.Errorf("database after the recovery: %d sandboxes, want only the parent", l)
		}
	})
	t.Run("AlreadyExists keeps its handling", func(t *testing.T) {
		p := newPersistFixture(t)
		parent := p.create(t, `{"name":"parent"}`)
		p.fw.set(func(f *fsWorker) { f.forkRefusal = `{"error":"AlreadyExists"}` })
		if code, _, _ := fork(p, parent); code != 502 {
			t.Errorf("fork answered AlreadyExists: %d, want 502", code)
		}
		var child *model.Sandbox
		for _, sb := range p.srv.st.Snapshot().Sandboxes {
			if sb.ID != parent {
				c := sb
				child = &c
			}
		}
		if child == nil || child.State != model.StateError {
			t.Errorf("child after AlreadyExists: %+v, want kept in error", child)
		}
	})
}

// GF-13: a fork answered busy, or refused for good, did not happen: the
// parent gets back the clocks it bumped.
func TestForkRefusedByTheWorkerRestoresTheParentsClocks(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(f *fsWorker)
	}{
		{"busy", func(f *fsWorker) { f.forkBusy = true }},
		{"refused for good", func(f *fsWorker) { f.forkRefusal = `{"error":"InvalidState: sleeping","state":"sleeping"}` }},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			p.fw.set(c.set)
			if w := p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`); w.Code != 409 {
				t.Fatalf("fork: %d %s, want 409", w.Code, w.Body)
			}
			if sb := p.mem(parent); sb.LastActivity != 1000 || sb.SleptAt != 2000 {
				t.Errorf("parent clocks after a fork the worker refused: last_activity %d slept_at %d, want 1000 and 2000", sb.LastActivity, sb.SleptAt)
			}
		})
	}
}

// ---- B2: a delete the worker refuses for now ----

func TestDeleteRefusedByTheWorkerRecordsItsStateAndRelays(t *testing.T) {
	gaveUp := `{"error":"busy: the VM is stopped, but an operation in progress has not let go of it yet; try again","state":"stopped"}`
	t.Run("the worker names a state", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.fw.set(func(f *fsWorker) { f.deleteRefusal = gaveUp })
		w := p.do("DELETE", "/api/v1/sandboxes/"+id, "")
		if w.Code != 409 || w.Body.String() != gaveUp || w.Header().Get("Retry-After") != "2" {
			t.Errorf("delete refused: %d %s Retry-After=%q, want 409 relayed with Retry-After 2", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
		sb := p.mem(id)
		if sb == nil || sb.State != model.StateStopped {
			t.Fatalf("record after the refusal: %+v, want kept, stopped", sb)
		}
		if saved := p.savedSandbox(t, id); saved == nil || saved.State != model.StateStopped {
			t.Errorf("database: %+v, want the record kept, stopped", saved)
		}
		if evs := p.events(t, id); !equalStrings(evs, []string{"created", "stopped"}) {
			t.Errorf("events: %v, want [created stopped]", evs)
		}
	})
	t.Run("plain busy", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.fw.set(func(f *fsWorker) { f.deleteRefusal = `{"error":"busy"}` })
		w := p.do("DELETE", "/api/v1/sandboxes/"+id, "")
		if w.Code != 409 || w.Body.String() != `{"error":"busy"}` || w.Header().Get("Retry-After") != "2" {
			t.Errorf("delete answered busy: %d %s Retry-After=%q, want 409 busy with Retry-After 2", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
		if sb := p.mem(id); sb == nil || sb.State != model.StateRunning {
			t.Errorf("record: %+v, want kept, running", sb)
		}
	})
	t.Run("force keeps its meaning", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.fw.set(func(f *fsWorker) { f.deleteRefusal = gaveUp })
		if w := p.do("DELETE", "/api/v1/sandboxes/"+id+"?force=1", ""); w.Code != 204 {
			t.Errorf("forced delete past a refusal: %d %s, want 204", w.Code, w.Body)
		}
		if p.mem(id) != nil {
			t.Error("forced delete kept the record")
		}
	})
}

// ---- B3: lifecycle verbs on a record still being created ----

func TestLifecycleVerbsOnACreatingRecordAreRefused(t *testing.T) {
	p := newPersistFixture(t)
	release := make(chan struct{})
	p.fw.set(func(f *fsWorker) { f.onCreate = func() { <-release } })
	created := make(chan int, 1)
	go func() { created <- p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`).Code }()
	waitFor(t, "the worker's create", 3*time.Second, func() bool { return hasPrefixCall(p.fw.snapshot(), "POST /v1/vms") })
	var id string
	for _, sb := range p.srv.st.Snapshot().Sandboxes {
		id = sb.ID
	}
	if sb := p.mem(id); sb == nil || sb.State != model.StateCreating {
		t.Fatalf("record while the worker creates: %+v, want creating", sb)
	}

	n := p.fw.count()
	for _, verb := range []string{"sleep", "wake", "stop", "start", "pause", "resume"} {
		w := p.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
		if w.Code != 409 || w.Body.String() != refuseCreating {
			t.Errorf("%s of a creating record: %d %s, want 409 %s", verb, w.Code, w.Body, refuseCreating)
		}
	}
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("verbs on a creating record reached the worker: %v", calls)
	}
	close(release)
	if code := <-created; code != 201 {
		t.Fatalf("create: %d", code)
	}
	if sb := p.savedSandbox(t, id); sb == nil || sb.State != model.StateRunning {
		t.Errorf("record after the create's last save: %+v, want running", sb)
	}
}

// ---- B4: a registration while the store is failing ----

// The recovery loop is held between its copy and its healthy mark; a
// registration lands in between. It must still reach the database: skipped
// after reading "failing" outside saveMu, it stayed in memory only while the
// store was then reported healthy.
func TestRegistrationWhileFailingIsSavedAfterTheRecovery(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour // the attempt below is this test's
	p.failStore(t)
	p.fs.heal()

	release := make(chan struct{})
	held := p.fs.holdNextSave(release)
	recovered := make(chan error, 1)
	go func() { recovered <- p.srv.recoverStore() }()
	<-held // the loop has its copy, without the registration below
	if w := p.do("POST", "/api/v1/agents/register", `{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`); w.Code != 200 {
		t.Fatalf("register while failing: %d %s", w.Code, w.Body)
	}
	close(release)
	if err := <-recovered; err != nil {
		t.Fatalf("recovery attempt: %v", err)
	}
	if p.srv.storeFailing() {
		t.Fatal("store still failing after the recovery")
	}
	// The registration's save, off the response path (this also ends the
	// recovery loop, which with its hour-long interval would never tick).
	p.srv.stopAndWait()
	if l := len(p.saved(t).Nodes); l != 2 {
		t.Errorf("saved nodes after the recovery: %d, want 2 — the registration is in memory only while the store says healthy", l)
	}
}

// ---- B5, B9: read errors are counted and answered 503 ----

func TestStoreReadErrorsAreCounted(t *testing.T) {
	p := newPersistFixture(t)
	tenantID, key := mintTenant(t, p.h, "acme")
	before := storeReadErrors.Load()
	p.fs.mu.Lock()
	p.fs.keyErr = true
	p.fs.mu.Unlock()
	wantStoreUnavailable(t, doRequest(p.h, "GET", "/api/v1/sandboxes", "", "Bearer "+key), "keyed request with the key lookup failing")
	p.fs.mu.Lock()
	p.fs.keyErr, p.fs.tenantErr = false, true
	p.fs.mu.Unlock()
	wantStoreUnavailable(t, p.do("POST", "/api/v1/tenants/"+tenantID+"/keys", ""), "key create with the tenant read failing")
	if got := storeReadErrors.Load() - before; got != 2 {
		t.Errorf("read errors counted: %d, want 2", got)
	}
	p.fs.mu.Lock()
	p.fs.tenantErr = false
	p.fs.mu.Unlock()
	w := p.do("GET", "/metrics", "")
	if !strings.Contains(w.Body.String(), "\nfelucca_store_read_errors_total ") || !strings.Contains(w.Body.String(), "\nfelucca_usage_rows_dropped_total ") {
		t.Errorf("/metrics lacks felucca_store_read_errors_total or felucca_usage_rows_dropped_total")
	}
	if !strings.Contains(w.Body.String(), "felucca_store_healthy 1\n") {
		t.Error("a read outage marked the write health down")
	}
}

// ---- B8: the recovery attempt's exec flush is outside saveMu ----

// With the exec flush of a recovery attempt held (a locked database), a
// request's save while failing is still refused at once: it waits for saveMu,
// and the flush does not hold it.
func TestRecoveryFlushDoesNotHoldTheSaveMutex(t *testing.T) {
	p := newPersistFixture(t)
	p.srv.retryInterval = time.Hour
	p.failStore(t)
	p.fs.heal()
	p.srv.queueExecUsage(store.UsageEvent{SandboxID: "sb-x", Event: "exec", TS: 1})
	hold, held := make(chan struct{}), make(chan struct{})
	p.fs.mu.Lock()
	p.fs.batchHold, p.fs.batchHeld = hold, held
	p.fs.mu.Unlock()
	recovered := make(chan error, 1)
	go func() { recovered <- p.srv.recoverStore() }()
	<-held
	saved := make(chan error, 1)
	go func() { saved <- p.srv.saveState() }()
	select {
	case err := <-saved:
		if !errors.Is(err, errStoreFailing) {
			t.Errorf("save while failing: %v, want errStoreFailing", err)
		}
	case <-time.After(time.Second):
		t.Error("a request's save waited for the recovery attempt's exec flush")
	}
	close(hold)
	if err := <-recovered; err != nil {
		t.Fatalf("recovery attempt: %v", err)
	}
	if p.srv.storeFailing() {
		t.Error("store still failing after a recovery attempt whose writes both succeeded")
	}
}
