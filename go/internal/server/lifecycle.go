// Lifecycle policies (v4 P5.2): auto-sleep after idle, auto-delete after a
// long sleep. Per-tenant defaults (store.Tenant) with per-sandbox overrides
// (model.Sandbox.IdleSleepS/AsleepDeleteS; 0 = inherit, -1 = disabled); a
// background sweep enforces them from the activity clocks feluccad already
// owns (exec, wake, gateway ingress reports). Design: ADR-0009.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/store"
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
		id string
		// tenant is the owner at decision time. It is not used to decide
		// anything — only to key the unreachable set, so that one tenant's
		// transport failure cannot defer a co-tenant's reclamation. See below.
		tenant string
		sleep  bool // false = delete
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

	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
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
					acts = append(acts, action{sb.ID, sb.TenantID, true})
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
					acts = append(acts, action{sb.ID, sb.TenantID, false})
				}
			}
		}
	}()

	// unreachable collects, for THIS sweep only, the (node, tenant) pairs whose
	// agent could not be reached at all (a transport failure, not an answer). It
	// bounds what one dead node costs the sweep.
	//
	// The actions run sequentially and each dials through a client with a 30s
	// timeout, so a permanently-gone node holding K expired sandboxes used to
	// cost K x 30s EVERY pass: the 15s ticker just dropped its ticks and
	// auto-sleep enforcement for every other tenant queued behind that one node.
	// The number of expired sandboxes on a node is tenant-controllable and
	// unbounded; the number of nodes is not. Skipping the rest of a node's
	// actions once its agent has proved unreachable turns the first into the
	// second.
	//
	// The TENANT is in the key, and that is not incidental. Keyed on the node
	// alone, one transport failure deferred auto-sleep AND auto-delete for every
	// remaining sandbox on that node — co-tenants included — for the whole pass.
	// A tenant who can keep their own node's agent timing out once per sweep
	// (their workload is what makes it time out) would then suppress idle
	// reclamation for everyone else on that node indefinitely, and idle
	// reclamation is the mechanism that frees that node's RAM. So the skip is
	// scoped to the tenant whose action actually proved the node unreachable.
	//
	// The cost that buys back is bounded and not tenant-controllable: at most one
	// dial per (dead node, tenant with sandboxes on it) per sweep, where the
	// tenant count is an operator-provisioned number, instead of one per expired
	// sandbox, which any single tenant can inflate at will.
	//
	// It keys on a TRANSPORT failure and nothing else. An agent that ANSWERS —
	// 500, 409, anything — is reachable and cheap, and its refusal is about that
	// one sandbox; treating those as "node is down" would let one permanently
	// failing sandbox defer its neighbours on a healthy node forever.
	//
	// Nothing is abandoned: an action skipped here is retried on the next sweep,
	// exactly as a failed one is. See autoDelete on why the record stays.
	unreachable := map[nodeTenant]bool{}

	// Rotate where the pass starts. acts is built in deterministic state order,
	// so a fixed start means the same sandboxes are always enforced first and,
	// whenever a sweep runs long enough for the 15s ticker to drop a tick, the
	// same tail is always the part that loses it — permanently, to the same
	// tenants. Whoever sits at the head of the state order would starve everyone
	// behind them. Rotating gives every action an equal share of the head across
	// successive sweeps; nothing else about the pass depends on the order.
	off := 0
	if len(acts) > 0 {
		off = int(srv.sweepSeq.Add(1) % uint64(len(acts)))
	}
	// Sleeps before deletes. Both are bounded above, but auto-sleep is the
	// deadline-carrying half (a sandbox burning a node's RAM past its idle
	// policy), while a delete is of something already asleep — so if a sweep is
	// going to run long, it runs long after the sleeps, not before them.
	for i := range acts {
		if a := acts[(i+off)%len(acts)]; a.sleep {
			srv.autoSleep(a.id, a.tenant, now, unreachable)
		}
	}
	for i := range acts {
		if a := acts[(i+off)%len(acts)]; !a.sleep {
			srv.autoDelete(a.id, a.tenant, unreachable)
		}
	}
}

// nodeTenant is the key of the per-sweep unreachable set: a node's agent address
// and the tenant whose action proved it unreachable. See lifecycleSweep for why
// the tenant belongs in the key.
type nodeTenant struct {
	addr   string
	tenant string
}

// nodeUnreachable records a feluccad→agent transport failure against addr, for
// this tenant, for the remainder of one sweep. err is the dial error: only a
// transport failure counts (resp is nil), never a status the agent answered with.
func nodeUnreachable(unreachable map[nodeTenant]bool, addr, tenant string, err error) {
	if unreachable == nil || err == nil || addr == "" {
		return
	}
	unreachable[nodeTenant{addr, tenant}] = true
}

