// Usage aggregation (v4 P5.3): GET /api/v1/tenants/{id}/usage?from&to folds
// the append-only usage_events stream (P0) into billable totals. Events stay
// the source of truth — this endpoint is a pure view, computable
// retroactively for any window the retention policy still covers.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// usageTotals is the wire shape of the usage endpoint.
type usageTotals struct {
	TenantID string `json:"tenant_id"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	// Hours are clipped to [from, to]; a sandbox still running at `to`
	// accrues up to min(now, to).
	SandboxHours float64 `json:"sandbox_hours"`
	VcpuHours    float64 `json:"vcpu_hours"`
	MemGiBHours  float64 `json:"mem_gib_hours"`
	DiskGBHours  float64 `json:"disk_gb_hours"`
	Execs        int64   `json:"execs"`
	Events       int64   `json:"events"`
}

// runStart marks a transition into "running" for interval folding;
// runEnd marks a transition out of it. "exec" is activity, not a transition.
var runStart = map[string]bool{
	"created": true, "started": true, "resumed": true, "woken": true, "forked": true,
}
var runEnd = map[string]bool{
	"paused": true, "slept": true, "stopped": true, "deleted": true,
}

// aggregateUsage folds the tenant's full event history (ts <= to, in order)
// into window totals. It needs the pre-window events to know each sandbox's
// state and shape at the window's start.
func aggregateUsage(events []store.UsageEvent, tenantID string, from, to, now int64) usageTotals {
	res := usageTotals{TenantID: tenantID, From: from, To: to}
	clipEnd := to
	if now < clipEnd {
		clipEnd = now
	}

	type open struct {
		since  int64
		vcpus  uint32
		memMiB uint64
		diskGB uint32
	}
	running := map[string]open{}

	accumulate := func(o open, until int64) {
		start, end := o.since, until
		if start < from {
			start = from
		}
		if end > clipEnd {
			end = clipEnd
		}
		if end <= start {
			return
		}
		h := float64(end-start) / 3600
		res.SandboxHours += h
		res.VcpuHours += h * float64(o.vcpus)
		res.MemGiBHours += h * float64(o.memMiB) / 1024
		// Events carry the raw disk_gb; 0 means "unresized base image",
		// which occupies BaseImageDiskGB — same rule as the quota gate.
		disk := o.diskGB
		if disk == 0 {
			disk = model.BaseImageDiskGB
		}
		res.DiskGBHours += h * float64(disk)
	}

	for _, e := range events {
		if e.TS >= from && e.TS <= to {
			res.Events++
			if e.Event == "exec" {
				res.Execs++
			}
		}
		switch {
		case runStart[e.Event]:
			if _, ok := running[e.SandboxID]; !ok {
				running[e.SandboxID] = open{e.TS, e.Vcpus, e.MemMiB, e.DiskGB}
			}
		case runEnd[e.Event]:
			if o, ok := running[e.SandboxID]; ok {
				accumulate(o, e.TS)
				delete(running, e.SandboxID)
			}
		}
	}
	// Still running at the window's end.
	for _, o := range running {
		accumulate(o, clipEnd)
	}
	return res
}

// tenantUsage handles GET /api/v1/tenants/{id}/usage?from=<unix>&to=<unix>.
// Reachable by the admin for any tenant and by a tenant key for its own id
// (routed in serveAPI). Defaults: from = to-30d, to = now.
func (srv *Server) tenantUsage(w http.ResponseWriter, r *http.Request, tenantID string) {
	t, err := srv.db.GetTenant(tenantID)
	if err != nil {
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if t == nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	now := time.Now().Unix()
	to := now
	if v := r.URL.Query().Get("to"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeJSON(w, 400, []byte(`{"error":"bad to"}`))
			return
		}
		to = n
	}
	from := to - 30*86400
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSON(w, 400, []byte(`{"error":"bad from"}`))
			return
		}
		from = n
	}
	if from > to {
		writeJSON(w, 400, []byte(`{"error":"from after to"}`))
		return
	}

	events, err := srv.db.ListUsage(tenantID, from, to)
	if err != nil {
		slog.Error("usage", "tenant", tenantID, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	res := aggregateUsage(events, tenantID, from, to, now)
	b, _ := json.Marshal(res)
	writeJSON(w, 200, b)
}
