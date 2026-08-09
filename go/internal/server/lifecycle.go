// Lifecycle policies (v4 P5.2): auto-sleep after idle, auto-delete after a
// long sleep. Per-tenant defaults (store.Tenant) with per-sandbox overrides
// (model.Sandbox.IdleSleepS/AsleepDeleteS; 0 = inherit, -1 = disabled); a
// background sweep enforces them from the activity clocks hearthd already
// owns (exec, wake, gateway ingress reports). Design: ADR-0009.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

const (
	// Policy bounds. The minima keep a typo'd "1" from making a sandbox
	// unusable (sleeping between keystrokes); the maximum is one year.
	minIdleSleepS    = 5
	minAsleepDeleteS = 30
	maxPolicyS       = 365 * 24 * 3600

	// lifecycleInterval is the sweep cadence: policies are second-granular
	// but enforcement within ~15s of the deadline is plenty.
	lifecycleInterval = 15 * time.Second
)

// validPolicy reports whether a wire policy value is acceptable:
// 0 (inherit), -1 (disabled), or [min, maxPolicyS] seconds.
func validPolicy(v, min int64) bool {
	return v == 0 || v == model.PolicyDisabled || (v >= min && v <= maxPolicyS)
}

// effectivePolicy resolves override → tenant default → disabled, returning
// seconds (0 = no policy). The tenant may be nil (admin-context sandboxes,
// store blips — fail open to "no policy", never to surprise deletion).
func effectivePolicy(override int64, t *store.Tenant, ttl bool) int64 {
	if override == model.PolicyDisabled {
		return 0
	}
	if override > 0 {
		return override
	}
	if t == nil {
		return 0
	}
	if ttl {
		return t.DefaultAsleepDeleteS
	}
	return t.DefaultIdleSleepS
}

// LifecycleLoop runs the idle/TTL sweep every lifecycleInterval until stop
// closes. Usage-event retention (v4 P5.3) piggybacks here hourly.
func (srv *Server) LifecycleLoop(stop <-chan struct{}) {
	t := time.NewTicker(lifecycleInterval)
	defer t.Stop()
	var lastPrune int64
	for {
		select {
		case <-t.C:
			now := time.Now().Unix()
			srv.lifecycleSweep(now)
			if srv.cfg.UsageRetentionDays > 0 && now-lastPrune >= 3600 {
				lastPrune = now
				cutoff := now - srv.cfg.UsageRetentionDays*86400
				if n, err := srv.db.PruneUsage(cutoff); err != nil {
					slog.Error("lifecycle: prune usage", "err", err)
				} else if n > 0 {
					slog.Info("lifecycle: pruned usage events", "count", n, "older_than_days", srv.cfg.UsageRetentionDays)
				}
			}
		case <-stop:
			return
		}
	}
}

// lifecycleSweep applies the policies once: decisions are taken in one pass
// under the state lock, the (slow) agent calls happen after it is released.
func (srv *Server) lifecycleSweep(now int64) {
	type action struct {
		id    string
		sleep bool // false = delete
	}
	var acts []action

	// One GetTenant per distinct tenant per sweep, memoized. The indexed
	// point read under the state lock matches the existing recordUsage
	// discipline (AppendUsage also writes while holding it).
	tenants := map[string]*store.Tenant{}
	lookup := func(id string) *store.Tenant {
		if id == "" {
			return nil
		}
		if t, ok := tenants[id]; ok {
			return t
		}
		t, err := srv.db.GetTenant(id)
		if err != nil {
			t = nil
		}
		tenants[id] = t
		return t
	}

	srv.st.Lock()
	for _, sb := range srv.st.Sandboxes {
		switch sb.State {
		case model.StateRunning:
			idle := effectivePolicy(sb.IdleSleepS, lookup(sb.TenantID), false)
			if idle <= 0 {
				continue
			}
			last := sb.LastActivity
			if last == 0 {
				last = sb.CreatedAt
			}
			if now-last >= idle {
				acts = append(acts, action{sb.ID, true})
			}
		case model.StateSleeping:
			if sb.SleptAt == 0 {
				// Sleeper adopted from a pre-P5 snapshot: stamp now, so a
				// TTL counts from the upgrade — never retroactively.
				sb.SleptAt = now
				continue
			}
			ttl := effectivePolicy(sb.AsleepDeleteS, lookup(sb.TenantID), true)
			if ttl > 0 && now-sb.SleptAt >= ttl {
				acts = append(acts, action{sb.ID, false})
			}
		}
	}
	srv.st.Unlock()

	for _, a := range acts {
		if a.sleep {
			srv.autoSleep(a.id, now)
		} else {
			srv.autoDelete(a.id)
		}
	}
}

