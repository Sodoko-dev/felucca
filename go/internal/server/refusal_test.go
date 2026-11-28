// Tests for FLU-4 S10: feluccad learns the worker's state from a 409 that
// names it. Internal package, on the FLU-5 fixtures (persist_test.go), so the
// saved state, the saved usage events and the store's health can be read back.
package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// refusingWorker answers every lifecycle verb (POST .../<verb>) with the
// status and body it is given, and everything else like fsWorker. onCall, when
// set, runs before the answer (the compare-and-set test changes the record
// there, while feluccad waits on the worker).
type refusingWorker struct {
	fsWorker
	mu     sync.Mutex
	status int
	body   string
	onCall func()
}

func (rw *refusingWorker) answer(status int, body string) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.status, rw.body = status, body
}

func (rw *refusingWorker) before(fn func()) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.onCall = fn
}

func (rw *refusingWorker) handler() http.HandlerFunc {
	fallback := rw.fsWorker.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		rw.mu.Lock()
		status, body, onCall := rw.status, rw.body, rw.onCall
		rw.mu.Unlock()
		verb := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		if r.Method == http.MethodPost && status != 0 && lifecycleVerbs[verb] {
			rw.fsWorker.set(func(fw *fsWorker) { fw.calls = append(fw.calls, r.Method+" "+r.URL.Path) })
			if onCall != nil {
				onCall()
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		fallback(w, r)
	}
}

var lifecycleVerbs = map[string]bool{
	"sleep": true, "wake": true, "stop": true, "start": true, "pause": true, "resume": true,
}

type refusalFixture struct {
	*persistFixture
	rw *refusingWorker
}

// newRefusalFixture is newPersistFixture with a worker that can refuse.
func newRefusalFixture(t *testing.T) *refusalFixture {
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
	rw := &refusingWorker{}
	ts := httptest.NewServer(rw.handler())
	t.Cleanup(ts.Close)
	srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, time.Now().Unix())
	if err := srv.save(nil); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	p := &persistFixture{srv: srv, fs: fs, sqlite: sqlite, fw: &rw.fsWorker, path: cfg.DBPath, h: srv.Handler()}
	return &refusalFixture{persistFixture: p, rw: rw}
}

// setRecord puts the record in a state with the given clocks and saves it, so
// memory and database agree before the request under test.
func (f *refusalFixture) setRecord(t *testing.T, id string, s model.SandboxState, lastActivity, sleptAt int64) {
	t.Helper()
	func() {
		f.srv.st.Lock()
		defer f.srv.st.Unlock()
		sb := f.srv.st.FindSandbox(id)
		sb.State, sb.LastActivity, sb.SleptAt = s, lastActivity, sleptAt
	}()
	if err := f.srv.save(nil); err != nil {
		t.Fatalf("save: %v", err)
	}
}

func (f *refusalFixture) saveCalls() int {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	return f.fs.saveCalls
}

var refusedStateEvent = map[model.SandboxState]string{
	model.StateStopped:  "stopped",
	model.StateSleeping: "slept",
	model.StatePaused:   "paused",
	model.StateRunning:  "started",
	model.StateError:    "stopped",
}

// For each verb and each of the five states: the record takes the worker's
// state, the clocks move as the normal handler moves them, exactly one usage
// event of the right kind is saved with it, and the caller gets the worker's
// 409 body unchanged.
func TestRefusalWithAStateIsAdopted(t *testing.T) {
	for verb := range lifecycleVerbs {
		for to, event := range refusedStateEvent {
			t.Run(verb+"/"+string(to), func(t *testing.T) {
				f := newRefusalFixture(t)
				id := f.create(t, `{"name":"a"}`)
				from := model.StateRunning
				if to == model.StateRunning {
					from = model.StateSleeping
				}
				f.setRecord(t, id, from, 1, 1)
				before := len(f.events(t, id))
				body := `{"error":"InvalidState: not here","state":"` + string(to) + `"}`
				f.rw.answer(409, body)

				start := time.Now().Unix()
				w := f.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
				if w.Code != 409 || w.Body.String() != body {
					t.Fatalf("answer: %d %s, want 409 %s", w.Code, w.Body, body)
				}
				mem := f.mem(id)
				if mem.State != to {
					t.Errorf("memory: %s, want %s", mem.State, to)
				}
				switch to {
				case model.StateSleeping:
					if mem.SleptAt < start || mem.LastActivity != 1 {
						t.Errorf("clocks: slept_at %d (want >= %d), last_activity %d (want 1)", mem.SleptAt, start, mem.LastActivity)
					}
				case model.StateRunning:
					if mem.LastActivity < start || mem.SleptAt != 0 {
						t.Errorf("clocks: last_activity %d (want >= %d), slept_at %d (want 0)", mem.LastActivity, start, mem.SleptAt)
					}
				default:
					if mem.LastActivity != 1 || mem.SleptAt != 1 {
						t.Errorf("clocks moved: last_activity %d, slept_at %d, want 1 and 1", mem.LastActivity, mem.SleptAt)
					}
				}
				if got := f.savedSandbox(t, id); got == nil || got.State != to {
					t.Errorf("saved record: %+v, want state %s", got, to)
				}
				if got := f.events(t, id)[before:]; !equalStrings(got, []string{event}) {
					t.Errorf("saved events: %v, want [%s]", got, event)
				}
			})
		}
	}
}