// autoSleep is the sweep's twin of the sleep handler: best-effort (an agent
// failure is just retried by the next sweep), and it GCs the sandbox's
// dynamic (ensure-created, all-digit-named) exposes — the ADR-0007 deferral.
// The next gateway request re-ensures them; auto-wake makes that seamless.
// unreachable is the sweep's per-pass set of (node, tenant) pairs whose agent
// could not be reached; see lifecycleSweep.
func (srv *Server) autoSleep(id, tenant string, now int64, unreachable map[nodeTenant]bool) {
	agentAddr, state := srv.agentAddrAndState(id)
	if state != model.StateRunning || agentAddr == "" {
		return // raced a manual transition or lost the node; next sweep re-decides
	}
	if unreachable[nodeTenant{agentAddr, tenant}] {
		// This tenant already timed this node out once this sweep; retried next
		// pass. A co-tenant on the same node is unaffected — see lifecycleSweep.
		return
	}
	// Each sweep action is its own traceable actor: the ID lets operators
	// correlate agent sleep calls with this sandbox's sweep decision.
	sweepID := newTraceID("sweep")
	// Through the dial choke point like every other feluccad→agent call: the
	// sweep gets THIS node's credential, never the control-plane admin token.
	// It used to pass srv.cfg.Token, which made "set idle_sleep_s: 5 on a
	// sandbox scheduled to a node I control" a 20-second recovery of the fleet
	// admin key — see nodeDial.
	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, "/v1/vms/"+id+"/sleep", nil, sweepID)
	if err != nil || resp.Status >= 300 {
		nodeUnreachable(unreachable, agentAddr, tenant, err)
		slog.Warn("lifecycle: auto-sleep agent call failed", "sandbox", id)
		return
	}

	srv.st.SetSandboxState(id, model.StateSleeping)
	var dynamic []model.Expose
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		if sb := srv.st.FindSandbox(id); sb != nil {
			sb.SleptAt = now
			srv.recordUsage(sb, "slept")
			for _, e := range sb.Exposes {
				if isAllDigits(e.Name) {
					dynamic = append(dynamic, e)
				}
			}
		}
	}()
	for _, e := range dynamic {
		srv.dropExpose(id, e, sweepID)
	}
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	slog.Info("lifecycle: auto-slept sandbox", "sandbox", id, "reason", "idle")
}

// autoDelete is the sweep's twin of the delete handler.
//
// It is NOT best-effort about the agent call, and that is the whole point. It
// used to ignore the result ("_, _ =") and remove the sandbox regardless, which
// meant any failure to reach the worker left a live microVM there with no
// control-plane record: still holding vCPUs, memory and its nft DNAT rules,
// unbillable, and unreachable by every API path — an orphan nobody would look
// for. The credential split made that reachable on a 15-second timer rather
// than only by operator action, but the bug was never really about credentials:
// a node rebooting, a network blip, an agent restart all produced it.
//
// A failed call therefore leaves the record exactly as it was. The sandbox is
// still asleep and still past its TTL, so the NEXT sweep retries — the state
// machine is the retry loop, and no VM is abandoned by a transient failure. A
// node that is permanently gone stops the retries from ever succeeding, which is
// why the manual handler keeps an explicit ?force=1 (see deleteSandbox): the
// sweep never abandons a VM on its own, and an operator always has a way to
// resolve a record that can no longer be completed.
//
// The retry is bounded, though. Each attempt dials through a client with a 30s
// timeout, so K expired sandboxes on a node that is permanently gone would cost
// the sweep K x 30s on every pass — one dead node starving auto-sleep for every
// other tenant. unreachable (see lifecycleSweep) collapses that to one dial per
// dead node per TENANT per sweep. The record is still kept; only the redundant
// attempts by the same tenant against an agent that has already proved
// unreachable this pass are skipped.
func (srv *Server) autoDelete(id, tenant string, unreachable map[nodeTenant]bool) {
	var agentAddr string
	var usage model.Sandbox
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || sb.State != model.StateSleeping {
			return false // raced a manual transition; next sweep re-decides
		}
		usage = *sb
		if sb.NodeID != nil {
			if node := srv.st.FindNode(*sb.NodeID); node != nil {
				agentAddr = node.Addr
			}
		}
		return true
	}() {
		return
	}

	if unreachable[nodeTenant{agentAddr, tenant}] {
		// This tenant already timed this node out once this sweep. The record
		// stays exactly as a failed attempt would leave it, and the next sweep
		// retries. A co-tenant on the same node is unaffected.
		slog.Warn("lifecycle: auto-delete deferred, this node's agent was already unreachable for this tenant this sweep; retrying next sweep",
			"sandbox", id, "node", agentAddr)
		return
	}

	// Each sweep action is its own traceable actor.
	sweepID := newTraceID("sweep")
	if agentAddr != "" {
		// Per-node credential, never srv.cfg.Token — see autoSleep and nodeDial.
		resp, err := srv.dialNode(agentAddr).do(http.MethodDelete, "/v1/vms/"+id, nil, sweepID)
		// A 404 means the VM is already gone on the worker: the record follows it.
		if err != nil || (resp.Status >= 300 && resp.Status != 404) {
			status := 0
			if resp != nil {
				status = resp.Status
			}
			nodeUnreachable(unreachable, agentAddr, tenant, err)
			slog.Warn("lifecycle: auto-delete agent call failed, keeping the sandbox so its VM is not orphaned; retrying next sweep",
				"sandbox", id, "node", agentAddr, "request_id", sweepID, "status", status, "err", err)
			return
		}
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
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
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
	}()

	if agentAddr != "" && !shared {
		if err := srv.agentUnexpose(agentAddr, id, e.GuestPort, reqID); err != nil {
			slog.Warn("lifecycle: drop expose failed", "sandbox", id, "expose", e.Name, "err", err)
			return
		}
	}
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil {
			return
		}
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
	}()
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
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		for _, id := range req.SandboxIDs {
			if sb := srv.st.FindSandbox(id); sb != nil {
				sb.LastActivity = now
			}
		}
	}()
	writeEmpty(w, 204)
}