// autoSleep is the sweep's twin of the sleep handler: best-effort (an agent
// failure is just retried by the next sweep), and it GCs the sandbox's
// dynamic (ensure-created, all-digit-named) exposes — the ADR-0007 deferral.
// The next gateway request re-ensures them; auto-wake makes that seamless.
func (srv *Server) autoSleep(id string, now int64) {
	agentAddr, state := srv.agentAddrAndState(id)
	if state != model.StateRunning || agentAddr == "" {
		return // raced a manual transition or lost the node; next sweep re-decides
	}
	// Each sweep action is its own traceable actor: the ID lets operators
	// correlate agent sleep calls with this sandbox's sweep decision.
	sweepID := newTraceID("sweep")
	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := agentclient.Request(host, port, http.MethodPost, "/v1/vms/"+id+"/sleep", nil, srv.cfg.Token, sweepID)
	if err != nil || resp.Status >= 300 {
		slog.Warn("lifecycle: auto-sleep agent call failed", "sandbox", id)
		return
	}

	srv.st.SetSandboxState(id, model.StateSleeping)
	var dynamic []model.Expose
	srv.st.Lock()
	if sb := srv.st.FindSandbox(id); sb != nil {
		sb.SleptAt = now
		srv.recordUsage(sb, "slept")
		for _, e := range sb.Exposes {
			if isAllDigits(e.Name) {
				dynamic = append(dynamic, e)
			}
		}
	}
	srv.st.Unlock()
	for _, e := range dynamic {
		srv.dropExpose(id, e, sweepID)
	}
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	slog.Info("lifecycle: auto-slept sandbox", "sandbox", id, "reason", "idle")
}

// autoDelete is the sweep's twin of the delete handler (same best-effort
// agent semantics).
func (srv *Server) autoDelete(id string) {
	var agentAddr string
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || sb.State != model.StateSleeping {
		srv.st.Unlock()
		return // raced a manual transition; next sweep re-decides
	}
	usage := *sb
	if sb.NodeID != nil {
		if node := srv.st.FindNode(*sb.NodeID); node != nil {
			agentAddr = node.Addr
		}
	}
	srv.st.Unlock()

	// Each sweep action is its own traceable actor.
	sweepID := newTraceID("sweep")
	if agentAddr != "" {
		host, port := agentclient.SplitHostPort(agentAddr)
		_, _ = agentclient.Request(host, port, http.MethodDelete, "/v1/vms/"+id, nil, srv.cfg.Token, sweepID)
	}
	srv.st.RemoveSandbox(id)
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	srv.recordUsage(&usage, "deleted")
	slog.Info("lifecycle: auto-deleted sandbox", "sandbox", id, "reason", "asleep past TTL")
}

// dropExpose removes one expose row and its agent DNAT rule, preserving the
// shared-guest-port rule like the unexpose handler. Best-effort: on agent
// failure the row stays (the rule is still live) for a later manual delete.
func (srv *Server) dropExpose(id string, e model.Expose, reqID string) {
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

	var agentAddr string
	shared := false
	srv.st.Lock()
	if sb := srv.st.FindSandbox(id); sb != nil {
		if sb.NodeID != nil {
			if node := srv.st.FindNode(*sb.NodeID); node != nil {
				agentAddr = node.Addr
			}
		}
		for _, other := range sb.Exposes {
			if other.Name != e.Name && other.GuestPort == e.GuestPort {
				shared = true
				break
			}
		}
	}
	srv.st.Unlock()

	if agentAddr != "" && !shared {
		if err := srv.agentUnexpose(agentAddr, id, e.GuestPort, reqID); err != nil {
			slog.Warn("lifecycle: drop expose failed", "sandbox", id, "expose", e.Name, "err", err)
			return
		}
	}
	srv.st.Lock()
	if sb := srv.st.FindSandbox(id); sb != nil {
		kept := sb.Exposes[:0]
		for _, other := range sb.Exposes {
			if other.Name != e.Name {
				kept = append(kept, other)
			}
		}
		sb.Exposes = kept
		if len(sb.Exposes) == 0 {
			sb.Exposes = nil
		}
	}
	srv.st.Unlock()
}

// agentAddrAndState returns the sandbox's current state and its node's agent
// address ("" when the sandbox or node is gone).
func (srv *Server) agentAddrAndState(id string) (string, model.SandboxState) {
	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(id)
	if sb == nil {
		return "", ""
	}
	if sb.NodeID == nil {
		return "", sb.State
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		return "", sb.State
	}
	return node.Addr, sb.State
}

// isAllDigits marks the dynamic-expose name namespace (port-in-hostname
// routes are created under their decimal port; named exposes can never be
// all digits — validExposeName rejects them).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// routesActivity handles POST /api/v1/routes/activity (admin/gateway only —
// enforced by the caller's admin gate): the gateway's batched "these
// sandboxes served ingress traffic" report. Memory-only stamp; it reaches
// the store with the next snapshot persist, which is the right durability
// for a clock that only steers auto-sleep.
func (srv *Server) routesActivity(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxIDs []string `json:"sandbox_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	now := time.Now().Unix()
	srv.st.Lock()
	for _, id := range req.SandboxIDs {
		if sb := srv.st.FindSandbox(id); sb != nil {
			sb.LastActivity = now
		}
	}
	srv.st.Unlock()
	writeEmpty(w, 204)
}