// A 409 that does not name one of the five states is forwarded as it came and
// nothing is recorded: busy, an older agent's refusal, the agent-only states,
// a state feluccad does not know, a body that is not JSON.
func TestRefusalWithoutAUsableStateIsOnlyForwarded(t *testing.T) {
	bodies := []string{
		`{"error":"busy"}`,
		`{"error":"InvalidState: cannot start a paused VM"}`,
		`{"error":"InvalidState: x","state":"creating"}`,
		`{"error":"InvalidState: x","state":"pooled"}`,
		`{"error":"InvalidState: x","state":"dreaming"}`,
		`{"error":"InvalidState: x","state":""}`,
		`{"error":"InvalidState: x","state":7}`,
		`not json`,
		``,
	}
	for verb := range lifecycleVerbs {
		for _, body := range bodies {
			t.Run(verb+"/"+body, func(t *testing.T) {
				f := newRefusalFixture(t)
				id := f.create(t, `{"name":"a"}`)
				before := len(f.events(t, id))
				saves := f.saveCalls()
				f.rw.answer(409, body)
				w := f.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
				if w.Code != 409 || w.Body.String() != body {
					t.Fatalf("answer: %d %q, want 409 %q", w.Code, w.Body, body)
				}
				if s := f.mem(id).State; s != model.StateRunning {
					t.Errorf("memory: %s, want running", s)
				}
				if got := f.events(t, id)[before:]; len(got) != 0 {
					t.Errorf("events: %v, want none", got)
				}
				if n := f.saveCalls() - saves; n != 0 {
					t.Errorf("%d saves, want none", n)
				}
			})
		}
	}
}

// A 409 body larger than feluccad relays is not forwarded and not read.
func TestOversizedRefusalIsNotRelayed(t *testing.T) {
	f := newRefusalFixture(t)
	id := f.create(t, `{"name":"a"}`)
	big := `{"state":"stopped","error":"` + strings.Repeat("x", maxRefusalBody) + `"}`
	f.rw.answer(409, big)
	w := f.do("POST", "/api/v1/sandboxes/"+id+"/sleep", "")
	if w.Code != 409 || w.Body.String() != `{"error":"agent refused the request"}` {
		t.Fatalf("answer: %d (%d bytes), want 409 with the generic body", w.Code, w.Body.Len())
	}
	if s := f.mem(id).State; s != model.StateRunning {
		t.Errorf("memory: %s, want running", s)
	}
}

// The worker's state is recorded only over the state the request started from:
// a record that changed while feluccad waited on the worker is newer news.
func TestRefusalDoesNotOverwriteARecordThatChanged(t *testing.T) {
	for verb := range lifecycleVerbs {
		t.Run(verb, func(t *testing.T) {
			f := newRefusalFixture(t)
			id := f.create(t, `{"name":"a"}`)
			before := len(f.events(t, id))
			f.rw.before(func() {
				f.srv.st.Lock()
				defer f.srv.st.Unlock()
				f.srv.st.FindSandbox(id).State = model.StatePaused
			})
			f.rw.answer(409, `{"error":"InvalidState: x","state":"stopped"}`)
			w := f.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
			if w.Code != 409 {
				t.Fatalf("answer: %d %s, want 409", w.Code, w.Body)
			}
			if s := f.mem(id).State; s != model.StatePaused {
				t.Errorf("memory: %s, want paused (the change made meanwhile)", s)
			}
			if got := f.events(t, id)[before:]; len(got) != 0 {
				t.Errorf("events: %v, want none", got)
			}
		})
	}
}

