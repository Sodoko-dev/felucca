// Tests for FLU-5: a save that fails, fails the request. Internal package so
// they can wrap the store, reach the save path directly and swap the agent hook.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// ---- Fixtures ----

// faultStore wraps the real SQLite store. Its snapshot save can be made to let
// `pass` saves through and then fail `fail` of them (<0: every one), or to wait
// until released; its key lookup can be made to fail; its exec batch write can
// be made to fail.
type faultStore struct {
	store.Store
	mu        sync.Mutex
	pass      int
	fail      int
	block     chan struct{}
	keyErr    bool
	tenantErr bool
	batchErr  bool
	saveCalls int
}

var errInjected = errors.New("injected: database is locked")

func (f *faultStore) SaveSnapshot(snap *state.Snapshot, usage []store.UsageEvent) error {
	f.mu.Lock()
	f.saveCalls++
	block := f.block
	failing := false
	switch {
	case f.pass > 0:
		f.pass--
	case f.fail != 0:
		failing = true
		if f.fail > 0 {
			f.fail--
		}
	}
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if failing {
		return errInjected
	}
	return f.Store.SaveSnapshot(snap, usage)
}

func (f *faultStore) LookupKeyByHash(hash string) (string, error) {
	f.mu.Lock()
	fail := f.keyErr
	f.mu.Unlock()
	if fail {
		return "", errInjected
	}
	return f.Store.LookupKeyByHash(hash)
}

func (f *faultStore) GetTenant(id string) (*store.Tenant, error) {
	f.mu.Lock()
	fail := f.tenantErr
	f.mu.Unlock()
	if fail {
		return nil, errInjected
	}
	return f.Store.GetTenant(id)
}

func (f *faultStore) AppendUsageBatch(events []store.UsageEvent) error {
	f.mu.Lock()
	fail := f.batchErr
	f.mu.Unlock()
	if fail {
		return errInjected
	}
	return f.Store.AppendUsageBatch(events)
}

// failAfter lets `pass` saves through, then fails `n` (<0: all).
func (f *faultStore) failAfter(pass, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pass, f.fail = pass, n
}

func (f *faultStore) failAll() { f.failAfter(0, -1) }
func (f *faultStore) heal()    { f.failAfter(0, 0) }

// fsWorker is a fake worker agent for every call feluccad makes: it records
// "METHOD path" and answers success unless told otherwise.
type fsWorker struct {
	mu          sync.Mutex
	calls       []string
	failCreate  bool
	failFork    bool
	deleteDelay time.Duration
	port        int
}

func (f *fsWorker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+p)
		failCreate, failFork, delay := f.failCreate, f.failFork, f.deleteDelay
		f.port++
		port := 20000 + f.port
		f.mu.Unlock()
		ok := func(body string) { w.WriteHeader(200); _, _ = w.Write([]byte(body)) }
		switch {
		case r.Method == http.MethodPost && p == "/v1/vms":
			if failCreate {
				w.WriteHeader(500)
				return
			}
			ok(`{"ip":"10.231.0.9"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/fork"):
			if failFork {
				w.WriteHeader(500)
				return
			}
			ok(`{"ip":"10.231.0.10"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/expose"):
			ok(fmt.Sprintf(`{"node_port":%d}`, port))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/exec"):
			ok(`{"exit_code":0,"stdout":"","stderr":""}`)
		case r.Method == http.MethodDelete && !strings.HasSuffix(p, "/expose"):
			time.Sleep(delay)
			ok(`{"ok":true}`)
		default:
			ok(`{"ok":true,"wake_ms":7}`)
		}
	}
}

func (f *fsWorker) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fsWorker) count() int { return len(f.snapshot()) }

func (f *fsWorker) since(n int) []string { return f.snapshot()[n:] }

func (f *fsWorker) set(fn func(*fsWorker)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

type persistFixture struct {
	srv    *Server
	fs     *faultStore
	sqlite *store.SQLite
	fw     *fsWorker
	path   string
	h      http.Handler
}

func newPersistFixture(t *testing.T) *persistFixture {
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
	fs := &faultStore{Store: sqlite}
	srv := New(cfg, state.New(), fs)
	t.Cleanup(srv.stopBackground)
	fw := &fsWorker{}
	ts := httptest.NewServer(fw.handler())
	t.Cleanup(ts.Close)
	srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, time.Now().Unix())
	if err := srv.save(nil); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	return &persistFixture{srv: srv, fs: fs, sqlite: sqlite, fw: fw, path: cfg.DBPath, h: srv.Handler()}
}

func (p *persistFixture) do(method, path, body string) *httptest.ResponseRecorder {
	return doRequest(p.h, method, path, body, adminAuth)
}

// create makes a running sandbox through the API (healthy store).
func (p *persistFixture) create(t *testing.T, body string) string {
	t.Helper()
	w := p.do("POST", "/api/v1/sandboxes", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var sb model.Sandbox
	if err := json.Unmarshal(w.Body.Bytes(), &sb); err != nil {
		t.Fatalf("create body: %v", err)
	}
	return sb.ID
}

func (p *persistFixture) mem(id string) *model.Sandbox {
	p.srv.st.Lock()
	defer p.srv.st.Unlock()
	if sb := p.srv.st.FindSandbox(id); sb != nil {
		c := *sb
		return &c
	}
	return nil
}

// saved reads the working set back from the database.
func (p *persistFixture) saved(t *testing.T) *state.State {
	t.Helper()
	st := state.New()
	if err := p.sqlite.LoadInto(st); err != nil {
		t.Fatalf("load: %v", err)
	}
	return st
}

func (p *persistFixture) savedSandbox(t *testing.T, id string) *model.Sandbox {
	t.Helper()
	st := p.saved(t)
	return st.FindSandbox(id)
}

// events returns the saved usage events of one sandbox, in order.
func (p *persistFixture) events(t *testing.T, id string) []string {
	t.Helper()
	evs, err := p.sqlite.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatalf("list usage: %v", err)
	}
	var out []string
	for _, e := range evs {
		if e.SandboxID == id && e.Event != "exec" {
			out = append(out, e.Event)
		}
	}
	return out
}

