package server_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/server"
	"github.com/alpham/infra-saas/hearth/internal/state"
)

func newTestServer(t *testing.T, token string) (*server.Server, *state.State) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     token,
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	st := state.New()
	return server.New(cfg, st), st
}

func do(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ---- Healthz ----

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/healthz", "", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	var m map[string]bool
	json.Unmarshal(w.Body.Bytes(), &m)
	if !m["ok"] {
		t.Error("ok should be true")
	}
}

// ---- Bearer auth middleware ----

func TestAuthNoToken(t *testing.T) {
	// When no token configured, all /api/* is open.
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	if w.Code != 200 {
		t.Errorf("open without token: status %d", w.Code)
	}
}

func TestAuthWithTokenMissing(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	if w.Code != 401 {
		t.Errorf("missing auth: status %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "unauthorized" {
		t.Errorf("error body: %v", m)
	}
}

func TestAuthWithTokenWrong(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer wrongtoken")
	if w.Code != 401 {
		t.Errorf("wrong auth: status %d", w.Code)
	}
}

func TestAuthWithTokenCorrect(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken")
	if w.Code != 200 {
		t.Errorf("correct auth: status %d", w.Code)
	}
}

// ---- 404 for unknown routes ----

func TestUnknownRoute(t *testing.T) {
	srv, _ := newTestServer(t, "")
	tests := []struct {
		method, path string
	}{
		{"GET", "/api/v1/unknown"},
		{"POST", "/api/v1/nodes"},      // wrong method on known path → 404 (no 405)
		{"DELETE", "/api/v1/sandboxes"}, // wrong method → 404
		{"PUT", "/api/v1/sandboxes"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(srv.Handler(), tc.method, tc.path, "", "")
			if w.Code != 404 {
				t.Errorf("expected 404, got %d", w.Code)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "not found" {
				t.Errorf("error body: %v", m)
			}
		})
	}
}

// ---- Content-Length on responses ----

func TestContentLengthSet(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/healthz", "", "")
	cl := w.Header().Get("Content-Length")
	if cl == "" {
		t.Error("Content-Length not set")
	}
	if cl != "11" { // {"ok":true} = 11 bytes
		t.Errorf("Content-Length: %q", cl)
	}
}

// ---- Nodes ----

func TestListNodesEmpty(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &m)
	var nodes []json.RawMessage
	json.Unmarshal(m["nodes"], &nodes)
	if len(nodes) != 0 {
		t.Errorf("expected empty nodes, got %d", len(nodes))
	}
}

func TestAgentRegisterAndList(t *testing.T) {
	srv, _ := newTestServer(t, "")
	// Register a node.
	w := do(srv.Handler(), "POST", "/api/v1/agents/register",
		`{"hostname":"h1","addr":"1.2.3.4:9090","cpus":4,"mem_total_mib":8192}`, "")
	if w.Code != 200 {
		t.Fatalf("register: %d body=%s", w.Code, w.Body)
	}
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)
	id := reg["id"]
	if id == "" {
		t.Fatal("no id in register response")
	}

	// List nodes.
	w2 := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	var nl map[string][]map[string]json.RawMessage
	json.Unmarshal(w2.Body.Bytes(), &nl)
	if len(nl["nodes"]) != 1 {
		t.Errorf("expected 1 node, got %d", len(nl["nodes"]))
	}
}

func TestAgentRegisterIdempotent(t *testing.T) {
	srv, _ := newTestServer(t, "")
	body := `{"hostname":"h1","addr":"addr1","cpus":2,"mem_total_mib":1024}`
	w1 := do(srv.Handler(), "POST", "/api/v1/agents/register", body, "")
	w2 := do(srv.Handler(), "POST", "/api/v1/agents/register", body, "")

	var r1, r2 map[string]string
	json.Unmarshal(w1.Body.Bytes(), &r1)
	json.Unmarshal(w2.Body.Bytes(), &r2)
	if r1["id"] != r2["id"] {
		t.Errorf("idempotent register: got different ids %q vs %q", r1["id"], r2["id"])
	}
}

func TestHeartbeatUnknown(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "POST", "/api/v1/agents/heartbeat",
		`{"id":"node-00000000-99","mem_free_mib":1000,"vm_count":0,"pool_size":0}`, "")
	if w.Code != 404 {
		t.Errorf("unknown heartbeat: %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "unknown node" {
		t.Errorf("error body: %v", m)
	}
}

// ---- Sandboxes ----

