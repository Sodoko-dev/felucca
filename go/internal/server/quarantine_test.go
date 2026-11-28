// Tests for FLU-6 (control plane): a released node port is quarantined, every
// expose request tells the worker what to avoid and what to prefer, the answer
// is checked before it is recorded, and churn is capped.
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/agentclient"
	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// ---- Fixture ----

// portWorker is a fake worker agent that hands out node ports the way the
// agent did before FLU-6: the LOWEST free one from 20000, idempotent per
// (vm, guest port), freed by unexpose and by the VM's delete. With honor set
// it is the new agent: it never returns a port in avoid, returns prefer when
// it is free, and says avoid_honored. Without it, it ignores both fields (an
// older agent).
type portWorker struct {
	mu        sync.Mutex
	honor     bool
	exhausted bool
	// script forces the next fresh answers, whatever the request says.
	script []uint16
	held   map[string]uint16 // "<vm>/<guest port>" -> node port
	reqs   []exposeReq
	unexp  []string // "<vm>/<guest port>"
	// hangUpOnDelete: a VM delete is carried out, then the connection is
	// closed without an answer — feluccad sees a failed call (a delete that
	// "timed out") while the worker has released the VM's ports.
	hangUpOnDelete bool
	// unexposeStatus, when not 0, answers every unexpose with that status
	// (the mapping is left as it is). hangUpOnUnexpose carries the unexpose
	// out, then hangs up (an unexpose that "timed out" in feluccad).
	unexposeStatus   int
	hangUpOnUnexpose bool
	// onExpose / onUnexpose / onFork run inside the call, before it is answered.
	onExpose, onUnexpose, onFork func()
	deletes                      int // VM deletes seen
}

type exposeReq struct {
	vm     string
	guest  uint16
	avoid  []uint16
	prefer uint16
	raw    string
}

func (f *portWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p := r.URL.Path
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(s string) { w.WriteHeader(200); _, _ = w.Write([]byte(s)) }
	var req struct {
		GuestPort uint16   `json:"guest_port"`
		Avoid     []uint16 `json:"avoid"`
		Prefer    uint16   `json:"prefer"`
	}
	_ = json.Unmarshal(body, &req)
	vm := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/vms/"), "/expose")
	key := fmt.Sprintf("%s/%d", vm, req.GuestPort)
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/expose"):
		f.reqs = append(f.reqs, exposeReq{vm: vm, guest: req.GuestPort, avoid: req.Avoid, prefer: req.Prefer, raw: string(body)})
		if f.onExpose != nil {
			f.onExpose()
		}
		port, have := f.held[key]
		if !have {
			if f.exhausted {
				w.WriteHeader(409)
				_, _ = w.Write([]byte(`{"error":"ports exhausted"}`))
				return
			}
			if len(f.script) > 0 {
				port, f.script = f.script[0], f.script[1:]
			} else {
				port = f.pickLocked(req.Avoid, req.Prefer)
			}
			f.held[key] = port
		}
		if f.honor {
			ok(fmt.Sprintf(`{"node_port":%d,"avoid_honored":true}`, port))
		} else {
			ok(fmt.Sprintf(`{"node_port":%d}`, port))
		}
	case r.Method == http.MethodDelete && strings.HasSuffix(p, "/expose"):
		f.unexp = append(f.unexp, key)
		if f.onUnexpose != nil {
			f.onUnexpose()
		}
		if f.unexposeStatus != 0 {
			w.WriteHeader(f.unexposeStatus)
			_, _ = w.Write([]byte(`{"error":"injected"}`))
			return
		}
		delete(f.held, key)
		if f.hangUpOnUnexpose {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		ok(`{"ok":true}`)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/fork"):
		if f.onFork != nil {
			f.onFork()
		}
		ok(`{"ip":"10.231.0.10"}`)
	case r.Method == http.MethodDelete:
		f.deletes++
		for k := range f.held {
			if strings.HasPrefix(k, vm+"/") {
				delete(f.held, k)
			}
		}
		if f.hangUpOnDelete {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		ok(`{"ok":true}`)
	case r.Method == http.MethodPost && p == "/v1/vms":
		ok(`{"ip":"10.231.0.9"}`)
	default:
		ok(`{"ok":true}`)
	}
}

func (f *portWorker) pickLocked(avoid []uint16, prefer uint16) uint16 {
	inUse := map[uint16]bool{}
	for _, p := range f.held {
		inUse[p] = true
	}
	if f.honor && prefer != 0 && !inUse[prefer] {
		return prefer
	}
	skip := map[uint16]bool{}
	if f.honor {
		for _, p := range avoid {
			skip[p] = true
		}
	}
	for p := uint16(20000); p < 30000; p++ {
		if !inUse[p] && !skip[p] {
			return p
		}
	}
	return 0
}

func (f *portWorker) set(fn func(*portWorker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *portWorker) lastReq(t *testing.T) exposeReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("no expose request reached the worker")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *portWorker) reqCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *portWorker) unexposed(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.unexp {
		if k == key {
			return true
		}
	}
	return false
}

