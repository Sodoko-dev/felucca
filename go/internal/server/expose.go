// Sandbox ingress (v4 P3): named multi-service expose API and the route
// table the gateway (cmd/felucca-gw) consumes. Each expose maps the hostname
// label "<name>--<sandbox-id>" to a worker node port that the agent DNATs to
// the guest service. Design: ADR-0007.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/agentclient"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
)

// Every accepted expose burns one port from the worker's fleet-wide
// 20000-29999 pool, which every VM of every tenant on that node draws from,
// and nothing gives a named one back (the lifecycle sweep only GCs the
// all-digit dynamic ones). Without a cap a single in-quota sandbox can loop
// distinct names until the node has no ports left for anybody else, so both
// counts are bounded: per sandbox, and per tenant so the sum across a
// tenant's sandboxes cannot drain a node either.
const (
	maxExposesPerSandbox = 16
	maxExposesPerTenant  = 128
)

// validExposeName: lowercase [a-z0-9-], 1..=32, no leading/trailing dash and
// no "--" (the label separator must stay unambiguous), not all digits
// (all-digit labels are the dynamic port-in-hostname namespace).
func validExposeName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	digitsOnly := true
	prevDash := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= '0' && c <= '9':
			prevDash = false
		case c >= 'a' && c <= 'z':
			digitsOnly = false
			prevDash = false
		case c == '-':
			if i == 0 || i == len(name)-1 || prevDash {
				return false
			}
			digitsOnly = false
			prevDash = true
		default:
			return false
		}
	}
	return !digitsOnly
}

// exposeURL renders the public URL for a hostname label, or "" when no
// ingress domain is configured (the lab default — labels still work via the
// gateway's Host matching).
func (srv *Server) exposeURL(label string) string {
	if srv.cfg.IngressDomain == "" {
		return ""
	}
	return "https://" + label + "." + srv.cfg.IngressDomain
}

type exposeView struct {
	Name      string `json:"name"`
	GuestPort uint16 `json:"guest_port"`
	NodePort  uint16 `json:"node_port"`
	Hostname  string `json:"hostname"`
	URL       string `json:"url,omitempty"`
}

func (srv *Server) exposeView(sbID string, e model.Expose) exposeView {
	label := e.Name + "--" + sbID
	return exposeView{
		Name: e.Name, GuestPort: e.GuestPort, NodePort: e.NodePort,
		Hostname: label, URL: srv.exposeURL(label),
	}
}

// Churn caps on released node ports (FLU-6). A released port is unusable for
// ingress.PortQuarantine, so expose/unexpose cycles shrink the worker's
// 10000-port pool for everybody on it. Both are counts of entries still in
// quarantine: per tenant per worker host (admin unmetered, as for the caps
// above), and per sandbox whoever owns it — the latter also bounds what the
// unauthenticated visitors of an admin-owned dynamic-port sandbox can cause.
const (
	maxQuarantinedPerTenantHost = 512
	maxQuarantinedPerSandbox    = 64
)

// Retry-After values (seconds) of the node-port answers.
const (
	portConflictRetryAfter   = "5"
	portsExhaustedRetryAfter = "30"
)

// errPortsExhausted is the worker's 409 "ports exhausted": it has no free node
// port at all.
var errPortsExhausted = errors.New("agent expose: ports exhausted")

// exposeAnswer is the worker's answer to an expose. AvoidHonored is true from
// an agent that read the request's avoid list and promises never to return a
// port in it; an older agent does not send it.
type exposeAnswer struct {
	NodePort     uint16 `json:"node_port"`
	AvoidHonored bool   `json:"avoid_honored"`
}

// agentExpose asks the owning agent to DNAT a node port to guestPort and
// returns its answer. The agent call is idempotent per (vm, guest_port).
// avoid is always sent (an empty array when there is nothing to avoid);
// prefer is omitted when 0.
func (srv *Server) agentExpose(agentAddr, sbID string, guestPort uint16, avoid []uint16, prefer uint16, reqID string) (exposeAnswer, error) {
	if avoid == nil {
		avoid = []uint16{}
	}
	body, _ := json.Marshal(struct {
		GuestPort uint16   `json:"guest_port"`
		Avoid     []uint16 `json:"avoid"`
		Prefer    uint16   `json:"prefer,omitempty"`
	}{guestPort, avoid, prefer})
	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := srv.agentCall(host, port, http.MethodPost, "/v1/vms/"+sbID+"/expose", body, reqID)
	if err != nil {
		return exposeAnswer{}, fmt.Errorf("agent unreachable: %w", err)
	}
	if resp.Status == 409 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(resp.Body, &e) == nil && e.Error == "ports exhausted" {
			return exposeAnswer{}, errPortsExhausted
		}
	}
	if resp.Status != 200 {
		return exposeAnswer{}, fmt.Errorf("agent expose status %d", resp.Status)
	}
	var ar exposeAnswer
	if json.Unmarshal(resp.Body, &ar) != nil || ar.NodePort == 0 {
		return exposeAnswer{}, fmt.Errorf("agent expose: bad response")
	}
	return ar, nil
}

