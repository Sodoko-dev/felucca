// Latency histograms + the authenticated per-tenant metrics surface (v4 P6).
// Design: ADR-0010. The three histograms live at package level, NOT in State:
// they are in-memory only (reset on restart is normal Prometheus counter
// semantics, documented in API-V2 §7) and carry their own mutex so the hot
// paths never touch the global state lock for metrics (P5 review lesson).
package server

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/alpham/infra-saas/felucca/internal/model"
)

// latencyBucketsMs are the shared upper bounds (ms) for all three histograms —
// wide enough for ~70 ms wakes and multi-second cold creates alike (+Inf is
// implicit as the final bucket).
var latencyBucketsMs = [...]uint64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000}

// latencyHist is a hand-rolled Prometheus histogram. The zero value is ready
// to use. counts are per-bucket (non-cumulative internally; emit() renders the
// cumulative form the text format requires).
type latencyHist struct {
	mu     sync.Mutex
	counts [len(latencyBucketsMs) + 1]uint64 // +1 = the +Inf bucket
	sumMs  uint64
	n      uint64
}

func (h *latencyHist) observe(ms uint64) {
	i := sort.Search(len(latencyBucketsMs), func(j int) bool { return ms <= latencyBucketsMs[j] })
	h.mu.Lock()
	h.counts[i]++ // i == len(...) means +Inf
	h.sumMs += ms
	h.n++
	h.mu.Unlock()
}

// emit writes the full HELP/TYPE/_bucket/_sum/_count block. Every bucket is
// ALWAYS emitted — even at zero observations — so scrape shape is stable and
// the conformance presence checks are deterministic (feluccad/24-observability).
func (h *latencyHist) emit(buf *bytes.Buffer, name, help string) {
	h.mu.Lock()
	counts := h.counts
	sum := h.sumMs
	n := h.n
	h.mu.Unlock()

	fmt.Fprintf(buf, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	var cum uint64
	for i, le := range latencyBucketsMs {
		cum += counts[i]
		fmt.Fprintf(buf, "%s_bucket{le=\"%d\"} %d\n", name, le, cum)
	}
	cum += counts[len(latencyBucketsMs)]
	fmt.Fprintf(buf, "%s_bucket{le=\"+Inf\"} %d\n", name, cum)
	fmt.Fprintf(buf, "%s_sum %d\n", name, sum)
	fmt.Fprintf(buf, "%s_count %d\n", name, n)
}

// The three process-lifetime histograms (ADR-0010): feluccad wall-clock — the
// latency a caller experiences, agent round-trip included.
var (
	wakeHist   latencyHist
	execHist   latencyHist
	createHist latencyHist
)

func observeWakeMs(ms uint64)   { wakeHist.observe(ms) }
func observeExecMs(ms uint64)   { execHist.observe(ms) }
func observeCreateMs(ms uint64) { createHist.observe(ms) }

// appendHistograms renders all three into the open /metrics payload
// (called from serveMetrics).
func appendHistograms(buf *bytes.Buffer) {
	wakeHist.emit(buf, "felucca_wake_duration_ms", "Wake latency (feluccad wall-clock, ms)")
	execHist.emit(buf, "felucca_exec_duration_ms", "Buffered exec latency (feluccad wall-clock, ms)")
	createHist.emit(buf, "felucca_create_duration_ms", "Create-to-201 latency (cold boots and pool claims mixed, ms)")
}

// serveTenantMetrics handles GET /api/v1/metrics/tenants — the AUTHENTICATED
// home of the tenant-labeled gauges that ADR-0009 deliberately kept off the
// open /metrics (tenant-inventory leak). Reachable by the admin token only;
// tenant keys get the routing layer's 404 like every admin surface. HELP/TYPE
// headers are always emitted so the scrape shape is stable with zero tenants.
func (srv *Server) serveTenantMetrics(w http.ResponseWriter) {
	type tshape struct {
		sandboxes, running int
		vcpus              uint64
		memMiB             uint64
		diskGB             uint64
	}
	perTenant := map[string]*tshape{}
	srv.st.Lock()
	for _, sb := range srv.st.Sandboxes {
		key := sb.TenantID
		if key == "" {
			// The admin context: labeled explicitly — an empty label value
			// reads as a bug in every Prometheus UI.
			key = "admin"
		}
		t := perTenant[key]
		if t == nil {
			t = &tshape{}
			perTenant[key] = t
		}
		t.sandboxes++
		if sb.State == model.StateRunning {
			t.running++
		}
		t.vcpus += uint64(sb.VCPUs)
		t.memMiB += sb.MemMiB
		t.diskGB += uint64(sb.EffectiveDiskGB())
	}
	srv.st.Unlock()

	// Stable output order (map iteration is random; scrapes should diff clean).
	ids := make([]string, 0, len(perTenant))
	for id := range perTenant {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var buf bytes.Buffer
	emit := func(name, help string, val func(*tshape) uint64) {
		fmt.Fprintf(&buf, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, id := range ids {
			fmt.Fprintf(&buf, "%s{tenant=%q} %d\n", name, id, val(perTenant[id]))
		}
	}
	emit("felucca_tenant_sandboxes", "Sandboxes per tenant", func(t *tshape) uint64 { return uint64(t.sandboxes) })
	emit("felucca_tenant_running", "Running sandboxes per tenant", func(t *tshape) uint64 { return uint64(t.running) })
	emit("felucca_tenant_vcpus", "Allocated vCPUs per tenant", func(t *tshape) uint64 { return t.vcpus })
	emit("felucca_tenant_mem_mib", "Allocated memory per tenant (MiB)", func(t *tshape) uint64 { return t.memMiB })
	emit("felucca_tenant_disk_gb", "Allocated disk per tenant (GB, quota-effective)", func(t *tshape) uint64 { return t.diskGB })

	body := buf.Bytes()
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	w.Write(body) //nolint:errcheck
}