type qFixture struct {
	srv    *Server
	cfg    *config.Config
	fs     *faultStore
	sqlite *store.SQLite
	pw     *portWorker
	h      http.Handler
	addr   string
	host   string
	nodeID string
	now    *atomic.Int64
}

const qTenant = "tn-q"

func newQFixture(t *testing.T, honor bool) *qFixture {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
	}
	sqlite, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	if err := sqlite.CreateTenant(&store.Tenant{ID: qTenant, Name: "q", CreatedAt: 1}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	fs := &faultStore{Store: sqlite}
	srv := New(cfg, state.New(), fs)
	t.Cleanup(srv.stopAndWait)
	now := &atomic.Int64{}
	now.Store(time.Now().Unix())
	srv.st.Clock = now.Load
	pw := &portWorker{honor: honor, held: map[string]uint16{}}
	ts := httptest.NewServer(pw)
	t.Cleanup(ts.Close)
	nodeID := srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, now.Load())
	host, _ := agentclient.SplitHostPort(ts.URL)
	return &qFixture{srv: srv, cfg: cfg, fs: fs, sqlite: sqlite, pw: pw, h: srv.Handler(),
		addr: ts.URL, host: host, nodeID: nodeID, now: now}
}

// sandbox adds a running sandbox of tenant on nodeID with the given rows.
func (fx *qFixture) sandboxOn(nodeID, tenant string, exposes ...model.Expose) string {
	fx.srv.st.Lock()
	defer fx.srv.st.Unlock()
	sb := fx.srv.st.CreateSandbox("q", "default", nodeID, 1, 256, 1)
	sb.TenantID = tenant
	sb.State = model.StateRunning
	ip := "10.231.0.5"
	sb.IP = &ip
	sb.Exposes = append([]model.Expose(nil), exposes...)
	return sb.ID
}

func (fx *qFixture) sandbox(tenant string, exposes ...model.Expose) string {
	return fx.sandboxOn(fx.nodeID, tenant, exposes...)
}

func (fx *qFixture) expose(id, name string, port int) *httptest.ResponseRecorder {
	return doRequest(fx.h, "POST", "/api/v1/sandboxes/"+id+"/expose", fmt.Sprintf(`{"name":%q,"port":%d}`, name, port), adminAuth)
}

func (fx *qFixture) quarantine() []model.ReleasedPort {
	fx.srv.st.Lock()
	defer fx.srv.st.Unlock()
	return append([]model.ReleasedPort(nil), fx.srv.st.Quarantine...)
}

func (fx *qFixture) setQuarantine(q []model.ReleasedPort) {
	fx.srv.st.Lock()
	defer fx.srv.st.Unlock()
	fx.srv.st.Quarantine = q
}

func (fx *qFixture) rows(id string) []model.Expose {
	fx.srv.st.Lock()
	defer fx.srv.st.Unlock()
	if sb := fx.srv.st.FindSandbox(id); sb != nil {
		return append([]model.Expose(nil), sb.Exposes...)
	}
	return nil
}

func (fx *qFixture) saved(t *testing.T) *state.State {
	t.Helper()
	st := state.New()
	if err := fx.sqlite.LoadInto(st); err != nil {
		t.Fatalf("load: %v", err)
	}
	return st
}

