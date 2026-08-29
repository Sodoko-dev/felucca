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
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
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
	// auths records the Authorization header of every call, in order — the
	// sweep is a hearthd→agent dial and what it presents is a security
	// property (see TestSweepNeverSendsTheAdminTokenToANode).
	auths []string
	// failDeletes makes every VM delete answer 500, standing in for the whole
	// family of reasons a worker can refuse one: a node rebooting, an agent
	// restart, a network blip, a credential the node no longer accepts.
	failDeletes bool
}

func (a *lcAgent) setFailDeletes(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failDeletes = v
}

func (a *lcAgent) deletesFail() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failDeletes
}

func (a *lcAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		a.mu.Lock()
		a.auths = append(a.auths, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if r.Method == http.MethodDelete && !strings.HasSuffix(r.URL.Path, "/expose") && a.deletesFail() {
			a.mu.Lock()
			a.calls = append(a.calls, call)
			a.mu.Unlock()
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":"vm busy"}`))
			return
		}
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

// authSnapshot copies the Authorization headers seen so far.
func (a *lcAgent) authSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
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

// An auto-delete that ignores the agent's answer and drops the record anyway
// ORPHANS the microVM: it keeps running on the worker with its vCPUs, its
// memory and its nft DNAT rules, it is unbillable, and no control-plane path can
// reach it again — nobody will even go looking, because as far as hearthd is
// concerned the sandbox is gone. The sweep runs this every 15 seconds against
// every expired sleeper, so any agent-call failure was enough.
//
// The record therefore stays, and the sweep IS the retry loop: still asleep,
// still past its TTL, tried again next tick.
func TestSweepAutoDeleteKeepsTheSandboxWhenTheAgentFails(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.SetSandboxState(id, model.StateSleeping)
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.AsleepDeleteS = 30
	sb.SleptAt = now - 60
	srv.st.Unlock()

	fa.setFailDeletes(true)
	srv.lifecycleSweep(now)

	if !fa.saw("DELETE /v1/vms/" + id) {
		t.Fatalf("agent delete was not attempted; calls: %v", fa.snapshot())
	}
	srv.st.Lock()
	kept := srv.st.FindSandbox(id)
	srv.st.Unlock()
	if kept == nil {
		t.Fatal("sandbox was removed although the worker refused the delete: its VM is now an orphan")
	}
	if kept.State != model.StateSleeping {
		t.Errorf("state after the failed delete: %q, want it left sleeping so the next sweep retries", kept.State)
	}

	// The worker comes back: the very next sweep completes the delete.
	fa.setFailDeletes(false)
	srv.lifecycleSweep(now)
	srv.st.Lock()
	gone := srv.st.FindSandbox(id) == nil
	srv.st.Unlock()
	if !gone {
		t.Error("sandbox still present after the retry sweep reached the agent")
	}
}

// deadNode is a listener that accepts a connection and immediately closes it:
// the client sees a TRANSPORT failure, which is what a node that is gone
// produces — except that a real one produces it only after the agent client's
// 30s timeout. It counts accepts, so the test can assert how many times the
// sweep tried to reach it.
type deadNode struct {
	ln     net.Listener
	mu     sync.Mutex
	accept int
}

func newDeadNode(t *testing.T) *deadNode {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &deadNode{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			d.accept++
			d.mu.Unlock()
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *deadNode) addr() string { return "http://" + d.ln.Addr().String() }

func (d *deadNode) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.accept
}

// The sweep runs its actions sequentially and each one dials the agent through a
// client with a 30s timeout. Keeping the record when an auto-delete fails is
// right — dropping it orphans a live microVM — but it made the RETRY unbounded:
// a node that is permanently gone holding K TTL-expired sandboxes was dialed K
// times every pass, so one sweep cost up to K x 30s, the 15s ticker dropped its
// ticks, and auto-sleep enforcement for every OTHER tenant queued behind that
// single dead node. K is tenant-controllable; the number of nodes is not. One
// dial per unreachable node per sweep is the bound.
func TestSweepDialsAnUnreachableNodeOnceHoweverManySandboxesItHolds(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()
	dead := newDeadNode(t)

	srv.st.Lock()
	srv.st.Nodes[0].Addr = dead.addr()
	nodeID := srv.st.Nodes[0].ID
	ids := []string{id}
	for i := 0; i < 4; i++ {
		sb := srv.st.CreateSandbox(fmt.Sprintf("expired-%d", i), "default", nodeID, 1, 256, now)
		ids = append(ids, sb.ID)
	}
	srv.st.Unlock()
	for _, sid := range ids {
		srv.st.SetSandboxState(sid, model.StateSleeping)
	}
	srv.st.Lock()
	for _, sid := range ids {
		sb := srv.st.FindSandbox(sid)
		sb.AsleepDeleteS = 30
		sb.SleptAt = now - 60 // all five are past the TTL
	}
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	if got := dead.dials(); got != 1 {
		t.Errorf("the sweep dialed one unreachable node %d times for %d expired sandboxes; want exactly 1 — the rest are retried next sweep",
			got, len(ids))
	}
	// Nothing was abandoned: every record is still there for the next sweep.
	srv.st.Lock()
	kept := 0
	for _, sid := range ids {
		if srv.st.FindSandbox(sid) != nil {
			kept++
		}
	}
	srv.st.Unlock()
	if kept != len(ids) {
		t.Errorf("%d of %d sandboxes survived the sweep against an unreachable node; want all of them kept so no VM is orphaned",
			kept, len(ids))
	}

	// The bound is per-sweep and nothing else: the node comes back and the very
	// next pass completes every one of them.
	ts := httptest.NewServer(fa.handler())
	t.Cleanup(ts.Close)
	srv.st.Lock()
	srv.st.Nodes[0].Addr = ts.URL
	srv.st.Unlock()
	srv.lifecycleSweep(now)
	srv.st.Lock()
	left := 0
	for _, sid := range ids {
		if srv.st.FindSandbox(sid) != nil {
			left++
		}
	}
	srv.st.Unlock()
	if left != 0 {
		t.Errorf("%d sandboxes still present after the node came back; the skip must defer a retry, never cancel it", left)
	}
}

// The bound above is per (node, TENANT), and this is why. Keyed on the node
// alone it was a cross-tenant availability lever: one transport failure deferred
// auto-sleep AND auto-delete for every remaining sandbox on that node — other
// tenants' included — for the whole pass. A tenant able to keep their own node's
// agent timing out once per sweep (their workload is what makes it time out)
// would suppress idle reclamation for their co-tenants indefinitely, and idle
// reclamation is what frees that node's RAM.
//
// So: a noisy tenant with many expired sandboxes still costs exactly one dial,
// and the quiet co-tenant behind them still gets theirs.
func TestSweepDoesNotLetOneTenantsDeadNodeDialDeferACoTenant(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()
	dead := newDeadNode(t)

	srv.st.Lock()
	srv.st.Nodes[0].Addr = dead.addr()
	nodeID := srv.st.Nodes[0].ID
	// tn-noisy holds the seeded sandbox plus four more; tn-quiet holds one, and
	// sits behind them all in state order.
	ids := []string{id}
	for i := 0; i < 4; i++ {
		sb := srv.st.CreateSandbox(fmt.Sprintf("noisy-%d", i), "default", nodeID, 1, 256, now)
		ids = append(ids, sb.ID)
	}
	quiet := srv.st.CreateSandbox("quiet", "default", nodeID, 1, 256, now)
	quietID := quiet.ID
	srv.st.Unlock()

	all := append(append([]string(nil), ids...), quietID)
	for _, sid := range all {
		srv.st.SetSandboxState(sid, model.StateSleeping)
	}
	srv.st.Lock()
	for _, sid := range ids {
		sb := srv.st.FindSandbox(sid)
		sb.TenantID = "tn-noisy"
		sb.AsleepDeleteS = 30
		sb.SleptAt = now - 60
	}
	qsb := srv.st.FindSandbox(quietID)
	qsb.TenantID = "tn-quiet"
	qsb.AsleepDeleteS = 30
	qsb.SleptAt = now - 60
	srv.st.Unlock()

	srv.lifecycleSweep(now)

	// One dial for the noisy tenant (its other four are deferred, which is the
	// bound this keeps) and one for the quiet tenant. Not one in total.
	if got := dead.dials(); got != 2 {
		t.Errorf("the sweep dialed the unreachable node %d times for two tenants (5 expired sandboxes + 1); want exactly 2 — one per tenant, so a noisy co-tenant cannot suppress another tenant's reclamation",
			got)
	}
	// Nothing was abandoned on either side.
	srv.st.Lock()
	kept := 0
	for _, sid := range all {
		if srv.st.FindSandbox(sid) != nil {
			kept++
		}
	}
	srv.st.Unlock()
	if kept != len(all) {
		t.Errorf("%d of %d sandboxes survived the sweep against an unreachable node; want all of them kept so no VM is orphaned", kept, len(all))
	}
}

// acts is built in deterministic state order, so with a fixed starting point the
// same sandboxes are always enforced first — and whenever a sweep runs long
// enough for the 15s ticker to drop a tick, the same tail always loses it. That
// is a starvation lever on its own: whoever sits at the head of the state order
// starves everyone behind them. Each sweep therefore starts at a rotating offset.
func TestSweepRotatesWhereThePassStarts(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.Lock()
	nodeID := *srv.st.FindSandbox(id).NodeID
	second := srv.st.CreateSandbox("second", "default", nodeID, 1, 256, now)
	id2 := second.ID
	srv.st.Unlock()
	for _, sid := range []string{id, id2} {
		srv.st.SetSandboxState(sid, model.StateSleeping)
	}
	srv.st.Lock()
	for _, sid := range []string{id, id2} {
		sb := srv.st.FindSandbox(sid)
		sb.AsleepDeleteS = 30
		sb.SleptAt = now - 60
	}
	srv.st.Unlock()

	// The agent ANSWERS 500, so nothing is removed and both are re-attempted on
	// every pass: two sweeps over the same two actions, and the only thing that
	// can differ between them is where the pass started.
	fa.setFailDeletes(true)

	firstOf := func() string {
		before := len(fa.snapshot())
		srv.lifecycleSweep(now)
		after := fa.snapshot()
		if len(after) <= before {
			t.Fatalf("sweep made no agent calls; calls: %v", after)
		}
		return after[before]
	}
	a, b := firstOf(), firstOf()
	if a == b {
		t.Errorf("two consecutive sweeps both started with %q: the pass order is fixed, so the head of the state order is served first forever and the tail is what a long sweep drops", a)
	}
}

// An agent that ANSWERS is reachable and cheap, and its refusal is about that
// one sandbox. Treating a 500 as "this node is down" would let one permanently
// failing sandbox defer its neighbours on a healthy node forever — a starvation
// bug traded for the timeout bug above.
func TestSweepKeepsWorkingOnANodeThatAnswersWithAnError(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()

	srv.st.Lock()
	nodeID := *srv.st.FindSandbox(id).NodeID
	sb2 := srv.st.CreateSandbox("second", "default", nodeID, 1, 256, now)
	id2 := sb2.ID
	srv.st.Unlock()
	for _, sid := range []string{id, id2} {
		srv.st.SetSandboxState(sid, model.StateSleeping)
	}
	srv.st.Lock()
	for _, sid := range []string{id, id2} {
		sb := srv.st.FindSandbox(sid)
		sb.AsleepDeleteS = 30
		sb.SleptAt = now - 60
	}
	srv.st.Unlock()

	fa.setFailDeletes(true)
	srv.lifecycleSweep(now)

	for _, sid := range []string{id, id2} {
		if !fa.saw("DELETE /v1/vms/" + sid) {
			t.Errorf("sandbox %s was never attempted: a node that answers 500 is not an unreachable node; calls: %v", sid, fa.snapshot())
		}
	}
}

// A worker that already has no such VM is not a failure: 404 means the thing the
// record stands for is gone, so the record goes with it. Without this the sweep
// would retry forever on a node that answered honestly.
func TestSweepAutoDeleteTreatsAgent404AsDone(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	now := time.Now().Unix()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.calls = append(fa.calls, r.Method+" "+r.URL.Path)
		fa.mu.Unlock()
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"no such vm"}`))
	}))
	t.Cleanup(ts.Close)

	srv.st.Lock()
	srv.st.Nodes[0].Addr = ts.URL
	srv.st.Unlock()
	srv.st.SetSandboxState(id, model.StateSleeping)
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.AsleepDeleteS = 30
	sb.SleptAt = now - 60
	srv.st.Unlock()

	srv.lifecycleSweep(now)
	srv.st.Lock()
	gone := srv.st.FindSandbox(id) == nil
	srv.st.Unlock()
	if !gone {
		t.Error("sandbox kept after the agent said the VM does not exist")
	}
}

// The manual handler has the same duty — it is where the sweep's behaviour was
// copied from — plus the escape hatch the sweep deliberately lacks: a node that
// is permanently gone must not leave a record nobody can ever remove.
func TestDeleteSandboxKeepsTheRecordUnlessForced(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa)
	h := srv.Handler()
	fa.setFailDeletes(true)

	w := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id, "", adminAuth)
	if w.Code != 502 {
		t.Fatalf("delete against a refusing agent: got %d %s, want 502", w.Code, w.Body)
	}
	srv.st.Lock()
	kept := srv.st.FindSandbox(id) != nil
	srv.st.Unlock()
	if !kept {
		t.Fatal("sandbox removed although the worker refused the delete: its VM is now an orphan")
	}

	// A TENANT cannot force: abandoning a VM is a decision to leave something
	// running that nothing will bill or reap, and only whoever can go look at
	// the node is in a position to take it.
	w = doRequest(h, "POST", "/api/v1/tenants", `{"name":"forcer"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body)
	}
	var created struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("tenant body: %v", err)
	}
	srv.st.Lock()
	srv.st.FindSandbox(id).TenantID = created.Tenant.ID
	srv.st.Unlock()
	if w := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"?force=1", "", "Bearer "+created.APIKey); w.Code != 502 {
		t.Errorf("tenant forcing past a failed agent call: got %d %s, want 502", w.Code, w.Body)
	}
	srv.st.Lock()
	stillThere := srv.st.FindSandbox(id) != nil
	srv.st.Unlock()
	if !stillThere {
		t.Fatal("a tenant force-dropped the record: the VM is orphaned and unbillable")
	}

	// The operator decides, explicitly, to abandon it.
	if w := doRequest(h, "DELETE", "/api/v1/sandboxes/"+id+"?force=1", "", adminAuth); w.Code != 204 {
		t.Fatalf("forced delete: got %d %s, want 204", w.Code, w.Body)
	}
	srv.st.Lock()
	gone := srv.st.FindSandbox(id) == nil
	srv.st.Unlock()
	if !gone {
		t.Error("forced delete left the record behind: a sandbox on a dead node would be unremovable")
	}
}

