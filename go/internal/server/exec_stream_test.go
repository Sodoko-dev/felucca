// Tests for the streamed exec arm of execSandbox (v4 P5.1). Internal package
// (like templates_test.go) so the fake agent can be registered as a real node
// address — the relay speaks real HTTP to the agent, and the SSE side lands
// in the recorder doRequest returns.
package server

import (
	"encoding/json"
	"io"
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

// execAgent fakes the worker exec API: it records the forwarded body and,
// when that body carries "stream":true, replies with NDJSON frames flushed
// one at a time (the chunked shape the real agent produces). A non-zero
// status short-circuits to an error reply instead.
type execAgent struct {
	mu       sync.Mutex
	lastBody []byte
	status   int      // 0 → 200
	frames   []string // NDJSON lines written on the streaming path
}

func (a *execAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.lastBody = body
		status := a.status
		frames := append([]string(nil), a.frames...)
		a.mu.Unlock()
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"agent says no"}`))
			return
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(200)
			fl, _ := w.(http.Flusher)
			for _, line := range frames {
				_, _ = w.Write([]byte(line + "\n"))
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		// Buffered exec: one JSON document.
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true,"exit_code":0,"stdout":"hi\n","stderr":"","truncated":false}`))
	}
}

func (a *execAgent) body() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return string(a.lastBody)
}

// newExecStreamTestServer wires a server to the fake agent as its one node
// and seeds a sandbox in the given state, returning the sandbox id.
func newExecStreamTestServer(t *testing.T, fa *execAgent, sbState model.SandboxState) (*Server, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
	}
	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(cfg, state.New(), db)

	ts := httptest.NewServer(fa.handler())
	t.Cleanup(ts.Close)
	now := time.Now().Unix()
	nodeID := srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, now)
	srv.st.Lock()
	sb := srv.st.CreateSandbox("streamer", "default", nodeID, 1, 256, now)
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id, sbState)
	return srv, id
}

// sseEvents splits an SSE body into its data payloads, failing the test on
// any frame that isn't exactly "data: <payload>\n\n".
func sseEvents(t *testing.T, body string) []string {
	t.Helper()
	var events []string
	for _, chunk := range strings.Split(body, "\n\n") {
		if chunk == "" {
			continue
		}
		if !strings.HasPrefix(chunk, "data: ") {
			t.Fatalf("SSE chunk without data prefix: %q", chunk)
		}
		events = append(events, strings.TrimPrefix(chunk, "data: "))
	}
	return events
}

func TestExecStreamHappyPath(t *testing.T) {
	frames := []string{
		`{"stream":"stdout","data":"line one\n"}`,
		`{"stream":"stdout","data":"line two\n"}`,
		`{"stream":"stderr","data":"warning\n"}`,
		`{"done":true,"ok":true,"exit_code":7,"truncated":false}`,
	}
	fa := &execAgent{frames: frames}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["echo","hi"],"timeout_ms":1000}`, adminAuth)
	if w.Code != 200 {
		t.Fatalf("stream exec: got %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream; charset=utf-8" {
		t.Errorf("content-type: %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control: %q", cc)
	}
	if ab := w.Header().Get("X-Accel-Buffering"); ab != "no" {
		t.Errorf("x-accel-buffering: %q", ab)
	}

	events := sseEvents(t, w.Body.String())
	if len(events) != len(frames) {
		t.Fatalf("got %d events %v, want %d", len(events), events, len(frames))
	}
	for i, want := range frames {
		if events[i] != want {
			t.Errorf("event %d: got %q, want %q", i, events[i], want)
		}
	}
	var final struct {
		Done     bool `json:"done"`
		OK       bool `json:"ok"`
		ExitCode int  `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-1]), &final); err != nil {
		t.Fatalf("final frame: %v", err)
	}
	if !final.Done || !final.OK || final.ExitCode != 7 {
		t.Errorf("final frame: %+v", final)
	}
}

