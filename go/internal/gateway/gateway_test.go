package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/ingress"
)

// testEnv wires a fake feluccad (route table + ensure + wake) and two fake
// backends (echoing which backend, Host and path they saw) behind a Gateway.
type testEnv struct {
	gw       *Gateway
	backend  *httptest.Server // echoes be=1
	backend2 *httptest.Server // echoes be=2
	feluccad *httptest.Server
	routes   atomic.Value // []Route
	ensure   func(sandboxID string, port uint16) int
	backends atomic.Int64
	conns    atomic.Int64 // connections accepted by backend
	conns2   atomic.Int64 // connections accepted by backend2
	fetches  atomic.Int64 // GET /api/v1/routes served
	wakes    atomic.Int64 // POST .../wake served
	skew     atomic.Int64 // added to the gateway's clock (time.Duration)

	ensureRetry string       // Retry-After on a non-200 ensure answer; set before requests
	ensures     atomic.Int64 // POST /api/v1/routes/ensure served
	failFetch   atomic.Bool  // GET /api/v1/routes answers 500
	activities  atomic.Int64 // POST /api/v1/routes/activity served

	hmu     sync.Mutex
	onFetch func(n int64)                        // runs after the table is read, before it is sent
	onWake  func(w http.ResponseWriter, n int64) // answers the wake; nil = 404
	// activity, when set, holds every activity report until it is closed.
	activity chan struct{}
}

func (env *testEnv) setOnFetch(f func(n int64)) {
	env.hmu.Lock()
	env.onFetch = f
	env.hmu.Unlock()
}

func (env *testEnv) setOnWake(f func(w http.ResponseWriter, n int64)) {
	env.hmu.Lock()
	env.onWake = f
	env.hmu.Unlock()
}

