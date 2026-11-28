// Tests for what the lab's persistence gate found (F1, F2): a record whose
// create answer was lost learns its address from the worker, and the final
// exec flush survives a locked database.
package server

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// lostAddress makes id look like a record settled to error at start-up whose
// create answer (and with it the address) was lost.
func (p *persistFixture) lostAddress(id string) {
	p.srv.st.Lock()
	defer p.srv.st.Unlock()
	sb := p.srv.st.FindSandbox(id)
	sb.State, sb.IP = model.StateError, nil
}

func TestStartOfARecordWithoutAnAddressTakesItFromTheWorker(t *testing.T) {
	t.Run("the worker lists it", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.lostAddress(id)
		p.fw.set(func(f *fsWorker) {
			f.list = `{"vms":[{"id":"other","ip":"10.231.0.7"},{"id":"` + id + `","ip":"10.231.0.42"}]}`
		})
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/start", ""); w.Code != 200 {
			t.Fatalf("start: %d %s", w.Code, w.Body)
		}
		if sb := p.mem(id); sb.State != model.StateRunning || sb.IP == nil || *sb.IP != "10.231.0.42" {
			t.Fatalf("record after the start: %s ip %v, want running with the worker's address", sb.State, sb.IP)
		}
		if sb := p.savedSandbox(t, id); sb.IP == nil || *sb.IP != "10.231.0.42" {
			t.Errorf("database: ip %v, want the address saved with the state", sb.IP)
		}
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"web","port":8080}`); w.Code != 201 {
			t.Errorf("expose after the address was learnt: %d %s, want 201", w.Code, w.Body)
		}
	})
	t.Run("the worker's list fails", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.lostAddress(id)
		p.fw.set(func(f *fsWorker) { f.listStatus = 500 })
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/start", ""); w.Code != 200 {
			t.Fatalf("start with the list failing: %d %s, want 200", w.Code, w.Body)
		}
		if sb := p.mem(id); sb.State != model.StateRunning || sb.IP != nil {
			t.Errorf("record: %s ip %v, want running, address still unset", sb.State, sb.IP)
		}
	})
	t.Run("a record with an address is not asked about", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/stop", ""); w.Code != 200 {
			t.Fatalf("stop: %d", w.Code)
		}
		n := p.fw.count()
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/start", ""); w.Code != 200 {
			t.Fatalf("start: %d", w.Code)
		}
		if hasPrefixCall(p.fw.since(n), "GET /v1/vms") {
			t.Errorf("worker calls: %v, want no list for a record that has an address", p.fw.since(n))
		}
	})
	t.Run("a refusal that records running learns it too", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		p.lostAddress(id)
		p.fw.set(func(f *fsWorker) {
			f.startRefusal = `{"error":"InvalidState: running","state":"running"}`
			f.list = `{"vms":[{"id":"` + id + `","ip":"10.231.0.44"}]}`
		})
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/start", ""); w.Code != 409 {
			t.Fatalf("start the worker refuses: %d %s, want 409", w.Code, w.Body)
		}
		if sb := p.mem(id); sb.State != model.StateRunning || sb.IP == nil || *sb.IP != "10.231.0.44" {
			t.Errorf("record after the refusal: %s ip %v, want running with the worker's address", sb.State, sb.IP)
		}
	})
	t.Run("a wake learns it too", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		func() {
			p.srv.st.Lock()
			defer p.srv.st.Unlock()
			sb := p.srv.st.FindSandbox(id)
			sb.State, sb.IP = model.StateSleeping, nil
		}()
		p.fw.set(func(f *fsWorker) { f.list = `{"vms":[{"id":"` + id + `","ip":"10.231.0.43"}]}` })
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/wake", ""); w.Code != 200 {
			t.Fatalf("wake: %d %s", w.Code, w.Body)
		}
		if sb := p.mem(id); sb.IP == nil || *sb.IP != "10.231.0.43" {
			t.Errorf("record after the wake: ip %v, want the worker's address", sb.IP)
		}
	})
}

// ---- F2: the final exec flush against a locked database ----

func TestFinalExecFlushIsTriedAgain(t *testing.T) {
	p := newPersistFixture(t)
	for i := 0; i < 3; i++ {
		p.srv.queueExecUsage(store.UsageEvent{SandboxID: "sb-x", Event: "exec", TS: int64(i + 1)})
	}
	p.fs.mu.Lock()
	p.fs.batchFails = 2 // the first two attempts meet a locked database
	p.fs.mu.Unlock()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.srv.UsageFlushLoop(stop); close(done) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the final flush did not end")
	}
	evs, err := p.sqlite.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Errorf("rows after the stop: %d, want the 3 queued rows (written by the third attempt)", len(evs))
	}
}

func TestFinalExecFlushSaysWhatItLost(t *testing.T) {
	p := newPersistFixture(t)
	logs := captureLog(t)
	for i := 0; i < 3; i++ {
		p.srv.queueExecUsage(store.UsageEvent{SandboxID: "sb-x", Event: "exec", TS: int64(i + 1)})
	}
	p.fs.mu.Lock()
	p.fs.batchErr = true
	p.fs.mu.Unlock()
	p.srv.SetFinalFlushDeadline(time.Now().Add(1500 * time.Millisecond))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.srv.UsageFlushLoop(stop); close(done) }()
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the final flush did not end at its deadline")
	}
	l := logs()
	if strings.Count(l, "final exec flush failed; the queued exec usage rows are lost") != 1 || !strings.Contains(l, "rows=3") {
		t.Errorf("want one ERROR naming the 3 lost rows:\n%s", l)
	}
	if !strings.Contains(l, "trying again") {
		t.Errorf("the final flush was not tried again before giving up:\n%s", l)
	}
}
