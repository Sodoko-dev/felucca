// Tests for the usage aggregation (usage.go, v4 P5.3): the pure
// aggregateUsage fold over []store.UsageEvent literals, plus an endpoint
// smoke for GET /api/v1/tenants/{id}/usage (admin reach, tenant self-access,
// cross-tenant 404, window validation). Internal package so aggregateUsage is
// reachable and doRequest/adminAuth (join_test.go / expose_test.go) apply.
package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// closeTo compares hour totals that may not be exactly representable in
// binary. Cases below use == only for exact values (1.0, 2.0, 0.5).
func closeTo(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

// ev builds one usage event row. The shape fields only matter on run-start
// events — aggregateUsage snapshots them when the interval opens.
func ev(sb, event string, ts int64, vcpus uint32, memMiB uint64, diskGB uint32) store.UsageEvent {
	return store.UsageEvent{
		TenantID: "tn-x", SandboxID: sb, Event: event,
		Vcpus: vcpus, MemMiB: memMiB, DiskGB: diskGB, TS: ts,
	}
}

func TestAggregateFullHour(t *testing.T) {
	t0 := int64(1_000_000)
	events := []store.UsageEvent{
		ev("sb-1", "created", t0, 2, 512, 0),
		ev("sb-1", "deleted", t0+3600, 0, 0, 0),
	}
	res := aggregateUsage(events, "tn-x", t0, t0+3600, t0+7200)
	if res.SandboxHours != 1.0 {
		t.Errorf("sandbox_hours: %v, want 1.0", res.SandboxHours)
	}
	if res.VcpuHours != 2.0 {
		t.Errorf("vcpu_hours: %v, want 2.0", res.VcpuHours)
	}
	if res.MemGiBHours != 0.5 { // 512 MiB = 0.5 GiB, exact in binary
		t.Errorf("mem_gib_hours: %v, want 0.5", res.MemGiBHours)
	}
	// disk_gb == 0 means the unresized base image: BaseImageDiskGB counts.
	if want := float64(model.BaseImageDiskGB); res.DiskGBHours != want {
		t.Errorf("disk_gb_hours: %v, want %v", res.DiskGBHours, want)
	}
	if res.Events != 2 {
		t.Errorf("events: %d, want 2", res.Events)
	}

	// Explicit disk: 8 GB for one hour.
	events[0].DiskGB = 8
	res = aggregateUsage(events, "tn-x", t0, t0+3600, t0+7200)
	if res.DiskGBHours != 8.0 {
		t.Errorf("disk_gb_hours (8GB): %v, want 8.0", res.DiskGBHours)
	}
}

func TestAggregateClipsStillRunning(t *testing.T) {
	t0 := int64(1_000_000)
	events := []store.UsageEvent{ev("sb-1", "created", t0, 1, 1024, 0)}

	// No terminal event, now inside the window: clipped at now → 1h.
	res := aggregateUsage(events, "tn-x", t0, t0+7200, t0+3600)
	if !closeTo(res.SandboxHours, 1.0) {
		t.Errorf("clip at now: sandbox_hours %v, want 1.0", res.SandboxHours)
	}
	// now past the window's end: clipped at to → 1h.
	res = aggregateUsage(events, "tn-x", t0, t0+3600, t0+7200)
	if !closeTo(res.SandboxHours, 1.0) {
		t.Errorf("clip at to: sandbox_hours %v, want 1.0", res.SandboxHours)
	}
}

func TestAggregatePreWindowStart(t *testing.T) {
	// Created two hours before the window: only the in-window hour counts,
	// and only the in-window event (deleted) is tallied.
	from := int64(1_000_000)
	events := []store.UsageEvent{
		ev("sb-1", "created", from-7200, 1, 1024, 0),
		ev("sb-1", "deleted", from+3600, 0, 0, 0),
	}
	res := aggregateUsage(events, "tn-x", from, from+7200, from+10000)
	if !closeTo(res.SandboxHours, 1.0) {
		t.Errorf("sandbox_hours: %v, want 1.0", res.SandboxHours)
	}
	if res.Events != 1 {
		t.Errorf("events: %d, want 1 (created is pre-window)", res.Events)
	}
}

func TestAggregateSleepPausesClock(t *testing.T) {
	// 30 min running, 1h asleep, 30 min running: exactly 1.0 sandbox-hours
	// (1800s + 1800s = 3600s; both halves are exact in binary).
	t0 := int64(1_000_000)
	events := []store.UsageEvent{
		ev("sb-1", "created", t0, 1, 1024, 0),
		ev("sb-1", "slept", t0+1800, 0, 0, 0),
		ev("sb-1", "woken", t0+5400, 1, 1024, 0),
		ev("sb-1", "deleted", t0+7200, 0, 0, 0),
	}
	res := aggregateUsage(events, "tn-x", t0, t0+7200, t0+10000)
	if res.SandboxHours != 1.0 {
		t.Errorf("sandbox_hours: %v, want exactly 1.0", res.SandboxHours)
	}
}

func TestAggregateExecs(t *testing.T) {
	// Three execs in the window count; the pre-window one doesn't. Events
	// counts every in-window row; execs open no run interval.
	from := int64(1_000_000)
	events := []store.UsageEvent{
		ev("sb-1", "exec", from-100, 0, 0, 0), // before the window
		ev("sb-1", "exec", from+10, 0, 0, 0),
		ev("sb-1", "exec", from+20, 0, 0, 0),
		ev("sb-1", "exec", from+30, 0, 0, 0),
	}
	res := aggregateUsage(events, "tn-x", from, from+3600, from+7200)
	if res.Execs != 3 {
		t.Errorf("execs: %d, want 3", res.Execs)
	}
	if res.Events != 3 {
		t.Errorf("events: %d, want 3", res.Events)
	}
	if res.SandboxHours != 0 {
		t.Errorf("sandbox_hours: %v, want 0 (exec is not a transition)", res.SandboxHours)
	}
}

func TestAggregateDoubleStartTolerated(t *testing.T) {
	// Two consecutive run-starts without a run-end between (created, then a
	// stray woken): the map keeps the first interval and ignores the second —
	// one clean hour at the original shape, never double-counted.
	t0 := int64(1_000_000)
	events := []store.UsageEvent{
		ev("sb-1", "created", t0, 1, 1024, 0),
		ev("sb-1", "woken", t0+1800, 4, 4096, 0), // bigger shape: must be ignored
		ev("sb-1", "deleted", t0+3600, 0, 0, 0),
	}
	res := aggregateUsage(events, "tn-x", t0, t0+3600, t0+7200)
	if res.SandboxHours != 1.0 {
		t.Errorf("sandbox_hours: %v, want 1.0", res.SandboxHours)
	}
	if res.VcpuHours != 1.0 {
		t.Errorf("vcpu_hours: %v, want 1.0 (shape from the first start)", res.VcpuHours)
	}
}

// ---- Endpoint smoke ----

// newUsageTestServer builds a server with an admin token and a throwaway
// SQLite store; no node/agent is needed — usage rows are appended directly.
func newUsageTestServer(t *testing.T) *Server {
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
	return New(cfg, state.New(), db)
}

// mintTenant creates a tenant via the admin API and returns (id, api key) —
// the same flow expose_test.go uses for tenant-scoping checks.
func mintTenant(t *testing.T, h http.Handler, name string) (string, string) {
	t.Helper()
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"`+name+`"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant %s: %d %s", name, w.Code, w.Body.String())
	}
	var resp struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create tenant %s: bad body %q: %v", name, w.Body, err)
	}
	return resp.Tenant.ID, resp.APIKey
}

