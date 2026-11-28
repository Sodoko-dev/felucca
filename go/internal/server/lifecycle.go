// Lifecycle policies (v4 P5.2): auto-sleep after idle, auto-delete after a
// long sleep. Per-tenant defaults (store.Tenant) with per-sandbox overrides
// (model.Sandbox.IdleSleepS/AsleepDeleteS; 0 = inherit, -1 = disabled); a
// background sweep enforces them from the activity clocks feluccad already
// owns (exec, wake, gateway ingress reports). Design: ADR-0009.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/ingress"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
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

	// The sweep's drop of dynamic exposes holds the global exposeMu across
	// each worker call, so its calls are bounded much tighter than the
	// client's 30 s: dropCallTimeout per call, and dropPassBudget of worker
	// time per pass — once a pass's drops have spent it, the rest wait for
	// the next pass. A pass's drops therefore hold exposeMu for at most
	// dropPassBudget + dropCallTimeout, well inside lifecycleInterval.
	dropPassBudget = 5 * time.Second
)

// dropCallTimeout is the limit of one drop's worker call (see dropPassBudget).
// A variable for tests.
var dropCallTimeout = 5 * time.Second

// dropKey names a dynamic expose whose drop got no answer in time
// (srv.unansweredDrops).
type dropKey struct{ sandbox, name string }

// dropBudget is the worker time one pass's drops may still spend.
type dropBudget struct{ left time.Duration }

// validPolicy reports whether a wire policy value is acceptable:
// 0 (inherit), -1 (disabled), or [min, maxPolicyS] seconds.
func validPolicy(v, min int64) bool {
	return v == 0 || v == model.PolicyDisabled || (v >= min && v <= maxPolicyS)
}

// effectivePolicy resolves override → tenant default → disabled, returning
// seconds (0 = no policy). The tenant may be nil (admin-context sandboxes, a
// tenant row that no longer exists — "no policy", never surprise deletion). A
// row that could not be READ does not get here: the sweep skips its sandboxes.
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
	// Nothing while the store is failing: every action ends in a save that
	// would fail, and the gate refuses the same changes to every caller.
	if srv.storeFailing() {
		return
	}

	type action struct {
		id string
		// tenant is the owner at decision time. It is not used to decide
		// anything — only to key the unreachable set, so that one tenant's
		// transport failure cannot defer a co-tenant's reclamation. See below.
		tenant string
		sleep  bool // false = delete, or drop when drop is set
		// drop: GC the dynamic exposes of a sandbox asleep for at least the
		// port quarantine (dropDynamicExposes).
		drop bool
	}
	var acts []action

	// The tenants' policies are read BEFORE the decision pass and off the
	// state lock: one GetTenant per distinct tenant per sweep. A store read
	// under the global lock made every request wait on the database.
	var tenantIDs []string
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		seen := map[string]bool{}
		for _, sb := range srv.st.Sandboxes {
			if sb.TenantID != "" && !seen[sb.TenantID] &&
				(sb.State == model.StateRunning || sb.State == model.StateSleeping) {
				seen[sb.TenantID] = true
				tenantIDs = append(tenantIDs, sb.TenantID)
			}
		}
	}()
	// A tenant whose row could not be read is NOT "no policy": its sandboxes
	// are skipped this pass (unreadable), and the next sweep tries again.
	tenants := map[string]*store.Tenant{}
	unreadable := map[string]bool{}
	for _, id := range tenantIDs {
		t, err := srv.db.GetTenant(id)
		if err != nil {
			storeReadFailed()
			slog.Warn("lifecycle: tenant policy unreadable, skipping its sandboxes this sweep", "tenant", id, "err", err)
			unreadable[id] = true
			continue
		}
		tenants[id] = t
	}
	lookup := func(id string) *store.Tenant { return tenants[id] }

	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		for _, sb := range srv.st.Sandboxes {
			if unreadable[sb.TenantID] {
				continue
			}
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
					acts = append(acts, action{sb.ID, sb.TenantID, true, false})
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
					acts = append(acts, action{sb.ID, sb.TenantID, false, false})
				} else if longAsleepLocked(sb, now) && hasDynamicExpose(sb) && srv.dropDialableLocked(sb, now) {
					// Not before: a sandbox that wakes within the quarantine
					// keeps its rows and ports (FLU-6 R12).
					acts = append(acts, action{sb.ID, sb.TenantID, false, true})
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
		if a := acts[(i+off)%len(acts)]; !a.sleep && !a.drop {
			srv.autoDelete(a.id, a.tenant, unreachable)
		}
	}
	budget := &dropBudget{left: dropPassBudget}
	// First the drops that got no answer in an earlier pass, whatever the
	// sandbox's state now (see repeatUnansweredDrops).
	srv.repeatUnansweredDrops(now, unreachable, budget)
	for i := range acts {
		if a := acts[(i+off)%len(acts)]; a.drop {
			srv.dropDynamicExposes(a.id, a.tenant, now, unreachable, budget)
		}
	}
}

