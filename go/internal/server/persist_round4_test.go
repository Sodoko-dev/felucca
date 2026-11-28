// Tests for the fourth review (fix-go-round4.md): a delete refusal that does
// not end by waiting, a drop that got no answer in time, and a port
// quarantined both for this sandbox and for another.
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
)

// ---- D5: only "busy" is relayed ----

func TestDeleteRefusalThatDoesNotEndByWaitingIsAFailedDelete(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	refusal := `{"error":"InvalidState: firecracker 4242 still holds the instance directory","state":"stopped"}`
	p.fw.set(func(f *fsWorker) { f.deleteRefusal = refusal })
	w := p.do("DELETE", "/api/v1/sandboxes/"+id, "")
	if w.Code != 502 || !strings.HasPrefix(w.Body.String(), `{"error":"agent delete failed`) {
		t.Errorf("delete refused with InvalidState: %d %s, want the 502 agent delete failed", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "4242") || w.Header().Get("Retry-After") != "" {
		t.Errorf("the answer relays the worker's text or a Retry-After: %s Retry-After=%q", w.Body, w.Header().Get("Retry-After"))
	}
	if sb := p.mem(id); sb == nil || sb.State != model.StateStopped {
		t.Errorf("record: %+v, want kept, with the state the worker named (stopped)", sb)
	}
}

// ---- D4: a drop that got no answer in time is repeated ----

// dropWorker answers every unexpose after delay with status, and counts them.
type dropWorker struct {
	mu     sync.Mutex
	delay  time.Duration
	status int
	calls  int
}

func (d *dropWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	delay, status := d.delay, d.status
	if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/expose") {
		d.calls++
	}
	d.mu.Unlock()
	time.Sleep(delay)
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (d *dropWorker) set(delay time.Duration, status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delay, d.status = delay, status
}

func (d *dropWorker) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestDropWithoutAnAnswerIsRepeatedEvenAfterAWake(t *testing.T) {
	old := dropCallTimeout
	dropCallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { dropCallTimeout = old })

	setup := func(t *testing.T) (*qFixture, *dropWorker, string) {
		fx := newQFixture(t, true)
		dw := &dropWorker{delay: time.Second}
		ts := httptest.NewServer(dw)
		t.Cleanup(ts.Close)
		nodeID := fx.srv.st.RegisterNode("slow", ts.URL, 4, 8192, fx.now.Load())
		id := fx.longSleeperOn(nodeID, 20500)
		fx.srv.lifecycleSweep(fx.now.Load())
		if dw.count() != 1 || len(fx.rows(id)) != 1 {
			t.Fatalf("first pass: %d calls, rows %+v; want one call that timed out and the row kept", dw.count(), fx.rows(id))
		}
		// The sandbox wakes before the next pass.
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			sb := fx.srv.st.FindSandbox(id)
			sb.State, sb.SleptAt = model.StateRunning, 0
		}()
		return fx, dw, id
	}

	t.Run("the worker answers", func(t *testing.T) {
		fx, dw, id := setup(t)
		dw.set(0, 200)
		fx.srv.lifecycleSweep(fx.now.Load())
		if dw.count() != 2 {
			t.Errorf("unexpose calls: %d, want the drop repeated once", dw.count())
		}
		if rows := fx.rows(id); len(rows) != 0 {
			t.Errorf("rows after the repeated drop: %+v, want the row gone (the gateway re-creates it)", rows)
		}
		q := fx.quarantine()
		if len(q) != 1 || q[0].Port != 20500 || q[0].SandboxID != id {
			t.Errorf("quarantine: %+v, want the dropped row's port", q)
		}
		if saved := fx.saved(t); len(saved.FindSandbox(id).Exposes) != 0 {
			t.Errorf("the repeated drop was not saved: %+v", saved.FindSandbox(id).Exposes)
		}
		fx.srv.lifecycleSweep(fx.now.Load())
		if dw.count() != 2 {
			t.Errorf("unexpose calls after the drop was answered: %d, want no more", dw.count())
		}
	})
	t.Run("a refusal ends it", func(t *testing.T) {
		fx, dw, id := setup(t)
		dw.set(0, 500)
		fx.srv.lifecycleSweep(fx.now.Load())
		fx.srv.lifecycleSweep(fx.now.Load())
		if dw.count() != 2 {
			t.Errorf("unexpose calls: %d, want one repeat, then none (the worker answered)", dw.count())
		}
		if rows := fx.rows(id); len(rows) != 1 {
			t.Errorf("rows after a refused repeat: %+v, want the row kept (the rule is live)", rows)
		}
	})
	t.Run("forgotten when the sandbox is gone", func(t *testing.T) {
		fx, dw, id := setup(t)
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			fx.srv.st.RemoveSandboxLocked(id)
		}()
		dw.set(0, 200)
		fx.srv.lifecycleSweep(fx.now.Load())
		if dw.count() != 1 {
			t.Errorf("unexpose calls: %d, want none for a sandbox that is gone", dw.count())
		}
		fx.srv.exposeMu.Lock()
		n := len(fx.srv.unansweredDrops)
		fx.srv.exposeMu.Unlock()
		if n != 0 {
			t.Errorf("entries kept: %d, want 0", n)
		}
	})
}

// ---- D6: a port quarantined both for this sandbox and for another ----

// The answer is a port this sandbox released for this guest port AND another
// sandbox released too: handing it back would let the other hostname reach
// this service. Refused and undone, like any quarantined port from an agent
// that honours avoid.
func TestAnswerQuarantinedForThisAndAnotherSandboxIsRefused(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant)
	now := fx.now.Load()
	fx.setQuarantine([]model.ReleasedPort{
		{Host: fx.host, Port: 20801, SandboxID: s, GuestPort: 80, ReleasedAt: now - 100},
		{Host: fx.host, Port: 20801, SandboxID: "sb-other", GuestPort: 81, ReleasedAt: now - 10},
	})
	fx.pw.set(func(f *portWorker) { f.script = []uint16{20801} })
	wantConflict(t, fx.expose(s, "web", 80), "expose answered with a port another sandbox released too")
	if rows := fx.rows(s); len(rows) != 0 {
		t.Errorf("the refused mapping was recorded: %+v", rows)
	}
	if !fx.pw.unexposed(s + "/80") {
		t.Error("the refused mapping was not removed from the worker")
	}
	if q := fx.quarantine(); len(q) != 2 {
		t.Errorf("quarantine: %+v, want both entries kept", q)
	}
}
