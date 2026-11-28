// Tests for the third review of the port quarantine (GF-n in fix-go-round3.md,
// PART A).
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// silentNode accepts connections and never answers: a worker that hangs.
type silentNode struct {
	ln     net.Listener
	mu     sync.Mutex
	accept int
	conns  []net.Conn
}

func newSilentNode(t *testing.T) *silentNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &silentNode{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.accept++
			s.conns = append(s.conns, c)
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			c.Close()
		}
	})
	return s
}

func (s *silentNode) addr() string { return "http://" + s.ln.Addr().String() }

func (s *silentNode) dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accept
}

// longSleeperOn adds a sandbox on nodeID asleep for longer than the
// quarantine, with one dynamic row.
func (fx *qFixture) longSleeperOn(nodeID string, port uint16) string {
	now := fx.now.Load()
	id := fx.sandboxOn(nodeID, qTenant, model.Expose{Name: "8080", GuestPort: 8080, NodePort: port})
	fx.srv.st.Lock()
	defer fx.srv.st.Unlock()
	sb := fx.srv.st.FindSandbox(id)
	sb.State = model.StateSleeping
	sb.SleptAt = now - quarantineSeconds - 60
	return id
}

// ---- A1 (GF-1): the drop never holds exposeMu for long ----

// A worker whose heartbeat is older than the ready limit is never dialed by
// the drop, and an expose on another node is answered at once while the pass
// runs.
func TestDropSkipsANodeThatIsNotReady(t *testing.T) {
	fx := newQFixture(t, true)
	dark := newSilentNode(t)
	now := fx.now.Load()
	darkID := fx.srv.st.RegisterNode("dark", dark.addr(), 4, 8192, now-60)
	sleeper := fx.longSleeperOn(darkID, 20500)
	live := fx.sandbox(qTenant)

	done := make(chan struct{})
	go func() {
		defer close(done)
		fx.srv.lifecycleSweep(now)
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	wantCreated(t, fx.expose(live, "web", 80), "expose on a ready node while the pass runs")
	if d := time.Since(start); d > time.Second {
		t.Errorf("an expose on another node waited %v behind the drop", d)
	}
	<-done
	if n := dark.dials(); n != 0 {
		t.Errorf("the drop dialed a node that is not ready %d times", n)
	}
	if len(fx.rows(sleeper)) != 1 {
		t.Errorf("rows of the sleeper on the dark node: %+v, want kept", fx.rows(sleeper))
	}
}

// A ready worker that never answers: the drop's call ends at its own limit
// (5 s, not 30 s), the row stays, and the node is unreachable for the rest of
// the pass.
func TestDropCallHasItsOwnLimit(t *testing.T) {
	fx := newQFixture(t, true)
	hung := newSilentNode(t)
	now := fx.now.Load()
	hungID := fx.srv.st.RegisterNode("hung", hung.addr(), 4, 8192, now)
	a := fx.longSleeperOn(hungID, 20500)
	b := fx.longSleeperOn(hungID, 20501)

	start := time.Now()
	fx.srv.lifecycleSweep(now)
	d := time.Since(start)
	if d < dropCallTimeout-time.Second || d > dropCallTimeout+2*time.Second {
		t.Errorf("the pass took %v, want about the drop's %v limit", d, dropCallTimeout)
	}
	if n := hung.dials(); n != 1 {
		t.Errorf("dials to the hung worker: %d, want 1 (the second sleeper waits for the next pass)", n)
	}
	if len(fx.rows(a)) != 1 || len(fx.rows(b)) != 1 {
		t.Errorf("rows: %+v %+v, want both kept", fx.rows(a), fx.rows(b))
	}
}

// slowWorker answers every unexpose after delay.
type slowWorker struct {
	mu    sync.Mutex
	delay time.Duration
	calls int
}

func (s *slowWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delay := s.delay
	if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/expose") {
		s.calls++
	}
	s.mu.Unlock()
	time.Sleep(delay)
	w.WriteHeader(200)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *slowWorker) set(delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = delay
}