// dropDialableLocked reports whether the drop may be scheduled for sb: its
// node is ready by its heartbeat, so a silent worker is never dialed by the
// drop (it holds exposeMu across the call). A sandbox whose node record is
// gone is scheduled too: its drop makes no worker call at all, it only
// removes rows. Caller holds the state lock.
func (srv *Server) dropDialableLocked(sb *model.Sandbox, now int64) bool {
	if sb.NodeID == nil {
		return true
	}
	n := srv.st.FindNode(*sb.NodeID)
	return n == nil || n.NodeStatus(now) == "ready"
}

// hasDynamicExpose reports whether sb has a dynamic (all-digit) expose.
func hasDynamicExpose(sb *model.Sandbox) bool {
	for _, e := range sb.Exposes {
		if isAllDigits(e.Name) {
			return true
		}
	}
	return false
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
// failure is just retried by the next sweep). The sandbox's dynamic
// (ensure-created, all-digit-named) exposes — the ADR-0007 deferral — are
// GC'd later, by dropDynamicExposes. unreachable is the sweep's per-pass set of (node, tenant) pairs whose agent
// could not be reached; see lifecycleSweep.
func (srv *Server) autoSleep(id, tenant string, now int64, unreachable map[nodeTenant]bool) {
	// Checked per action, not once per sweep: a save that failed earlier in
	// this sweep has the store failing, and every later action would carry out
	// its worker call only to fail its save too.
	if srv.storeFailing() {
		return
	}
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
	if err == nil && resp.Status == 409 {
		// The worker refused: when it names the state it holds, the record
		// takes it, so a sandbox the worker holds as stopped is not put to
		// sleep again every pass (FLU-4 S10). There is no caller to answer.
		srv.adoptRefusedState(id, state, resp.Body, sweepID)
		slog.Warn("lifecycle: auto-sleep refused by the worker", "sandbox", id, "request_id", sweepID)
		return
	}
	if err != nil || resp.Status >= 300 {
		nodeUnreachable(unreachable, agentAddr, tenant, err)
		slog.Warn("lifecycle: auto-sleep agent call failed", "sandbox", id)
		return
	}

	// The dynamic exposes are NOT dropped here (FLU-6 R12): dropping them
	// releases their node ports into quarantine, and a sandbox that wakes
	// within it would get fresh ports and quarantine more. They go once the
	// sandbox has slept for ingress.PortQuarantine (dropDynamicExposes).
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		if sb := srv.st.FindSandbox(id); sb != nil {
			sb.State = model.StateSleeping
			sb.SleptAt = now
			srv.queueUsageLocked(sb, "slept")
		}
	}()
	if err := srv.saveState(); err != nil {
		// Memory keeps what the worker did (the VM IS asleep); the recovery
		// loop writes it.
		slog.Error("lifecycle: auto-sleep applied on the worker but not saved", "sandbox", id, "request_id", sweepID, "err", err)
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
	// Per action, as in autoSleep.
	if srv.storeFailing() {
		return
	}
	var agentAddr string
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || sb.State != model.StateSleeping {
			return false // raced a manual transition; next sweep re-decides
		}
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
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		srv.queueUsageLocked(srv.st.FindSandbox(id), "deleted")
		srv.st.RemoveSandboxLocked(id)
	}()
	if err := srv.saveState(); err != nil {
		// Memory keeps what the worker did (the VM is gone); the recovery loop
		// writes it.
		slog.Error("lifecycle: auto-delete applied on the worker but not saved", "sandbox", id, "request_id", sweepID, "err", err)
	}
	slog.Info("lifecycle: auto-deleted sandbox", "sandbox", id, "reason", "asleep past TTL")
}

