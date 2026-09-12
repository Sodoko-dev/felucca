// Tests for the sandbox ingress endpoints (expose.go). Internal package so
// they can swap srv.agentCall — no real agent is present in the test
// environment.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alpham/infra-saas/felucca/internal/agentclient"
	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// fakeAgent stubs the worker's expose API: allocates sequential node ports
// per (vm, guest_port) and records unexpose calls.
type fakeAgent struct {
	mu       sync.Mutex
	next     uint16
	ports    map[string]uint16 // "<id>/<guest_port>" -> node_port
	removed  []string
	failNext bool
}

func (f *fakeAgent) call(host string, port uint16, method, path string, body []byte, reqID string) (*agentclient.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext {
		f.failNext = false
		return nil, fmt.Errorf("fake agent down")
	}
	var req struct {
		GuestPort uint16 `json:"guest_port"`
	}
	_ = json.Unmarshal(body, &req)
	switch method {
	case http.MethodPost:
		key := path + "/" + fmt.Sprint(req.GuestPort)
		np, ok := f.ports[key]
		if !ok {
			f.next++
			np = 20000 + f.next
			f.ports[key] = np
		}
		return &agentclient.Response{Status: 200, Body: []byte(fmt.Sprintf(`{"node_port":%d}`, np))}, nil
	case http.MethodDelete:
		f.removed = append(f.removed, path+"/"+fmt.Sprint(req.GuestPort))
		return &agentclient.Response{Status: 200, Body: []byte(`{"ok":true}`)}, nil
	}
	return &agentclient.Response{Status: 404, Body: []byte(`{}`)}, nil
}

// newExposeTestServer returns a server with one ready node and one running
// admin-owned sandbox (with an IP), plus the fake agent.
func newExposeTestServer(t *testing.T) (*Server, *fakeAgent, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:         "admin-tok",
		UIDir:         tmp,
		StatePath:     filepath.Join(tmp, "state.json"),
		DBPath:        filepath.Join(tmp, "felucca.db"),
		IngressDomain: "sb.lab.test",
	}
	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(cfg, state.New(), db)
	fa := &fakeAgent{ports: map[string]uint16{}}
	srv.agentCall = fa.call

	// RegisterNode locks internally; CreateSandbox expects the caller-held
	// lock (mirrors the handlers).
	nodeID := srv.st.RegisterNode("worker-1", "192.168.104.1:9090", 4, 8192, 1000)
	srv.st.Lock()
	sb := srv.st.CreateSandbox("web", "default", nodeID, 1, 256, 1000)
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxIP(id, "10.231.0.5")
	srv.st.SetSandboxState(id, "running")
	return srv, fa, id
}

const adminAuth = "Bearer admin-tok"

func TestExposeHappyPath(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()

	w := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("expose: got %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Name      string `json:"name"`
		GuestPort uint16 `json:"guest_port"`
		NodePort  uint16 `json:"node_port"`
		Hostname  string `json:"hostname"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Hostname != "odoo--"+id {
		t.Errorf("hostname: got %q", resp.Hostname)
	}
	if resp.URL != "https://odoo--"+id+".sb.lab.test" {
		t.Errorf("url: got %q", resp.URL)
	}
	if resp.NodePort < 20000 || resp.GuestPort != 8069 {
		t.Errorf("ports: node %d guest %d", resp.NodePort, resp.GuestPort)
	}

	// Same name+port again is idempotent (200, same node port).
	w2 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	if w2.Code != 200 {
		t.Fatalf("idempotent expose: got %d", w2.Code)
	}
	// Same name, different port conflicts.
	w3 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8072}`, adminAuth)
	if w3.Code != 409 {
		t.Fatalf("conflicting expose: got %d", w3.Code)
	}
	// The sandbox JSON now carries the expose.
	w4 := doRequest(h, "GET", "/api/v1/sandboxes/"+id, "", adminAuth)
	var sb struct {
		Exposes []struct{ Name string `json:"name"` } `json:"exposes"`
	}
	_ = json.Unmarshal(w4.Body.Bytes(), &sb)
	if len(sb.Exposes) != 1 || sb.Exposes[0].Name != "odoo" {
		t.Errorf("sandbox exposes: %s", w4.Body.String())
	}
}

