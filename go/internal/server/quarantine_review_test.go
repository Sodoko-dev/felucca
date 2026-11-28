// Tests for the review of the port quarantine (RQ-n in fix-go-ports.md).
package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/ingress"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

var quarantineSeconds = int64(ingress.PortQuarantine / time.Second)

// ---- RQ-2: the drop is bounded like the other sweep actions ----

// The twin of TestSweepDialsAnUnreachableNodeOnceHoweverManySandboxesItHolds
// for the drop of dynamic exposes: K long sleepers with a dynamic row on a
// worker that is down cost ONE dial per tenant per pass, and none at all when
// an auto-sleep of the same tenant already found the worker unreachable.
func TestSweepDropDialsAnUnreachableNodeOnceHoweverManySandboxesItHolds(t *testing.T) {
	fa := &lcAgent{}
	srv, first := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()
	dead := newDeadNode(t)

	srv.st.Lock()
	srv.st.Nodes[0].Addr = dead.addr()
	nodeID := srv.st.Nodes[0].ID
	srv.st.RemoveSandboxLocked(first)
	var ids []string
	for i := 0; i < 5; i++ {
		sb := srv.st.CreateSandbox(fmt.Sprintf("sleeper-%d", i), "default", nodeID, 1, 256, now)
		sb.State = model.StateSleeping
		sb.SleptAt = now - quarantineSeconds - 60
		sb.Exposes = []model.Expose{
			{Name: "8080", GuestPort: 8080, NodePort: uint16(20100 + 2*i)},
			{Name: "8081", GuestPort: 8081, NodePort: uint16(20101 + 2*i)},
		}
		ids = append(ids, sb.ID)
	}
	srv.st.Unlock()

	srv.lifecycleSweep(now)
	if got := dead.dials(); got != 1 {
		t.Errorf("the drop dialed an unreachable node %d times for %d long sleepers; want exactly 1", got, len(ids))
	}
	srv.st.Lock()
	rows := 0
	for _, id := range ids {
		rows += len(srv.st.FindSandbox(id).Exposes)
	}
	srv.st.Unlock()
	if rows != 2*len(ids) {
		t.Errorf("%d rows left of %d: nothing may be dropped on a failed call", rows, 2*len(ids))
	}

	// An auto-sleep of the same tenant that finds the node unreachable first
	// leaves the drops nothing to dial.
	srv.st.Lock()
	running := srv.st.CreateSandbox("idle", "default", nodeID, 1, 256, now)
	running.State = model.StateRunning
	running.IdleSleepS = 5
	running.LastActivity = now - 60
	srv.st.Unlock()
	before := dead.dials()
	srv.lifecycleSweep(now)
	if got := dead.dials() - before; got != 1 {
		t.Errorf("auto-sleep plus drops against a node found unreachable: %d dials, want 1", got)
	}

	// The bound is per pass: the worker comes back and the next pass drops all.
	ts := httptest.NewServer(fa.handler())
	t.Cleanup(ts.Close)
	srv.st.Lock()
	srv.st.Nodes[0].Addr = ts.URL
	// Out of the way: the idle sandbox would be put to sleep, not dropped.
	srv.st.RemoveSandboxLocked(running.ID)
	srv.st.Unlock()
	srv.lifecycleSweep(now)
	srv.st.Lock()
	rows = 0
	for _, id := range ids {
		rows += len(srv.st.FindSandbox(id).Exposes)
	}
	srv.st.Unlock()
	if rows != 0 {
		t.Errorf("%d dynamic rows left after the worker came back", rows)
	}
}

// ---- RQ-3: a (sandbox, guest port) on two node ports ----