func TestListSandboxesEmpty(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/api/v1/sandboxes", "", "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var m map[string][]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m["sandboxes"]) != 0 {
		t.Error("expected empty sandboxes")
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/api/v1/sandboxes/sb-00000000-99", "", "")
	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCreateSandboxNoNode(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "POST", "/api/v1/sandboxes",
		`{"name":"test","namespace":"default"}`, "")
	if w.Code != 503 {
		t.Errorf("expected 503, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "no ready node" {
		t.Errorf("error body: %v", m)
	}
}

// ---- Static serving ----

func TestStaticTraversalRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, path := range []string{"/../etc/passwd", "/foo/../bar", "/..%2Fetc"} {
		w := do(srv.Handler(), "GET", path, "", "")
		// Traversal: 404 with JSON error.
		if w.Code != 404 {
			// Note: /..%2F might not decode to ".." — only check literal ".."
			continue
		}
	}
	// Literal .. in path must be rejected.
	w := do(srv.Handler(), "GET", "/foo/../bar.html", "", "")
	if w.Code != 404 {
		t.Errorf("traversal path: expected 404, got %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not found" {
		t.Errorf("traversal error body: %v", m)
	}
}

func TestStaticIndexFallback(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>hi</html>"), 0o644)
	srv := server.New(cfg, state.New())

	// Unknown path should fall back to index.html (SPA routing).
	w := do(srv.Handler(), "GET", "/some/spa/route", "", "")
	if w.Code != 200 {
		t.Errorf("SPA fallback: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html>") {
		t.Error("SPA fallback: expected index.html content")
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("SPA fallback Content-Type: %q", ct)
	}
}

func TestStaticUINotFound(t *testing.T) {
	srv, _ := newTestServer(t, "") // UIDir is empty tmp dir, no index.html
	w := do(srv.Handler(), "GET", "/", "", "")
	if w.Code != 404 {
		t.Errorf("no ui: status %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "ui not found" {
		t.Errorf("error body: %v", m)
	}
}

func TestMIMETypes(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{UIDir: tmp, StatePath: filepath.Join(tmp, "state.json")}
	files := map[string]string{
		"app.js":   "application/javascript",
		"style.css": "text/css",
		"data.json": "application/json",
		"img.svg":  "image/svg+xml",
		"img.png":  "image/png",
		"file.bin": "application/octet-stream",
	}
	for name := range files {
		os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644)
	}
	srv := server.New(cfg, state.New())
	for name, wantCT := range files {
		w := do(srv.Handler(), "GET", "/"+name, "", "")
		got := w.Header().Get("Content-Type")
		if got != wantCT {
			t.Errorf("%s: Content-Type %q, want %q", name, got, wantCT)
		}
	}
}

// ---- Metrics ----

func TestMetricsOpen(t *testing.T) {
	srv, _ := newTestServer(t, "secrettoken")
	// /metrics is open even when token is configured.
	w := do(srv.Handler(), "GET", "/metrics", "", "")
	if w.Code != 200 {
		t.Errorf("metrics status: %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/plain; version=0.0.4" {
		t.Errorf("metrics Content-Type: %q", ct)
	}
}

func TestMetricsAllStates(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/metrics", "", "")
	body := w.Body.String()
	for _, st := range []string{"creating", "running", "paused", "stopped", "sleeping", "error"} {
		if !strings.Contains(body, `state="`+st+`"`) {
			t.Errorf("metrics missing state=%q", st)
		}
	}
}

func TestMetricsExecsTotal(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/metrics", "", "")
	if !strings.Contains(w.Body.String(), "hearth_execs_total") {
		t.Error("metrics missing hearth_execs_total")
	}
}

// ---- Exec endpoint ----

// seedRunningNode registers a node and injects a sandbox in the given state
// directly into the state store, returning the sandbox id and the fake agent
// server URL (so the caller can set up stub responses).
func seedSandboxWithState(t *testing.T, h http.Handler, st *state.State, sbState model.SandboxState) (srvHandler *server.Server, sbID string) {
	t.Helper()
	// We need an actual server with the real state already seeded; cast back.
	// Instead: register node via API, then inject sandbox via state directly.
	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	_ = h // unused — caller passes nil to signal "use the returned srv"
	srv := server.New(cfg, st)
	return srv, ""
}

// newServerWithRunningFakeAgent creates a test server with one registered node
// pointing at a fake agent httptest.Server, and a sandbox in the given state.
func newServerWithFakeAgent(t *testing.T, agentHandler http.HandlerFunc, sbState model.SandboxState) (*server.Server, *state.State, string, *httptest.Server) {
	t.Helper()
	fakeAgent := httptest.NewServer(agentHandler)
	t.Cleanup(fakeAgent.Close)

	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	st := state.New()
	srv := server.New(cfg, st)

	// Register a node pointing at the fake agent.
	agentURL := fakeAgent.URL // e.g. http://127.0.0.1:PORT
	now := time.Now().Unix()
	nodeID := st.RegisterNode("testhost", agentURL, 4, 8192, now)

	// Create sandbox directly in state.
	st.Lock()
	sb := st.CreateSandbox("testsb", "default", nodeID, 1, 256, now)
	sbID := sb.ID
	st.Unlock()
	st.SetSandboxState(sbID, sbState)

	return srv, st, sbID, fakeAgent
}

func TestExecUnknownID(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "POST", "/api/v1/sandboxes/sb-nonexistent/exec",
		`{"cmd":["echo","hi"]}`, "")
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not found" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecNonRunning(t *testing.T) {
	for _, sbState := range []model.SandboxState{model.StateStopped, model.StatePaused, model.StateSleeping, model.StateCreating} {
		sbState := sbState
		t.Run(string(sbState), func(t *testing.T) {
			srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
				// should never be called
				t.Error("agent should not be called for non-running sandbox")
			}, sbState)
			w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
				`{"cmd":["echo","hi"]}`, "")
			if w.Code != 409 {
				t.Fatalf("state=%s: expected 409, got %d body=%s", sbState, w.Code, w.Body)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "not running" {
				t.Errorf("state=%s error body: %v", sbState, m)
			}
		})
	}
}

func TestExecHappyPath(t *testing.T) {
	agentResp := `{"ok":true,"exit_code":0,"stdout":"hello\n","stderr":""}`
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("agent: unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(agentResp)))
		w.WriteHeader(200)
		io.WriteString(w, agentResp)
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["echo","hello"]}`, "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body)
	}
	// Body must be passed through verbatim.
	if w.Body.String() != agentResp {
		t.Errorf("body mismatch: got %q want %q", w.Body.String(), agentResp)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: %q", ct)
	}
	if cl := w.Header().Get("Content-Length"); cl == "" {
		t.Error("Content-Length not set")
	}
}

func TestExecAgent501(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		body := `{"error":"guest agent unavailable"}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(501)
		io.WriteString(w, body)
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["ls"]}`, "")
	if w.Code != 501 {
		t.Fatalf("expected 501, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "guest agent unavailable" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecAgentDown(t *testing.T) {
	// Start a fake agent and immediately close it so the connection is refused.
	fakeAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	fakeAgent.Close() // closed before the request arrives

	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	st := state.New()
	srv := server.New(cfg, st)
	now := time.Now().Unix()
	nodeID := st.RegisterNode("testhost", fakeAgent.URL, 4, 8192, now)
	st.Lock()
	sb := st.CreateSandbox("testsb", "default", nodeID, 1, 256, now)
	sbID := sb.ID
	st.Unlock()
	st.SetSandboxState(sbID, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["ls"]}`, "")
	if w.Code != 502 {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "agent exec failed" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecBadJSON(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent should not be called for bad json")
	}, model.StateRunning)

	for _, body := range []string{
		`not json`,
		`{"cmd":[]}`,           // empty cmd
		`{"cmd":123}`,          // cmd not array
		`{"cmd":[1,2,3]}`,      // cmd elements not strings
	} {
		body := body
		t.Run(body, func(t *testing.T) {
			w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID), body, "")
			if w.Code != 400 {
				t.Fatalf("body=%q: expected 400, got %d resp=%s", body, w.Code, w.Body)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "bad json" {
				t.Errorf("body=%q error: %v", body, m)
			}
		})
	}
}

func TestExecTimeoutCap(t *testing.T) {
	// Verify that timeout_ms > 300000 is capped: the agent receives timeout_ms == 300000.
	var receivedTimeoutMs int64
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TimeoutMs int64 `json:"timeout_ms"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		receivedTimeoutMs = req.TimeoutMs
		body := `{"ok":true,"exit_code":0,"stdout":"","stderr":""}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(200)
		io.WriteString(w, body)
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["true"],"timeout_ms":999999}`, "")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if receivedTimeoutMs != 300000 {
		t.Errorf("agent received timeout_ms=%d, want 300000", receivedTimeoutMs)
	}
}