func TestExposeValidation(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()
	bad := []string{
		`{"name":"Odoo","port":1}`,            // uppercase
		`{"name":"-a","port":1}`,              // leading dash
		`{"name":"a-","port":1}`,              // trailing dash
		`{"name":"a--b","port":1}`,            // double dash (separator)
		`{"name":"8069","port":1}`,            // all digits (dynamic namespace)
		`{"name":"","port":1}`,                // empty
		`{"name":"` + string(make([]byte, 0)) + `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","port":1}`, // 33 chars
		`{"name":"ok","port":0}`,              // port 0
		`not json`,
	}
	for _, body := range bad {
		w := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", body, adminAuth)
		if w.Code != 400 {
			t.Errorf("body %s: got %d, want 400", body, w.Code)
		}
	}
}

func TestExposeNoIPAndUnknownSandbox(t *testing.T) {
	srv, _, _ := newExposeTestServer(t)
	h := srv.Handler()

	srv.st.Lock()
	noip := srv.st.CreateSandbox("noip", "default", "", 1, 256, 1000)
	noipID := noip.ID
	srv.st.Unlock()

	w := doRequest(h, "POST", "/api/v1/sandboxes/"+noipID+"/expose", `{"name":"a","port":80}`, adminAuth)
	if w.Code != 409 {
		t.Errorf("no-ip expose: got %d, want 409", w.Code)
	}
	w2 := doRequest(h, "POST", "/api/v1/sandboxes/sb-none/expose", `{"name":"a","port":80}`, adminAuth)
	if w2.Code != 404 {
		t.Errorf("unknown sandbox: got %d, want 404", w2.Code)
	}
}

func TestExposeTenantScoping(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()

	// Mint a tenant key; the sandbox is admin-owned, so the tenant sees 404.
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"acme"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body.String())
	}
	var tr struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tr)

	w2 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"x","port":80}`, "Bearer "+tr.APIKey)
	if w2.Code != 404 {
		t.Errorf("foreign expose: got %d, want 404", w2.Code)
	}
	w3 := doRequest(h, "GET", "/api/v1/routes", "", "Bearer "+tr.APIKey)
	if w3.Code != 404 {
		t.Errorf("tenant routes: got %d, want 404 (admin-only)", w3.Code)
	}
	w4 := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/x", "", "Bearer "+tr.APIKey)
	if w4.Code != 404 {
		t.Errorf("foreign unexpose: got %d, want 404", w4.Code)
	}
}

func TestUnexpose(t *testing.T) {
	srv, fa, id := newExposeTestServer(t)
	h := srv.Handler()

	doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	w := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/odoo", "", adminAuth)
	if w.Code != 204 {
		t.Fatalf("unexpose: got %d", w.Code)
	}
	if len(fa.removed) != 1 {
		t.Errorf("agent unexpose calls: %d, want 1", len(fa.removed))
	}
	// Gone from the sandbox and from the routes table.
	w2 := doRequest(h, "GET", "/api/v1/routes", "", adminAuth)
	var routes struct {
		Routes []json.RawMessage `json:"routes"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &routes)
	if len(routes.Routes) != 0 {
		t.Errorf("routes after unexpose: %s", w2.Body.String())
	}
	// Unknown name 404s.
	w3 := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/odoo", "", adminAuth)
	if w3.Code != 404 {
		t.Errorf("double unexpose: got %d", w3.Code)
	}
}

func TestUnexposeKeepsSharedPortRule(t *testing.T) {
	srv, fa, id := newExposeTestServer(t)
	h := srv.Handler()

	doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"a","port":80}`, adminAuth)
	doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"b","port":80}`, adminAuth)
	w := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/a", "", adminAuth)
	if w.Code != 204 {
		t.Fatalf("unexpose a: got %d", w.Code)
	}
	// The DNAT rule is shared with "b": the agent must NOT have been told to
	// remove it.
	if len(fa.removed) != 0 {
		t.Errorf("agent unexpose calls: %v, want none (port shared)", fa.removed)
	}
}