// S exposes web:8080 on P1; an unexpose of web times out in feluccad after the
// worker removed the mapping; routes/ensure for 8080 then gets P2. The rows
// follow the worker (P1 quarantined at once), and removing both names
// quarantines P2: no port leaves its last row unquarantined.
func TestAGuestPortRemappedByTheWorkerIsQuarantined(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant)
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		fx.srv.st.FindSandbox(s).AllowDynamicPorts = true
	}()
	p1 := wantCreated(t, fx.expose(s, "web", 8080), "expose web")

	fx.pw.set(func(f *portWorker) { f.hangUpOnUnexpose = true })
	if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/web", "", adminAuth); w.Code != 502 {
		t.Fatalf("unexpose that timed out: %d %s, want 502", w.Code, w.Body)
	}
	// The worker allocates from a cursor (R4): its next fresh port is not P1.
	fx.pw.set(func(f *portWorker) { f.hangUpOnUnexpose, f.script = false, []uint16{p1 + 7} })

	if w := doRequest(fx.h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+s+`","port":8080}`, adminAuth); w.Code != 200 {
		t.Fatalf("ensure: %d %s", w.Code, w.Body)
	}
	rows := fx.rows(s)
	if len(rows) != 2 || rows[0].NodePort != rows[1].NodePort || rows[0].NodePort == p1 {
		t.Fatalf("rows after the worker remapped 8080: %+v, want both names on the new port", rows)
	}
	p2 := rows[0].NodePort
	if q := fx.quarantine(); len(q) != 1 || q[0].Port != p1 {
		t.Fatalf("quarantine after the remap: %+v, want the old port %d", q, p1)
	}
	if saved := fx.saved(t); len(saved.Quarantine) != 1 || saved.FindSandbox(s).Exposes[0].NodePort != p2 {
		t.Errorf("the remap was not saved with the record: %+v", saved.Quarantine)
	}

	doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/web", "", adminAuth)
	doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/8080", "", adminAuth)
	ports := map[uint16]bool{}
	for _, q := range fx.quarantine() {
		ports[q.Port] = true
	}
	if !ports[p1] || !ports[p2] {
		t.Errorf("quarantined ports after both names went: %v, want %d and %d", ports, p1, p2)
	}
}

// ---- RQ-7: a 404 from the worker's unexpose ----

func TestUnexpose404MeansTheMappingIsGone(t *testing.T) {
	t.Run("handler", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(qTenant)
		p := wantCreated(t, fx.expose(s, "web", 80), "expose")
		fx.pw.set(func(f *portWorker) { f.unexposeStatus = 404 })
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+s+"/expose/web", "", adminAuth); w.Code != 204 {
			t.Fatalf("unexpose answered 404 by the worker: %d %s, want 204", w.Code, w.Body)
		}
		if q := fx.quarantine(); len(fx.rows(s)) != 0 || len(q) != 1 || q[0].Port != p {
			t.Errorf("rows %+v quarantine %+v, want the row gone and %d quarantined", fx.rows(s), q, p)
		}
	})
	t.Run("sweep", func(t *testing.T) {
		fx := newQFixture(t, true)
		now := fx.now.Load()
		s := fx.sandbox(qTenant, model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20000})
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			sb := fx.srv.st.FindSandbox(s)
			sb.State = model.StateSleeping
			sb.SleptAt = now - quarantineSeconds
		}()
		fx.pw.set(func(f *portWorker) { f.unexposeStatus = 404 })
		fx.srv.lifecycleSweep(now)
		if len(fx.rows(s)) != 0 || len(fx.quarantine()) != 1 {
			t.Errorf("rows %+v quarantine %+v, want the row dropped and its port quarantined", fx.rows(s), fx.quarantine())
		}
	})
}

// ---- RQ-8: the drop re-checks that the sandbox is still long asleep ----

func TestDropRechecksTheSleepRightBeforeTheWorkerCall(t *testing.T) {
	setup := func(t *testing.T) (*qFixture, string, model.Expose, int64) {
		fx := newQFixture(t, true)
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
		return fx, s, e, now
	}
	t.Run("woke since the decision", func(t *testing.T) {
		fx, s, e, now := setup(t)
		fx.srv.st.SetSandboxState(s, model.StateRunning)
		if ok, err := fx.srv.dropExpose(s, e, "req-drop", now, &dropBudget{left: dropPassBudget}); ok || err != nil || fx.pw.unexposed(s+"/8080") || len(fx.rows(s)) != 1 {
			t.Errorf("drop of a woken sandbox: ok=%v err=%v unexposed=%v rows=%+v, want nothing done", ok, err, fx.pw.unexposed(s+"/8080"), fx.rows(s))
		}
	})
	t.Run("asleep again, but not for the quarantine", func(t *testing.T) {
		fx, s, e, now := setup(t)
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			fx.srv.st.FindSandbox(s).SleptAt = now - 10
		}()
		if ok, _ := fx.srv.dropExpose(s, e, "req-drop", now, &dropBudget{left: dropPassBudget}); ok || fx.pw.unexposed(s+"/8080") {
			t.Errorf("drop of a sandbox asleep for 10 s: ok=%v, unexposed=%v, want nothing done", ok, fx.pw.unexposed(s+"/8080"))
		}
	})
	t.Run("woke during the worker call", func(t *testing.T) {
		fx, s, e, now := setup(t)
		logs := captureLog(t)
		fx.pw.set(func(f *portWorker) {
			f.onUnexpose = func() { fx.srv.st.SetSandboxState(s, model.StateRunning) }
		})
		if ok, err := fx.srv.dropExpose(s, e, "req-drop", now, &dropBudget{left: dropPassBudget}); !ok || err != nil {
			t.Fatalf("drop: ok=%v err=%v", ok, err)
		}
		// The rule is gone on the worker, so the row goes with it (the
		// gateway re-ensures the route), and the loss is logged.
		if len(fx.rows(s)) != 0 || !strings.Contains(logs(), "woke while its dynamic expose was being dropped") {
			t.Errorf("rows %+v; log:\n%s", fx.rows(s), logs())
		}
	})
}