func nodePortOf(t *testing.T, w *httptest.ResponseRecorder) uint16 {
	t.Helper()
	var v struct {
		NodePort uint16 `json:"node_port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.NodePort == 0 {
		t.Fatalf("expose answer %d %s: no node port", w.Code, w.Body)
	}
	return v.NodePort
}

func wantCreated(t *testing.T, w *httptest.ResponseRecorder, what string) uint16 {
	t.Helper()
	if w.Code != 201 {
		t.Fatalf("%s: %d %s, want 201", what, w.Code, w.Body)
	}
	return nodePortOf(t, w)
}

func wantConflict(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != 503 || w.Body.String() != `{"error":"node port conflict, retry"}` || w.Header().Get("Retry-After") != "5" {
		t.Errorf("%s: %d %s Retry-After=%q, want 503 node port conflict with Retry-After 5", what, w.Code, w.Body, w.Header().Get("Retry-After"))
	}
}

func wantChurn(t *testing.T, w *httptest.ResponseRecorder, retryAfter, what string) {
	t.Helper()
	if w.Code != 429 || w.Body.String() != `{"error":"expose churn limit reached"}` || w.Header().Get("Retry-After") != retryAfter {
		t.Errorf("%s: %d %s Retry-After=%q, want 429 expose churn limit reached with Retry-After %s", what, w.Code, w.Body, w.Header().Get("Retry-After"), retryAfter)
	}
}

func sortedPorts(ps []uint16) []uint16 {
	out := append([]uint16{}, ps...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// ---- R2': every removal path quarantines, once per port ----

func TestEveryRemovalPathQuarantinesItsPort(t *testing.T) {
	entry := func(fx *qFixture, port uint16, id string, guest uint16) []model.ReleasedPort {
		return []model.ReleasedPort{{Host: fx.host, Port: port, SandboxID: id, GuestPort: guest, TenantID: qTenant, ReleasedAt: fx.now.Load()}}
	}

	t.Run("unexpose", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandbox(qTenant)
		port := wantCreated(t, fx.expose(id, "web", 80), "expose")
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/web", "", adminAuth); w.Code != 204 {
			t.Fatalf("unexpose: %d %s", w.Code, w.Body)
		}
		if got, want := fx.quarantine(), entry(fx, port, id, 80); !reflect.DeepEqual(got, want) {
			t.Errorf("quarantine: %+v, want %+v", got, want)
		}
	})

	t.Run("two names on one guest port", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandbox(qTenant)
		pa := wantCreated(t, fx.expose(id, "a", 80), "expose a")
		if pb := wantCreated(t, fx.expose(id, "b", 80), "expose b"); pb != pa {
			t.Fatalf("names on one guest port got ports %d and %d", pa, pb)
		}
		doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/a", "", adminAuth)
		if q := fx.quarantine(); len(q) != 0 {
			t.Fatalf("removing one of two names quarantined %+v", q)
		}
		doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/b", "", adminAuth)
		if got, want := fx.quarantine(), entry(fx, pa, id, 80); !reflect.DeepEqual(got, want) {
			t.Errorf("after the second name: %+v, want %+v", got, want)
		}
	})

	t.Run("delete", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandbox(qTenant)
		port := wantCreated(t, fx.expose(id, "web", 80), "expose")
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id, "", adminAuth); w.Code != 204 {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		if got, want := fx.quarantine(), entry(fx, port, id, 80); !reflect.DeepEqual(got, want) {
			t.Errorf("quarantine: %+v, want %+v", got, want)
		}
	})

	t.Run("auto-delete", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandbox(qTenant)
		port := wantCreated(t, fx.expose(id, "web", 80), "expose")
		now := fx.now.Load()
		func() {
			fx.srv.st.Lock()
			defer fx.srv.st.Unlock()
			sb := fx.srv.st.FindSandbox(id)
			sb.State = model.StateSleeping
			sb.SleptAt = now - 100
			sb.AsleepDeleteS = 30
		}()
		fx.srv.lifecycleSweep(now)
		if fx.rows(id) != nil {
			t.Fatal("sandbox not auto-deleted")
		}
		if got, want := fx.quarantine(), entry(fx, port, id, 80); !reflect.DeepEqual(got, want) {
			t.Errorf("quarantine: %+v, want %+v", got, want)
		}
	})

	t.Run("clean-up after a create or fork whose last save failed", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandbox(qTenant)
		port := wantCreated(t, fx.expose(id, "web", 80), "expose")
		fx.srv.st.SetSandboxState(id, model.StateError) // what the save's undo leaves
		fx.srv.removeUnsavedVM(fx.addr, id, "req-cleanup")
		if got, want := fx.quarantine(), entry(fx, port, id, 80); !reflect.DeepEqual(got, want) {
			t.Errorf("quarantine: %+v, want %+v", got, want)
		}
	})

	t.Run("a sandbox without a node releases nothing", func(t *testing.T) {
		fx := newQFixture(t, true)
		id := fx.sandboxOn("node-gone", qTenant,
			model.Expose{Name: "a", GuestPort: 80, NodePort: 20000},
			model.Expose{Name: "b", GuestPort: 81, NodePort: 20001})
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/a", "", adminAuth); w.Code != 204 {
			t.Fatalf("unexpose: %d %s", w.Code, w.Body)
		}
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id, "", adminAuth); w.Code != 204 {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		if q := fx.quarantine(); len(q) != 0 {
			t.Errorf("quarantine: %+v, want none", q)
		}
	})
}

// The entry is saved in the same transaction as the row's removal, and it
// still keeps the port away after a restart.
func TestQuarantineIsSavedWithTheRemovalAndSurvivesARestart(t *testing.T) {
	fx := newQFixture(t, true)
	id := fx.sandbox(qTenant)
	port := wantCreated(t, fx.expose(id, "web", 80), "expose")

	fx.fs.failAll()
	wantApplied(t, doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/web", "", adminAuth), "unexpose, save failing")
	saved := fx.saved(t)
	if sb := saved.FindSandbox(id); sb == nil || len(sb.Exposes) != 1 || len(saved.Quarantine) != 0 {
		t.Fatalf("after the failed save: rows %+v quarantine %+v, want the row and no entry", sb, saved.Quarantine)
	}
	fx.fs.heal()
	if err := fx.srv.recoverStore(); err != nil {
		t.Fatalf("recovery save: %v", err)
	}
	saved = fx.saved(t)
	if sb := saved.FindSandbox(id); sb == nil || len(sb.Exposes) != 0 || len(saved.Quarantine) != 1 || saved.Quarantine[0].Port != port {
		t.Fatalf("after the next save: rows %+v quarantine %+v, want no row and the entry", sb, saved.Quarantine)
	}

	// A restart: a new server on what the database holds.
	srv2 := New(fx.cfg, saved, fx.sqlite)
	t.Cleanup(srv2.stopAndWait)
	b := func() string {
		srv2.st.Lock()
		defer srv2.st.Unlock()
		sb := srv2.st.CreateSandbox("b", "default", fx.nodeID, 1, 256, 1)
		sb.State = model.StateRunning
		ip := "10.231.0.6"
		sb.IP = &ip
		return sb.ID
	}()
	w := doRequest(srv2.Handler(), "POST", "/api/v1/sandboxes/"+b+"/expose", `{"name":"web","port":80}`, adminAuth)
	if got := wantCreated(t, w, "expose after restart"); got == port {
		t.Errorf("after a restart the released port %d went to another sandbox", port)
	}
	if req := fx.pw.lastReq(t); !reflect.DeepEqual(req.avoid, []uint16{port}) {
		t.Errorf("avoid after restart: %v, want [%d]", req.avoid, port)
	}
}

// ---- R3': the request ----

func TestExposeRequestCarriesAvoidAndPreferForItsHost(t *testing.T) {
	fx := newQFixture(t, true)
	sameHost := fx.srv.st.RegisterNode("worker-1-reinstalled", "http://"+fx.host+":1", 4, 8192, fx.now.Load())
	otherHost := fx.srv.st.RegisterNode("worker-2", "10.9.9.9:9090", 4, 8192, fx.now.Load())
	s := fx.sandbox(qTenant,
		model.Expose{Name: "web", GuestPort: 80, NodePort: 20500},
		model.Expose{Name: "api", GuestPort: 81, NodePort: 20501})
	fx.pw.set(func(f *portWorker) { f.held[s+"/80"] = 20500 })
	fx.sandboxOn(sameHost, qTenant, model.Expose{Name: "x", GuestPort: 90, NodePort: 20600})
	fx.sandboxOn(otherHost, qTenant, model.Expose{Name: "y", GuestPort: 90, NodePort: 20700})
	now := fx.now.Load()
	fx.setQuarantine([]model.ReleasedPort{
		{Host: fx.host, Port: 20800, SandboxID: "sb-x", GuestPort: 80, TenantID: qTenant, ReleasedAt: now - 10},
		{Host: "10.9.9.9", Port: 20900, SandboxID: "sb-y", GuestPort: 80, ReleasedAt: now - 10},
		{Host: fx.host, Port: 20801, SandboxID: s, GuestPort: 82, TenantID: qTenant, ReleasedAt: now - 10},
		{Host: fx.host, Port: 20802, SandboxID: s, GuestPort: 84, TenantID: qTenant, ReleasedAt: now - 10},
	})

	// Same guest port as an existing row: its own port is not avoided, and
	// nothing is preferred (it holds one).
	if got := wantCreated(t, fx.expose(s, "web2", 80), "expose web2"); got != 20500 {
		t.Errorf("web2: port %d, want the idempotent 20500", got)
	}
	req := fx.pw.lastReq(t)
	if want := []uint16{20501, 20600, 20800, 20801, 20802}; !reflect.DeepEqual(sortedPorts(req.avoid), want) {
		t.Errorf("avoid: %v, want %v (rows of every sandbox on the host, quarantined ports; not its own for this guest port, nothing of the other host)", req.avoid, want)
	}
	if strings.Contains(req.raw, "prefer") {
		t.Errorf("prefer sent with nothing to prefer: %s", req.raw)
	}

	// The port this sandbox last held for this guest port is preferred (and
	// not in avoid); when the worker returns it, its entry goes.
	if got := wantCreated(t, fx.expose(s, "svc", 82), "expose svc"); got != 20801 {
		t.Errorf("svc: port %d, want the preferred 20801", got)
	}
	req = fx.pw.lastReq(t)
	if req.prefer != 20801 {
		t.Errorf("prefer: %d, want 20801 (%s)", req.prefer, req.raw)
	}
	if want := []uint16{20500, 20501, 20600, 20800, 20802}; !reflect.DeepEqual(sortedPorts(req.avoid), want) {
		t.Errorf("avoid with prefer: %v, want %v", req.avoid, want)
	}
	for _, q := range fx.quarantine() {
		if q.Port == 20801 {
			t.Errorf("the preferred port is still quarantined after its owner got it back: %+v", q)
		}
	}

	// Another guest port of the same sandbox, and the same guest port of
	// another sandbox: no prefer.
	wantCreated(t, fx.expose(s, "z", 83), "expose z")
	if req := fx.pw.lastReq(t); req.prefer != 0 || !containsPort(req.avoid, 20802) {
		t.Errorf("other guest port: prefer %d avoid %v, want no prefer and 20802 avoided", req.prefer, req.avoid)
	}
	v := fx.sandbox(qTenant)
	wantCreated(t, fx.expose(v, "w", 84), "expose on another sandbox")
	if req := fx.pw.lastReq(t); req.prefer != 0 || !containsPort(req.avoid, 20802) || !containsPort(req.avoid, 20500) {
		t.Errorf("another sandbox: prefer %d avoid %v, want no prefer, 20802 and 20500 avoided", req.prefer, req.avoid)
	}
}

func containsPort(ps []uint16, p uint16) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

// A port another (sandbox, guest port) released later is never preferred: its
// hostname would reach this sandbox.
func TestPreferSkipsAPortSomeoneElseReleasedToo(t *testing.T) {
	fx := newQFixture(t, true)
	s := fx.sandbox(qTenant)
	now := fx.now.Load()
	fx.setQuarantine([]model.ReleasedPort{
		{Host: fx.host, Port: 20801, SandboxID: s, GuestPort: 82, ReleasedAt: now - 100},
		{Host: fx.host, Port: 20801, SandboxID: "sb-x", GuestPort: 80, ReleasedAt: now - 10},
	})
	got := wantCreated(t, fx.expose(s, "svc", 82), "expose")
	if req := fx.pw.lastReq(t); req.prefer != 0 || got == 20801 {
		t.Errorf("prefer %d, port %d: a port another sandbox released must not be preferred", req.prefer, got)
	}
}

// ---- R5': the answer ----

func TestExposeAnswerIsCheckedBeforeItIsRecorded(t *testing.T) {
	t.Run("a port another sandbox's row holds", func(t *testing.T) {
		fx := newQFixture(t, true)
		logs := captureLog(t)
		a := fx.sandbox(qTenant, model.Expose{Name: "web", GuestPort: 80, NodePort: 20000})
		b := fx.sandbox(qTenant)
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20000} })
		wantConflict(t, fx.expose(b, "web", 80), "expose onto a live row")
		if rows := fx.rows(a); len(rows) != 0 {
			t.Errorf("the stale row was kept: %+v", rows)
		}
		if rows := fx.rows(b); len(rows) != 0 {
			t.Errorf("the refused mapping was recorded: %+v", rows)
		}
		q := fx.quarantine()
		if len(q) != 1 || q[0].Port != 20000 || q[0].SandboxID != a || q[0].GuestPort != 80 {
			t.Errorf("quarantine: %+v, want the stale row's port under its sandbox", q)
		}
		if !fx.pw.unexposed(b + "/80") {
			t.Error("the new mapping was not removed from the worker")
		}
		if saved := fx.saved(t); len(saved.FindSandbox(a).Exposes) != 0 || len(saved.Quarantine) != 1 {
			t.Errorf("the stale row's removal was not saved: %+v %+v", saved.FindSandbox(a).Exposes, saved.Quarantine)
		}
		if l := logs(); !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "sandbox="+b) || !strings.Contains(l, "stale_sandbox="+a) {
			t.Errorf("no error naming both sandboxes:\n%s", l)
		}
		if got := wantCreated(t, fx.expose(b, "web", 80), "retry"); got == 20000 {
			t.Errorf("retry got the quarantined port %d", got)
		}
	})

	t.Run("a quarantined port from an agent that honours avoid", func(t *testing.T) {
		fx := newQFixture(t, true)
		b := fx.sandbox(qTenant)
		fx.setQuarantine([]model.ReleasedPort{{Host: fx.host, Port: 20010, SandboxID: "sb-x", GuestPort: 80, ReleasedAt: fx.now.Load() - 10}})
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20010} })
		wantConflict(t, fx.expose(b, "web", 80), "broken promise")
		if rows := fx.rows(b); len(rows) != 0 {
			t.Errorf("recorded: %+v", rows)
		}
		if !fx.pw.unexposed(b + "/80") {
			t.Error("the new mapping was not removed from the worker")
		}
		if q := fx.quarantine(); len(q) != 1 {
			t.Errorf("quarantine changed: %+v", q)
		}
	})

	t.Run("a quarantined port from an older agent", func(t *testing.T) {
		fx := newQFixture(t, false)
		logs := captureLog(t)
		b := fx.sandbox(qTenant)
		fx.setQuarantine([]model.ReleasedPort{{Host: fx.host, Port: 20010, SandboxID: "sb-x", GuestPort: 80, ReleasedAt: fx.now.Load() - 10}})
		fx.pw.set(func(f *portWorker) { f.script = []uint16{20010} })
		if got := wantCreated(t, fx.expose(b, "web", 80), "older agent"); got != 20010 {
			t.Errorf("port %d, want 20010 accepted", got)
		}
		if l := logs(); !strings.Contains(l, "level=WARN") || !strings.Contains(l, "node_port=20010") || !strings.Contains(l, "released_ago_s=10") {
			t.Errorf("no warning naming the port and its age:\n%s", l)
		}
	})

	t.Run("ports exhausted", func(t *testing.T) {
		fx := newQFixture(t, true)
		b := fx.sandbox(qTenant)
		fx.pw.set(func(f *portWorker) { f.exhausted = true })
		w := fx.expose(b, "web", 80)
		if w.Code != 503 || w.Body.String() != `{"error":"no free node port on this worker"}` || w.Header().Get("Retry-After") != "30" {
			t.Errorf("exhausted: %d %s Retry-After=%q, want 503 no free node port with Retry-After 30", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
	})
}

// ---- R6': churn caps ----

func fillQuarantine(n int, fn func(i int) model.ReleasedPort) []model.ReleasedPort {
	q := make([]model.ReleasedPort, n)
	for i := range q {
		q[i] = fn(i)
	}
	return q
}

func TestChurnCaps(t *testing.T) {
	t.Run("per tenant per host", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(qTenant)
		now := fx.now.Load()
		q := fillQuarantine(maxQuarantinedPerTenantHost, func(i int) model.ReleasedPort {
			at := now - 10
			if i == 7 {
				at = now - 100 // the oldest: expires in 800 s
			}
			return model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: fmt.Sprintf("sb-old-%d", i), GuestPort: 80, TenantID: qTenant, ReleasedAt: at}
		})
		fx.setQuarantine(q)
		wantChurn(t, fx.expose(s, "web", 80), "800", "at the tenant cap")
		if n := fx.pw.reqCount(); n != 0 {
			t.Errorf("the worker was asked %d times past the cap", n)
		}
		// Entries of another host do not count against this one.
		q[0].Host = "10.9.9.9"
		fx.setQuarantine(q)
		wantCreated(t, fx.expose(s, "web", 80), "one under the cap on this host")
	})

	t.Run("admin is unmetered for the tenant cap", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(adminTenant)
		now := fx.now.Load()
		fx.setQuarantine(fillQuarantine(maxQuarantinedPerTenantHost+50, func(i int) model.ReleasedPort {
			return model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: fmt.Sprintf("sb-old-%d", i), GuestPort: 80, TenantID: adminTenant, ReleasedAt: now - 10}
		}))
		wantCreated(t, fx.expose(s, "web", 80), "admin past the tenant cap")
	})

	t.Run("per sandbox, admin too", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(adminTenant)
		now := fx.now.Load()
		fx.setQuarantine(fillQuarantine(maxQuarantinedPerSandbox, func(i int) model.ReleasedPort {
			at := now - 50
			if i == 3 {
				at = now - 300 // the oldest: expires in 600 s
			}
			return model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: s, GuestPort: uint16(1000 + i), ReleasedAt: at}
		}))
		wantChurn(t, fx.expose(s, "web", 80), "600", "at the sandbox cap")
	})

	t.Run("a request that needs no fresh port is not capped", func(t *testing.T) {
		fx := newQFixture(t, true)
		s := fx.sandbox(qTenant, model.Expose{Name: "web", GuestPort: 80, NodePort: 20000})
		fx.pw.set(func(f *portWorker) { f.held[s+"/80"] = 20000 })
		now := fx.now.Load()
		// One over the cap: the preferred entry leaves quarantine below, and
		// the sandbox must still be at the cap afterwards.
		q := fillQuarantine(maxQuarantinedPerSandbox+1, func(i int) model.ReleasedPort {
			return model.ReleasedPort{Host: fx.host, Port: uint16(21000 + i), SandboxID: s, GuestPort: uint16(1000 + i), TenantID: qTenant, ReleasedAt: now - 10}
		})
		q[0] = model.ReleasedPort{Host: fx.host, Port: 20900, SandboxID: s, GuestPort: 82, TenantID: qTenant, ReleasedAt: now - 10}
		fx.setQuarantine(q)
		if got := wantCreated(t, fx.expose(s, "web2", 80), "existing mapping at the cap"); got != 20000 {
			t.Errorf("web2: %d, want 20000", got)
		}
		if got := wantCreated(t, fx.expose(s, "svc", 82), "preferred port at the cap"); got != 20900 {
			t.Errorf("svc: %d, want the preferred 20900", got)
		}
		wantChurn(t, fx.expose(s, "new", 83), "890", "a fresh port at the cap")
	})

	// The fork's re-expose goes through the same helper: at the tenant cap the
	// fork still succeeds and the child has no exposes (its parent keeps its
	// own); the worker is never asked for a port.
	t.Run("fork re-expose", func(t *testing.T) {
		srv, fa, nodeID := newForkTestServer(t)
		auth, parent := forkTenantFixture(t, srv, nodeID, 3, 3)
		now := time.Now().Unix()
		func() {
			srv.st.Lock()
			defer srv.st.Unlock()
			tenant := srv.st.FindSandbox(parent).TenantID
			host, _ := srv.st.NodeHostLocked(srv.st.FindSandbox(parent))
			srv.st.Quarantine = fillQuarantine(maxQuarantinedPerTenantHost, func(i int) model.ReleasedPort {
				return model.ReleasedPort{Host: host, Port: uint16(22000 + i), SandboxID: fmt.Sprintf("sb-old-%d", i), GuestPort: 80, TenantID: tenant, ReleasedAt: now - 10}
			})
		}()
		w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"child"}`, auth)
		if w.Code != 201 {
			t.Fatalf("fork: %d %s", w.Code, w.Body)
		}
		if got := forkChildExposes(t, w.Body.Bytes()); len(got) != 0 {
			t.Errorf("child exposes at the churn cap: %+v, want none", got)
		}
		if n := fa.exposeCalls(); n != 0 {
			t.Errorf("worker expose calls at the churn cap: %d, want 0", n)
		}
		srv.st.Lock()
		n := len(srv.st.FindSandbox(parent).Exposes)
		srv.st.Unlock()
		if n != 3 {
			t.Errorf("parent exposes: %d, want 3", n)
		}
	})
}