// nodeHostOf is the dial target of the fake agent the test server dials, split
// the way the credential key is (host, port).
func nodeHostOf(t *testing.T, srv *Server) (string, uint16) {
	t.Helper()
	srv.st.Lock()
	defer srv.st.Unlock()
	if len(srv.st.Nodes) == 0 {
		t.Fatal("no node registered")
	}
	host, port := agentclient.SplitHostPort(srv.st.Nodes[0].Addr)
	return nodeHostKey(host), port
}

// M3: the sweep is a hearthd→agent dial like any other and must present the
// credential minted for THAT node. Both sweep call sites used to hand over
// srv.cfg.Token — the control-plane admin key, which is remote root on every
// worker. A tenant may set idle_sleep_s to the 5s minimum, PickNode schedules
// onto the emptiest (freshly enrolled) node, and the sweep runs every 15s: one
// join token, or one compromised worker, recovered the fleet admin credential
// in about twenty seconds. The per-node credential is worth exactly one worker
// only if NO dial path can substitute the admin token for it.
func TestSweepNeverSendsTheAdminTokenToANode(t *testing.T) {
	fa := &lcAgent{}
	srv, id := newLifecycleTestServer(t, fa) // cfg.Token is "admin-tok"
	now := time.Now().Unix()

	// Enroll the node the way a join does: its own hearthd→agent bearer.
	const nodeTok = "hearth_nt_lifecycle"
	host, port := nodeHostOf(t, srv)
	if err := srv.putNodeCred(host, port, nodeTok, now); err != nil {
		t.Fatalf("put node cred: %v", err)
	}

	// Sweep 1: idle past the policy → auto-sleep dials the agent.
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	sb.IdleSleepS = 5
	sb.LastActivity = now - 10
	srv.st.Unlock()
	srv.lifecycleSweep(now)
	if !fa.saw("POST /v1/vms/" + id + "/sleep") {
		t.Fatalf("auto-sleep did not dial the agent; calls: %v", fa.snapshot())
	}

	// Sweep 2: asleep past the TTL → auto-delete dials the agent.
	srv.st.Lock()
	sb = srv.st.FindSandbox(id)
	sb.AsleepDeleteS = 30
	sb.SleptAt = now - 60
	srv.st.Unlock()
	srv.lifecycleSweep(now)
	if !fa.saw("DELETE /v1/vms/" + id) {
		t.Fatalf("auto-delete did not dial the agent; calls: %v", fa.snapshot())
	}

	auths := fa.authSnapshot()
	if len(auths) < 2 {
		t.Fatalf("expected at least the sleep and delete dials, got %d", len(auths))
	}
	calls := fa.snapshot()
	for i, got := range auths {
		if strings.Contains(got, srv.cfg.Token) {
			t.Fatalf("sweep dial %d (%s) carried the control-plane admin token: %q",
				i, calls[i], got)
		}
		if got != "Bearer "+nodeTok {
			t.Errorf("sweep dial %d (%s) presented %q, want the node's own credential %q",
				i, calls[i], got, "Bearer "+nodeTok)
		}
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