// newEchoBackend answers with its id and what it saw; an "Upgrade: echo-test"
// request is switched to a raw connection that echoes one line.
func newEchoBackend(t *testing.T, env *testEnv, id string, conns *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.backends.Add(1)
		if strings.EqualFold(r.Header.Get("Upgrade"), "echo-test") {
			conn, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: echo-test\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			brw.WriteString("echo:" + line)
			brw.Flush()
			return
		}
		fmt.Fprintf(w, "be=%s backend saw host=%s path=%s xfp=%s", id, r.Host, r.URL.Path, r.Header.Get("X-Forwarded-Proto"))
	}))
	if conns != nil {
		srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
			if s == http.StateNew {
				conns.Add(1)
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func newTestEnv(t *testing.T, limits map[string]TenantLimit) *testEnv {
	t.Helper()
	env := &testEnv{}

	env.backend = newEchoBackend(t, env, "1", &env.conns)
	env.backend2 = newEchoBackend(t, env, "2", &env.conns2)

	env.feluccad = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-tok" {
			w.WriteHeader(404)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/sandboxes/") && strings.HasSuffix(r.URL.Path, "/wake") && r.Method == http.MethodPost {
			n := env.wakes.Add(1)
			env.hmu.Lock()
			f := env.onWake
			env.hmu.Unlock()
			if f == nil {
				w.WriteHeader(404)
				return
			}
			f(w, n)
			return
		}
		switch r.URL.Path {
		case "/api/v1/routes":
			n := env.fetches.Add(1)
			if env.failFetch.Load() {
				w.WriteHeader(500)
				return
			}
			routes, _ := env.routes.Load().([]Route)
			env.hmu.Lock()
			f := env.onFetch
			env.hmu.Unlock()
			if f != nil {
				f(n)
			}
			_ = json.NewEncoder(w).Encode(struct {
				Routes []Route `json:"routes"`
			}{routes})
		case "/api/v1/routes/activity":
			env.activities.Add(1)
			env.hmu.Lock()
			hold := env.activity
			env.hmu.Unlock()
			if hold != nil {
				<-hold
			}
			fmt.Fprint(w, "{}")
		case "/api/v1/routes/ensure":
			var req struct {
				SandboxID string `json:"sandbox_id"`
				Port      uint16 `json:"port"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			env.ensures.Add(1)
			code := 404
			if env.ensure != nil {
				code = env.ensure(req.SandboxID, req.Port)
			}
			if code < 0 { // no answer at all: the connection just closes
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			if code != 200 && env.ensureRetry != "" {
				w.Header().Set("Retry-After", env.ensureRetry)
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
		FeluccadURL:  env.feluccad.URL,
		Token:        "admin-tok",
		Refresh:      time.Hour, // tests refresh explicitly
		TenantLimits: limits,
	})
	// The real clock plus a skew a test can move: tables age without sleeping.
	env.gw.now = func() time.Time { return time.Now().Add(time.Duration(env.skew.Load())) }
	return env
}

// backendRoute builds a route to the first echo backend.
func (env *testEnv) backendRoute(label, sandboxID, tenant, state string) Route {
	return routeTo(env.backend, label, sandboxID, tenant, state)
}

// routeTo splits an httptest backend URL into host + port for a route.
func routeTo(srv *httptest.Server, label, sandboxID, tenant, state string) Route {
	u, _ := url.Parse(srv.URL)
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
		"odoo--sb-2.sb.lab.test",   // unknown sandbox
		"web--sb-1.sb.lab.test",    // unknown name
		"odoo--sb-1.other.test",    // wrong domain
		"x.odoo--sb-1.sb.lab.test", // nested label
		"sb.lab.test",              // bare domain
		"odoosb-1.sb.lab.test",     // no separator
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

	// feluccad goes away; the old-but-working table keeps serving while it is
	// younger than max-stale...
	env.feluccad.Close()
	env.gw.refresh()
	if w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("after outage: got %d, want 200 from cached route", w.Code)
	}
	env.skew.Store(int64(540 * time.Second))
	env.gw.refresh()
	if w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("outage inside max-stale: got %d, want 200 from cached route", w.Code)
	}
	// ...and fails closed beyond it: its node ports may belong to others now.
	env.skew.Store(int64(660 * time.Second))
	env.gw.refresh()
	assertTableUnavailable(t, doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"))
}

func assertTableUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" || !strings.Contains(w.Body.String(), `{"error":"route table unavailable"}`) {
		t.Errorf("got %d Retry-After=%q %q, want 503 route table unavailable with Retry-After: 5",
			w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
}

// TestOldTableFailsClosed: a table older than max-stale, or none at all,
// answers 503 for every sandbox request; /readyz follows, /healthz does not.
func TestOldTableFailsClosed(t *testing.T) {
	env := newTestEnv(t, nil)
	host := "odoo--sb-1.sb.lab.test"
	ready := func() int { return doHost(t, env.gw, "gw.lab.test", "/readyz").Code }
	healthy := func() *httptest.ResponseRecorder { return doHost(t, env.gw, "gw.lab.test", "/healthz") }
	checkHealthz := func(when string) {
		t.Helper()
		if w := healthy(); w.Code != 200 || w.Body.String() != "ok\n" {
			t.Errorf("%s: healthz got %d %q, want 200 ok", when, w.Code, w.Body.String())
		}
	}

	// Before the first fetch: 503, not 404.
	assertTableUnavailable(t, doHost(t, env.gw, host, "/"))
	if c := ready(); c != 503 {
		t.Errorf("before first fetch: readyz %d, want 503", c)
	}
	checkHealthz("before first fetch")

	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))
	if w := doHost(t, env.gw, host, "/"); w.Code != 200 {
		t.Errorf("fresh: got %d", w.Code)
	}
	if w := doHost(t, env.gw, "gw.lab.test", "/readyz"); w.Code != 200 || w.Body.String() != "ok\n" {
		t.Errorf("fresh: readyz %d %q, want 200 ok", w.Code, w.Body.String())
	}
	checkHealthz("fresh")

	env.skew.Store(int64(540 * time.Second))
	if w := doHost(t, env.gw, host, "/"); w.Code != 200 {
		t.Errorf("540s old: got %d, want 200", w.Code)
	}
	if c := ready(); c != 200 {
		t.Errorf("540s old: readyz %d, want 200", c)
	}

	env.skew.Store(int64(660 * time.Second))
	assertTableUnavailable(t, doHost(t, env.gw, host, "/"))
	assertTableUnavailable(t, doHost(t, env.gw, "web--sb-9.sb.lab.test", "/")) // unknown label too
	if c := ready(); c != 503 {
		t.Errorf("660s old: readyz %d, want 503", c)
	}
	checkHealthz("stale")

	// A later successful fetch serves again.
	env.gw.refresh()
	if w := doHost(t, env.gw, host, "/"); w.Code != 200 {
		t.Errorf("after a fresh fetch: got %d, want 200", w.Code)
	}
	if c := ready(); c != 200 {
		t.Errorf("after a fresh fetch: readyz %d, want 200", c)
	}
	checkHealthz("fresh again")
}

// TestOldTableWallClockAge: with times that carry no monotonic reading (the
// wall clock, as after a host suspend), a table older than max-stale still
// fails closed, and a fetch afterwards serves again.
func TestOldTableWallClockAge(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.now = func() time.Time { return time.Now().Round(0).Add(time.Duration(env.skew.Load())) }
	host := "odoo--sb-1.sb.lab.test"
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))
	if w := doHost(t, env.gw, host, "/"); w.Code != 200 {
		t.Errorf("fresh: got %d, want 200", w.Code)
	}
	env.skew.Store(int64(660 * time.Second))
	assertTableUnavailable(t, doHost(t, env.gw, host, "/"))
	if c := doHost(t, env.gw, "gw.lab.test", "/readyz").Code; c != 503 {
		t.Errorf("wall-clock stale: readyz %d, want 503", c)
	}
	env.gw.refresh()
	if w := doHost(t, env.gw, host, "/"); w.Code != 200 {
		t.Errorf("after a fresh fetch: got %d, want 200", w.Code)
	}
}

func TestTableAgeTakesTheLargerClock(t *testing.T) {
	for _, c := range []struct {
		name            string
		mono, wall, age time.Duration
	}{
		{"frozen host: monotonic stopped, wall kept counting", 10 * time.Second, 700 * time.Second, 700 * time.Second},
		{"wall clock stepped back", 700 * time.Second, -5 * time.Second, 700 * time.Second},
		{"clocks agree", 30 * time.Second, 30 * time.Second, 30 * time.Second},
	} {
		if got := tableAge(c.mono, c.wall); got != c.age {
			t.Errorf("%s: tableAge(%v, %v) = %v, want %v", c.name, c.mono, c.wall, got, c.age)
		}
	}
}

// TestOlderFetchNeverReplacesNewerTable: a fetch that started first but
// finished last must not put its (older) table back.
func TestOlderFetchNeverReplacesNewerTable(t *testing.T) {
	env := newTestEnv(t, nil)
	env.routes.Store([]Route{env.backendRoute("old--sb-1", "sb-1", "t-1", "running")})
	slowStarted, release := make(chan struct{}), make(chan struct{})
	env.setOnFetch(func(n int64) {
		if n == 1 { // the slow one: it has already read the OLD table
			close(slowStarted)
			<-release
		}
	})
	done := make(chan struct{})
	go func() { env.gw.refresh(); close(done) }()
	<-slowStarted

	env.skew.Store(int64(time.Second)) // the second fetch starts strictly later
	env.routes.Store([]Route{env.backendRoute("new--sb-2", "sb-2", "t-1", "running")})
	env.gw.refresh()
	close(release)
	<-done

	if w := doHost(t, env.gw, "new--sb-2.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("newer table: got %d, want 200", w.Code)
	}
	if w := doHost(t, env.gw, "old--sb-1.sb.lab.test", "/"); w.Code != 404 {
		t.Errorf("older table came back: old route got %d, want 404", w.Code)
	}
}

func TestValidateTimings(t *testing.T) {
	for _, c := range []struct {
		refresh, maxStale time.Duration
		ok                bool
		names             string // a bound the message must name
	}{
		{5 * time.Second, 600 * time.Second, true, ""},
		{5 * time.Second, 30 * time.Second, true, ""},
		{5 * time.Second, 29 * time.Second, false, "30s"},
		{5 * time.Second, 601 * time.Second, false, "10m0s"},
		{1 * time.Second, 22 * time.Second, true, ""},
		{500 * time.Millisecond, 600 * time.Second, false, "1s"},
		{0, 600 * time.Second, false, "1s"},
		{60 * time.Second, 140 * time.Second, true, ""},
		{60 * time.Second, 139 * time.Second, false, "2m20s"},
		{61 * time.Second, 600 * time.Second, false, "1m0s"},
	} {
		err := ValidateTimings(c.refresh, c.maxStale)
		if (err == nil) != c.ok {
			t.Errorf("refresh=%v max-stale=%v: err=%v, want ok=%v", c.refresh, c.maxStale, err, c.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.names) {
			t.Errorf("refresh=%v max-stale=%v: %q does not name the bound %s", c.refresh, c.maxStale, err, c.names)
		}
	}
}

func TestTransportIdleTimeoutIsTheQuarantineConstant(t *testing.T) {
	gw := New(Config{Domain: "sb.lab.test"})
	if gw.transport.IdleConnTimeout != ingress.UpstreamIdleTimeout {
		t.Errorf("IdleConnTimeout %v, want ingress.UpstreamIdleTimeout %v", gw.transport.IdleConnTimeout, ingress.UpstreamIdleTimeout)
	}
	if gw.cfg.MaxStale != ingress.GatewayMaxStaleCeiling {
		t.Errorf("default MaxStale %v, want %v", gw.cfg.MaxStale, ingress.GatewayMaxStaleCeiling)
	}
}

// ---- FLU-6 R9: upstream connections are pooled per sandbox ----

// TestUpstreamConnectionNotReusedForAnotherSandbox: the same node:port moves
// from sandbox A to sandbox B; B's request must open a new connection, not
// ride A's pooled one (which the worker may still pin to A).
func TestUpstreamConnectionNotReusedForAnotherSandbox(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("web--sb-a", "sb-a", "t-1", "running"))
	if w := doHost(t, env.gw, "web--sb-a.sb.lab.test", "/"); w.Code != 200 {
		t.Fatalf("A: got %d", w.Code)
	}
	if c := env.conns.Load(); c != 1 {
		t.Fatalf("after A: %d connections, want 1", c)
	}

	env.setRoutes(env.backendRoute("web--sb-b", "sb-b", "t-2", "running")) // same node:port
	if w := doHost(t, env.gw, "web--sb-b.sb.lab.test", "/"); w.Code != 200 {
		t.Fatalf("B: got %d", w.Code)
	}
	if c := env.conns.Load(); c != 2 {
		t.Errorf("after B: %d connections, want 2 (B must not reuse A's)", c)
	}
}

// TestUpstreamNewConnectionWhenNodeAddressChanges: the same sandbox and guest
// port move to another node address; the next request goes there on a new
// connection, not over the idle one to the old address.
func TestUpstreamNewConnectionWhenNodeAddressChanges(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("web--sb-a", "sb-a", "t-1", "running"))
	if w := doHost(t, env.gw, "web--sb-a.sb.lab.test", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "be=1") {
		t.Fatalf("before the move: got %d %q", w.Code, w.Body.String())
	}
	env.setRoutes(routeTo(env.backend2, "web--sb-a", "sb-a", "t-1", "running")) // same sandbox + guest port
	if w := doHost(t, env.gw, "web--sb-a.sb.lab.test", "/"); w.Code != 200 || !strings.Contains(w.Body.String(), "be=2") {
		t.Errorf("after the move: got %d %q, want backend 2 (the new node address)", w.Code, w.Body.String())
	}
	if c := env.conns2.Load(); c != 1 {
		t.Errorf("%d connections to the new address, want 1", c)
	}
}

// TestUpstreamAuthorityRoundTrip: the authority keeps the node address intact
// through IPv6 literals (bracketed, as feluccad sends them, or not), names
// with dots and upper case, and a host name lowercased on the way.
func TestUpstreamAuthorityRoundTrip(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"10.0.0.7", "10.0.0.7:20001"},
		{"[fd00::2]", "[fd00::2]:20001"},
		{"fd00::2", "[fd00::2]:20001"},
		{"[::1]", "[::1]:20001"},
		{"Node-A.Example.", "Node-A.Example.:20001"},
	} {
		r := Route{SandboxID: "Sb-X.y", GuestPort: 8069, NodeHost: c.host, NodePort: 20001}
		a := upstreamAuthority(r)
		for _, form := range []string{a, strings.ToLower(a)} {
			got, err := upstreamDialAddr(form)
			if err != nil || got != c.want {
				t.Errorf("%q via %q: got %q err=%v, want %q", c.host, form, got, err, c.want)
			}
		}
		if _, err := url.Parse("http://" + a + "/"); err != nil {
			t.Errorf("%q: authority %q is not a valid URL host: %v", c.host, a, err)
		}
	}
	for _, bad := range []string{"10.0.0.7:80", "61-1-zz-2" + upstreamSuffix, "61-1-31" + upstreamSuffix, "61-1-31-99999" + upstreamSuffix} {
		if _, err := upstreamDialAddr(bad); err == nil {
			t.Errorf("%q decoded, want an error", bad)
		}
	}
}

// TestIPv6NodeHost: feluccad sends an IPv6 worker as "[addr]"; requests to it
// must reach it (they were 502 when the brackets were doubled).
func TestIPv6NodeHost(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here (%v); the address building is covered by TestUpstreamAuthorityRoundTrip", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "be=v6")
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	env := newTestEnv(t, nil)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	var p uint16
	fmt.Sscanf(port, "%d", &p)
	env.setRoutes(Route{Hostname: "web--sb-6", SandboxID: "sb-6", TenantID: "t-1", Name: "web",
		NodeHost: "[::1]", NodePort: p, GuestPort: 80, State: "running"})
	if w := doHost(t, env.gw, "web--sb-6.sb.lab.test", "/"); w.Code != 200 || w.Body.String() != "be=v6" {
		t.Errorf("got %d %q, want 200 from the IPv6 backend", w.Code, w.Body.String())
	}
}

// TestTableFreshTakesTheLargerAge: through tableFresh itself, a stopped
// monotonic clock with a moving wall clock (a frozen host) fails closed.
func TestTableFreshTakesTheLargerAge(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))
	for _, c := range []struct {
		mono, wall time.Duration
		code       int
	}{
		{10 * time.Second, 10 * time.Second, 200},
		{10 * time.Second, 700 * time.Second, 503}, // frozen host
		{700 * time.Second, -5 * time.Second, 503}, // wall clock stepped back
	} {
		env.gw.ages = func(time.Time, time.Time) (time.Duration, time.Duration) { return c.mono, c.wall }
		if w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"); w.Code != c.code {
			t.Errorf("mono %v wall %v: got %d, want %d", c.mono, c.wall, w.Code, c.code)
		}
	}
}

func TestNewClampsMaxStale(t *testing.T) {
	gw := New(Config{Domain: "sb.lab.test", MaxStale: time.Hour})
	if gw.cfg.MaxStale != ingress.GatewayMaxStaleCeiling {
		t.Errorf("MaxStale %v, want clamped to %v", gw.cfg.MaxStale, ingress.GatewayMaxStaleCeiling)
	}
}

// TestRefreshCadenceIndependentOfActivityReport: a hanging activity report
// must not hold up the table refresh.
func TestRefreshCadenceIndependentOfActivityReport(t *testing.T) {
	env := newTestEnv(t, nil)
	hang := make(chan struct{})
	env.hmu.Lock()
	env.activity = hang
	env.hmu.Unlock()
	t.Cleanup(func() { close(hang) }) // runs before the fake feluccad closes
	gw := New(Config{Domain: "sb.lab.test", FeluccadURL: env.feluccad.URL, Token: "admin-tok", Refresh: 50 * time.Millisecond})
	gw.markActive("sb-1")
	stop := make(chan struct{})
	go gw.Run(stop)
	time.Sleep(600 * time.Millisecond)
	close(stop)
	if n := env.fetches.Load(); n < 5 {
		t.Errorf("%d table fetches in 600ms at a 50ms refresh while the activity report hangs, want at least 5", n)
	}
	if n := env.activities.Load(); n < 1 {
		t.Errorf("%d activity reports, want at least 1 (the test must hang one)", n)
	}
}

// The 502 answer hides the cause; the log must carry it.
func TestUpstreamErrorIsLogged(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&mu, &buf}, nil)))
	defer slog.SetDefault(prev)

	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("odoo--sb-1", "sb-1", "t-1", "running"))
	env.backend.Close()
	if w := doHost(t, env.gw, "odoo--sb-1.sb.lab.test", "/"); w.Code != 502 {
		t.Fatalf("got %d, want 502", w.Code)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	if !strings.Contains(out, "gw: upstream") || !strings.Contains(out, "label=odoo--sb-1") || !strings.Contains(out, "err=") {
		t.Errorf("log %q: want the upstream error with its label", out)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Control: requests for one sandbox do reuse the pooled connection (keep-alive
// is on; only the key changed).
func TestUpstreamConnectionReusedForSameSandbox(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("web--sb-a", "sb-a", "t-1", "running"))
	for i := 0; i < 3; i++ {
		if w := doHost(t, env.gw, "web--sb-a.sb.lab.test", "/"); w.Code != 200 {
			t.Fatalf("request %d: got %d", i, w.Code)
		}
	}
	if c := env.conns.Load(); c != 1 {
		t.Errorf("%d connections for three requests to one sandbox, want 1", c)
	}
}

// TestUpgradeThroughProxy: an Upgrade request still switches protocols through
// the gateway and carries bytes both ways.
func TestUpgradeThroughProxy(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes(env.backendRoute("ws--sb-1", "sb-1", "t-1", "running"))
	front := httptest.NewServer(env.gw)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: ws--sb-1.sb.lab.test\r\nConnection: Upgrade\r\nUpgrade: echo-test\r\n\r\n")
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 101 {
		t.Fatalf("upgrade: got %d, want 101", resp.StatusCode)
	}
	fmt.Fprint(conn, "ping\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo:ping\n" {
		t.Errorf("after upgrade: read %q err=%v, want echo:ping", line, err)
	}
}

// ---- FLU-7: each wake waiter resolves its own route ----

// sleepingPair is one sandbox with two services on two different backends.
// They are two services, so they differ in guest port. (Two NAMES for one
// guest port share its node port, and then one upstream pool for both is
// right; two node ports need two guest ports.)
func (env *testEnv) sleepingPair(state string) []Route {
	b := routeTo(env.backend2, "b--sb-w", "sb-w", "t-1", state)
	b.GuestPort = 3000
	return []Route{routeTo(env.backend, "a--sb-w", "sb-w", "t-1", state), b}
}

// TestWakeEachWaiterGetsItsOwnRoute: two hostnames of one sleeping sandbox
// requested at the same moment each reach their OWN backend.
func TestWakeEachWaiterGetsItsOwnRoute(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		time.Sleep(50 * time.Millisecond) // long enough for the second request to join
		env.routes.Store(env.sleepingPair("running"))
		w.WriteHeader(200)
	})
	const rounds = 20
	for round := 0; round < rounds; round++ {
		env.setRoutes(env.sleepingPair("sleeping")...)
		start := make(chan struct{})
		var wg sync.WaitGroup
		got := make([]*httptest.ResponseRecorder, 2)
		for i, host := range []string{"a--sb-w.sb.lab.test", "b--sb-w.sb.lab.test"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got[i] = doHost(t, env.gw, host, "/")
			}()
		}
		close(start)
		wg.Wait()
		for i, want := range []string{"be=1 backend saw host=a--sb-w", "be=2 backend saw host=b--sb-w"} {
			if got[i].Code != 200 || !strings.Contains(got[i].Body.String(), want) {
				t.Errorf("round %d label %d: got %d %q, want %q", round, i, got[i].Code, got[i].Body.String(), want)
			}
		}
	}
	wakes := env.wakes.Load()
	t.Logf("%d wakes for %d rounds of two requests", wakes, rounds)
	if wakes >= 2*rounds {
		t.Errorf("%d wakes: no round shared a wake, so the waiter path was not exercised", wakes)
	}
}

// wakeOne requests label once with auto-wake on and reports the response, the
// table fetches made after the (first) wake call, and the elapsed time.
func (env *testEnv) wakeOne(t *testing.T, host string) (*httptest.ResponseRecorder, int64, time.Duration) {
	t.Helper()
	var atWake atomic.Int64
	atWake.Store(-1)
	env.hmu.Lock()
	inner := env.onWake
	env.onWake = func(w http.ResponseWriter, n int64) {
		if n == 1 {
			atWake.Store(env.fetches.Load())
		}
		inner(w, n)
	}
	env.hmu.Unlock()
	began := time.Now()
	w := doHost(t, env.gw, host, "/")
	el := time.Since(began)
	if atWake.Load() < 0 {
		t.Fatal("the wake was never called")
	}
	return w, env.fetches.Load() - atWake.Load(), el
}

func TestWakeAccepted2xxThenRunning(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.routes.Store(env.sleepingPair("running"))
		w.WriteHeader(202)
	})
	w, fetches, _ := env.wakeOne(t, "b--sb-w.sb.lab.test")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "be=2") {
		t.Errorf("got %d %q, want 200 from backend 2", w.Code, w.Body.String())
	}
	if fetches != 1 || env.wakes.Load() != 1 {
		t.Errorf("fetches after wake %d, wakes %d; want 1 and 1", fetches, env.wakes.Load())
	}
}

// The poll ends as soon as ANY route of the sandbox shows running, not only
// the requested label; the request then resolves its own label (still
// sleeping in this table) and gets the page, with no second wake.
func TestWake2xxAnotherLabelRunningFirst(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		rs := env.sleepingPair("sleeping")
		rs[1].State = "running"
		env.routes.Store(rs)
		w.WriteHeader(200)
	})
	w, fetches, _ := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if fetches != 1 {
		t.Errorf("fetches after wake %d, want 1 (b--sb-w running ends the poll)", fetches)
	}
	if env.wakes.Load() != 1 {
		t.Errorf("wakes %d, want 1", env.wakes.Load())
	}
	if w.Code != 503 || !strings.Contains(w.Body.String(), "asleep") {
		t.Errorf("got %d %q, want the sleeping page", w.Code, w.Body.String())
	}
}

func TestWake503RetryAfterThen2xx(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, n int64) {
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			return
		}
		env.routes.Store(env.sleepingPair("running"))
		w.WriteHeader(200)
	})
	w, _, el := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "be=1") {
		t.Errorf("got %d %q, want 200 from backend 1", w.Code, w.Body.String())
	}
	if env.wakes.Load() != 2 {
		t.Errorf("wakes %d, want 2 (resent after Retry-After)", env.wakes.Load())
	}
	if el < time.Second {
		t.Errorf("resent after %v, want after the 1s Retry-After", el)
	}
}

// feluccad answers 503 + Retry-After also when the wake was applied but not
// saved; its table already shows running, so one look is enough: no resend,
// no wait for the Retry-After.
func TestWake503AppliedButNotSavedSeesRunning(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.routes.Store(env.sleepingPair("running"))
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		fmt.Fprint(w, `{"error":"applied but not saved","applied":true}`)
	})
	w, _, el := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "be=1") {
		t.Errorf("got %d %q, want 200 from backend 1", w.Code, w.Body.String())
	}
	if env.wakes.Load() != 1 {
		t.Errorf("wakes %d, want exactly 1", env.wakes.Load())
	}
	if el >= 3*time.Second {
		t.Errorf("took %v, want well under 3s", el)
	}
}

func TestWake409NoPolling(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) { w.WriteHeader(409) })
	w, fetches, _ := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if fetches != 1 {
		t.Errorf("fetches after wake %d, want exactly 1", fetches)
	}
	if w.Code != 503 || !strings.Contains(w.Body.String(), "asleep") {
		t.Errorf("got %d %q, want the sleeping page", w.Code, w.Body.String())
	}
}

// TestWakeRefusedIsHeld (FLU-7 review): a refused wake (409: busy, e.g. a
// fork of it is running) is not sent again for wakeRefusedHold, so a client
// looping on a sleeping sandbox cannot turn each request into a wake, a worker
// call and a full table fetch. After the hold the next request wakes again.
func TestWakeRefusedIsHeld(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) { w.WriteHeader(409) })
	for i := 0; i < 21; i++ {
		w := doHost(t, env.gw, "a--sb-w.sb.lab.test", "/")
		if w.Code != 503 || !strings.Contains(w.Body.String(), "asleep") {
			t.Fatalf("request %d: got %d %q, want the sleeping page", i, w.Code, w.Body.String())
		}
	}
	if n := env.wakes.Load(); n != 1 {
		t.Errorf("one 409 then 20 requests inside the hold: %d wakes, want 1", n)
	}
	env.skew.Add(int64(wakeRefusedHold + time.Second))
	env.gw.refresh() // keep the table fresh at the moved clock (it also prunes)
	doHost(t, env.gw, "b--sb-w.sb.lab.test", "/")
	if n := env.wakes.Load(); n != 2 {
		t.Errorf("after the hold: %d wakes, want 2", n)
	}
	env.gw.wmu.Lock()
	held := len(env.gw.refused)
	env.gw.wmu.Unlock()
	if held != 1 {
		t.Errorf("%d refused marks, want 1 (the new refusal)", held)
	}
	env.setRoutes() // the sandbox leaves the table: its mark goes with it
	env.gw.wmu.Lock()
	held = len(env.gw.refused)
	env.gw.wmu.Unlock()
	if held != 0 {
		t.Errorf("%d refused marks after the sandbox left the table, want 0", held)
	}
}

func TestWake404ResolvesAgainstNewTable(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.routes.Store([]Route{}) // the sandbox is gone
		w.WriteHeader(404)
	})
	w, fetches, _ := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if fetches != 1 {
		t.Errorf("fetches after wake %d, want exactly 1", fetches)
	}
	if w.Code != 404 || !strings.Contains(w.Body.String(), "no such service") {
		t.Errorf("got %d %q, want 404 no such service", w.Code, w.Body.String())
	}
}

// A wake whose answer never arrives (transport error) is followed by the poll,
// as before: someone else may have woken the sandbox.
func TestWakeTransportErrorPolls(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	var wakeFetch atomic.Int64
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		wakeFetch.Store(env.fetches.Load())
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	env.setOnFetch(func(n int64) {
		// The first poll has already read the table and answers "asleep";
		// the second sees it up.
		if w := wakeFetch.Load(); w > 0 && n == w+1 {
			env.routes.Store(env.sleepingPair("running"))
		}
	})
	w, fetches, _ := env.wakeOne(t, "a--sb-w.sb.lab.test")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "be=1") {
		t.Errorf("got %d %q, want 200 from backend 1", w.Code, w.Body.String())
	}
	if fetches != 2 || env.wakes.Load() != 1 {
		t.Errorf("fetches after wake %d, wakes %d; want 2 and 1", fetches, env.wakes.Load())
	}
}

// TestWakePanicDoesNotParkLaterRequests (FLU-7 W3).
func TestWakePanicDoesNotParkLaterRequests(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.gw.wake = func(string) { panic("wake path blew up") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the wake seam did not panic")
			}
		}()
		doHost(t, env.gw, "a--sb-w.sb.lab.test", "/")
	}()

	env.gw.wake = func(string) {}
	done := make(chan int, 1)
	go func() { done <- doHost(t, env.gw, "b--sb-w.sb.lab.test", "/").Code }()
	select {
	case code := <-done:
		if code != 503 {
			t.Errorf("after the panic: got %d, want the sleeping page", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request after the panicked wake is parked")
	}
	env.gw.wmu.Lock()
	n := len(env.gw.waking)
	env.gw.wmu.Unlock()
	if n != 0 {
		t.Errorf("%d wake entries left behind", n)
	}
}

// TestEnsure503IsRetryable (FLU-7 W4): feluccad refusing an ensure with 503
// is a retryable 503 for the client, not a 404.
func TestEnsure503IsRetryable(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes()
	env.ensure = func(string, uint16) int { return 503 }
	w := doHost(t, env.gw, "8069--sb-dyn.sb.lab.test", "/")
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Errorf("got %d Retry-After=%q %q, want 503 with Retry-After: 5", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
}

// TestEnsureAnswersMap: every ensure answer other than 200/403/404 is a
// retryable answer for the client, never "no such service".
func TestEnsureAnswersMap(t *testing.T) {
	for _, c := range []struct {
		name      string
		code      int    // feluccad's answer; -1 = none at all
		retry     string // feluccad's Retry-After
		wantCode  int
		wantRetry string
	}{
		{"429 churn cap, Retry-After passed through", 429, "17", 429, "17"},
		{"429 without Retry-After", 429, "", 429, "5"},
		{"503, Retry-After passed through", 503, "9", 503, "9"},
		{"503 without Retry-After", 503, "", 503, "5"},
		{"503 with a date Retry-After", 503, "Wed, 21 Oct 2026 07:28:00 GMT", 503, "5"},
		{"no answer", -1, "", 503, "5"},
		{"409 no address yet", 409, "3", 503, "5"},
		{"502", 502, "", 503, "5"},
	} {
		env := newTestEnv(t, nil)
		env.setRoutes()
		env.ensureRetry = c.retry
		code := c.code
		env.ensure = func(string, uint16) int { return code }
		w := doHost(t, env.gw, "8069--sb-dyn.sb.lab.test", "/")
		if w.Code != c.wantCode || w.Header().Get("Retry-After") != c.wantRetry {
			t.Errorf("%s: got %d Retry-After=%q %q, want %d with Retry-After %q",
				c.name, w.Code, w.Header().Get("Retry-After"), w.Body.String(), c.wantCode, c.wantRetry)
		}
	}
}

// The ensure succeeded but the table fetch after it failed: retryable.
func TestEnsureThenTableFetchFails(t *testing.T) {
	env := newTestEnv(t, nil)
	env.setRoutes()
	env.ensure = func(string, uint16) int {
		env.failFetch.Store(true)
		return 200
	}
	w := doHost(t, env.gw, "8069--sb-dyn.sb.lab.test", "/")
	if w.Code != 503 || w.Header().Get("Retry-After") != "5" {
		t.Errorf("got %d Retry-After=%q %q, want 503 with Retry-After: 5", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
}

// After a wake the second resolution runs the lazy ensure again for an
// all-digit label the woken table no longer has.
func TestSecondResolutionEnsuresDynamicLabel(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	web := func(state string) Route { return env.backendRoute("web--sb-d", "sb-d", "t-1", state) }
	dyn := func(state string) Route {
		r := env.backendRoute("8069--sb-d", "sb-d", "t-1", state)
		r.GuestPort = 8069
		return r
	}
	env.setRoutes(web("sleeping"))
	env.ensure = func(string, uint16) int {
		if env.ensures.Load() == 1 {
			env.routes.Store([]Route{web("sleeping"), dyn("sleeping")})
		} else {
			env.routes.Store([]Route{web("running"), dyn("running")})
		}
		return 200
	}
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.routes.Store([]Route{web("running")}) // the dynamic expose is gone
		w.WriteHeader(200)
	})
	w := doHost(t, env.gw, "8069--sb-d.sb.lab.test", "/")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "host=8069--sb-d") {
		t.Errorf("got %d %q, want 200 proxied", w.Code, w.Body.String())
	}
	if n := env.ensures.Load(); n != 2 {
		t.Errorf("%d ensures, want 2 (one per resolution)", n)
	}
	if n := env.wakes.Load(); n != 1 {
		t.Errorf("%d wakes, want 1", n)
	}
}

// The second resolution checks the table's age like a fresh request.
func TestSecondResolutionFindsTableStale(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.failFetch.Store(true)                // feluccad's table is gone...
		env.skew.Store(int64(660 * time.Second)) // ...and the gateway's is now old
		w.WriteHeader(409)
	})
	assertTableUnavailable(t, doHost(t, env.gw, "a--sb-w.sb.lab.test", "/"))
	if n := env.wakes.Load(); n != 1 {
		t.Errorf("%d wakes, want 1", n)
	}
}

// The second resolution is the same client request: it takes no second
// rate-limit token (with rps 1 the bucket is empty after the first pass).
func TestSecondResolutionTakesNoRateToken(t *testing.T) {
	env := newTestEnv(t, map[string]TenantLimit{"t-1": {RPS: 1}})
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		env.routes.Store(env.sleepingPair("running"))
		w.WriteHeader(200)
	})
	if w := doHost(t, env.gw, "a--sb-w.sb.lab.test", "/"); w.Code != 200 {
		t.Errorf("got %d %q, want 200 (not a 429 from its own second pass)", w.Code, w.Body.String())
	}
}

// Requests parked on a wake that then panics are released (FLU-7 W3).
func TestWakePanicReleasesParkedWaiters(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.setRoutes(env.sleepingPair("sleeping")...)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	env.gw.wake = func(string) {
		if calls.Add(1) > 1 {
			return
		}
		close(started)
		<-release
		panic("wake path blew up")
	}
	go func() {
		defer func() { recover() }()
		doHost(t, env.gw, "a--sb-w.sb.lab.test", "/")
	}()
	<-started
	const waiters = 3
	codes := make(chan int, waiters)
	for i := 0; i < waiters; i++ {
		go func() { codes <- doHost(t, env.gw, "b--sb-w.sb.lab.test", "/").Code }()
	}
	time.Sleep(100 * time.Millisecond) // let them park on the shared wake
	close(release)
	for i := 0; i < waiters; i++ {
		select {
		case code := <-codes:
			if code != 503 {
				t.Errorf("waiter: got %d, want the sleeping page", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%d of %d waiters still parked after the wake panicked", waiters-i, waiters)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("%d wake calls, want 1 (the waiters were parked, not leaders)", n)
	}
}

// The resend of a 503-refused wake stops when the budget is spent; the
// request then gets the page for the state the table shows.
func TestWakeResendBudgetEnds(t *testing.T) {
	env := newTestEnv(t, nil)
	env.gw.cfg.AutoWake = true
	env.gw.wakeBudget = 2500 * time.Millisecond
	env.setRoutes(env.sleepingPair("sleeping")...)
	env.setOnWake(func(w http.ResponseWriter, _ int64) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(503)
	})
	w := doHost(t, env.gw, "a--sb-w.sb.lab.test", "/")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "asleep") {
		t.Errorf("got %d %q, want the sleeping page", w.Code, w.Body.String())
	}
	// Sends at about 0 s, 1 s and 2 s; the next would start past 2.5 s.
	if n := env.wakes.Load(); n < 2 || n > 3 {
		t.Errorf("%d wakes inside a 2.5s budget with Retry-After 1, want 2 or 3", n)
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