// dropDynamicExposes is the sweep's GC of a sleeping sandbox's dynamic
// (all-digit-named) exposes, once it has slept for ingress.PortQuarantine
// (FLU-6 R12). Each row goes with its DNAT rule; its node port is quarantined
// with the row (state.RemoveExposeLocked). A durable change like any other:
// not attempted while the store is failing (an earlier action of this sweep
// may have failed it), saved, a failed save logged — memory keeps what the
// worker did and the recovery loop writes it.
//
// Bounded like autoDelete: a transport failure enters the sweep's unreachable
// set for (node, tenant) and ends this action, and the set is consulted
// before every dial. Each dial holds exposeMu (every expose in the fleet
// waits behind it), so a worker that is down costs this action at most ONE
// call per tenant per pass, and none at all when another action of the pass
// already found it unreachable.
func (srv *Server) dropDynamicExposes(id, tenant string, now int64, unreachable map[nodeTenant]bool, budget *dropBudget) {
	if srv.storeFailing() {
		return
	}
	agentAddr, state := srv.agentAddrAndState(id)
	if state != model.StateSleeping {
		return // woke (or was deleted) since the decision; it keeps its rows
	}
	if agentAddr != "" && unreachable[nodeTenant{agentAddr, tenant}] {
		return // retried next sweep; see lifecycleSweep
	}
	var dynamic []model.Expose
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		if sb := srv.st.FindSandbox(id); sb != nil {
			for _, e := range sb.Exposes {
				if isAllDigits(e.Name) {
					dynamic = append(dynamic, e)
				}
			}
		}
	}()
	if len(dynamic) == 0 {
		return
	}
	sweepID := newTraceID("sweep")
	dropped := 0
	for _, e := range dynamic {
		// A save that failed since this action began (another request's)
		// will refuse this one's too: stop unexposing.
		if srv.storeFailing() {
			break
		}
		if agentAddr != "" && unreachable[nodeTenant{agentAddr, tenant}] {
			break
		}
		if budget.left <= 0 {
			break // this pass's worker time is spent; the next pass goes on
		}
		ok, err := srv.dropExpose(id, e, sweepID, now, budget)
		if err != nil {
			// Only a transport failure marks the node unreachable; an answer
			// refusing the unexpose is about this sandbox. Either way the
			// rest are retried by the next sweep.
			nodeUnreachable(unreachable, agentAddr, tenant, transportErr(err))
			break
		}
		if !ok {
			break // woke (or was deleted): it keeps the rest of its rows
		}
		dropped++
	}
	if dropped == 0 {
		return
	}
	if err := srv.saveState(); err != nil {
		slog.Error("lifecycle: dynamic exposes dropped on the worker but not saved", "sandbox", id, "request_id", sweepID, "err", err)
	}
	slog.Info("lifecycle: dropped dynamic exposes of a long-sleeping sandbox", "sandbox", id, "count", dropped, "request_id", sweepID)
}

// longAsleepLocked reports whether sb is asleep and has been for at least the
// port quarantine at now: the condition under which the sweep drops its
// dynamic rows. Caller holds the state lock.
func longAsleepLocked(sb *model.Sandbox, now int64) bool {
	return sb != nil && sb.State == model.StateSleeping && sb.SleptAt > 0 &&
		now-sb.SleptAt >= int64(ingress.PortQuarantine/time.Second)
}

// dropExpose removes one dynamic expose row of a long-sleeping sandbox and its
// agent DNAT rule, leaving the worker's mapping in place while another row
// has the same guest port (state.GuestPortShared), like the unexpose handler.
// ok is false when nothing was done because the sandbox is no longer long
// asleep (or the row is gone). The worker call is limited to dropCallTimeout
// and its duration is charged to budget. On agent failure the
// row stays (the rule is still live) and the error is returned: the sweep
// retries. A 404 from the worker is the mapping being gone (agentUnexpose):
// the row goes. It does not save; the caller does.
func (srv *Server) dropExpose(id string, e model.Expose, reqID string, now int64, budget *dropBudget) (ok bool, err error) {
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

	// Re-checked right before the worker call, in the critical section that
	// reads what to call: a wake that completed since the sweep's decision
	// keeps the DNAT rule of what is now a running sandbox.
	var agentAddr string
	var row model.Expose
	shared := false
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if !longAsleepLocked(sb, now) {
			return false
		}
		found := false
		for _, o := range sb.Exposes {
			if o.Name == e.Name {
				row, found = o, true
				break
			}
		}
		if !found {
			return false
		}
		if sb.NodeID != nil {
			if node := srv.st.FindNode(*sb.NodeID); node != nil {
				agentAddr = node.Addr
			}
		}
		shared = state.GuestPortShared(sb, row)
		return true
	}() {
		return false, nil
	}

	if agentAddr != "" && !shared {
		// exposeMu stays held across the call (releasing it would reopen the
		// race with a concurrent routes/ensure); the call's own limit and the
		// pass's budget are what bound it.
		start := time.Now()
		err := srv.agentUnexposeWithin(agentAddr, id, row.GuestPort, reqID, dropCallTimeout)
		budget.left -= time.Since(start)
		if err != nil {
			if noAnswerInTime(err) {
				srv.rememberUnansweredDrop(id, row)
			}
			slog.Warn("lifecycle: drop expose failed", "sandbox", id, "expose", e.Name, "err", err)
			return false, err
		}
	}
	delete(srv.unansweredDrops, dropKey{id, row.Name})
	woke := false
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		// And again before the row goes. A sandbox that woke DURING the call
		// has lost the rule on the worker already; its row goes too (a row
		// naming a port with no rule would answer the gateway's next request
		// from a dead port, while without it the gateway re-ensures the
		// route), and the loss is logged — after the lock.
		if sb := srv.st.FindSandbox(id); sb != nil && !longAsleepLocked(sb, now) {
			woke = true
		}
		srv.st.RemoveExposeLocked(id, e.Name)
	}()
	if woke {
		slog.Warn("lifecycle: the sandbox woke while its dynamic expose was being dropped; the route is re-created on its next request",
			"sandbox", id, "expose", e.Name, "request_id", reqID)
	}
	return true, nil
}