// healAndSave heals the store and saves, as the recovery loop would.
func (p *persistFixture) healAndSave(t *testing.T) {
	t.Helper()
	p.fs.heal()
	if err := p.srv.save(nil); err != nil {
		t.Fatalf("save after heal: %v", err)
	}
	if p.srv.storeFailing() {
		t.Fatal("store still failing after a successful save")
	}
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", d, what)
}

func wantStoreUnavailable(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != 503 || w.Body.String() != `{"error":"store unavailable"}` || w.Header().Get("Retry-After") != "5" {
		t.Errorf("%s: %d %s Retry-After=%q, want 503 store unavailable with Retry-After 5", what, w.Code, w.Body, w.Header().Get("Retry-After"))
	}
}

func wantApplied(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != 503 || w.Body.String() != `{"error":"applied but not saved","applied":true}` || w.Header().Get("Retry-After") != "5" {
		t.Errorf("%s: %d %s Retry-After=%q, want 503 applied but not saved with Retry-After 5", what, w.Code, w.Body, w.Header().Get("Retry-After"))
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasPrefixCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// ---- P13 ----

// One heartbeat carrying a value database/sql cannot bind (a uint64 with the
// high bit set) used to make EVERY later save fail: the value sits in memory
// and is rewritten by each snapshot. Both halves of the fix are pinned: the
// handler refuses it, and the store clamps whatever reaches it anyway.
func TestHugeMemoryValueCannotPoisonEverySave(t *testing.T) {
	srv, _, _ := newExposeTestServer(t)
	nodeID := srv.st.Nodes[0].ID

	body := fmt.Sprintf(`{"id":%q,"mem_free_mib":%d,"vm_count":1,"pool_size":0}`, nodeID, uint64(1)<<63)
	w := doRequest(srv.Handler(), "POST", "/api/v1/agents/heartbeat", body, adminAuth)
	if w.Code != 400 {
		t.Errorf("heartbeat with mem_free_mib 2^63: %d %s, want 400", w.Code, w.Body)
	}

	// The store half, reached directly: whatever is in memory must be savable.
	srv.st.Heartbeat(nodeID, uint64(1)<<63, 1, 0, time.Now().Unix())
	if err := srv.save(nil); err != nil {
		t.Fatalf("save after a 2^63 heartbeat: %v — one value in memory now fails every save", err)
	}
}

// The bound is 2^40 MiB on both routes that report node memory.
func TestNodeMemoryBounds(t *testing.T) {
	srv, _, _ := newExposeTestServer(t)
	h := srv.Handler()
	nodeID := srv.st.Nodes[0].ID
	hb := func(v uint64) int {
		return doRequest(h, "POST", "/api/v1/agents/heartbeat",
			fmt.Sprintf(`{"id":%q,"mem_free_mib":%d}`, nodeID, v), adminAuth).Code
	}
	if c := hb(1 << 40); c != 200 {
		t.Errorf("heartbeat at the bound: %d, want 200", c)
	}
	if c := hb(1<<40 + 1); c != 400 {
		t.Errorf("heartbeat above the bound: %d, want 400", c)
	}
	reg := func(v uint64) *httptest.ResponseRecorder {
		return doRequest(h, "POST", "/api/v1/agents/register",
			fmt.Sprintf(`{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":%d}`, v), adminAuth)
	}
	if w := reg(1<<40 + 1); w.Code != 400 || !strings.Contains(w.Body.String(), "mem_total_mib") {
		t.Errorf("register above the bound: %d %s, want 400", w.Code, w.Body)
	}
	if w := reg(1 << 40); w.Code != 200 {
		t.Errorf("register at the bound: %d %s, want 200", w.Code, w.Body)
	}
}

// ---- P5: every save site, when its save fails ----

func TestEverySaveSiteWhenTheSaveFails(t *testing.T) {
	t.Run("create/first save", func(t *testing.T) {
		p := newPersistFixture(t)
		p.fs.failAll()
		n := p.fw.count()
		wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`), "create, first save failing")
		if calls := p.fw.since(n); len(calls) != 0 {
			t.Errorf("worker called although the record was never saved: %v", calls)
		}
		if l := len(p.srv.st.Snapshot().Sandboxes); l != 0 {
			t.Errorf("memory holds %d sandboxes after the undo, want 0", l)
		}
		p.healAndSave(t)
		if l := len(p.saved(t).Sandboxes); l != 0 {
			t.Errorf("database holds %d sandboxes, want 0", l)
		}
	})

	t.Run("create/worker failed", func(t *testing.T) {
		p := newPersistFixture(t)
		p.fw.set(func(f *fsWorker) { f.failCreate = true })
		p.fs.failAfter(1, -1)
		w := p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`)
		if w.Code != 502 {
			t.Errorf("create whose worker failed and whose save failed: %d %s, want the 502", w.Code, w.Body)
		}
		snap := p.srv.st.Snapshot()
		if len(snap.Sandboxes) != 1 || snap.Sandboxes[0].State != model.StateError {
			t.Fatalf("memory: %+v, want one sandbox in error", snap.Sandboxes)
		}
		id := snap.Sandboxes[0].ID
		p.healAndSave(t)
		if sb := p.savedSandbox(t, id); sb == nil || sb.State != model.StateError {
			t.Errorf("saved: %+v, want error", sb)
		}
		if evs := p.events(t, id); len(evs) != 0 {
			t.Errorf("events: %v, want none", evs)
		}
	})

	t.Run("create/last save", func(t *testing.T) {
		p := newPersistFixture(t)
		p.fs.failAfter(1, -1)
		n := p.fw.count()
		wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`), "create, last save failing")
		waitFor(t, "the background delete on the worker", 3*time.Second, func() bool {
			return hasPrefixCall(p.fw.since(n), "DELETE /v1/vms/sb-")
		})
		calls := p.fw.since(n)
		id := strings.TrimPrefix(calls[len(calls)-1], "DELETE /v1/vms/")
		if calls[0] != "POST /v1/vms" {
			t.Errorf("worker calls: %v, want the create then the delete", calls)
		}
		waitFor(t, "the record to be removed", 3*time.Second, func() bool { return p.mem(id) == nil })
		p.healAndSave(t)
		if sb := p.savedSandbox(t, id); sb != nil {
			t.Errorf("ghost row in the database: %+v", sb)
		}
		if evs := p.events(t, id); !equalStrings(evs, []string{"created", "stopped", "deleted"}) {
			t.Errorf("events: %v, want [created stopped deleted]", evs)
		}
	})

	forkSetup := func(t *testing.T) (*persistFixture, string) {
		p := newPersistFixture(t)
		return p, p.create(t, `{"name":"parent"}`)
	}
	t.Run("fork/first save", func(t *testing.T) {
		p, parent := forkSetup(t)
		p.fs.failAll()
		n := p.fw.count()
		wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`), "fork, first save failing")
		if calls := p.fw.since(n); len(calls) != 0 {
			t.Errorf("worker called: %v", calls)
		}
		if l := len(p.srv.st.Snapshot().Sandboxes); l != 1 {
			t.Errorf("memory holds %d sandboxes, want only the parent", l)
		}
	})
	t.Run("fork/worker failed", func(t *testing.T) {
		p, parent := forkSetup(t)
		p.fw.set(func(f *fsWorker) { f.failFork = true })
		p.fs.failAfter(1, -1)
		if w := p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`); w.Code != 502 {
			t.Errorf("fork: %d %s, want 502", w.Code, w.Body)
		}
		var child *model.Sandbox
		for _, sb := range p.srv.st.Snapshot().Sandboxes {
			if sb.ID != parent {
				c := sb
				child = &c
			}
		}
		if child == nil || child.State != model.StateError {
			t.Errorf("child in memory: %+v, want error", child)
		}
	})
	t.Run("fork/last save", func(t *testing.T) {
		p, parent := forkSetup(t)
		p.fs.failAfter(1, -1)
		n := p.fw.count()
		wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"kid"}`), "fork, last save failing")
		waitFor(t, "the background delete of the child", 3*time.Second, func() bool {
			return hasPrefixCall(p.fw.since(n), "DELETE /v1/vms/sb-")
		})
		calls := p.fw.since(n)
		child := strings.TrimPrefix(calls[len(calls)-1], "DELETE /v1/vms/")
		if calls[0] != "POST /v1/vms/"+parent+"/fork" {
			t.Errorf("worker calls: %v", calls)
		}
		waitFor(t, "the child record to be removed", 3*time.Second, func() bool { return p.mem(child) == nil })
		p.healAndSave(t)
		if evs := p.events(t, child); !equalStrings(evs, []string{"forked", "stopped", "deleted"}) {
			t.Errorf("child events: %v, want [forked stopped deleted]", evs)
		}
		if p.savedSandbox(t, child) != nil {
			t.Error("ghost child row in the database")
		}
	})

	// The verbs that keep what the worker did: 503 applied, memory changed,
	// worker called, and the event written by the save that heals.
	type verb struct {
		name     string
		prepare  func(p *persistFixture, t *testing.T, id string)
		method   string
		path     string
		body     string
		call     string // worker call expected (prefix)
		check    func(t *testing.T, sb *model.Sandbox)
		wantEvts []string // events after "created"
	}
	verbs := []verb{
		{"delete", nil, "DELETE", "", "", "DELETE /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb != nil {
					t.Errorf("memory still holds the deleted sandbox")
				}
			}, []string{"deleted"}},
		{"sleep", nil, "POST", "/sleep", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StateSleeping || sb.SleptAt == 0 {
					t.Errorf("memory: %s sleptAt=%d, want sleeping", sb.State, sb.SleptAt)
				}
			}, []string{"slept"}},
		{"wake", func(p *persistFixture, t *testing.T, id string) {
			if w := p.do("POST", "/api/v1/sandboxes/"+id+"/sleep", ""); w.Code != 200 {
				t.Fatalf("sleep: %d", w.Code)
			}
		}, "POST", "/wake", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StateRunning {
					t.Errorf("memory: %s, want running", sb.State)
				}
			}, []string{"slept", "woken"}},
		{"stop", nil, "POST", "/stop", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StateStopped {
					t.Errorf("memory: %s, want stopped", sb.State)
				}
			}, []string{"stopped"}},
		{"start", nil, "POST", "/start", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StateRunning {
					t.Errorf("memory: %s, want running", sb.State)
				}
			}, []string{"started"}},
		{"pause", nil, "POST", "/pause", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StatePaused {
					t.Errorf("memory: %s, want paused", sb.State)
				}
			}, []string{"paused"}},
		{"resume", nil, "POST", "/resume", "", "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if sb.State != model.StateRunning {
					t.Errorf("memory: %s, want running", sb.State)
				}
			}, []string{"resumed"}},
		{"expose", nil, "POST", "/expose", `{"name":"web","port":8080}`, "POST /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if len(sb.Exposes) != 1 || sb.Exposes[0].Name != "web" {
					t.Errorf("memory exposes: %+v, want web", sb.Exposes)
				}
			}, nil},
		{"unexpose", func(p *persistFixture, t *testing.T, id string) {
			if w := p.do("POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"web","port":8080}`); w.Code != 201 {
				t.Fatalf("expose: %d %s", w.Code, w.Body)
			}
		}, "DELETE", "/expose/web", "", "DELETE /v1/vms/",
			func(t *testing.T, sb *model.Sandbox) {
				if len(sb.Exposes) != 0 {
					t.Errorf("memory exposes: %+v, want none", sb.Exposes)
				}
			}, nil},
	}
	for _, v := range verbs {
		t.Run(v.name, func(t *testing.T) {
			p := newPersistFixture(t)
			id := p.create(t, `{"name":"a"}`)
			if v.prepare != nil {
				v.prepare(p, t, id)
			}
			before := p.savedSandbox(t, id)
			p.fs.failAll()
			n := p.fw.count()
			wantApplied(t, p.do(v.method, "/api/v1/sandboxes/"+id+v.path, v.body), v.name)
			if calls := p.fw.since(n); len(calls) != 1 || !strings.HasPrefix(calls[0], v.call+id) {
				t.Errorf("worker calls: %v, want one %s%s...", calls, v.call, id)
			}
			v.check(t, p.mem(id))
			// Nothing of it reached the database yet: neither the state nor its event.
			if got := p.savedSandbox(t, id); got == nil || got.State != before.State || len(got.Exposes) != len(before.Exposes) {
				t.Errorf("database changed by a failed save: %+v (before %+v)", got, before)
			}
			p.healAndSave(t)
			want := append([]string{"created"}, v.wantEvts...)
			if evs := p.events(t, id); !equalStrings(evs, want) {
				t.Errorf("events after the healing save: %v, want %v", evs, want)
			}
			mem, saved := p.mem(id), p.savedSandbox(t, id)
			if (mem == nil) != (saved == nil) || (mem != nil && (mem.State != saved.State || len(mem.Exposes) != len(saved.Exposes))) {
				t.Errorf("after the healing save, database %+v differs from memory %+v", saved, mem)
			}
		})
	}

	t.Run("routes/ensure", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a","allow_dynamic_ports":true}`)
		p.fs.failAll()
		wantApplied(t, p.do("POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+id+`","port":8080}`), "ensure")
		if sb := p.mem(id); len(sb.Exposes) != 1 || sb.Exposes[0].Name != "8080" {
			t.Errorf("memory exposes: %+v", sb.Exposes)
		}
	})

	t.Run("sweep/auto-sleep", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		now := time.Now().Unix()
		func() {
			p.srv.st.Lock()
			defer p.srv.st.Unlock()
			sb := p.srv.st.FindSandbox(id)
			sb.IdleSleepS = 5
			sb.LastActivity = now - 60
		}()
		p.fs.failAll()
		n := p.fw.count()
		p.srv.lifecycleSweep(now)
		if calls := p.fw.since(n); len(calls) != 1 || calls[0] != "POST /v1/vms/"+id+"/sleep" {
			t.Errorf("worker calls: %v", calls)
		}
		if sb := p.mem(id); sb.State != model.StateSleeping {
			t.Errorf("memory: %s, want sleeping", sb.State)
		}
		if !p.srv.storeFailing() {
			t.Error("store not marked failing")
		}
		// While failing, the sweep does nothing at all.
		n = p.fw.count()
		p.srv.lifecycleSweep(now + 3600)
		if calls := p.fw.since(n); len(calls) != 0 {
			t.Errorf("sweep acted while the store was failing: %v", calls)
		}
		p.healAndSave(t)
		if evs := p.events(t, id); !equalStrings(evs, []string{"created", "slept"}) {
			t.Errorf("events: %v", evs)
		}
	})

	t.Run("sweep/auto-delete", func(t *testing.T) {
		p := newPersistFixture(t)
		id := p.create(t, `{"name":"a"}`)
		if w := p.do("POST", "/api/v1/sandboxes/"+id+"/sleep", ""); w.Code != 200 {
			t.Fatalf("sleep: %d", w.Code)
		}
		now := time.Now().Unix()
		func() {
			p.srv.st.Lock()
			defer p.srv.st.Unlock()
			sb := p.srv.st.FindSandbox(id)
			sb.AsleepDeleteS = 30
			sb.SleptAt = now - 60
		}()
		p.fs.failAll()
		n := p.fw.count()
		p.srv.lifecycleSweep(now)
		if calls := p.fw.since(n); len(calls) != 1 || calls[0] != "DELETE /v1/vms/"+id {
			t.Errorf("worker calls: %v", calls)
		}
		if p.mem(id) != nil {
			t.Error("memory still holds the auto-deleted sandbox")
		}
		p.healAndSave(t)
		if evs := p.events(t, id); !equalStrings(evs, []string{"created", "slept", "deleted"}) {
			t.Errorf("events: %v", evs)
		}
		if p.savedSandbox(t, id) != nil {
			t.Error("database still holds the auto-deleted sandbox")
		}
	})

	t.Run("register without a join token", func(t *testing.T) {
		p := newPersistFixture(t)
		p.fs.failAll()
		w := p.do("POST", "/api/v1/agents/register", `{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`)
		if w.Code != 200 {
			t.Errorf("register with a failing save: %d %s, want 200", w.Code, w.Body)
		}
		if l := len(p.srv.st.Snapshot().Nodes); l != 2 {
			t.Errorf("memory nodes: %d, want 2", l)
		}
		p.healAndSave(t)
		if l := len(p.saved(t).Nodes); l != 2 {
			t.Errorf("saved nodes: %d, want 2", l)
		}
	})
}

// ---- P5/P15: create's last save fails once while the worker's delete is slow ----

func TestCreateWhoseLastSaveFailsLeavesNoGhost(t *testing.T) {
	p := newPersistFixture(t)
	p.fw.set(func(f *fsWorker) { f.deleteDelay = 300 * time.Millisecond })
	p.fs.failAfter(1, 1)

	start := time.Now()
	n := p.fw.count()
	wantStoreUnavailable(t, p.do("POST", "/api/v1/sandboxes", `{"name":"a"}`), "create")
	if took := time.Since(start); took >= 300*time.Millisecond {
		t.Errorf("the answer took %v: it waited for the worker delete (P15)", took)
	}
	waitFor(t, "the store to be healthy again", 5*time.Second, func() bool {
		return !p.srv.storeFailing() && hasPrefixCall(p.fw.since(n), "DELETE /v1/vms/sb-")
	})
	calls := p.fw.since(n)
	id := strings.TrimPrefix(calls[len(calls)-1], "DELETE /v1/vms/")
	waitFor(t, "the record to be removed", 3*time.Second, func() bool { return p.mem(id) == nil })
	// The removal's own save healed the store.
	waitFor(t, "the removal to be saved", 3*time.Second, func() bool { return p.savedSandbox(t, id) == nil && !p.srv.storeFailing() })

	open := false
	for _, e := range p.events(t, id) {
		switch {
		case runStart[e]:
			open = true
		case runEnd[e]:
			open = false
		}
	}
	if open {
		t.Errorf("usage interval left open: %v", p.events(t, id))
	}
}

// ---- P1: concurrent changes, then the saved snapshot equals memory ----

type sbView struct {
	ID, State, IP string
	Exposes       string
	SleptAt, Last int64
}

func viewOf(sbs []model.Sandbox) []sbView {
	var out []sbView
	for _, sb := range sbs {
		ip := ""
		if sb.IP != nil {
			ip = *sb.IP
		}
		e, _ := json.Marshal(sb.Exposes)
		out = append(out, sbView{sb.ID, string(sb.State), ip, string(e), sb.SleptAt, sb.LastActivity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func TestConcurrentChangesLeaveTheDatabaseEqualToMemory(t *testing.T) {
	p := newPersistFixture(t)
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, p.create(t, `{"name":"a"}`))
	}
	var wg sync.WaitGroup
	run := func(method, path, body string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.do(method, path, body)
		}()
	}
	for i := 0; i < 8; i++ {
		run("POST", "/api/v1/sandboxes", `{"name":"b"}`)
	}
	run("POST", "/api/v1/sandboxes/"+ids[0]+"/sleep", "")
	run("POST", "/api/v1/sandboxes/"+ids[1]+"/stop", "")
	run("DELETE", "/api/v1/sandboxes/"+ids[2], "")
	run("POST", "/api/v1/sandboxes/"+ids[3]+"/expose", `{"name":"web","port":80}`)
	run("POST", "/api/v1/sandboxes/"+ids[3]+"/expose", `{"name":"api","port":81}`)
	run("POST", "/api/v1/sandboxes/"+ids[4]+"/pause", "")
	run("POST", "/api/v1/sandboxes/"+ids[5]+"/fork", `{"name":"kid"}`)
	wg.Wait()

	mem := viewOf(p.srv.st.Snapshot().Sandboxes)
	var saved []model.Sandbox
	for _, sb := range p.saved(t).Sandboxes {
		saved = append(saved, *sb)
	}
	db := viewOf(saved)
	if fmt.Sprint(mem) != fmt.Sprint(db) {
		t.Fatalf("saved snapshot differs from memory after concurrent changes:\nmemory   %v\ndatabase %v", mem, db)
	}
}

// ---- P1 under -race: saves against in-place changes ----

func TestSavesRaceUnexposeHeartbeatAndRegistration(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	nodeID := p.srv.st.Nodes[0].ID
	stop := make(chan struct{})
	var wg sync.WaitGroup
	loop := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	loop(func(int) { _ = p.srv.save(nil) })
	loop(func(i int) {
		name := fmt.Sprintf("n%d", i%3)
		p.do("POST", "/api/v1/sandboxes/"+id+"/expose", fmt.Sprintf(`{"name":%q,"port":%d}`, name, 8000+i%3))
		p.do("DELETE", "/api/v1/sandboxes/"+id+"/expose/"+name, "")
	})
	loop(func(i int) {
		p.do("POST", "/api/v1/agents/heartbeat", fmt.Sprintf(`{"id":%q,"mem_free_mib":%d,"vm_count":%d}`, nodeID, i, i%7))
	})
	loop(func(i int) {
		p.do("POST", "/api/v1/agents/register", fmt.Sprintf(`{"hostname":"w2","addr":"203.0.113.9:9090","cpus":%d,"mem_total_mib":%d}`, i%8+1, 512+i))
	})
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	if err := p.srv.save(nil); err != nil {
		t.Fatalf("final save: %v", err)
	}
}

// ---- P4: the gate ----

func TestStoreGateRefusesChangesAndServesTheRest(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a","allow_dynamic_ports":true}`)
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"web","port":80}`); w.Code != 201 {
		t.Fatalf("expose: %d", w.Code)
	}
	tw := p.do("POST", "/api/v1/tenants", `{"name":"acme"}`)
	if tw.Code != 201 {
		t.Fatalf("tenant: %d", tw.Code)
	}
	var tn struct {
		Tenant struct{ ID string } `json:"tenant"`
		KeyID  string              `json:"key_id"`
	}
	_ = json.Unmarshal(tw.Body.Bytes(), &tn)
	jt := mintJoinToken(t, p.srv)
	nodeID := p.srv.st.Nodes[0].ID

	p.fs.failAll()
	if err := p.srv.save(nil); err == nil {
		t.Fatal("save did not fail")
	}
	n := p.fw.count()

	refused := []struct{ method, path, body, auth string }{
		{"POST", "/api/v1/sandboxes", `{"name":"b"}`, adminAuth},
		{"DELETE", "/api/v1/sandboxes/" + id, "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/sleep", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/wake", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/stop", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/start", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/pause", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/resume", "", adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/fork", `{"name":"kid"}`, adminAuth},
		{"POST", "/api/v1/sandboxes/" + id + "/expose", `{"name":"api","port":81}`, adminAuth},
		{"DELETE", "/api/v1/sandboxes/" + id + "/expose/web", "", adminAuth},
		{"POST", "/api/v1/routes/ensure", `{"sandbox_id":"` + id + `","port":9000}`, adminAuth},
		{"POST", "/api/v1/tenants", `{"name":"globex"}`, adminAuth},
		{"POST", "/api/v1/tenants/" + tn.Tenant.ID + "/keys", "", adminAuth},
		{"DELETE", "/api/v1/keys/" + tn.KeyID, "", adminAuth},
		{"POST", "/api/v1/templates", `{"name":"t1","image_file":"x"}`, adminAuth},
		{"DELETE", "/api/v1/templates/t1", "", adminAuth},
		{"POST", "/api/v1/join-tokens", "", adminAuth},
		{"POST", "/api/v1/agents/register", `{"hostname":"w3","addr":"203.0.113.10:9090","cpus":1,"mem_total_mib":512,"join_token":"` + jt + `"}`, adminAuth},
		{"POST", "/api/v1/nodes/join", `{"pubkey":"` + validPubKeyA + `","hostname":"w4"}`, "Bearer " + jt},
	}
	for _, rq := range refused {
		w := doRequest(p.h, rq.method, rq.path, rq.body, rq.auth)
		wantStoreUnavailable(t, w, rq.method+" "+rq.path)
	}
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("refused requests reached the worker: %v", calls)
	}
	if ok, _ := p.sqlite.CheckJoinToken(hashSecret(jt), time.Now().Unix()); !ok {
		t.Error("a refused enrollment burned the join token")
	}

	// Exempt: reads, exec, heartbeat, plain registration, route table, the
	// gateway's activity report, /metrics.
	exempt := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/sandboxes", "", 200},
		{"GET", "/api/v1/sandboxes/" + id, "", 200},
		{"GET", "/api/v1/routes", "", 200},
		{"GET", "/api/v1/tenants", "", 200},
		{"POST", "/api/v1/sandboxes/" + id + "/exec", `{"cmd":["true"]}`, 200},
		{"POST", "/api/v1/agents/heartbeat", `{"id":"` + nodeID + `","mem_free_mib":100}`, 200},
		{"POST", "/api/v1/agents/register", `{"hostname":"w2","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`, 200},
		{"POST", "/api/v1/routes/activity", `{"sandbox_ids":["` + id + `"]}`, 204},
	}
	for _, rq := range exempt {
		if w := p.do(rq.method, rq.path, rq.body); w.Code != rq.want {
			t.Errorf("%s %s while failing: %d %s, want %d", rq.method, rq.path, w.Code, w.Body, rq.want)
		}
	}
	if !hasPrefixCall(p.fw.since(n), "POST /v1/vms/"+id+"/exec") {
		t.Error("exec did not reach the worker while the store was failing")
	}
	mw := p.do("GET", "/metrics", "")
	if mw.Code != 200 || !strings.Contains(mw.Body.String(), "felucca_store_healthy 0\n") {
		t.Errorf("/metrics while failing: %d, want 200 with felucca_store_healthy 0", mw.Code)
	}
	for _, m := range []string{"felucca_persist_failures_total ", "felucca_usage_write_failures_total "} {
		if !strings.Contains(mw.Body.String(), m) {
			t.Errorf("/metrics lacks %s", m)
		}
	}
}