func TestRoutesTable(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()

	doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	w := doRequest(h, "GET", "/api/v1/routes", "", adminAuth)
	if w.Code != 200 {
		t.Fatalf("routes: got %d", w.Code)
	}
	var resp struct {
		Routes []struct {
			Hostname  string `json:"hostname"`
			SandboxID string `json:"sandbox_id"`
			NodeHost  string `json:"node_host"`
			NodePort  uint16 `json:"node_port"`
			State     string `json:"state"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Routes) != 1 {
		t.Fatalf("routes body: %s", w.Body.String())
	}
	r := resp.Routes[0]
	if r.Hostname != "odoo--"+id || r.NodeHost != "192.168.104.1" || r.NodePort < 20000 || r.State != "running" {
		t.Errorf("route: %+v", r)
	}
}

func TestEnsureRouteDynamicGate(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()

	// Default sandbox has dynamic ports off.
	w := doRequest(h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+id+`","port":8069}`, adminAuth)
	if w.Code != 403 {
		t.Fatalf("ensure (disabled): got %d", w.Code)
	}

	srv.st.Lock()
	srv.st.FindSandbox(id).AllowDynamicPorts = true
	srv.st.Unlock()

	w2 := doRequest(h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+id+`","port":8069}`, adminAuth)
	if w2.Code != 200 {
		t.Fatalf("ensure (enabled): got %d body %s", w2.Code, w2.Body.String())
	}
	var v struct {
		Hostname string `json:"hostname"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &v)
	if v.Hostname != "8069--"+id {
		t.Errorf("dynamic hostname: %q", v.Hostname)
	}
	// Repeat hits the idempotent path.
	w3 := doRequest(h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"`+id+`","port":8069}`, adminAuth)
	if w3.Code != 200 {
		t.Errorf("ensure repeat: got %d", w3.Code)
	}
	w4 := doRequest(h, "POST", "/api/v1/routes/ensure", `{"sandbox_id":"sb-none","port":1}`, adminAuth)
	if w4.Code != 404 {
		t.Errorf("ensure unknown: got %d", w4.Code)
	}
}

func TestExposeAgentFailure(t *testing.T) {
	srv, fa, id := newExposeTestServer(t)
	h := srv.Handler()
	fa.failNext = true
	w := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	if w.Code != 502 {
		t.Fatalf("agent-down expose: got %d", w.Code)
	}
	// Nothing persisted: a retry succeeds cleanly.
	w2 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"odoo","port":8069}`, adminAuth)
	if w2.Code != 201 {
		t.Errorf("retry after failure: got %d", w2.Code)
	}
}

// One sandbox must not be able to loop distinct names until the worker's
// shared node-port pool is empty.
func TestExposeSandboxCap(t *testing.T) {
	srv, _, id := newExposeTestServer(t)
	h := srv.Handler()

	for i := 0; i < maxExposesPerSandbox; i++ {
		body := fmt.Sprintf(`{"name":"svc-%d","port":%d}`, i, 8000+i)
		if w := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", body, adminAuth); w.Code != 201 {
			t.Fatalf("expose %d: got %d %s", i, w.Code, w.Body.String())
		}
	}
	over := fmt.Sprintf(`{"name":"svc-%d","port":%d}`, maxExposesPerSandbox, 8000+maxExposesPerSandbox)
	w := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", over, adminAuth)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "sandbox") {
		t.Fatalf("expose past the cap: got %d %s, want 429 sandbox", w.Code, w.Body.String())
	}
	// An existing name+port consumes no new port, so it is still served at
	// the cap.
	if w2 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", `{"name":"svc-0","port":8000}`, adminAuth); w2.Code != 200 {
		t.Errorf("idempotent repeat at the cap: got %d", w2.Code)
	}
	// Releasing one makes room again.
	if w3 := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"/expose/svc-0", "", adminAuth); w3.Code != 204 {
		t.Fatalf("unexpose: got %d", w3.Code)
	}
	if w4 := doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/expose", over, adminAuth); w4.Code != 201 {
		t.Errorf("expose after freeing a slot: got %d", w4.Code)
	}
}

// Nor may a tenant reach the same drain by spreading exposes over many
// in-quota sandboxes.
func TestExposeTenantCap(t *testing.T) {
	srv, _, base := newExposeTestServer(t)
	h := srv.Handler()

	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"acme"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body.String())
	}
	var tr struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil || tr.Tenant.ID == "" {
		t.Fatalf("tenant body: %s", w.Body.String())
	}
	auth := "Bearer " + tr.APIKey

	srv.st.Lock()
	nodeID := *srv.st.FindSandbox(base).NodeID
	srv.st.Unlock()

	// Seeding the rows directly is the point: the cap must read the tenant's
	// standing total, not just what this request path created.
	newSB := func(exposes int) string {
		srv.st.Lock()
		sb := srv.st.CreateSandbox("filler", "default", nodeID, 1, 256, 1000)
		sb.TenantID = tr.Tenant.ID
		for i := 0; i < exposes; i++ {
			sb.Exposes = append(sb.Exposes, model.Expose{
				Name: fmt.Sprintf("pre-%d", i), GuestPort: uint16(9000 + i), NodePort: uint16(21000 + i),
			})
		}
		sbID := sb.ID
		srv.st.Unlock()
		srv.st.SetSandboxIP(sbID, "10.231.0.6")
		srv.st.SetSandboxState(sbID, "running")
		return sbID
	}

	// Fill the tenant to one short of its total, keeping every sandbox under
	// the per-sandbox cap so it is the tenant total that bites.
	var last string
	for remaining := maxExposesPerTenant - 1; remaining > 0; {
		n := maxExposesPerSandbox - 1
		if n > remaining {
			n = remaining
		}
		last = newSB(n)
		remaining -= n
	}

	if w2 := doRequest(h, "POST", "/api/v1/sandboxes/"+last+"/expose", `{"name":"fill","port":7000}`, auth); w2.Code != 201 {
		t.Fatalf("expose at total-1: got %d %s", w2.Code, w2.Body.String())
	}
	w3 := doRequest(h, "POST", "/api/v1/sandboxes/"+last+"/expose", `{"name":"over","port":7001}`, auth)
	if w3.Code != 429 || !strings.Contains(w3.Body.String(), "tenant") {
		t.Errorf("expose past the tenant total: got %d %s, want 429 tenant", w3.Code, w3.Body.String())
	}
	// A brand-new sandbox is no way around it — the drain is the sum.
	fresh := newSB(0)
	if w4 := doRequest(h, "POST", "/api/v1/sandboxes/"+fresh+"/expose", `{"name":"over","port":7001}`, auth); w4.Code != 429 {
		t.Errorf("expose on a fresh sandbox past the tenant total: got %d %s", w4.Code, w4.Body.String())
	}
}

// ---- fork re-expose (the third writer of sb.Exposes) ----

// forkAgent is an httptest-backed worker. The fork handler calls
// agentclient.Request directly rather than the srv.agentCall seam, so the
// re-expose loop can only be driven against a real listener.
type forkAgent struct {
	mu      sync.Mutex
	exposes int // POST /v1/vms/{id}/expose calls
	next    uint16
}

func (f *forkAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ip":"10.231.0.10"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/expose"):
			f.exposes++
			f.next++
			w.WriteHeader(200)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"node_port":%d}`, 20000+f.next)))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

