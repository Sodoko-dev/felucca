package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testEnv wires a fake feluccad (route table + ensure) and a fake backend
// (echoing which Host/path it saw) behind a Gateway.
type testEnv struct {
	gw       *Gateway
	backend  *httptest.Server
	feluccad  *httptest.Server
	routes   atomic.Value // []Route
	ensure   func(sandboxID string, port uint16) int
	backends atomic.Int64
}

func newTestEnv(t *testing.T, limits map[string]TenantLimit) *testEnv {
	t.Helper()
	env := &testEnv{}

	env.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.backends.Add(1)
		fmt.Fprintf(w, "backend saw host=%s path=%s xfp=%s", r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(env.backend.Close)

	env.feluccad = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-tok" {
			w.WriteHeader(404)
			return
		}
		switch r.URL.Path {
		case "/api/v1/routes":
			routes, _ := env.routes.Load().([]Route)
			_ = json.NewEncoder(w).Encode(struct {
				Routes []Route `json:"routes"`
			}{routes})
		case "/api/v1/routes/ensure":
			var req struct {
				SandboxID string `json:"sandbox_id"`
				Port      uint16 `json:"port"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			code := 404
			if env.ensure != nil {
				code = env.ensure(req.SandboxID, req.Port)
			}
			w.WriteHeader(code)
			fmt.Fprint(w, "{}")
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(env.feluccad.Close)

	env.gw = New(Config{
		Domain:       "sb.lab.test",
		FeluccadURL:   env.feluccad.URL,
		Token:        "admin-tok",
		Refresh:      time.Hour, // tests refresh explicitly
		TenantLimits: limits,
	})
	return env
}

// backendHostPort splits the httptest backend URL into host + port for routes.
func (env *testEnv) backendRoute(label, sandboxID, tenant, state string) Route {
	u, _ := url.Parse(env.backend.URL)
	host := u.Hostname()
	var port uint16
	fmt.Sscanf(u.Port(), "%d", &port)
	name, _, _ := strings.Cut(label, "--")
	return Route{
		Hostname: label, SandboxID: sandboxID, TenantID: tenant, Name: name,
		NodeHost: host, NodePort: port, GuestPort: 8069, State: state,
	}
}

func (env *testEnv) setRoutes(routes ...Route) {
	env.routes.Store(routes)
	env.gw.refresh()
}

func doHost(t *testing.T, gw *Gateway, host, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "http://placeholder"+path, nil)
	req.Host = host
	w := httptest.NewRecorder()
	gw.ServeHTTP(w, req)
	return w
}

func TestProxyRunningRoute(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))

	w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/web/login?db=x")
	if w.Code != 200 {
		t.Fatalf("proxy: got %d body %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The inbound Host must reach the backend (vhost-aware guests), along
	// with X-Forwarded-Proto.
	if !strings.Contains(body, "host=odoo--sb-1.sb.lab.test") || !strings.Contains(body, "path=/web/login") || !strings.Contains(body, "xfp=http") {
		t.Errorf("backend saw: %s", body)
	}
	// Host header with a port routes identically.
	w2 := doHost(t, env.gw, "odoo--sb-1.sb.lab.test:8088", "/")
	if w2.Code != 200 {
		t.Errorf("with port: got %d", w2.Code)
	}
}

func TestUnknownHostAndLabelShapes(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))

	for _, host := range []string{
		"odoo--sb-2.sb.lab.test",      // unknown sandbox
		"web--sb-1.sb.lab.test",       // unknown name
		"odoo--sb-1.other.test",       // wrong domain
		"x.odoo--sb-1.sb.lab.test",    // nested label
		"sb.lab.test",                 // bare domain
		"odoosb-1.sb.lab.test",        // no separator
	} {
		if w := doHost(t, env.gw, host, "/"); w.Code != 404 {
			t.Errorf("%s: got %d, want 404", host, w.Code)
		}
	}
}

func TestSleepingAndErrorStates(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(
		env.backendRoute("odoo--sb-1", "sb-1", "t-1", "sleeping"),
		env.backendRoute("odoo--sb-2", "sb-2", "t-1", "error"),
	)
	w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "asleep") {
		t.Errorf("sleeping: got %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Error("sleeping: missing Retry-After")
	}
	w2 := doHost(t, env.gw, "odoo--sb-2.sb.lab.test", "/")
	if w2.Code != 503 || strings.Contains(w2.Body.String(), "asleep") {
		t.Errorf("error state: got %d %q", w2.Code, w2.Body.String())
	}
}

func TestDynamicPortEnsure(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes() // empty table

	// feluccad allows the ensure and then serves the new route.
	env.ensure = func(sandboxID string, port uint16) int {
		if sandboxID == "sb-dyn" && port == 8069 {
			env.routes.Store([]Route{env.backendRoute("8069--sb-dyn", "sb-dyn", "t-1", "running")})
			return 200
		}
		return 404
	}
	w := doHost(t, env.gw, "8069--sb-dyn.sb.lab.test", "/")
	if w.Code != 200 {
		t.Fatalf("dynamic: got %d body %s", w.Code, w.Body.String())
	}

	// Opt-out sandbox: feluccad says 403, the gateway forwards the refusal.
	env.ensure = func(string, uint16) int { return 403 }
	w2 := doHost(t, env.gw, "9000--sb-locked.sb.lab.test", "/")
	if w2.Code != 403 {
		t.Errorf("dynamic disabled: got %d", w2.Code)
	}

	// Named (non-digit) labels never trigger ensure.
	called := false
	env.ensure = func(string, uint16) int { called = true; return 200 }
	doHost(t, env.gw, "web--sb-x.sb.lab.test", "/")
	if called {
		t.Error("named label must not hit the dynamic ensure path")
	}

	// Non-canonical digits (leading zeros, oversize) never trigger ensure:
	// they could ensure a port whose canonical label never matches, turning
	// every request into feluccad round-trips.
	for _, host := range []string{"08080--sb-dyn.sb.lab.test", "070--sb-dyn.sb.lab.test", "99999--sb-dyn.sb.lab.test", "0--sb-dyn.sb.lab.test"} {
		called = false
		w := doHost(t, env.gw, host, "/")
		if called || w.Code != 404 {
			t.Errorf("%s: ensure called=%v code=%d, want skipped + 404", host, called, w.Code)
		}
	}
}

func TestNodeLessRoute503(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.backendRoute("odoo--sb-1", "sb-1", "t-1", "error")
	r.NodeHost = "" // node gone; feluccad still serves the row for state-aware errors
	env.setRoutes(r)
	w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "error") {
		t.Errorf("node-less route: got %d %q, want 503 naming the state", w.Code, w.Body.String())
	}
}

func TestTenantToggleAndRateLimit(t *testing.T) {
	off := false
	env := newTestEnv(t, map[string]TenantLimit{
		"t-off":  {Enabled: &off},
		"t-slow": {RPS: 1},
	})
	env.setRoutes(
		env.backendRoute("a--sb-1", "sb-1", "t-off", "running"),
		env.backendRoute("b--sb-2", "sb-2", "t-slow", "running"),
	)

	if w := doHost(t, env.gw, "a--sb-1.sb.lab.test", "/"); w.Code != 403 {
		t.Errorf("disabled tenant: got %d", w.Code)
	}
	// 1 rps with burst 1: first passes, immediate second is limited.
	if w := doHost(t, env.gw, "b--sb-2.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("first limited req: got %d", w.Code)
	}
	if w := doHost(t, env.gw, "b--sb-2.sb.lab.test", "/"); w.Code != 429 {
		t.Errorf("second limited req: got %d, want 429", w.Code)
	}
}

func TestRefreshSurvivesFeluccadOutage(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))

	// feluccad goes away; the stale-but-working table keeps serving.
	env.feluccad.Close()
	env.gw.refresh()
	if w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("after outage: got %d, want 200 from cached route", w.Code)
	}
}

func TestHealthz(t *testing.T) {
	env := newTestEnv(t, nil)
	w := doHost(t, env.gw, "gw.lab.test", "/healthz")
	if w.Code != 200 {
		t.Errorf("healthz: got %d", w.Code)
	}
}

func TestBackendDown502(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running")
	env.setRoutes(r)
	env.backend.Close()
	w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/")
	if w.Code != 502 {
		t.Errorf("backend down: got %d, want 502", w.Code)
	}
}