func TestExecStreamInterrupted(t *testing.T) {
	// Agent stream ends after one frame, no terminal done frame: feluccad
	// must append the synthetic interruption event.
	fa := &execAgent{frames: []string{`{"stream":"stdout","data":"partial\n"}`}}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["sleep","99"]}`, adminAuth)
	if w.Code != 200 {
		t.Fatalf("stream exec: got %d body %s", w.Code, w.Body.String())
	}
	events := sseEvents(t, w.Body.String())
	if len(events) != 2 {
		t.Fatalf("got %d events %v, want 2", len(events), events)
	}
	if events[0] != `{"stream":"stdout","data":"partial\n"}` {
		t.Errorf("first event: %q", events[0])
	}
	if events[1] != `{"done":true,"ok":false,"error":"stream interrupted"}` {
		t.Errorf("synthetic done frame: %q", events[1])
	}
}

func TestExecStreamAgent501(t *testing.T) {
	fa := &execAgent{status: 501}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["echo","hi"]}`, adminAuth)
	if w.Code != 501 {
		t.Fatalf("agent 501: got %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: %q (must stay JSON, not SSE)", ct)
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "guest agent unavailable" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecStreamUnknownSandbox(t *testing.T) {
	fa := &execAgent{}
	srv, _ := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/sb-nonexistent/exec?stream=1",
		`{"cmd":["echo","hi"]}`, adminAuth)
	if w.Code != 404 {
		t.Fatalf("unknown sandbox: got %d body %s", w.Code, w.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not found" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecStreamNotRunning(t *testing.T) {
	fa := &execAgent{}
	srv, id := newExecStreamTestServer(t, fa, model.StateStopped)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["echo","hi"]}`, adminAuth)
	if w.Code != 409 {
		t.Fatalf("stopped sandbox: got %d body %s", w.Code, w.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not running" {
		t.Errorf("error body: %v", m)
	}
	if fa.body() != "" {
		t.Error("agent should not be called for a non-running sandbox")
	}
}

func TestExecStreamForwardedBody(t *testing.T) {
	fa := &execAgent{frames: []string{`{"done":true,"ok":true,"exit_code":0,"truncated":false}`}}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["uname","-a"],"timeout_ms":2500}`, adminAuth)
	if w.Code != 200 {
		t.Fatalf("stream exec: got %d body %s", w.Code, w.Body.String())
	}
	forwarded := fa.body()
	if !strings.Contains(forwarded, `"stream":true`) {
		t.Errorf("forwarded body missing stream flag: %s", forwarded)
	}
	if !strings.Contains(forwarded, `"cmd":["uname","-a"]`) || !strings.Contains(forwarded, `"timeout_ms":2500`) {
		t.Errorf("forwarded body: %s", forwarded)
	}
}

func TestExecStreamTimeoutRange(t *testing.T) {
	// The stream arm derives its own connection deadline from timeout_ms, so
	// the range check must land before the branch — not only on the buffered
	// path.
	fa := &execAgent{frames: []string{`{"done":true,"ok":true,"exit_code":0,"truncated":false}`}}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec?stream=1",
		`{"cmd":["sleep","999999"],"timeout_ms":-9300000000000}`, adminAuth)
	if w.Code != 400 {
		t.Fatalf("negative timeout on stream arm: got %d body %s", w.Code, w.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "timeout_ms out of range" {
		t.Errorf("error body: %v", m)
	}
	if fa.body() != "" {
		t.Error("agent must not be called with an out-of-range timeout")
	}
}

func TestExecBufferedUnchanged(t *testing.T) {
	// Regression guard: without ?stream=1 the route stays the buffered JSON
	// proxy and the forwarded body omits the stream flag.
	fa := &execAgent{}
	srv, id := newExecStreamTestServer(t, fa, model.StateRunning)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+id+"/exec",
		`{"cmd":["echo","hi"],"timeout_ms":1000}`, adminAuth)
	if w.Code != 200 {
		t.Fatalf("buffered exec: got %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: %q", ct)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("buffered body not JSON: %v (%s)", err, w.Body.String())
	}
	if m["ok"] != true {
		t.Errorf("buffered body: %s", w.Body.String())
	}
	if strings.Contains(fa.body(), `"stream"`) {
		t.Errorf("buffered forwarded body must omit stream: %s", fa.body())
	}
}