// nodePortRefusal is an answer acquireNodePort (or its record callback) gives
// instead of a port.
type nodePortRefusal struct {
	status     int
	body       string
	retryAfter string // "" = no header
}

func (ref *nodePortRefusal) write(w http.ResponseWriter) {
	if ref.retryAfter != "" {
		w.Header().Set("Retry-After", ref.retryAfter)
	}
	writeJSON(w, ref.status, []byte(ref.body))
}

var (
	refuseNotFound       = &nodePortRefusal{status: 404, body: `{"error":"not found"}`}
	refuseAgentExpose    = &nodePortRefusal{status: 502, body: `{"error":"agent expose failed"}`}
	refusePortsExhausted = &nodePortRefusal{status: 503, body: `{"error":"no free node port on this worker"}`, retryAfter: portsExhaustedRetryAfter}
	refusePortConflict   = &nodePortRefusal{status: 503, body: `{"error":"node port conflict, retry"}`, retryAfter: portConflictRetryAfter}
)

// portLists is what feluccad has recorded for one worker host, seen from one
// (sandbox, guest port): the lists an expose request carries and its answer is
// checked against (FLU-6 R3', R5').
type portLists struct {
	// own: the node ports this sandbox's rows hold for this guest port. The
	// agent answers one of them idempotently; never avoided, never refused.
	own map[uint16]bool
	// others: node port -> the other sandboxes on this host whose rows hold it.
	others map[uint16][]string
	// ownOther: ports this sandbox holds for its other guest ports, with a
	// row that holds each.
	ownOther map[uint16]model.Expose
	// quarantined: node port -> its newest entry still in quarantine on this host.
	quarantined map[uint16]model.ReleasedPort
	// strangers: quarantined ports on this host that another (sandbox, guest
	// port) released. A quarantined port that is not one is this sandbox's
	// own, released for this same guest port.
	strangers map[uint16]bool
	// prefer: the quarantined port this sandbox last held for this guest port
	// (0: none).
	prefer uint16
}

// portListsLocked builds the lists for sb and guestPort on host. Caller holds
// the state lock.
func (srv *Server) portListsLocked(host string, sb *model.Sandbox, guestPort uint16, now int64) portLists {
	pl := portLists{
		own: map[uint16]bool{}, others: map[uint16][]string{}, ownOther: map[uint16]model.Expose{},
		quarantined: map[uint16]model.ReleasedPort{}, strangers: map[uint16]bool{},
	}
	for _, e := range sb.Exposes {
		if e.GuestPort == guestPort {
			pl.own[e.NodePort] = true
		} else if _, seen := pl.ownOther[e.NodePort]; !seen {
			pl.ownOther[e.NodePort] = e
		}
	}
	for _, other := range srv.st.Sandboxes {
		if other.ID == sb.ID || len(other.Exposes) == 0 {
			continue
		}
		if h, ok := srv.st.NodeHostLocked(other); !ok || h != host {
			continue
		}
		for _, e := range other.Exposes {
			if ids := pl.others[e.NodePort]; len(ids) == 0 || ids[len(ids)-1] != other.ID {
				pl.others[e.NodePort] = append(ids, other.ID)
			}
		}
	}
	// strangers: ports another (sandbox, guest port) released on this host.
	// Handing such a port back to this sandbox would let that other hostname
	// reach it, so it is never preferred.
	strangers := pl.strangers
	var mine *model.ReleasedPort
	for _, q := range srv.st.QuarantineLocked(now) {
		if q.Host != host {
			continue
		}
		if cur, ok := pl.quarantined[q.Port]; !ok || q.ReleasedAt >= cur.ReleasedAt {
			pl.quarantined[q.Port] = q
		}
		if q.SandboxID == sb.ID && q.GuestPort == guestPort {
			if mine == nil || q.ReleasedAt >= mine.ReleasedAt {
				c := q
				mine = &c
			}
		} else {
			strangers[q.Port] = true
		}
	}
	if mine != nil {
		_, heldForAnotherGuestPort := pl.ownOther[mine.Port]
		if len(pl.own) == 0 && !strangers[mine.Port] &&
			len(pl.others[mine.Port]) == 0 && !heldForAnotherGuestPort {
			pl.prefer = mine.Port
		}
	}
	return pl
}

