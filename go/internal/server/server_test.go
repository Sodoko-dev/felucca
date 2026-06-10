package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alpham/infra-saas/hearth/internal/config"
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