// ---- The incident: delete A, expose B on the same worker ----

func TestReleasedPortDoesNotGoToTheNextSandbox(t *testing.T) {
	t.Run("new agent", func(t *testing.T) {
		fx := newQFixture(t, true)
		a, b := fx.sandbox(qTenant), fx.sandbox(qTenant)
		pa := wantCreated(t, fx.expose(a, "web", 80), "expose A")
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+a, "", adminAuth); w.Code != 204 {
			t.Fatalf("delete A: %d %s", w.Code, w.Body)
		}
		if pb := wantCreated(t, fx.expose(b, "web", 80), "expose B"); pb == pa {
			t.Errorf("B got A's port %d within the quarantine", pa)
		}
	})

	t.Run("new agent, A's delete timed out in feluccad", func(t *testing.T) {
		fx := newQFixture(t, true)
		a, b := fx.sandbox(qTenant), fx.sandbox(qTenant)
		pa := wantCreated(t, fx.expose(a, "web", 80), "expose A")
		fx.pw.set(func(f *portWorker) { f.hangUpOnDelete = true })
		if w := doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+a, "", adminAuth); w.Code != 502 {
			t.Fatalf("delete A: %d %s, want 502 (the call failed)", w.Code, w.Body)
		}
		if len(fx.rows(a)) != 1 {
			t.Fatal("A's row was not kept")
		}
		if pb := wantCreated(t, fx.expose(b, "web", 80), "expose B"); pb == pa {
			t.Errorf("B got A's port %d while A's row still holds it", pa)
		}
	})

	// The documented behaviour with an agent that ignores avoid: a merely
	// quarantined port is accepted with a warning (the exposure ends when the
	// agent is upgraded).
	t.Run("older agent", func(t *testing.T) {
		fx := newQFixture(t, false)
		logs := captureLog(t)
		a, b := fx.sandbox(qTenant), fx.sandbox(qTenant)
		pa := wantCreated(t, fx.expose(a, "web", 80), "expose A")
		doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+a, "", adminAuth)
		if pb := wantCreated(t, fx.expose(b, "web", 80), "expose B"); pb != pa {
			t.Errorf("older agent: B got %d, want A's %d (lowest free)", pb, pa)
		}
		if l := logs(); !strings.Contains(l, fmt.Sprintf("node_port=%d", pa)) || !strings.Contains(l, "released_ago_s=") {
			t.Errorf("no warning:\n%s", l)
		}
	})

	// ... and a port a live row holds takes the conflict path: the stale row
	// goes into quarantine and B is told to retry; the retry then gets the
	// merely quarantined port, with the warning.
	t.Run("older agent, A's delete timed out in feluccad", func(t *testing.T) {
		fx := newQFixture(t, false)
		a, b := fx.sandbox(qTenant), fx.sandbox(qTenant)
		pa := wantCreated(t, fx.expose(a, "web", 80), "expose A")
		fx.pw.set(func(f *portWorker) { f.hangUpOnDelete = true })
		doRequest(fx.h, "DELETE", "/api/v1/sandboxes/"+a, "", adminAuth)
		fx.pw.set(func(f *portWorker) { f.hangUpOnDelete = false })
		wantConflict(t, fx.expose(b, "web", 80), "expose B onto A's stale row")
		if len(fx.rows(a)) != 0 {
			t.Error("A's stale row was kept")
		}
		if q := fx.quarantine(); len(q) != 1 || q[0].SandboxID != a || q[0].Port != pa {
			t.Errorf("quarantine: %+v", q)
		}
		if pb := wantCreated(t, fx.expose(b, "web", 80), "retry"); pb != pa {
			t.Errorf("retry with an older agent: %d, want %d", pb, pa)
		}
	})
}