// avoid is the request's avoid list: every port recorded for the host (rows
// of all sandboxes on it, and the quarantined ports) except this sandbox's
// own for this guest port and the preferred port, sorted.
func (pl portLists) avoid() []uint16 {
	set := map[uint16]bool{}
	for p := range pl.others {
		set[p] = true
	}
	for p := range pl.ownOther {
		set[p] = true
	}
	for p := range pl.quarantined {
		set[p] = true
	}
	out := make([]uint16, 0, len(set))
	for p := range set {
		if !pl.own[p] && p != pl.prefer {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// churnRefusalLocked is the R6' cap check for a request that needs a fresh
// port: nil, or the 429 to answer. Caller holds the state lock.
//
// The per-sandbox cap counts distinct (node port, guest port) pairs on the
// host the request is for: after two nodes swapped addresses every entry
// exists under both hosts (state.rehostQuarantineLocked copies them), and two
// copies of one release are one port taken. A pair counts until its last copy
// expires.
func (srv *Server) churnRefusalLocked(host string, sb *model.Sandbox, now int64) *nodePortRefusal {
	var tenantN int
	var tenantNext int64 = -1
	type pair struct{ port, guest uint16 }
	sandboxPairs := map[pair]int64{} // pair -> seconds until its last copy expires
	for _, q := range srv.st.QuarantineLocked(now) {
		left := state.QuarantineExpiresIn(q, now)
		if sb.TenantID != adminTenant && q.Host == host && q.TenantID == sb.TenantID {
			tenantN++
			if tenantNext < 0 || left < tenantNext {
				tenantNext = left
			}
		}
		if q.SandboxID == sb.ID && q.Host == host {
			k := pair{q.Port, q.GuestPort}
			if left > sandboxPairs[k] {
				sandboxPairs[k] = left
			}
		}
	}
	sandboxN := len(sandboxPairs)
	var sandboxNext int64 = -1
	for _, left := range sandboxPairs {
		if sandboxNext < 0 || left < sandboxNext {
			sandboxNext = left
		}
	}
	refuse := func(next int64) *nodePortRefusal {
		if next < 1 {
			next = 1
		}
		return &nodePortRefusal{status: 429, body: `{"error":"expose churn limit reached"}`, retryAfter: strconv.FormatInt(next, 10)}
	}
	if tenantN >= maxQuarantinedPerTenantHost {
		return refuse(tenantNext)
	}
	if sandboxN >= maxQuarantinedPerSandbox {
		return refuse(sandboxNext)
	}
	return nil
}

// acquireNodePort is THE way feluccad gets a node port from a worker: the
// expose handlers and the fork's re-expose all come through here, so the
// churn caps and the answer check cannot be skipped by one of them. The
// caller holds exposeMu (lock order exposeMu -> saveMu -> st).
//
//  1. Under the state lock: the lists for this worker host, and — when the
//     request needs a fresh port (no row of this sandbox maps the guest port,
//     nothing to prefer) — the churn caps.
//  2. Off the lock: the worker call, carrying avoid and prefer.
//  3. Under the state lock again, in one critical section with the record:
//     the answer is checked against the lists as they are NOW (a delete does
//     not take exposeMu). A port another sandbox's row holds means that row is
//     stale — the worker is the authority on who holds a port — so it is
//     removed and quarantined, and the new mapping is removed from the worker
//     again. A quarantined port that is not prefer is refused the same way
//     when the agent promised to honour avoid, and accepted with a warning
//     from an older agent (the exposure FLU-6 closes, until it is upgraded).
//     Otherwise record runs (it may refuse too), and a returned prefer port
//     leaves quarantine: its last owner holds it again for the same service.
//
// The stale-row removal is saved here (its failure logged: the answer is the
// 503 either way); a successful record is the caller's to save.
func (srv *Server) acquireNodePort(agentAddr, sbID string, guestPort uint16, reqID string,
	record func(sb *model.Sandbox, port uint16) *nodePortRefusal) (uint16, *nodePortRefusal) {
	host, _ := agentclient.SplitHostPort(agentAddr)

	var avoid []uint16
	var prefer uint16
	var refused *nodePortRefusal
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(sbID)
		if sb == nil {
			refused = refuseNotFound
			return
		}
		now := srv.st.Now()
		pl := srv.portListsLocked(host, sb, guestPort, now)
		avoid, prefer = pl.avoid(), pl.prefer
		if len(pl.own) == 0 && pl.prefer == 0 {
			refused = srv.churnRefusalLocked(host, sb, now)
		}
	}()
	if refused != nil {
		return 0, refused
	}

	ans, err := srv.agentExpose(agentAddr, sbID, guestPort, avoid, prefer, reqID)
	if errors.Is(err, errPortsExhausted) {
		slog.Warn("expose: the worker has no free node port", "sandbox", sbID, "node", agentAddr, "request_id", reqID)
		return 0, refusePortsExhausted
	}
	if err != nil {
		slog.Error("expose agent failed", "sandbox", sbID, "guest_port", guestPort, "err", err, "request_id", reqID)
		return 0, refuseAgentExpose
	}

	port := ans.NodePort
	undo, staleRemoved := false, false
	// Log lines are collected under the state lock and written after it: a
	// slow log sink must not hold every request behind the lock.
	type logLine struct {
		level slog.Level
		msg   string
		args  []any
	}
	var logs []logLine
	logf := func(level slog.Level, msg string, args ...any) {
		logs = append(logs, logLine{level, msg, append(args, "request_id", reqID)})
	}
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(sbID)
		if sb == nil {
			// Deleted while we talked to the agent; the agent-side rule died
			// with the VM (or will at the next rebuild).
			refused = refuseNotFound
			return
		}
		now := srv.st.Now()
		pl := srv.portListsLocked(host, sb, guestPort, now)
		// ownReleased: the answer is quarantined only by this sandbox's own
		// entries for this same guest port (the prefer case, or the worker's
		// idempotent answer for a mapping whose rows feluccad already
		// removed): its old hostname reaches the same service again.
		_, quarantined := pl.quarantined[port]
		ownReleased := quarantined && !pl.strangers[port]
		if !pl.own[port] {
			if others := pl.others[port]; len(others) > 0 {
				for _, other := range others {
					srv.st.RemoveExposesOnPortLocked(other, port)
					logf(slog.LevelError, "expose: the worker handed out a node port another sandbox's route row holds; that row is stale and is removed, its port quarantined",
						"sandbox", sbID, "stale_sandbox", other, "node_host", host, "node_port", port)
				}
				staleRemoved, undo, refused = true, true, refusePortConflict
				return
			}
			if holder, ok := pl.ownOther[port]; ok {
				// Recorded, it would put two guest ports on one node port: the
				// second name would be served by the first one's service. An
				// agent that ignores avoid answers this again on every retry;
				// what ends it is removing the expose that holds the port.
				msg := fmt.Sprintf("node port conflict: the worker answered node port %d, which this sandbox's expose %q (guest port %d) holds; removing that expose ends it",
					port, holder.Name, holder.GuestPort)
				b, _ := json.Marshal(struct {
					Error string `json:"error"`
				}{msg})
				logf(slog.LevelError, "expose: the worker returned a node port this sandbox holds for another guest port; refused until that expose is removed",
					"sandbox", sbID, "guest_port", guestPort, "node_host", host, "node_port", port,
					"held_by_expose", holder.Name, "held_for_guest_port", holder.GuestPort)
				undo = true
				refused = &nodePortRefusal{status: 503, body: string(b), retryAfter: portConflictRetryAfter}
				return
			}
			if q, ok := pl.quarantined[port]; ok && port != prefer && !ownReleased {
				if ans.AvoidHonored {
					logf(slog.LevelError, "expose: the worker returned a quarantined node port it was told to avoid; refused",
						"sandbox", sbID, "node_host", host, "node_port", port, "released_by", q.SandboxID)
					undo, refused = true, refusePortConflict
					return
				}
				logf(slog.LevelWarn, "expose: an agent that does not honour avoid returned a quarantined node port; accepted (upgrade the agent)",
					"sandbox", sbID, "node_host", host, "node_port", port, "released_by", q.SandboxID,
					"released_ago_s", now-q.ReleasedAt)
			}
		}
		// The rows of this guest port follow the worker, the authority: when
		// it mapped a port they do not all name — none of them (its old
		// mapping is gone: an unexpose that timed out in feluccad after the
		// worker carried it out), or one of several (rows written before
		// this release that hold one guest port on two node ports) — every
		// row of the guest port is re-pointed to the answer and the other
		// ports are quarantined. Saved with the record.
		if len(pl.own) > 1 || (len(pl.own) == 1 && !pl.own[port]) {
			old := srv.st.RepointGuestPortLocked(sbID, guestPort, port)
			logf(slog.LevelWarn, "expose: the worker mapped this guest port to one node port; the sandbox's rows follow it and the other ports are quarantined",
				"sandbox", sbID, "guest_port", guestPort, "node_host", host, "node_port", port, "old_ports", fmt.Sprint(old))
		}
		if refused = record(sb, port); refused != nil {
			return
		}
		if (prefer != 0 && port == prefer) || ownReleased {
			srv.st.UnquarantineLocked(host, port, sbID, guestPort)
		}
	}()
	for _, l := range logs {
		slog.Log(context.Background(), l.level, l.msg, l.args...)
	}
	if staleRemoved {
		if err := srv.saveState(); err != nil {
			slog.Error("expose: stale route row removed but not saved (the recovery loop will write it)", "sandbox", sbID, "request_id", reqID, "err", err)
		}
	}
	if undo {
		if err := srv.agentUnexpose(agentAddr, sbID, guestPort, reqID); err != nil {
			slog.Error("expose: refused mapping could not be removed from the worker", "sandbox", sbID, "guest_port", guestPort, "node_port", port, "err", err, "request_id", reqID)
		}
	}
	if refused != nil {
		return 0, refused
	}
	return port, nil
}

func (srv *Server) agentUnexpose(agentAddr, sbID string, guestPort uint16, reqID string) error {
	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := srv.agentCall(host, port, http.MethodDelete, "/v1/vms/"+sbID+"/expose", unexposeBody(guestPort), reqID)
	return unexposeResult(resp, err)
}

// agentUnexposeWithin is agentUnexpose with a shorter limit than the client's
// 30 s, for the sweep's drop: it holds exposeMu across the call, and every
// expose in the fleet waits for it. A time-out is a transport failure.
func (srv *Server) agentUnexposeWithin(agentAddr, sbID string, guestPort uint16, reqID string, timeout time.Duration) error {
	resp, err := srv.dialNode(agentAddr).doWithin(timeout, http.MethodDelete, "/v1/vms/"+sbID+"/expose", unexposeBody(guestPort), reqID)
	return unexposeResult(resp, err)
}

// doWithin is nodeDial.do with a shorter limit for this one call. It is part
// of the choke point (TestNoDialPathCanCarryTheAdminToken): the credential is
// the dial's own.
func (d nodeDial) doWithin(timeout time.Duration, method, path string, body []byte, reqID string) (*agentclient.Response, error) {
	return agentclient.RequestWithin(d.host, d.port, method, path, body, d.token, reqID, timeout)
}

func unexposeBody(guestPort uint16) []byte {
	body, _ := json.Marshal(struct {
		GuestPort uint16 `json:"guest_port"`
	}{guestPort})
	return body
}

// unexposeResult reads the worker's answer to an unexpose.
func unexposeResult(resp *agentclient.Response, err error) error {
	if err != nil {
		return &agentUnreachableError{err: err}
	}
	// 404: the worker does not know the VM (re-installed under the same
	// address, or the VM is gone), so it holds no mapping for it either — the
	// rule is gone, as both delete paths treat a 404. A missing mapping of a
	// known VM is already a 200.
	if resp.Status == 404 {
		return nil
	}
	if resp.Status != 200 {
		return fmt.Errorf("agent unexpose status %d", resp.Status)
	}
	return nil
}

// agentUnreachableError is a worker call that got no answer at all (a
// transport failure), as opposed to an answer refusing it. Only this kind
// enters the sweep's per-pass unreachable set (nodeUnreachable).
type agentUnreachableError struct{ err error }

func (e *agentUnreachableError) Error() string { return "agent unreachable: " + e.err.Error() }
func (e *agentUnreachableError) Unwrap() error { return e.err }

// transportErr returns err when it is a transport failure, nil otherwise:
// the argument nodeUnreachable expects.
func transportErr(err error) error {
	var u *agentUnreachableError
	if errors.As(err, &u) {
		return err
	}
	return nil
}

// exposeSandbox handles POST /api/v1/sandboxes/{id}/expose {"name","port"}.
// 201 with the expose view; idempotent 200 when the same name+port already
// exists; 409 when the name is taken by a different port.
func (srv *Server) exposeSandbox(w http.ResponseWriter, r *http.Request, id, tenant string) {
	var req struct {
		Name string `json:"name"`
		Port uint16 `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if !validExposeName(req.Name) {
		writeJSON(w, 400, []byte(`{"error":"bad name: 1-32 of [a-z0-9-], no edge/double dash, not all digits"}`))
		return
	}
	if req.Port == 0 {
		writeJSON(w, 400, []byte(`{"error":"port required"}`))
		return
	}
	srv.exposeCommon(w, r, id, tenant, req.Name, req.Port, false)
}

// tenantExposeCount totals a tenant's exposes across all of its sandboxes.
// Caller must hold the state lock.
func (srv *Server) tenantExposeCount(tenantID string) int {
	n := 0
	for _, sb := range srv.st.Sandboxes {
		if sb.TenantID == tenantID {
			n += len(sb.Exposes)
		}
	}
	return n
}

// exposeCommon is the shared expose path for the named API and the dynamic
// ensure-route path (which uses the all-digit name namespace). dynamic only
// changes the success status code shape (200 route view vs 201 expose view).
func (srv *Server) exposeCommon(w http.ResponseWriter, r *http.Request, id, tenant, name string, guestPort uint16, dynamic bool) {
	// One ingress mutation at a time (see exposeMu); the state lock is still
	// taken per-section so reads elsewhere never wait on the agent call.
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()
	// The gate again, now that this request holds exposeMu. It was admitted
	// while the store was healthy, but may have waited here behind an expose
	// whose save then failed: that one is answered 503, and its row is in
	// memory, so without this the idempotent repeat below would answer 200 for
	// a row that is not in the database. Race-free: whoever marks the store
	// failing in an expose still holds exposeMu when it does.
	if srv.refusedWhileStoreFailing(w) {
		return
	}

	// Resolve + validate under the lock; do the agent call outside it.
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	if sb.IP == nil || sb.NodeID == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"sandbox has no address yet"}`))
		return
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"node gone"}`))
		return
	}
	agentAddr := node.Addr
	for _, e := range sb.Exposes {
		if e.Name == name {
			if e.GuestPort == guestPort {
				v := srv.exposeView(sb.ID, e)
				srv.st.Unlock()
				b, _ := json.Marshal(v)
				writeJSON(w, 200, b)
				return
			}
			srv.st.Unlock()
			writeJSON(w, 409, []byte(`{"error":"name already exposed with a different port"}`))
			return
		}
	}
	// Only a new name reaches the agent's port pool, so the caps go after the
	// name scan — an idempotent repeat must not start failing at the limit.
	if len(sb.Exposes) >= maxExposesPerSandbox {
		srv.st.Unlock()
		writeJSON(w, 429, []byte(`{"error":"expose limit reached for this sandbox"}`))
		return
	}
	// The tenant total is keyed on the sandbox's owner, not the caller: the
	// dynamic ensure-route path calls in as admin on a tenant's sandbox. Admin
	// itself is unmetered, matching the sandbox quota gate.
	if sb.TenantID != adminTenant && srv.tenantExposeCount(sb.TenantID) >= maxExposesPerTenant {
		srv.st.Unlock()
		writeJSON(w, 429, []byte(`{"error":"expose limit reached for this tenant"}`))
		return
	}
	srv.st.Unlock()

	nodePort, refused := srv.acquireNodePort(agentAddr, id, guestPort, reqID(r.Context()),
		func(sb *model.Sandbox, nodePort uint16) *nodePortRefusal {
			// Re-check the name in the recording critical section.
			for _, e := range sb.Exposes {
				if e.Name == name {
					if e.GuestPort != guestPort {
						return &nodePortRefusal{status: 409, body: `{"error":"name already exposed with a different port"}`}
					}
					return nil
				}
			}
			sb.Exposes = append(sb.Exposes, model.Expose{Name: name, GuestPort: guestPort, NodePort: nodePort})
			return nil
		})
	if refused != nil {
		refused.write(w)
		return
	}
	v := srv.exposeView(id, model.Expose{Name: name, GuestPort: guestPort, NodePort: nodePort})
	// Called with exposeMu held: the lock order is exposeMu -> saveMu -> st.
	// A failed save keeps the row in memory (the rule exists on the worker)
	// and is not undone there: another saved name on the same guest port may
	// depend on that rule.
	if err := srv.saveState(); err != nil {
		slog.Error("expose: applied on the worker but not saved", "sandbox", id, "expose", name, "request_id", reqID(r.Context()), "err", err)
		appliedNotSaved(w)
		return
	}
	b, _ := json.Marshal(v)
	if dynamic {
		writeJSON(w, 200, b)
	} else {
		writeJSON(w, 201, b)
	}
}

// reExposeFork re-creates a forked parent's ingress on the child (same names
// and guest ports, fresh node ports). The fork path is the THIRD writer of
// sb.Exposes, so it takes the same exposeMu and honours the same two caps as
// exposeCommon, and gets each port through acquireNodePort like it (the churn
// caps and the answer check, FLU-6). At a churn cap the loop stops: the fork
// still succeeds, with the exposes recorded so far.
//
// The per-sandbox cap is inherited (a parent can hold at most
// maxExposesPerSandbox), but the per-tenant total is NOT: without this check a
// tenant with a large or unset max_sandboxes forks a 16-expose parent over and
// over until the worker's shared 20000-29999 node-port pool is empty and every
// other tenant on that node 502s. Both caps are re-read per expose because the
// loop itself is what moves the totals.
//
// Best-effort per expose, matching the fork contract: a skipped or failed one
// degrades that single URL, not the fork.
func (srv *Server) reExposeFork(agentAddr, childID string, parentExposes []model.Expose, reqID string) {
	if len(parentExposes) == 0 {
		return
	}
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

	for _, e := range parentExposes {
		// Checked once exposeMu is held, and per expose: with the store
		// failing the fork's last save will be refused, and every port taken
		// here would only sit in quarantine against the tenant's churn cap
		// once that fork's clean-up removes the child.
		if srv.storeFailing() {
			slog.Warn("fork: re-expose stopped, the store is failing", "sandbox", childID, "request_id", reqID)
			return
		}
		srv.st.Lock()
		child := srv.st.FindSandbox(childID)
		if child == nil {
			// Deleted mid-fork; the agent-side rules die with the VM.
			srv.st.Unlock()
			return
		}
		if len(child.Exposes) >= maxExposesPerSandbox {
			srv.st.Unlock()
			slog.Warn("fork: re-expose skipped, sandbox expose cap reached",
				"sandbox", childID, "expose", e.Name, "request_id", reqID)
			return
		}
		// Keyed on the child's owner, like exposeCommon: admin is unmetered.
		if child.TenantID != adminTenant && srv.tenantExposeCount(child.TenantID) >= maxExposesPerTenant {
			tenantID := child.TenantID
			srv.st.Unlock()
			slog.Warn("fork: re-expose skipped, tenant expose cap reached",
				"sandbox", childID, "tenant", tenantID, "expose", e.Name, "request_id", reqID)
			// Every later expose would hit the same total.
			return
		}
		srv.st.Unlock()

		_, refused := srv.acquireNodePort(agentAddr, childID, e.GuestPort, reqID,
			func(c *model.Sandbox, nodePort uint16) *nodePortRefusal {
				c.Exposes = append(c.Exposes, model.Expose{Name: e.Name, GuestPort: e.GuestPort, NodePort: nodePort})
				return nil
			})
		switch {
		case refused == nil:
		case refused == refuseNotFound:
			// Deleted mid-fork; the agent-side rules die with the VM.
			return
		case refused.status == 429:
			// A churn cap: every later expose of the child would hit it too.
			slog.Warn("fork: re-expose skipped, expose churn limit reached",
				"sandbox", childID, "expose", e.Name, "retry_after", refused.retryAfter, "request_id", reqID)
			return
		default:
			slog.Warn("fork: re-expose failed", "sandbox", childID, "expose", e.Name, "status", refused.status, "error", refused.body, "request_id", reqID)
		}
	}
}

// unexposeSandbox handles DELETE /api/v1/sandboxes/{id}/expose/{name}.
// The agent rule is removed first: on agent failure the row stays (the call
// is retryable) rather than leaving an orphaned DNAT rule with no record.
func (srv *Server) unexposeSandbox(w http.ResponseWriter, r *http.Request, id, name, tenant string) {
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()
	// The gate again under exposeMu, as in exposeCommon: an unexpose that
	// waited here behind one whose save failed must not answer from memory.
	if srv.refusedWhileStoreFailing(w) {
		return
	}

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	var row model.Expose
	found := false
	for _, e := range sb.Exposes {
		if e.Name == name {
			row, found = e, true
			break
		}
	}
	guestPort := row.GuestPort
	if !found {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	var agentAddr string
	if sb.NodeID != nil {
		if node := srv.st.FindNode(*sb.NodeID); node != nil {
			agentAddr = node.Addr
		}
	}
	srv.st.Unlock()

	// Only skip the agent when there is no node to talk to (e.g. error state
	// after a node loss) — the row is all that's left then.
	if agentAddr != "" {
		// Another row on the same guest port (under another name, on any node
		// port) still needs the worker's mapping: the worker removes by guest
		// port, so the call is skipped (state.GuestPortShared).
		shared := false
		srv.st.Lock()
		if sb := srv.st.FindSandbox(id); sb != nil {
			shared = state.GuestPortShared(sb, row)
		}
		srv.st.Unlock()
		if !shared {
			// A 404 is not an error here: the worker does not know the VM, so
			// the mapping is gone and the row goes (agentUnexpose).
			if err := srv.agentUnexpose(agentAddr, id, guestPort, reqID(r.Context())); err != nil {
				slog.Error("unexpose agent failed", "sandbox", id, "expose", name, "err", err, "request_id", reqID(r.Context()))
				writeJSON(w, 502, []byte(`{"error":"agent unexpose failed"}`))
				return
			}
		}
	}

	// The row goes and, when it was the last on its guest port, its node port
	// is quarantined in the same critical section (state.RemoveExposeLocked).
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		srv.st.RemoveExposeLocked(id, name)
	}()
	if err := srv.saveState(); err != nil {
		slog.Error("unexpose: applied on the worker but not saved", "sandbox", id, "expose", name, "request_id", reqID(r.Context()), "err", err)
		appliedNotSaved(w)
		return
	}
	w.WriteHeader(204)
}

// routeView is one gateway routing entry: everything felucca-gw needs to take
// a Host header to a worker backend and render state-aware errors.
type routeView struct {
	Hostname          string `json:"hostname"`
	SandboxID         string `json:"sandbox_id"`
	TenantID          string `json:"tenant_id"`
	Name              string `json:"name"`
	NodeHost          string `json:"node_host"`
	NodePort          uint16 `json:"node_port"`
	GuestPort         uint16 `json:"guest_port"`
	State             string `json:"state"`
	AllowDynamicPorts bool   `json:"allow_dynamic_ports"`
}

// listRoutes handles GET /api/v1/routes (admin/gateway only — enforced by the
// caller's admin gate): the full route table, one row per expose.
func (srv *Server) listRoutes(w http.ResponseWriter) {
	srv.st.Lock()
	routes := make([]routeView, 0, 8)
	for _, sb := range srv.st.Sandboxes {
		if len(sb.Exposes) == 0 {
			continue
		}
		// A sandbox whose node is gone still gets rows (NodeHost "") so the
		// gateway can answer with its state instead of a generic 404.
		nodeHost := ""
		if sb.NodeID != nil {
			if node := srv.st.FindNode(*sb.NodeID); node != nil {
				nodeHost, _ = agentclient.SplitHostPort(node.Addr)
			}
		}
		for _, e := range sb.Exposes {
			routes = append(routes, routeView{
				Hostname:          e.Name + "--" + sb.ID,
				SandboxID:         sb.ID,
				TenantID:          sb.TenantID,
				Name:              e.Name,
				NodeHost:          nodeHost,
				NodePort:          e.NodePort,
				GuestPort:         e.GuestPort,
				State:             string(sb.State),
				AllowDynamicPorts: sb.AllowDynamicPorts,
			})
		}
	}
	srv.st.Unlock()
	b, _ := json.Marshal(struct {
		Routes []routeView `json:"routes"`
	}{routes})
	writeJSON(w, 200, b)
}

// ensureRoute handles POST /api/v1/routes/ensure {"sandbox_id","port"}
// (admin/gateway only): the dynamic port-in-hostname path. It lazily creates
// an expose named after the port — gated by the sandbox's opt-in.
func (srv *Server) ensureRoute(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxID string `json:"sandbox_id"`
		Port      uint16 `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SandboxID == "" || req.Port == 0 {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	srv.st.Lock()
	sb := srv.st.FindSandbox(req.SandboxID)
	if sb == nil {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	if !sb.AllowDynamicPorts {
		srv.st.Unlock()
		writeJSON(w, 403, []byte(`{"error":"dynamic ports disabled for this sandbox"}`))
		return
	}
	srv.st.Unlock()
	// Dynamic exposes live in the reserved all-digit name namespace.
	srv.exposeCommon(w, r, req.SandboxID, adminTenant, strconv.Itoa(int(req.Port)), req.Port, true)
}