func (s *slowWorker) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A worker that takes 2 s per call, ten long sleepers on it: one pass spends
// its 5 s budget (three calls) and returns; later passes drop the rest.
func TestDropBudgetPerPass(t *testing.T) {
	fx := newQFixture(t, true)
	sw := &slowWorker{delay: 2 * time.Second}
	ts := httptest.NewServer(sw)
	t.Cleanup(ts.Close)
	now := fx.now.Load()
	slowID := fx.srv.st.RegisterNode("slow", ts.URL, 4, 8192, now)
	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, fx.longSleeperOn(slowID, uint16(20500+i)))
	}
	left := func() int {
		n := 0
		for _, id := range ids {
			n += len(fx.rows(id))
		}
		return n
	}

	start := time.Now()
	fx.srv.lifecycleSweep(now)
	d := time.Since(start)
	if n := sw.count(); n > 3 {
		t.Errorf("one pass made %d calls to a 2 s worker, want at most 3", n)
	}
	if d > 8*time.Second {
		t.Errorf("the pass took %v, want about 6-7 s", d)
	}
	if got := left(); got != 10-sw.count() {
		t.Errorf("%d rows left after %d calls", got, sw.count())
	}

	sw.set(0)
	for i := 0; i < 5 && left() > 0; i++ {
		fx.srv.lifecycleSweep(now)
	}
	if got := left(); got != 0 {
		t.Errorf("%d rows left after later passes", got)
	}
}

// ---- A2 (GF-2): two predicates ----

// splitFixture: web:8080 -> 20001 (stale) and 8080:8080 -> 20002 (live); the
// worker maps 8080 to 20002.
func splitFixture(t *testing.T) (*qFixture, string) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant,
		model.Expose{Name: "web", GuestPort: 8080, NodePort: 20001},
		model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20002})
	fx.pw.set(func(f *portWorker) { f.held[s+"/8080"] = 20002 })
	return fx, s
}

func quarantinedPorts(fx *qFixture) map[uint16]bool {
	out := map[uint16]bool{}
	for _, q := range fx.quarantine() {
		out[q.Port] = true
	}
	return out
}

func TestRemovingTheStaleNameKeepsTheLiveMapping(t *testing.T) {
	fx, s := splitFixture(t)
	if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/web", "", adminAuth); w.Code != 204 {
		t.Fatalf("unexpose web: %d %s", w.Code, w.Body)
	}
	if fx.pw.unexposed(s + "/8080") {
		t.Error("the worker was told to remove guest port 8080: the live row's mapping is gone")
	}
	fx.pw.mu.Lock()
	held := fx.pw.held[s+"/8080"]
	fx.pw.mu.Unlock()
	if held != 20002 {
		t.Errorf("worker mapping for 8080: %d, want 20002 kept", held)
	}
	if rows := fx.rows(s); len(rows) != 1 || rows[0].Name != "8080" || rows[0].NodePort != 20002 {
		t.Errorf("rows: %+v, want only the live 8080 -> 20002", rows)
	}
	if q := quarantinedPorts(fx); !q[20001] || q[20002] {
		t.Errorf("quarantined: %v, want 20001 only", q)
	}
	w := doRequest(fx.h, "GET", "/api/v1/routes", "", adminAuth)
	if !strings.Contains(w.Body.String(), `"hostname":"8080--`+s+`"`) || !strings.Contains(w.Body.String(), `"node_port":20002`) {
		t.Errorf("the other hostname is not routed to 20002: %s", w.Body)
	}
}

func TestRemovingTheLiveNameThenExposingAgainRepairs(t *testing.T) {
	fx, s := splitFixture(t)
	if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/8080", "", adminAuth); w.Code != 204 {
		t.Fatalf("unexpose 8080: %d %s", w.Code, w.Body)
	}
	if fx.pw.unexposed(s + "/8080") {
		t.Error("the worker was told to remove guest port 8080 while web still has it")
	}
	if q := quarantinedPorts(fx); !q[20002] {
		t.Errorf("quarantined: %v, want 20002 (no row names it any more)", q)
	}
	if got := wantCreated(t, fx.expose(s, "api", 8080), "expose of guest port 8080"); got != 20002 {
		t.Errorf("expose: port %d, want the worker's 20002 accepted", got)
	}
	for _, r := range fx.rows(s) {
		if r.NodePort != 20002 {
			t.Errorf("row %+v not re-pointed to 20002", r)
		}
	}
	if q := quarantinedPorts(fx); !q[20001] || q[20002] {
		t.Errorf("quarantined after the repair: %v, want 20001 and not 20002", q)
	}
}

// The worker confirms one of several ports this sandbox holds for the guest
// port: all rows of it are re-pointed to the answer, the others quarantined.
func TestConfirmingOneOfSeveralPortsRepoints(t *testing.T) {
	fx, s := splitFixture(t)
	if got := wantCreated(t, fx.expose(s, "api", 8080), "expose"); got != 20002 {
		t.Fatalf("port %d, want 20002", got)
	}
	for _, r := range fx.rows(s) {
		if r.NodePort != 20002 {
			t.Errorf("row %+v not re-pointed", r)
		}
	}
	if q := quarantinedPorts(fx); !q[20001] {
		t.Errorf("quarantined: %v, want 20001", q)
	}
}