// ---- RQ-9: a port this sandbox holds for another guest port ----

func TestAnswerNamingThisSandboxsOtherGuestPortIsRefused(t *testing.T) {
	fx := newQFixture(t, true)
	logs := captureLog(t)
	s := fx.sandbox(qTenant, model.Expose{Name: "api", GuestPort: 81, NodePort: 20050})
	fx.pw.set(func(f *portWorker) { f.script = []uint16{20050} })
	// GF-12: the answer names the expose that holds the port and what ends it.
	w := fx.expose(s, "web", 80)
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" ||
		w.Body.String() != `{"error":"node port conflict: the worker answered node port 20050, which this sandbox's expose \"api\" (guest port 81) holds; removing that expose ends it"}` {
		t.Errorf("answer: %d %s Retry-After=%q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if rows := fx.rows(s); len(rows) != 1 || rows[0].Name != "api" {
		t.Errorf("rows: %+v, want only api", rows)
	}
	if !fx.pw.unexposed(s + "/80") {
		t.Error("the new mapping was not removed from the worker")
	}
	l := logs()
	if !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "another guest port") ||
		!strings.Contains(l, "held_by_expose=api") || !strings.Contains(l, "held_for_guest_port=81") {
		t.Errorf("no error naming the expose that holds the port:\n%s", l)
	}
}

// ---- RQ-10: double-booked rows from before are reported at start-up ----

func TestDoubleBookedRowsAreReportedAtStartUp(t *testing.T) {
	logs := captureLog(t)
	st := state.New()
	nodeID := st.RegisterNode("w1", "http://10.0.0.1:9090", 4, 8192, 1)
	st.Lock()
	a := st.CreateSandbox("a", "default", nodeID, 1, 256, 1)
	a.Exposes = []model.Expose{{Name: "web", GuestPort: 80, NodePort: 20001}}
	b := st.CreateSandbox("b", "default", nodeID, 1, 256, 1)
	b.Exposes = []model.Expose{{Name: "web", GuestPort: 80, NodePort: 20001}}
	st.Unlock()
	db, err := store.OpenSQLite(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(&config.Config{Token: "admin-tok"}, st, db)
	t.Cleanup(srv.stopAndWait)
	l := logs()
	if !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "node_port=20001") ||
		!strings.Contains(l, a.ID) || !strings.Contains(l, b.ID) || !strings.Contains(l, "delete or unexpose") {
		t.Errorf("no start-up error naming the port, both sandboxes and what to do:\n%s", l)
	}
}

// ---- RQ-11: a clock stepped back does not lengthen Retry-After ----

func TestChurnRetryAfterIsAtMostTheQuarantine(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(adminTenant)
	now := fx.now.Load()
	fx.setQuarantine(fillQuarantine(maxQuarantinedPerSandbox, func(i int) model.ReleasedPort {
		return model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: s, GuestPort: uint16(1000 + i), ReleasedAt: now + 3600}
	}))
	wantChurn(t, fx.expose(s, "web", 80), "900", "entries released 'in the future'")
}

// ---- RQ-16: what the suite did not pin ----

func TestPreferIsWithheldForAPortInUse(t *testing.T) {
	t.Run("another sandbox's live row", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(qTenant)
		fx.sandbox(qTenant, model.Expose{Name: "x", GuestPort: 90, NodePort: 20801})
		fx.setQuarantine([]model.ReleasedPort{{Host: fx.host, Port: 20801, SandboxID: s, GuestPort: 82, ReleasedAt: fx.now.Load() - 10}})
		wantCreated(t, fx.expose(s, "svc", 82), "expose")
		if req := fx.pw.lastReq(t); req.prefer != 0 {
			t.Errorf("prefer %d: a port another sandbox's row holds must not be preferred", req.prefer)
		}
	})
	t.Run("this sandbox's row for another guest port", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(qTenant, model.Expose{Name: "api", GuestPort: 81, NodePort: 20801})
		fx.setQuarantine([]model.ReleasedPort{{Host: fx.host, Port: 20801, SandboxID: s, GuestPort: 82, ReleasedAt: fx.now.Load() - 10}})
		wantCreated(t, fx.expose(s, "svc", 82), "expose")
		if req := fx.pw.lastReq(t); req.prefer != 0 {
			t.Errorf("prefer %d: a port this sandbox holds for another guest port must not be preferred", req.prefer)
		}
	})
}