func (f *forkAgent) exposeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exposes
}

func newForkTestServer(t *testing.T) (*Server, *forkAgent, string) {
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
	fa := &forkAgent{}
	ts := httptest.NewServer(fa.handler())
	t.Cleanup(ts.Close)
	nodeID := srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, 1000)
	return srv, fa, nodeID
}

// forkTenantFixture creates a tenant, a running parent sandbox holding
// parentExposes seeded exposes, and enough filler sandboxes to bring the
// tenant's standing total to `total`. It returns the tenant auth header and
// the parent's id.
func forkTenantFixture(t *testing.T, srv *Server, nodeID string, parentExposes, total int) (string, string) {
	t.Helper()
	h := srv.Handler()
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"acme"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body.String())
	}
	var tr struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tr); err != nil || tr.Tenant.ID == "" {
		t.Fatalf("tenant body: %s", w.Body.String())
	}

	newSB := func(name string, exposes int) string {
		srv.st.Lock()
		sb := srv.st.CreateSandbox(name, "default", nodeID, 1, 256, 1000)
		sb.TenantID = tr.Tenant.ID
		for i := 0; i < exposes; i++ {
			sb.Exposes = append(sb.Exposes, model.Expose{
				Name: fmt.Sprintf("svc-%d", i), GuestPort: uint16(9000 + i), NodePort: uint16(21000 + i),
			})
		}
		id := sb.ID
		srv.st.Unlock()
		srv.st.SetSandboxIP(id, "10.231.0.5")
		srv.st.SetSandboxState(id, "running")
		return id
	}

	parent := newSB("parent", parentExposes)
	for remaining := total - parentExposes; remaining > 0; {
		n := maxExposesPerSandbox
		if n > remaining {
			n = remaining
		}
		newSB("filler", n)
		remaining -= n
	}
	return "Bearer " + tr.APIKey, parent
}