func TestTenantUsageEndpoint(t *testing.T) {
	srv := newUsageTestServer(t)
	h := srv.Handler()
	idA, keyA := mintTenant(t, h, "acme")
	_, keyB := mintTenant(t, h, "globex")

	// One sandbox, one clean hour, well in the past so now never clips.
	t0 := time.Now().Unix() - 86400
	for _, e := range []store.UsageEvent{
		{TenantID: idA, SandboxID: "sb-1", Event: "created", Vcpus: 2, MemMiB: 1024, DiskGB: 0, TS: t0},
		{TenantID: idA, SandboxID: "sb-1", Event: "deleted", TS: t0 + 3600},
	} {
		if err := srv.db.AppendUsage(e); err != nil {
			t.Fatalf("append usage: %v", err)
		}
	}
	usagePath := fmt.Sprintf("/api/v1/tenants/%s/usage?from=%d&to=%d", idA, t0, t0+3600)

	var totals struct {
		TenantID     string  `json:"tenant_id"`
		SandboxHours float64 `json:"sandbox_hours"`
		VcpuHours    float64 `json:"vcpu_hours"`
		MemGiBHours  float64 `json:"mem_gib_hours"`
		DiskGBHours  float64 `json:"disk_gb_hours"`
		Execs        int64   `json:"execs"`
		Events       int64   `json:"events"`
	}

	// Admin reach.
	w := doRequest(h, "GET", usagePath, "", adminAuth)
	if w.Code != 200 {
		t.Fatalf("admin usage: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &totals); err != nil {
		t.Fatalf("usage body: %v (%s)", err, w.Body.String())
	}
	if totals.TenantID != idA {
		t.Errorf("tenant_id: %q, want %q", totals.TenantID, idA)
	}
	if totals.SandboxHours != 1.0 || totals.VcpuHours != 2.0 || totals.MemGiBHours != 1.0 {
		t.Errorf("hours: sandbox %v vcpu %v mem %v, want 1/2/1",
			totals.SandboxHours, totals.VcpuHours, totals.MemGiBHours)
	}
	// disk_gb == 0 → the 2 GB base image for one hour.
	if want := float64(model.BaseImageDiskGB); totals.DiskGBHours != want {
		t.Errorf("disk_gb_hours: %v, want %v", totals.DiskGBHours, want)
	}
	if totals.Execs != 0 || totals.Events != 2 {
		t.Errorf("execs/events: %d/%d, want 0/2", totals.Execs, totals.Events)
	}

	// A tenant may read its own usage with its API key.
	w2 := doRequest(h, "GET", usagePath, "", "Bearer "+keyA)
	if w2.Code != 200 {
		t.Errorf("own-key usage: %d %s", w2.Code, w2.Body.String())
	}
	// Another tenant's key gets 404 — no cross-tenant existence leak.
	w3 := doRequest(h, "GET", usagePath, "", "Bearer "+keyB)
	if w3.Code != 404 {
		t.Errorf("foreign-key usage: %d, want 404", w3.Code)
	}
	// Unknown tenant id as admin: 404.
	w4 := doRequest(h, "GET", "/api/v1/tenants/tn-none/usage", "", adminAuth)
	if w4.Code != 404 {
		t.Errorf("unknown tenant usage: %d, want 404", w4.Code)
	}
	// from after to: 400.
	w5 := doRequest(h, "GET", fmt.Sprintf("/api/v1/tenants/%s/usage?from=10&to=5", idA), "", adminAuth)
	if w5.Code != 400 {
		t.Errorf("from>to: %d, want 400", w5.Code)
	}
}