// ---- R12: the drop is a durable change like any other ----

func TestDynamicExposeDropWaitsWhileTheStoreIsFailing(t *testing.T) {
	fx := newQFixture(t, true)
	id := fx.sandbox(qTenant)
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		fx.srv.st.FindSandbox(id).AllowDynamicPorts = true
	}()
	if w := doRequest(fx.h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+id+`","port":8080}`, adminAuth); w.Code != 200 {
		t.Fatalf("ensure: %d %s", w.Code, w.Body)
	}
	now := fx.now.Load()
	func() {
		fx.srv.st.Lock()
		defer fx.srv.st.Unlock()
		sb := fx.srv.st.FindSandbox(id)
		sb.State = model.StateSleeping
		sb.SleptAt = now - 1000
	}()

	fx.fs.failAll()
	if err := fx.srv.saveState(); err == nil {
		t.Fatal("save did not fail")
	}
	fx.srv.lifecycleSweep(now)
	if fx.pw.unexposed(id+"/8080") || len(fx.rows(id)) != 1 {
		t.Fatalf("dropped while the store was failing: rows %+v", fx.rows(id))
	}

	fx.fs.heal()
	if err := fx.srv.recoverStore(); err != nil {
		t.Fatalf("recovery save: %v", err)
	}
	fx.srv.lifecycleSweep(now)
	if !fx.pw.unexposed(id+"/8080") || len(fx.rows(id)) != 0 {
		t.Fatalf("not dropped once the store recovered: rows %+v", fx.rows(id))
	}
	if saved := fx.saved(t); len(saved.FindSandbox(id).Exposes) != 0 || len(saved.Quarantine) != 1 {
		t.Errorf("drop not saved: %+v %+v", saved.FindSandbox(id).Exposes, saved.Quarantine)
	}
}

// ---- /metrics ----

func TestMetricNodePortsQuarantined(t *testing.T) {
	fx := newQFixture(t, true)
	now := fx.now.Load()
	fx.setQuarantine([]model.ReleasedPort{
		{Host: fx.host, Port: 20001, SandboxID: "sb-a", ReleasedAt: now - 10},
		{Host: fx.host, Port: 20001, SandboxID: "sb-x", ReleasedAt: now - 20}, // same port: counted once
		{Host: fx.host, Port: 20002, SandboxID: "sb-b", ReleasedAt: now - 899},
		{Host: fx.host, Port: 20003, SandboxID: "sb-c", ReleasedAt: now - 900}, // expired
	})
	w := doRequest(fx.h, "GET", "/metrics", "", adminAuth)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "\nfelucca_node_ports_quarantined 2\n") {
		t.Errorf("/metrics: %d, want felucca_node_ports_quarantined 2:\n%s", w.Code, w.Body)
	}
}