func TestSplitGuestPortIsReportedAtStartUp(t *testing.T) {
	logs := captureLog(t)
	st := state.New()
	nodeID := st.RegisterNode("w1", "http://10.0.0.1:9090", 4, 8192, 1)
	st.Lock()
	sb := st.CreateSandbox("a", "default", nodeID, 1, 256, 1)
	sb.Exposes = []model.Expose{
		{Name: "web", GuestPort: 8080, NodePort: 20001},
		{Name: "8080", GuestPort: 8080, NodePort: 20002},
	}
	st.Unlock()
	db, err := store.OpenSQLite(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(&config.Config{Token: "admin-tok"}, st, db)
	t.Cleanup(srv.stopAndWait)
	l := logs()
	if !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "sandbox="+sb.ID) || !strings.Contains(l, "guest_port=8080") ||
		!strings.Contains(l, "node_ports=20001,20002") || !strings.Contains(l, "repairs it") {
		t.Errorf("no start-up error for the split guest port:\n%s", l)
	}
}

// ---- A4 (GF-9): store health inside multi-call actions ----

func TestDropStopsWhenTheStoreFailsMidway(t *testing.T) {
	fx := newQFixture(t, true)
	now := fx.now.Load()
	s := fx.sandbox(qTenant,
		model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20000},
		model.Expose{Name: "8081", GuestPort: 8081, NodePort: 20001})
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		sb := fx.srv.st.FindSandbox(s)
		sb.State = model.StateSleeping
		sb.SleptAt = now - quarantineSeconds
	}()
	fx.pw.set(func(f *portWorker) {
		f.onUnexpose = func() {
			fx.fs.failAll()
			fx.srv.storeWriteFailed(errInjected) // another request's save failed
		}
	})
	fx.srv.lifecycleSweep(now)
	fx.pw.mu.Lock()
	n := len(fx.pw.unexp)
	fx.pw.mu.Unlock()
	if n != 1 {
		t.Errorf("unexposes after the store failed: %d calls, want 1", n)
	}
}

func TestForkReExposeStopsWhileTheStoreIsFailing(t *testing.T) {
	fx := newQFixture(t, true)
	parent := fx.sandbox(adminTenant,
		model.Expose{Name: "a", GuestPort: 80, NodePort: 20100},
		model.Expose{Name: "b", GuestPort: 81, NodePort: 20101},
		model.Expose{Name: "c", GuestPort: 82, NodePort: 20102})
	if err := fx.srv.saveState(); err != nil {
		t.Fatal(err)
	}
	fx.pw.set(func(f *portWorker) {
		f.onFork = func() {
			fx.fs.failAll()
			fx.srv.storeWriteFailed(errInjected)
		}
	})
	w := doRequest(fx.h, "POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`, adminAuth)
	if w.Code != 503 {
		t.Errorf("fork whose last save is refused: %d %s, want 503", w.Code, w.Body)
	}
	if n := fx.pw.reqCount(); n != 0 {
		t.Errorf("expose calls for a fork whose save will be refused: %d, want 0", n)
	}
	// Let the clean-up's worker call finish before the fixture goes.
	waitFor(t, "the clean-up's delete", 5*time.Second, func() bool {
		fx.pw.mu.Lock()
		defer fx.pw.mu.Unlock()
		return fx.pw.deletes > 0
	})
}

// ---- A6 (GF-14): the per-sandbox churn count ----

// After an address swap each entry exists twice; and entries of another host
// are not this request's. Distinct (node port, guest port) on the request's
// host are what counts.
func TestSandboxChurnCountsDistinctPortsOnTheRequestHost(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant)
	now := fx.now.Load()
	var q []model.ReleasedPort
	for i := 0; i < 40; i++ {
		e := model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: s, GuestPort: uint16(1000 + i), TenantID: qTenant, ReleasedAt: now - 10}
		q = append(q, e, e) // two copies of one release
		o := e
		o.Host = "10.9.9.9"
		q = append(q, o)
	}
	for i := 0; i < 30; i++ { // other ports, on another host: not this request's
		q = append(q, model.ReleasedPort{Host: "10.9.9.9", Port: uint16(22000 + i), SandboxID: s, GuestPort: uint16(2000 + i), TenantID: qTenant, ReleasedAt: now - 10})
	}
	fx.setQuarantine(q)
	wantCreated(t, fx.expose(s, "web", 80), "40 distinct ports on this host, 150 entries")
}