// ---- P3: the recovery loop ----

func TestRecoveryLoopCatchesUpWithoutARequest(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	failuresBefore := persistFailures.Load()
	p.fs.failAll()
	wantApplied(t, p.do("POST", "/api/v1/sandboxes/"+id+"/sleep", ""), "sleep")
	if persistFailures.Load() <= failuresBefore {
		t.Error("the failed save was not counted")
	}
	if w := p.do("GET", "/healthz", ""); w.Code != 503 {
		t.Errorf("healthz while failing: %d", w.Code)
	}
	p.fs.heal()
	waitFor(t, "the recovery loop to heal the store", 6*time.Second, func() bool { return !p.srv.storeFailing() })
	if sb := p.savedSandbox(t, id); sb == nil || sb.State != model.StateSleeping {
		t.Errorf("database after recovery: %+v, want sleeping", sb)
	}
	if evs := p.events(t, id); !equalStrings(evs, []string{"created", "slept"}) {
		t.Errorf("events after recovery: %v", evs)
	}
	if w := p.do("GET", "/healthz", ""); w.Code != 200 || w.Body.String() != `{"ok":true}` {
		t.Errorf("healthz after recovery: %d %s", w.Code, w.Body)
	}
	// And the loop ended: a later failure starts a new one, which heals too.
	waitFor(t, "the loop to stop", 3*time.Second, func() bool {
		p.srv.health.mu.Lock()
		defer p.srv.health.mu.Unlock()
		return !p.srv.health.recovering
	})
	p.fs.failAfter(0, 1)
	if err := p.srv.save(nil); err == nil {
		t.Fatal("save did not fail")
	}
	waitFor(t, "a second recovery", 6*time.Second, func() bool { return !p.srv.storeFailing() })
}