// noAnswerInTime reports whether a worker call ended at its time limit: the
// request may have reached the worker, which carries an unexpose out on its
// own task whether or not feluccad still waits for the answer.
func noAnswerInTime(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// rememberUnansweredDrop records a drop whose worker call got no answer in
// time. The row is kept (the rule may still be live), but the worker may have
// removed the rule: if the sandbox then woke, its dynamic hostname would be
// routed to a dead port until someone deleted that expose. The drop is
// repeated on the next passes until the worker answers
// (repeatUnansweredDrops). Caller holds exposeMu.
func (srv *Server) rememberUnansweredDrop(id string, row model.Expose) {
	if srv.unansweredDrops == nil {
		srv.unansweredDrops = map[dropKey]uint16{}
	}
	srv.unansweredDrops[dropKey{id, row.Name}] = row.NodePort
}

// repeatUnansweredDrops repeats, once per pass, every drop that got no answer
// in an earlier pass, whether or not the sandbox has woken since: unexpose
// again, then remove the row and quarantine its port as a drop does. An entry
// is forgotten when the worker answers — a refusal too, since then the rule is
// still live and the row matches it — or when its sandbox or row is gone (the
// row bounds the entries). It keeps to the pass's budget and unreachable set,
// and dials only a ready node, like the drop.
func (srv *Server) repeatUnansweredDrops(now int64, unreachable map[nodeTenant]bool, budget *dropBudget) {
	var keys []dropKey
	func() {
		srv.exposeMu.Lock()
		defer srv.exposeMu.Unlock()
		for k := range srv.unansweredDrops {
			keys = append(keys, k)
		}
	}()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].sandbox != keys[j].sandbox {
			return keys[i].sandbox < keys[j].sandbox
		}
		return keys[i].name < keys[j].name
	})
	removed := 0
	for _, k := range keys {
		if srv.storeFailing() || budget.left <= 0 {
			break
		}
		if srv.repeatDrop(k, now, unreachable, budget) {
			removed++
		}
	}
	if removed == 0 {
		return
	}
	if err := srv.saveState(); err != nil {
		slog.Error("lifecycle: repeated drops of dynamic exposes not saved", "count", removed, "err", err)
	}
	slog.Info("lifecycle: repeated the drops of dynamic exposes that got no answer in time", "count", removed)
}

// repeatDrop is one entry of repeatUnansweredDrops; true when the row went.
func (srv *Server) repeatDrop(k dropKey, now int64, unreachable map[nodeTenant]bool, budget *dropBudget) bool {
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

	var agentAddr, tenant string
	var row model.Expose
	shared, gone, dialable := false, false, true
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(k.sandbox)
		if sb == nil {
			gone = true
			return
		}
		found := false
		for _, o := range sb.Exposes {
			if o.Name == k.name && o.NodePort == srv.unansweredDrops[k] {
				row, found = o, true
				break
			}
		}
		if !found {
			gone = true
			return
		}
		tenant = sb.TenantID
		dialable = srv.dropDialableLocked(sb, now)
		if sb.NodeID != nil {
			if node := srv.st.FindNode(*sb.NodeID); node != nil {
				agentAddr = node.Addr
			}
		}
		shared = state.GuestPortShared(sb, row)
	}()
	if gone {
		delete(srv.unansweredDrops, k)
		return false
	}
	if !dialable || (agentAddr != "" && unreachable[nodeTenant{agentAddr, tenant}]) {
		return false // kept for a later pass
	}
	reqID := newTraceID("sweep")
	if agentAddr != "" && !shared {
		start := time.Now()
		err := srv.agentUnexposeWithin(agentAddr, k.sandbox, row.GuestPort, reqID, dropCallTimeout)
		budget.left -= time.Since(start)
		if err != nil {
			nodeUnreachable(unreachable, agentAddr, tenant, transportErr(err))
			if !noAnswerInTime(err) && transportErr(err) == nil {
				// Answered, refusing: the rule is live and the row matches it.
				delete(srv.unansweredDrops, k)
			}
			slog.Warn("lifecycle: repeated drop of a dynamic expose failed", "sandbox", k.sandbox, "expose", k.name, "request_id", reqID, "err", err)
			return false
		}
	}
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		srv.st.RemoveExposeLocked(k.sandbox, k.name)
	}()
	delete(srv.unansweredDrops, k)
	return true
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
