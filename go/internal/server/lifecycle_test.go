// Tests for the lifecycle policies (lifecycle.go, v4 P5.2): policy
// validation/resolution, the idle/TTL sweep, dynamic-expose GC on auto-sleep,
// and the gateway activity report. Internal package (like exec_stream_test.go)
// so the fake agent can be registered as a real node address — autoSleep and
// autoDelete speak real HTTP to it — and so the sweep can be driven directly
// with a synthetic clock instead of waiting out the ticker.
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
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// ---- Pure helpers ----

func TestValidPolicy(t *testing.T) {
	cases := []struct {
		v    int64
		want bool
	}{
		{0, true},                 // inherit
		{-1, true},                // explicitly disabled
		{minIdleSleepS, true},     // lower bound
		{minIdleSleepS - 1, false},
		{maxPolicyS, true},        // upper bound (one year)
		{maxPolicyS + 1, false},
		{-2, false}, // only -1 is a valid negative
	}
	for _, tc := range cases {
		if got := validPolicy(tc.v, minIdleSleepS); got != tc.want {
			t.Errorf("validPolicy(%d, minIdleSleepS) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestEffectivePolicy(t *testing.T) {
	tenant := &store.Tenant{DefaultIdleSleepS: 42, DefaultAsleepDeleteS: 99}
	cases := []struct {
		name     string
		override int64
		t        *store.Tenant
		ttl      bool
		want     int64
	}{
		{"disabled beats tenant default", -1, tenant, false, 0},
		{"override wins", 7, tenant, false, 7},
		{"inherit with nil tenant fails open", 0, nil, false, 0},
		{"inherit takes idle default", 0, tenant, false, 42},
		{"ttl picks asleep-delete default", 0, tenant, true, 99},
	}
	for _, tc := range cases {
		if got := effectivePolicy(tc.override, tc.t, tc.ttl); got != tc.want {
			t.Errorf("%s: effectivePolicy(%d, _, %v) = %d, want %d",
				tc.name, tc.override, tc.ttl, got, tc.want)
		}
	}
}

func TestIsAllDigits(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"8069", true},
		{"", false}, // empty is not a dynamic name
		{"odoo", false},
		{"80a", false},
	}
	for _, tc := range cases {
		if got := isAllDigits(tc.s); got != tc.want {
			t.Errorf("isAllDigits(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// ---- Sweep ----

// lcAgent fakes the worker lifecycle API: every call gets a 200 {"ok":true}
// (good for sleep, delete, and unexpose alike) and is recorded as
// "METHOD path" — unexpose calls additionally carry the guest port, the only
// piece of their body the sweep's GC sends.
type lcAgent struct {
	mu    sync.Mutex
	calls []string
}

func (a *lcAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/expose") {
			var req struct {
				GuestPort uint16 `json:"guest_port"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			call = fmt.Sprintf("%s:%d", call, req.GuestPort)
		}
		a.mu.Lock()
		a.calls = append(a.calls, call)
		a.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (a *lcAgent) saw(call string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.calls {
		if c == call {
			return true
		}
	}
	return false
}

func (a *lcAgent) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// snapshot copies the call log for failure messages (the handler appends from
// the httptest goroutine, so direct reads of a.calls would race).
func (a *lcAgent) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// newLifecycleTestServer wires a server to the fake agent as its one node and
// seeds a RUNNING sandbox, returning the sandbox id. Tests mutate the policy
// and clock fields directly under the state lock and then call lifecycleSweep
// with a synthetic now — LifecycleLoop is just a ticker around that.
func newLifecycleTestServer(t *testing.T, fa *lcAgent) (*Server, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "hearth.db"),
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
	sb := srv.st.CreateSandbox("sleepy", "default", nodeID, 1, 256, now)
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id, model.StateRunning)
	return srv, id
}

func TestSweepAutoSleep(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	// Give the sandbox a real tenant row so the "slept" usage event is
	// queryable (the admin tenant's id is "", which ListUsage can't isolate).
	tenant := &store.Tenant{ID: "tn-lc1", Name: "lc1", CreatedAt: now}
	if err := srv.db.CreateTenant(tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.TenantID = tenant.ID
	sb.IdleSleepS = 5
	sb.LastActivity = now - 10 // idle past the 5s threshold
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	if !fa.saw("POST /v1/vms/" + id + "/sleep") {
		t.Errorf("agent sleep not called; calls: %v", fa.snapshot())
	}
	srv.st.Lock()
	sb = srv.st.FindSandbox(id)
	gotState, sleptAt := sb.State, sb.SleptAt
	srv.st.Unlock()
	if gotState != model.StateSleeping {
		t.Errorf("state after sweep: %q, want sleeping", gotState)
	}
	if sleptAt <= 0 {
		t.Errorf("SleptAt not stamped: %d", sleptAt)
	}
	// The sweep leaves a "slept" metering event for the owning tenant.
	events, err := srv.db.ListUsage(tenant.ID, 0, now+3600)
	if err != nil {
		t.Fatalf("list usage: %v", err)
	}
	slept := false
	for _, e := range events {
		if e.Event == "slept" && e.SandboxID == id {
			slept = true
		}
	}
	if !slept {
		t.Errorf("no slept usage event; got %+v", events)
	}
}

func TestSweepRespectsActivity(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.IdleSleepS = 5
	sb.LastActivity = now // fresh activity: idle clock at zero
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	if n := fa.callCount(); n != 0 {
		t.Errorf("agent called %d times for an active sandbox; calls: %v", n, fa.snapshot())
	}
	srv.st.Lock()
	gotState := srv.st.FindSandbox(id).State
	srv.st.Unlock()
	if gotState != model.StateRunning {
		t.Errorf("state after sweep: %q, want running", gotState)
	}
}

func TestSweepTenantDefaultAndDisableOverride(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	tenant := &store.Tenant{ID: "tn-lc2", Name: "lc2", DefaultIdleSleepS: 5, CreatedAt: now}
	if err := srv.db.CreateTenant(tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Second sandbox on the same node: explicit -1 must beat the default.
	srv.st.Lock()
	sb1 := srv.st.FindSandbox(id)
	nodeID := *sb1.NodeID
	sb2 := srv.st.CreateSandbox("opted-out", "default", nodeID, 1, 256, now)
	id2 := sb2.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id2, model.StateRunning)

	srv.st.Lock()
	sb1 = srv.st.FindSandbox(id)
	sb1.TenantID = tenant.ID
	sb1.IdleSleepS = 0 // inherit the tenant default (5s)
	sb1.LastActivity = now - 10
	sb2 = srv.st.FindSandbox(id2)
	sb2.TenantID = tenant.ID
	sb2.IdleSleepS = model.PolicyDisabled
	sb2.LastActivity = now - 10
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	srv.st.Lock()
	state1 := srv.st.FindSandbox(id).State
	state2 := srv.st.FindSandbox(id2).State
	srv.st.Unlock()
	if state1 != model.StateSleeping {
		t.Errorf("inheriting sandbox: %q, want sleeping", state1)
	}
	if state2 != model.StateRunning {
		t.Errorf("opted-out sandbox: %q, want running", state2)
	}
	if fa.saw("POST /v1/vms/" + id2 + "/sleep") {
		t.Error("agent slept the -1 sandbox")
	}
}

func TestSweepAutoDelete(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.SetSandboxState(id, model.StateSleeping)
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.AsleepDeleteS = 30
	sb.SleptAt = now - 60 // asleep past the TTL
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	if !fa.saw("DELETE /v1/vms/" + id) {
		t.Errorf("agent delete not called; calls: %v", fa.snapshot())
	}
	srv.st.Lock()
	gone := srv.st.FindSandbox(id) == nil
	srv.st.Unlock()
	if !gone {
		t.Error("sandbox still present after auto-delete sweep")
	}
}

func TestSweepStampsPreP5Sleeper(t *testing.T) {
	// A sleeper adopted from a pre-P5 snapshot has SleptAt == 0: the first
	// sweep must stamp it with now (so the TTL counts from the upgrade) and
	// must NOT delete, however large the TTL backlog would look.
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.SetSandboxState(id, model.StateSleeping)
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.AsleepDeleteS = 30
	sb.SleptAt = 0
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	srv.st.Lock()
	sb = srv.st.FindSandbox(id)
	srv.st.Unlock()
	if sb == nil {
		t.Fatal("pre-P5 sleeper was deleted on the stamping sweep")
	}
	if sb.SleptAt != now {
		t.Errorf("SleptAt: %d, want stamped to now (%d)", sb.SleptAt, now)
	}
	if n := fa.callCount(); n != 0 {
		t.Errorf("agent called %d times; calls: %v", n, fa.snapshot())
	}
}

func TestSweepDropsDynamicExposes(t *testing.T) {
	// Auto-sleep GCs the dynamic (all-digit-named) exposes — the ADR-0007
	// deferral — and keeps the named ones.
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.IdleSleepS = 5
	sb.LastActivity = now - 10
	sb.Exposes = []model.Expose{
		{Name: "8069", GuestPort: 8069, NodePort: 20001}, // dynamic
		{Name: "web", GuestPort: 80, NodePort: 20002},    // named
	}
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	srv.st.Lock()
	sb = srv.st.FindSandbox(id)
	gotState := sb.State
	exposes := append([]model.Expose(nil), sb.Exposes...)
	srv.st.Unlock()
	if gotState != model.StateSleeping {
		t.Fatalf("state after sweep: %q, want sleeping", gotState)
	}
	if len(exposes) != 1 || exposes[0].Name != "web" {
		t.Errorf("exposes after sweep: %+v, want only \"web\"", exposes)
	}
	// The agent was told to drop exactly the dynamic row's DNAT rule.
	if !fa.saw(fmt.Sprintf("DELETE /v1/vms/%s/expose:8069", id)) {
		t.Errorf("agent unexpose for 8069 not seen; calls: %v", fa.snapshot())
	}
	if fa.saw(fmt.Sprintf("DELETE /v1/vms/%s/expose:80", id)) {
		t.Errorf("agent unexpose dropped the named expose; calls: %v", fa.snapshot())
	}
}

// ---- Gateway activity report ----

func TestRoutesActivity(t *testing.T) {
	srv, id := newLifecycleTestServer(t, &lcAgent{})

	// Age the activity clock so the stamp is observable.
	srv.st.Lock()
	srv.st.FindSandbox(id).LastActivity = 1000
	srv.st.Unlock()

	before := time.Now().Unix()
	w := doRequest(srv.Handler(), "POST", "/api/v1/routes/activity",
		`{"sandbox_ids":["`+id+`"]}`, adminAuth)
	if w.Code != 204 {
		t.Fatalf("activity report: got %d body %s", w.Code, w.Body.String())
	}
	srv.st.Lock()
	last := srv.st.FindSandbox(id).LastActivity
	srv.st.Unlock()
	if last < before {
		t.Errorf("LastActivity: %d, want >= %d", last, before)
	}
}