// A sleeper past both its TTL and the quarantine is deleted, not first
// stripped of its rows: one worker call, its ports quarantined once.
func TestTTLDeleteBeatsTheDropOnOnePass(t *testing.T) {
	fx := newQFixture(t, true)
	now := fx.now.Load()
	s := fx.sandbox(qTenant, model.Expose{Name: "8080", GuestPort: 8080, NodePort: 20000})
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		sb := fx.srv.st.FindSandbox(s)
		sb.State = model.StateSleeping
		sb.SleptAt = now - quarantineSeconds - 100
		sb.AsleepDeleteS = 30
	}()
	fx.srv.lifecycleSweep(now)
	if fx.rows(s) != nil || fx.pw.unexposed(s+"/8080") {
		t.Errorf("rows %+v, unexposed %v: want the sandbox deleted without a drop", fx.rows(s), fx.pw.unexposed(s+"/8080"))
	}
	if q := fx.quarantine(); len(q) != 1 || q[0].Port != 20000 {
		t.Errorf("quarantine: %+v, want 20000 once", q)
	}
}

// Deleted while its expose is at the worker: 404, nothing recorded, and no
// undo (the mapping went with the VM).
func TestSandboxDeletedWhileItsExposeIsAtTheWorker(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant)
	fx.pw.set(func(f *portWorker) { f.onExpose = func() { fx.srv.st.RemoveSandbox(s) } })
	if w := fx.expose(s, "web", 80); w.Code != 404 {
		t.Errorf("expose of a sandbox deleted meanwhile: %d %s, want 404", w.Code, w.Body)
	}
	if fx.rows(s) != nil || len(fx.quarantine()) != 0 || fx.pw.unexposed(s+"/80") {
		t.Errorf("rows %+v quarantine %+v unexposed %v, want nothing", fx.rows(s), fx.quarantine(), fx.pw.unexposed(s+"/80"))
	}
}

// The undo of a refused answer fails (the worker refuses to remove the new
// mapping): the answer is still the conflict, nothing is recorded, and the
// failure is logged — a mapping no row describes is left on the worker.
func TestFailedUndoAfterAConflict(t *testing.T) {
	fx := newQFixture(t, true)
	logs := captureLog(t)
	a := fx.sandbox(qTenant, model.Expose{Name: "web", GuestPort: 80, NodePort: 20000})
	b := fx.sandbox(qTenant)
	fx.pw.set(func(f *portWorker) {
		f.script = []uint16{20000}
		f.unexposeStatus = 500
	})
	wantConflict(t, fx.expose(b, "web", 80), "conflict with a failing undo")
	if len(fx.rows(b)) != 0 || len(fx.rows(a)) != 0 {
		t.Errorf("rows: a %+v b %+v, want the stale row removed and nothing recorded for b", fx.rows(a), fx.rows(b))
	}
	if !strings.Contains(logs(), "refused mapping could not be removed from the worker") {
		t.Errorf("failed undo not logged:\n%s", logs())
	}
}

// The conflict path's own save fails: the answer is still the conflict, the
// stale row is gone from memory (not from the database), the store is marked
// failing, and the recovery writes the removal with its quarantine entry.
func TestConflictPathSaveFailing(t *testing.T) {
	fx := newQFixture(t, true)
	a := fx.sandbox(qTenant, model.Expose{Name: "web", GuestPort: 80, NodePort: 20000})
	b := fx.sandbox(qTenant)
	if err := fx.srv.saveState(); err != nil {
		t.Fatal(err)
	}
	fx.fs.failAll()
	fx.pw.set(func(f *portWorker) { f.script = []uint16{20000} })
	wantConflict(t, fx.expose(b, "web", 80), "conflict, save failing")
	if len(fx.rows(a)) != 0 || !fx.srv.storeFailing() {
		t.Fatalf("memory rows %+v, failing %v: want the stale row removed and the store failing", fx.rows(a), fx.srv.storeFailing())
	}
	if saved := fx.saved(t); len(saved.FindSandbox(a).Exposes) != 1 || len(saved.Quarantine) != 0 {
		t.Errorf("database after the failed save: %+v %+v, want the old row and no entry", saved.FindSandbox(a).Exposes, saved.Quarantine)
	}
	fx.fs.heal()
	if err := fx.srv.recoverStore(); err != nil {
		t.Fatal(err)
	}
	if saved := fx.saved(t); len(saved.FindSandbox(a).Exposes) != 0 || len(saved.Quarantine) != 1 {
		t.Errorf("database after recovery: %+v %+v, want the removal and its entry", saved.FindSandbox(a).Exposes, saved.Quarantine)
	}
}

