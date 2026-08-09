// Tests for the P6 latency histograms and the authenticated per-tenant
// metrics surface (metrics.go). The handler is tested directly; its routing
// (admin-gate 404, 401 without auth) is covered by conformance
// hearthd/24-observability once the route lands in serveAPI.
package server

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLatencyHistZeroStateEmitsAllBuckets(t *testing.T) {
	var h latencyHist
	var buf bytes.Buffer
	h.emit(&buf, "test_ms", "help text")
	out := buf.String()
	// Every bound plus +Inf, sum, count — present even with zero observations
	// (the conformance case asserts scrape-shape stability).
	for _, want := range []string{
		`test_ms_bucket{le="5"} 0`,
		`test_ms_bucket{le="30000"} 0`,
		`test_ms_bucket{le="+Inf"} 0`,
		"test_ms_sum 0",
		"test_ms_count 0",
		"# TYPE test_ms histogram",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("zero-state emit missing %q in:\n%s", want, out)
		}
	}
}

func TestLatencyHistCumulativeBuckets(t *testing.T) {
	var h latencyHist
	// 3 ms -> le=5; 25 ms -> le=25 (boundary is inclusive); 70 ms -> le=100;
	// 99999 ms -> +Inf only.
	for _, ms := range []uint64{3, 25, 70, 99999} {
		h.observe(ms)
	}
	var buf bytes.Buffer
	h.emit(&buf, "m", "h")
	out := buf.String()
	for _, want := range []string{
		`m_bucket{le="5"} 1`,     // 3
		`m_bucket{le="10"} 1`,    // cumulative
		`m_bucket{le="25"} 2`,    // +25 (inclusive bound)
		`m_bucket{le="50"} 2`,    //
		`m_bucket{le="100"} 3`,   // +70
		`m_bucket{le="30000"} 3`, //
		`m_bucket{le="+Inf"} 4`,  // +99999
		"m_count 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// sum = 3+25+70+99999
	if !strings.Contains(out, "m_sum 100097") {
		t.Errorf("wrong sum in:\n%s", out)
	}
}

func TestServeTenantMetricsShapeAndScoping(t *testing.T) {
	fa := &execAgent{}
	srv, id := newExecStreamTestServer(t, fa, "running")
	// Give the seeded sandbox a tenant and add an admin-context one.
	srv.st.Lock()
	if sb := srv.st.FindSandbox(id); sb != nil {
		sb.TenantID = "tn-metrics"
		sb.DiskGB = 8
	}
	srv.st.Unlock()

	w := httptest.NewRecorder()
	srv.serveTenantMetrics(w)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type %q", ct)
	}
	out := w.Body.String()
	for _, want := range []string{
		"# TYPE hearth_tenant_sandboxes gauge",
		`hearth_tenant_sandboxes{tenant="tn-metrics"} 1`,
		`hearth_tenant_running{tenant="tn-metrics"} 1`,
		`hearth_tenant_vcpus{tenant="tn-metrics"} 1`,
		`hearth_tenant_disk_gb{tenant="tn-metrics"} 8`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestServeTenantMetricsEmptyStateKeepsHeaders(t *testing.T) {
	fa := &execAgent{}
	srv, id := newExecStreamTestServer(t, fa, "running")
	// RemoveSandbox locks internally (unlike FindSandbox) — no caller lock.
	srv.st.RemoveSandbox(id)

	w := httptest.NewRecorder()
	srv.serveTenantMetrics(w)
	// HELP/TYPE always emitted: scrape shape stable with zero tenants
	// (conformance asserts this without creating sandboxes).
	if !strings.Contains(w.Body.String(), "# HELP hearth_tenant_sandboxes") {
		t.Errorf("zero-state output missing HELP header:\n%s", w.Body.String())
	}
}