// The worker names the state feluccad already records: nothing to adopt, no
// event, no save.
func TestRefusalNamingTheRecordedStateChangesNothing(t *testing.T) {
	for verb := range lifecycleVerbs {
		t.Run(verb, func(t *testing.T) {
			f := newRefusalFixture(t)
			id := f.create(t, `{"name":"a"}`)
			f.setRecord(t, id, model.StateStopped, 1, 1)
			before := len(f.events(t, id))
			saves := f.saveCalls()
			body := `{"error":"InvalidState: x","state":"stopped"}`
			f.rw.answer(409, body)
			w := f.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
			if w.Code != 409 || w.Body.String() != body {
				t.Fatalf("answer: %d %s, want 409 %s", w.Code, w.Body, body)
			}
			if m := f.mem(id); m.State != model.StateStopped || m.LastActivity != 1 || m.SleptAt != 1 {
				t.Errorf("memory: %+v, want stopped with untouched clocks", m)
			}
			if got := f.events(t, id)[before:]; len(got) != 0 {
				t.Errorf("events: %v, want none", got)
			}
			if n := f.saveCalls() - saves; n != 0 {
				t.Errorf("%d saves, want none", n)
			}
		})
	}
}

// The sweep's own sleep adopts too: a sandbox its worker holds as stopped
// stops being put to sleep every pass.
func TestSweepSleepAdoptsTheRefusedState(t *testing.T) {
	f := newRefusalFixture(t)
	id := f.create(t, `{"name":"a"}`)
	now := time.Now().Unix()
	func() {
		f.srv.st.Lock()
		defer f.srv.st.Unlock()
		sb := f.srv.st.FindSandbox(id)
		sb.IdleSleepS = 5
		sb.LastActivity = now - 10
	}()
	before := len(f.events(t, id))
	f.rw.answer(409, `{"error":"InvalidState: x","state":"stopped"}`)

	f.srv.lifecycleSweep(now)
	if s := f.mem(id).State; s != model.StateStopped {
		t.Fatalf("memory after the sweep: %s, want stopped", s)
	}
	if got := f.savedSandbox(t, id); got == nil || got.State != model.StateStopped {
		t.Errorf("saved record: %+v, want stopped", got)
	}
	if got := f.events(t, id)[before:]; !equalStrings(got, []string{"stopped"}) {
		t.Errorf("events: %v, want [stopped]", got)
	}
	calls := f.fw.count()
	f.srv.lifecycleSweep(now + 15)
	if got := f.fw.since(calls); len(got) != 0 {
		t.Errorf("the next sweep called the worker again: %v", got)
	}
}

// A save that fails during the adoption is logged and does not change the
// answer: still the worker's 409; memory holds the adopted state and the store
// is marked failing, so the recovery loop writes it.
func TestRefusalAdoptionWhoseSaveFails(t *testing.T) {
	f := newRefusalFixture(t)
	id := f.create(t, `{"name":"a"}`)
	body := `{"error":"InvalidState: x","state":"stopped"}`
	f.rw.answer(409, body)
	f.fs.failAll()
	w := f.do("POST", "/api/v1/sandboxes/"+id+"/wake", "")
	if w.Code != 409 || w.Body.String() != body {
		t.Fatalf("answer: %d %s, want 409 %s", w.Code, w.Body, body)
	}
	if s := f.mem(id).State; s != model.StateStopped {
		t.Errorf("memory: %s, want stopped", s)
	}
	if !f.srv.storeFailing() {
		t.Error("store not marked failing")
	}
	before := len(f.events(t, id))
	f.healAndSave(t)
	if got := f.savedSandbox(t, id); got == nil || got.State != model.StateStopped {
		t.Errorf("saved after recovery: %+v, want stopped", got)
	}
	if got := f.events(t, id)[before:]; !equalStrings(got, []string{"stopped"}) {
		t.Errorf("events after recovery: %v, want [stopped]", got)
	}
}

// Every other refusal of sleep and wake is still a 502, a 404 included, and
// records nothing.
func TestSleepAndWakeOtherFailuresAreStill502(t *testing.T) {
	for _, verb := range []string{"sleep", "wake"} {
		for _, status := range []int{500, 404} {
			t.Run(verb+"/"+http.StatusText(status), func(t *testing.T) {
				f := newRefusalFixture(t)
				id := f.create(t, `{"name":"a"}`)
				f.rw.answer(status, `{"error":"x","state":"stopped"}`)
				w := f.do("POST", "/api/v1/sandboxes/"+id+"/"+verb, "")
				if w.Code != 502 {
					t.Errorf("answer: %d %s, want 502", w.Code, w.Body)
				}
				if s := f.mem(id).State; s != model.StateRunning {
					t.Errorf("memory: %s, want running", s)
				}
			})
		}
	}
}
