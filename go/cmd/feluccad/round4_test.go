package main

// Fourth review (fix-go-round4.md): a record left "creating" by a stopped
// control plane, and the listeners draining all at once.

import (
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
)

// D1: a create was in its worker call when the previous process stopped; the
// row loads as "creating". After boot it is error, in the database too; a
// start reaches the worker and a delete works; its usage interval opens and
// closes.
func TestBootSettlesARecordLeftCreating(t *testing.T) {
	cfg, db := bootFixture(t)
	var mu sync.Mutex
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/v1/vms" {
			// The worker finished the create on its own and holds the address.
			_, _ = w.Write([]byte(`{"vms":[{"id":"sb-left","ip":"10.231.0.42"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(ts.Close)
	st := state.New()
	nodeID := st.RegisterNode("worker-1", ts.URL, 4, 8192, time.Now().Unix())
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{ID: "sb-left", Name: "a", Namespace: "default", NodeID: &nodeID,
		State: model.StateCreating, VCPUs: 1, MemMiB: 256})
	if err := db.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })
	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatal(err)
	}
	if sb := got.FindSandbox("sb-left"); sb == nil || sb.State != model.StateError {
		t.Fatalf("database after boot: %+v, want the record in error (part of the start-up write)", sb)
	}

	h := srv.Handler()
	if w := req(h, "POST", "/api/v1/sandboxes/sb-left/start", ""); w.Code != 200 {
		t.Errorf("start of the settled record: %d %s, want 200", w.Code, w.Body)
	}
	mu.Lock()
	started := len(calls) == 2 && calls[0] == "POST /v1/vms/sb-left/start" && calls[1] == "GET /v1/vms"
	mu.Unlock()
	if !started {
		t.Errorf("worker calls: %v, want the start, then the list for the address the lost create answer carried", calls)
	}
	if w := req(h, "GET", "/api/v1/sandboxes/sb-left", ""); !strings.Contains(w.Body.String(), `"ip":"10.231.0.42"`) {
		t.Errorf("record after the start: %s, want the worker's address", w.Body)
	}
	if w := req(h, "DELETE", "/api/v1/sandboxes/sb-left", ""); w.Code != 204 {
		t.Errorf("delete of the settled record: %d %s, want 204", w.Code, w.Body)
	}
	evs, err := db.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, e := range evs {
		if e.SandboxID == "sb-left" {
			seq = append(seq, e.Event)
		}
	}
	if len(seq) != 2 || seq[0] != "started" || seq[1] != "deleted" {
		t.Errorf("usage events: %v, want [started deleted] (one interval, opened and closed)", seq)
	}
}

// D3: every listener stops accepting at the signal, while another one still
// drains a request: one after the other, the later ones kept accepting during
// the first's drain.
func TestStopDrainsAllListenersAtOnce(t *testing.T) {
	cfg, db := bootFixture(t)
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })
	old := stopDrain
	stopDrain = 2 * time.Second
	t.Cleanup(func() { stopDrain = old })

	inFlight := make(chan struct{})
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inFlight)
		<-release
	})
	var lns []net.Listener
	var servers []*http.Server
	for _, h := range []http.Handler{slow, srv.Handler()} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		hs := &http.Server{Handler: h, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, MaxHeaderBytes: maxHeaderBytes}
		go func() { _ = hs.Serve(ln) }()
		lns, servers = append(lns, ln), append(servers, hs)
	}
	go func() {
		if resp, err := http.Get("http://" + lns[0].Addr().String() + "/"); err == nil {
			resp.Body.Close()
		}
	}()
	<-inFlight

	sigs := make(chan os.Signal, 2)
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		<-stop
		close(flushed)
	}()
	done := make(chan struct{})
	go func() {
		stopOnSignal(sigs, servers, srv, stop, flushed, func(int) {})
		close(done)
	}()
	sigs <- syscall.SIGTERM
	time.Sleep(200 * time.Millisecond) // the first listener is still draining
	if c, err := net.DialTimeout("tcp", lns[1].Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("the second listener still accepts connections while the first drains")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not finish")
	}
}