// ---- Item 12: store READ errors are 503, not 500 ----

// readFailStore fails the reads the item names.
type readFailStore struct {
	store.Store
}

func (readFailStore) CheckJoinToken(string, int64) (bool, error) { return false, errInjected }
func (readFailStore) GetWgPeerByPubKey(string) (*store.WgPeer, error) {
	return nil, errInjected
}
func (readFailStore) GetTemplateByName(string) (*store.Template, error) { return nil, errInjected }
func (readFailStore) GetTenant(string) (*store.Tenant, error)           { return nil, errInjected }

// listFailStore lets the pubkey lookup through and fails the peer listing.
type listFailStore struct {
	store.Store
}

func (listFailStore) CheckJoinToken(string, int64) (bool, error) { return true, nil }
func (listFailStore) GetWgPeerByPubKey(string) (*store.WgPeer, error) {
	return nil, nil
}
func (listFailStore) ListWgPeers() ([]*store.WgPeer, error) { return nil, errInjected }

func TestStoreReadErrorsAre503(t *testing.T) {
	cases := []struct {
		name   string
		wrap   func(store.Store) store.Store
		method string
		path   string
		body   string
		auth   string
	}{
		{"join: token check", func(s store.Store) store.Store { return readFailStore{s} }, "POST", "/api/v1/nodes/join",
			`{"pubkey":"` + validPubKeyA + `","hostname":"w9"}`, "Bearer felucca_jt_" + strings.Repeat("a", 40)},
		{"join: peer lookup", func(s store.Store) store.Store { return pubkeyFailStore{s} }, "POST", "/api/v1/nodes/join",
			`{"pubkey":"` + validPubKeyA + `","hostname":"w9"}`, "Bearer felucca_jt_" + strings.Repeat("a", 40)},
		{"join: peer list", func(s store.Store) store.Store { return listFailStore{s} }, "POST", "/api/v1/nodes/join",
			`{"pubkey":"` + validPubKeyA + `","hostname":"w9"}`, "Bearer felucca_jt_" + strings.Repeat("a", 40)},
		{"create with a template", func(s store.Store) store.Store { return readFailStore{s} }, "POST", "/api/v1/sandboxes",
			`{"name":"a","template":"t1"}`, adminAuth},
		{"create template: tenant", func(s store.Store) store.Store { return readFailStore{s} }, "POST", "/api/v1/templates",
			`{"name":"t1","image_file":"x.ext4","tenant":"tn-1"}`, adminAuth},
		{"create template: name", func(s store.Store) store.Store { return readFailStore{s} }, "POST", "/api/v1/templates",
			`{"name":"t1","image_file":"x.ext4"}`, adminAuth},
		{"delete template", func(s store.Store) store.Store { return readFailStore{s} }, "DELETE", "/api/v1/templates/t1", "", adminAuth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newJoinServerWgIP(t, "10.100.0.1/24")
			srv.db = c.wrap(srv.db)
			h := srv.Handler()
			// Repeated: a read error must not feed the credential throttle.
			for i := 0; i < 12; i++ {
				w := doRequest(h, c.method, c.path, c.body, c.auth)
				if w.Code != 503 || w.Body.String() != `{"error":"store unavailable"}` || w.Header().Get("Retry-After") != "5" {
					t.Fatalf("attempt %d: %d %s Retry-After=%q, want 503 store unavailable with Retry-After 5", i, w.Code, w.Body, w.Header().Get("Retry-After"))
				}
			}
			if srv.storeFailing() {
				t.Error("a read error marked the store failing")
			}
		})
	}
}

// pubkeyFailStore lets the token through and fails the pubkey lookup.
type pubkeyFailStore struct {
	store.Store
}

func (pubkeyFailStore) CheckJoinToken(string, int64) (bool, error) { return true, nil }
func (pubkeyFailStore) GetWgPeerByPubKey(string) (*store.WgPeer, error) {
	return nil, errInjected
}