// The sweep's drop of a stale dynamic name: another row has the same guest
// port on the live node port, so the worker's mapping stays.
func TestDropOfAStaleDynamicNameKeepsTheLiveMapping(t *testing.T) {
	fx := newQFixture(t, true)
	now := fx.now.Load()
	s := fx.sandbox(qTenant,
		model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20001}, // stale, dynamic
		model.Expose{Name: "web", GuestPort: 8080, NodePort: 20002})  // live, named
	fx.pw.set(func(f *portWorker) { f.held[s+"/8080"] = 20002 })
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		sb := fx.srv.st.FindSandbox(s)
		sb.State = model.StateSleeping
		sb.SleptAt = now - quarantineSeconds
	}()
	fx.srv.lifecycleSweep(now)
	if fx.pw.unexposed(s + "/8080") {
		t.Error("the drop told the worker to remove guest port 8080; the live row needs it")
	}
	if rows := fx.rows(s); len(rows) != 1 || rows[0].Name != "web" {
		t.Errorf("rows: %+v, want only web", rows)
	}
	if q := quarantinedPorts(fx); !q[20001] || q[20002] {
		t.Errorf("quarantined: %v, want 20001 only", q)
	}
}

// ---- A7 (GF-15): no log call under the state lock ----

// lockProbe records log messages written while the state lock is held.
type lockProbe struct {
	st        *state.State
	mu        sync.Mutex
	underLock []string
}

func (h *lockProbe) Enabled(context.Context, slog.Level) bool { return true }
func (h *lockProbe) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *lockProbe) WithGroup(string) slog.Handler            { return h }
func (h *lockProbe) Handle(_ context.Context, r slog.Record) error {
	got := make(chan struct{})
	go func() {
		h.st.Lock()
		h.st.Unlock()
		close(got)
	}()
	select {
	case <-got:
	case <-time.After(200 * time.Millisecond):
		h.mu.Lock()
		h.underLock = append(h.underLock, r.Message)
		h.mu.Unlock()
	}
	return nil
}

func probeLog(t *testing.T, st *state.State) *lockProbe {
	t.Helper()
	p := &lockProbe{st: st}
	prev := slog.Default()
	slog.SetDefault(slog.New(p))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return p
}

func (h *lockProbe) check(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.underLock) > 0 {
		t.Errorf("logged under the state lock: %q", h.underLock)
	}
}

func TestPortLogsAreWrittenAfterTheStateLock(t *testing.T) {
	t.Run("stale row", func(t *testing.T) {
		fx := newQFixture(t, true)
		p := probeLog(t, fx.srv.st)
		fx.sandbox(qTenant, model.Expose{Name: "web", GuestPort: 80, NodePort: 20000})
		b := fx.sandbox(qTenant)
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20000} })
		wantConflict(t, fx.expose(b, "web", 80), "conflict")
		p.check(t)
	})
	t.Run("own other guest port", func(t *testing.T) {
		fx := newQFixture(t, true)
		p := probeLog(t, fx.srv.st)
		s := fx.sandbox(qTenant, model.Expose{Name: "api", GuestPort: 81, NodePort: 20050})
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20050} })
		fx.expose(s, "web", 80)
		p.check(t)
	})
	t.Run("quarantined, older agent", func(t *testing.T) {
		fx := newQFixture(t, false)
		p := probeLog(t, fx.srv.st)
		s := fx.sandbox(qTenant)
		fx.setQuarantine([]model.ReleasedPort{{Host: fx.host, Port: 20010, SandboxID: "sb-x", GuestPort: 80, ReleasedAt: fx.now.Load() - 10}})
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20010} })
		wantCreated(t, fx.expose(s, "web", 80), "older agent")
		p.check(t)
	})
	t.Run("re-point", func(t *testing.T) {
		fx, s := splitFixture(t)
		p := probeLog(t, fx.srv.st)
		wantCreated(t, fx.expose(s, "api", 8080), "confirm")
		p.check(t)
	})
	t.Run("woke during the drop", func(t *testing.T) {
		fx := newQFixture(t, true)
		p := probeLog(t, fx.srv.st)
		now := fx.now.Load()
		e := model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20000}
		s := fx.sandbox(qTenant, e)
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			sb := fx.srv.st.FindSandbox(s)
			sb.State = model.StateSleeping
			sb.SleptAt = now - quarantineSeconds
		}()
		fx.pw.set(func(f *portWorker) {
			f.onUnexpose = func() { fx.srv.st.SetSandboxState(s, model.StateRunning) }
		})
		if ok, err := fx.srv.dropExpose(s, e, "req", now, &dropBudget{left: dropPassBudget}); !ok || err != nil {
			t.Fatalf("drop: %v %v", ok, err)
		}
		p.check(t)
	})
}