func forkChildExposes(t *testing.T, body []byte) []model.Expose {
	t.Helper()
	var child struct {
		ID      string         `json:"id"`
		Exposes []model.Expose `json:"exposes"`
	}
	if err := json.Unmarshal(body, &child); err != nil {
		t.Fatalf("fork body: %v (%s)", err, body)
	}
	return child.Exposes
}

// A fork re-creates the parent's ingress on the child, which burns fresh node
// ports from the worker's shared pool. Forking a fully-exposed parent must
// therefore stop at the tenant total, or a tenant with a large max_sandboxes
// drains the node for everybody by forking instead of exposing.
func TestForkReExposeHonoursTenantCap(t *testing.T) {
	srv, fa, nodeID := newForkTestServer(t)
	auth, parent := forkTenantFixture(t, srv, nodeID, 4, maxExposesPerTenant)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"child"}`, auth)
	if w.Code != 201 {
		t.Fatalf("fork: got %d %s", w.Code, w.Body.String())
	}
	if got := forkChildExposes(t, w.Body.Bytes()); len(got) != 0 {
		t.Errorf("child exposes at the tenant cap: got %d (%v), want 0", len(got), got)
	}
	if n := fa.exposeCalls(); n != 0 {
		t.Errorf("agent expose calls at the tenant cap: got %d, want 0", n)
	}
}

// With room for only part of the parent's ingress, the fork takes exactly the
// slots that are left — the cap is a running total, not a per-request gate.
func TestForkReExposeStopsAtRemainingTenantRoom(t *testing.T) {
	srv, fa, nodeID := newForkTestServer(t)
	auth, parent := forkTenantFixture(t, srv, nodeID, 4, maxExposesPerTenant-2)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+parent+"/fork", `{"name":"child"}`, auth)
	if w.Code != 201 {
		t.Fatalf("fork: got %d %s", w.Code, w.Body.String())
	}
	if got := forkChildExposes(t, w.Body.Bytes()); len(got) != 2 {
		t.Errorf("child exposes with room for 2: got %d (%v), want 2", len(got), got)
	}
	if n := fa.exposeCalls(); n != 2 {
		t.Errorf("agent expose calls with room for 2: got %d, want 2", n)
	}
}

// An admin-owned parent is unmetered, like every other admin ingress path:
// the whole parent set is re-created.
func TestForkReExposeAdminUnmetered(t *testing.T) {
	srv, fa, nodeID := newForkTestServer(t)
	srv.st.Lock()
	parent := srv.st.CreateSandbox("parent", "default", nodeID, 1, 256, 1000)
	for i := 0; i < 3; i++ {
		parent.Exposes = append(parent.Exposes, model.Expose{
			Name: fmt.Sprintf("svc-%d", i), GuestPort: uint16(9000 + i), NodePort: uint16(21000 + i),
		})
	}
	parentID := parent.ID
	srv.st.Unlock()
	srv.st.SetSandboxIP(parentID, "10.231.0.5")
	srv.st.SetSandboxState(parentID, "running")

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+parentID+"/fork", `{"name":"child"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("fork: got %d %s", w.Code, w.Body.String())
	}
	if got := forkChildExposes(t, w.Body.Bytes()); len(got) != 3 {
		t.Errorf("admin child exposes: got %d (%v), want 3", len(got), got)
	}
	if n := fa.exposeCalls(); n != 3 {
		t.Errorf("admin agent expose calls: got %d, want 3", n)
	}
}

func TestValidExposeName(t *testing.T) {
	good := []string{"odoo", "a", "chat-woot", "x1", "a1-b2", "abcdefghijklmnopqrstuvwxyz123456"}
	for _, n := range good {
		if !validExposeName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	bad := []string{"", "Odoo", "-a", "a-", "a--b", "8069", "0", "a_b", "a.b",
		"abcdefghijklmnopqrstuvwxyz1234567"}
	for _, n := range bad {
		if validExposeName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
}
