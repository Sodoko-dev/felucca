package main

// The stop (PS-16, GF-7): SIGTERM or SIGINT drains the requests in flight,
// closes the listeners, ends the background loops and waits for the final
// exec flush; a second signal, or the hard limit, exits with status 1.

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

// slowWorker answers a create and an exec after a delay, and says when each
// arrived.
func slowWorker(t *testing.T, delay time.Duration) (*httptest.Server, chan string) {
	arrived := make(chan string, 8)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- r.Method + " " + r.URL.Path
		time.Sleep(delay)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			_, _ = w.Write([]byte(`{"ip":"10.231.0.9"}`))
		case strings.HasSuffix(r.URL.Path, "/exec"):
			_, _ = w.Write([]byte(`{"exit_code":0,"stdout":"","stderr":""}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(ts.Close)
	return ts, arrived
}

// A create and an exec are in flight when SIGTERM arrives: both finish, the
// create's row is saved running, and the exec's usage row is in the final
// flush — with the real flusher wired as main wires it.
func TestStopDrainsTheRequestsInFlightThenFlushes(t *testing.T) {
	cfg, db := bootFixture(t)
	ts, arrived := slowWorker(t, 300*time.Millisecond)
	st := state.New()
	nodeID := st.RegisterNode("worker-1", ts.URL, 4, 8192, time.Now().Unix())
	st.Sandboxes = append(st.Sandboxes, &model.Sandbox{ID: "sb-run", Name: "a", Namespace: "default", NodeID: &nodeID,
		State: model.StateRunning, VCPUs: 1, MemMiB: 256})
	if err := db.SaveSnapshot(st.Snapshot(), nil); err != nil {
		t.Fatal(err)
	}
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{
		Handler:        srv.Handler(),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   60 * time.Second,
		MaxHeaderBytes: maxHeaderBytes,
	}
	go func() { _ = hs.Serve(ln) }()
	// As main wires them.
	sigs := make(chan os.Signal, 2)
	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		srv.UsageFlushLoop(stop)
		close(flushed)
	}()
	exited := make(chan int, 2)
	done := make(chan struct{})
	go func() {
		stopOnSignal(sigs, []*http.Server{hs}, srv, stop, flushed, func(code int) { exited <- code })
		close(done)
	}()

	base := "http://" + ln.Addr().String()
	codes := make(map[string]int)
	var mu sync.Mutex
	var wg sync.WaitGroup
	send := func(what, path, body string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rq, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
			rq.Header.Set("Authorization", "Bearer admin-tok")
			resp, err := http.DefaultClient.Do(rq)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				codes[what] = -1
				return
			}
			resp.Body.Close()
			codes[what] = resp.StatusCode
		}()
	}
	send("create", "/api/v1/sandboxes", `{"name":"b"}`)
	send("exec", "/api/v1/sandboxes/sb-run/exec", `{"cmd":["true"]}`)
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(3 * time.Second):
			t.Fatal("the requests did not reach the worker")
		}
	}
	sigs <- syscall.SIGTERM
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop did not finish")
	}
	wg.Wait()
	if codes["create"] != 201 || codes["exec"] != 200 {
		t.Errorf("requests in flight at the signal: create %d, exec %d; want 201 and 200 (drained, not cut)", codes["create"], codes["exec"])
	}
	select {
	case code := <-exited:
		t.Errorf("the stop exited with %d", code)
	default:
	}
	got := state.New()
	if err := db.LoadInto(got); err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, sb := range got.Sandboxes {
		if sb.State == model.StateRunning {
			running++
		}
	}
	if len(got.Sandboxes) != 2 || running != 2 {
		t.Errorf("database after the stop: %d sandboxes, %d running; want the drained create saved running", len(got.Sandboxes), running)
	}
	evs, err := db.ListUsage("", 0, math.MaxInt32)
	if err != nil {
		t.Fatal(err)
	}
	execRow := false
	for _, e := range evs {
		execRow = execRow || (e.SandboxID == "sb-run" && e.Event == "exec")
	}
	if !execRow {
		t.Error("the drained exec's usage row was not in the final flush")
	}
	if c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second); err == nil {
		c.Close()
		t.Error("the listener still accepts connections after the stop")
	}
}

// A second signal ends a stop whose flush hangs, with status 1.
func TestSecondSignalExitsAtOnce(t *testing.T) {
	cfg, db := bootFixture(t)
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })
	sigs := make(chan os.Signal, 2)
	stop := make(chan struct{})
	flushed := make(chan struct{}) // a flush that hangs
	t.Cleanup(func() { close(flushed) })
	exited := make(chan int, 2)
	go stopOnSignal(sigs, nil, srv, stop, flushed, func(code int) { exited <- code })
	sigs <- syscall.SIGTERM
	<-stop
	select {
	case code := <-exited:
		t.Fatalf("exited (%d) before the second signal", code)
	case <-time.After(100 * time.Millisecond):
	}
	sigs <- os.Interrupt
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit status after a second signal: %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second signal did not end the stop")
	}
}

// The hard limit ends a stop that does not finish, with status 1.
func TestStopHardLimitExits(t *testing.T) {
	cfg, db := bootFixture(t)
	srv, err := boot(cfg, db)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { stopAndWait(srv) })
	old := stopHardLimit
	stopHardLimit = 200 * time.Millisecond
	t.Cleanup(func() { stopHardLimit = old })
	sigs := make(chan os.Signal, 2)
	flushed := make(chan struct{}) // a flush that hangs
	t.Cleanup(func() { close(flushed) })
	exited := make(chan int, 2)
	go stopOnSignal(sigs, nil, srv, make(chan struct{}), flushed, func(code int) { exited <- code })
	sigs <- syscall.SIGTERM
	select {
	case code := <-exited:
		if code != 1 {
			t.Errorf("exit status at the hard limit: %d, want 1", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hard limit did not end the stop")
	}
}