// A panic inside a retry does not end the loop (or the process).
func TestRecoveryLoopSurvivesAPanickingSave(t *testing.T) {
	tmp := t.TempDir()
	sqlite, err := store.OpenSQLite(filepath.Join(tmp, "felucca.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close() })
	ps := &panicStore{Store: sqlite}
	srv := newJoinServerStore(t, ps, tmp)
	t.Cleanup(srv.stopBackground)
	ps.armSave(2) // the first save and the first retry panic
	if err := srv.save(nil); err == nil {
		t.Fatal("a panicking save reported success")
	}
	waitFor(t, "recovery after a panicking retry", 8*time.Second, func() bool { return !srv.storeFailing() })
}

// ---- P9: /healthz ----

func TestHealthzBodiesAndNoStateLock(t *testing.T) {
	p := newPersistFixture(t)
	if w := p.do("GET", "/healthz", ""); w.Code != 200 || w.Body.String() != `{"ok":true}` {
		t.Errorf("healthy: %d %s", w.Code, w.Body)
	}
	p.fs.failAll()
	_ = p.srv.save(nil)
	if w := p.do("GET", "/healthz", ""); w.Code != 503 || w.Body.String() != `{"ok":false,"store":"failing"}` {
		t.Errorf("failing: %d %s", w.Code, w.Body)
	}

	p.srv.st.Lock()
	done := make(chan int, 1)
	go func() { done <- doRequest(p.h, "GET", "/healthz", "", "").Code }()
	select {
	case <-done:
	case <-time.After(time.Second):
		p.srv.st.Unlock()
		t.Fatal("/healthz waited for the state lock")
	}
	p.srv.st.Unlock()
}

// ---- P1/P8: a slow save, and a database another process holds locked ----

func TestSlowSaveDoesNotHoldTheStateLock(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	release := make(chan struct{})
	p.fs.mu.Lock()
	p.fs.block = release
	p.fs.mu.Unlock()
	saved := make(chan error, 1)
	go func() { saved <- p.srv.save(nil) }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if w := p.do("GET", "/api/v1/sandboxes", ""); w.Code != 200 {
		t.Errorf("list: %d", w.Code)
	}
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/exec", `{"cmd":["true"]}`); w.Code != 200 {
		t.Errorf("exec: %d %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("reads and exec took %v behind a slow save", took)
	}
	select {
	case <-saved:
		t.Fatal("the save finished early: the test did not overlap it")
	default:
	}
	p.fs.mu.Lock()
	p.fs.block = nil
	p.fs.mu.Unlock()
	close(release)
	if err := <-saved; err != nil {
		t.Errorf("save: %v", err)
	}
}

func TestReadsAndExecProceedWhileTheDatabaseIsWriteLocked(t *testing.T) {
	p := newPersistFixture(t)
	id := p.create(t, `{"name":"a"}`)
	_, key := mintTenant(t, p.h, "acme")

	other, err := sql.Open("sqlite", p.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx := context.Background()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}
	saved := make(chan error, 1)
	go func() { saved <- p.srv.save(nil) }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	if w := doRequest(p.h, "GET", "/api/v1/sandboxes", "", "Bearer "+key); w.Code != 200 {
		t.Errorf("tenant key lookup + list under a held write lock: %d %s", w.Code, w.Body)
	}
	if w := p.do("GET", "/api/v1/tenants", ""); w.Code != 200 {
		t.Errorf("tenant list under a held write lock: %d", w.Code)
	}
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/exec", `{"cmd":["true"]}`); w.Code != 200 {
		t.Errorf("exec under a held write lock: %d %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("reads and exec took %v under a held write lock, want under 500ms", took)
	}
	select {
	case err := <-saved:
		t.Fatalf("the save returned (%v) while the lock was held: the requests did not overlap it", err)
	default:
	}
	if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	<-saved
}

// ---- P8: key lookup store errors ----

func TestKeyLookupStoreErrorIs503AndNotThrottled(t *testing.T) {
	p := newPersistFixture(t)
	_, key := mintTenant(t, p.h, "acme")
	p.fs.mu.Lock()
	p.fs.keyErr = true
	p.fs.mu.Unlock()
	for i := 0; i < authFailThreshold+5; i++ {
		w := doRequest(p.h, "GET", "/api/v1/sandboxes", "", "Bearer "+key)
		wantStoreUnavailable(t, w, "key lookup with a store read error")
	}
	p.srv.authFails.mu.Lock()
	recorded := len(p.srv.authFails.fail)
	p.srv.authFails.mu.Unlock()
	if recorded != 0 {
		t.Errorf("store read errors were fed to the failed-credential throttle (%d records)", recorded)
	}
	p.fs.mu.Lock()
	p.fs.keyErr = false
	p.fs.mu.Unlock()
	if w := doRequest(p.h, "GET", "/api/v1/sandboxes", "", "Bearer "+key); w.Code != 200 {
		t.Errorf("valid key after the store recovered: %d", w.Code)
	}
	if w := doRequest(p.h, "GET", "/api/v1/sandboxes", "", "Bearer "+bogusAPIKey(1)); w.Code != 401 {
		t.Errorf("bad key after the outage: %d, want 401 (not throttled by the outage)", w.Code)
	}
}

// Quota and the sweep's tenant policy: a read error is not "no limit".
func TestTenantReadErrorsAreNotNoLimit(t *testing.T) {
	p := newPersistFixture(t)
	tenantID, key := mintTenant(t, p.h, "acme")
	parent := p.create(t, `{"name":"parent"}`)
	// A tenant-owned sandbox past an idle policy the TENANT row would decide.
	func() {
		p.srv.st.Lock()
		defer p.srv.st.Unlock()
		sb := p.srv.st.FindSandbox(parent)
		sb.TenantID = tenantID
		sb.IdleSleepS = 5
		sb.LastActivity = time.Now().Unix() - 60
	}()

	p.fs.mu.Lock()
	p.fs.tenantErr = true
	p.fs.mu.Unlock()
	n := p.fw.count()
	wantStoreUnavailable(t, doRequest(p.h, "POST", "/api/v1/sandboxes", `{"name":"a"}`, "Bearer "+key), "create with an unreadable quota")
	wantStoreUnavailable(t, doRequest(p.h, "POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"k"}`, "Bearer "+key), "fork with an unreadable quota")
	p.srv.lifecycleSweep(time.Now().Unix())
	if calls := p.fw.since(n); len(calls) != 0 {
		t.Errorf("worker called with the tenant row unreadable: %v", calls)
	}
	if sb := p.mem(parent); sb.State != model.StateRunning {
		t.Errorf("sweep acted on a sandbox whose tenant policy was unreadable: %s", sb.State)
	}

	p.fs.mu.Lock()
	p.fs.tenantErr = false
	p.fs.mu.Unlock()
	p.srv.lifecycleSweep(time.Now().Unix())
	if sb := p.mem(parent); sb.State != model.StateSleeping {
		t.Errorf("sweep after the store recovered: %s, want sleeping", sb.State)
	}
}

// ---- P7: the exec usage queue ----

func TestExecUsageQueueFlushesBoundsAndCountsDrops(t *testing.T) {
	p := newPersistFixture(t)
	before := usageWriteFailures.Load()
	for i := 0; i < maxExecUsageQueue+5; i++ {
		p.srv.queueExecUsage(store.UsageEvent{SandboxID: "sb-x", Event: "exec", TS: int64(i + 1)})
	}
	if got := usageWriteFailures.Load() - before; got != 5 {
		t.Errorf("dropped rows counted: %d, want 5", got)
	}

	// A failed flush keeps the rows and counts the failure.
	p.fs.mu.Lock()
	p.fs.batchErr = true
	p.fs.mu.Unlock()
	if err := p.srv.flushExecUsage(); err == nil {
		t.Fatal("flush against a failing store reported success")
	}
	if got := usageWriteFailures.Load() - before; got != 5+maxExecUsageQueue {
		t.Errorf("failures counted after a failed flush: %d, want %d", got, 5+maxExecUsageQueue)
	}
	p.fs.mu.Lock()
	p.fs.batchErr = false
	p.fs.mu.Unlock()
	if err := p.srv.flushExecUsage(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	evs, err := p.sqlite.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != maxExecUsageQueue {
		t.Fatalf("rows written: %d, want %d", len(evs), maxExecUsageQueue)
	}
	if evs[0].TS != 6 {
		t.Errorf("oldest written row has ts %d, want 6: the OLDEST five should have been dropped", evs[0].TS)
	}

	// An exec through the API queues, and the flusher writes.
	id := p.create(t, `{"name":"a"}`)
	if w := p.do("POST", "/api/v1/sandboxes/"+id+"/exec", `{"cmd":["true"]}`); w.Code != 200 {
		t.Fatalf("exec: %d", w.Code)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { p.srv.UsageFlushLoop(stop); close(done) }()
	waitFor(t, "the flusher to write the exec row", 3*time.Second, func() bool {
		evs, _ := p.sqlite.ListUsage("", 0, math.MaxInt32)
		for _, e := range evs {
			if e.SandboxID == id && e.Event == "exec" {
				return true
			}
		}
		return false
	})
	close(stop)
	<-done
}
