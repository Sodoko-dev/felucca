// Sandbox ingress (v4 P3): named multi-service expose API and the route
// table the gateway (cmd/hearth-gw) consumes. Each expose maps the hostname
// label "<name>--<sandbox-id>" to a worker node port that the agent DNATs to
// the guest service. Design: ADR-0007.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
	"github.com/alpham/infra-saas/hearth/internal/model"
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

// agentExpose asks the owning agent to DNAT a node port to guestPort and
// returns the allocated node port. The agent call is idempotent per
// (vm, guest_port).
func (srv *Server) agentExpose(agentAddr, sbID string, guestPort uint16, reqID string) (uint16, error) {
	body, _ := json.Marshal(struct {
		GuestPort uint16 `json:"guest_port"`
	}{guestPort})
	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := srv.agentCall(host, port, http.MethodPost, "/v1/vms/"+sbID+"/expose", body, reqID)
	if err != nil {
		return 0, fmt.Errorf("agent unreachable: %w", err)
	}
	if resp.Status != 200 {
		return 0, fmt.Errorf("agent expose status %d", resp.Status)
	}
	var ar struct {
		NodePort uint16 `json:"node_port"`
	}
	if json.Unmarshal(resp.Body, &ar) != nil || ar.NodePort == 0 {
		return 0, fmt.Errorf("agent expose: bad response")
	}
	return ar.NodePort, nil
}

func (srv *Server) agentUnexpose(agentAddr, sbID string, guestPort uint16, reqID string) error {
	body, _ := json.Marshal(struct {
		GuestPort uint16 `json:"guest_port"`
	}{guestPort})
	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := srv.agentCall(host, port, http.MethodDelete, "/v1/vms/"+sbID+"/expose", body, reqID)
	if err != nil {
		return fmt.Errorf("agent unreachable: %w", err)
	}
	if resp.Status != 200 {
		return fmt.Errorf("agent unexpose status %d", resp.Status)
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

// exposeCommon is the shared expose path for the named API and the dynamic
// ensure-route path (which uses the all-digit name namespace). dynamic only
// changes the success status code shape (200 route view vs 201 expose view).
func (srv *Server) exposeCommon(w http.ResponseWriter, r *http.Request, id, tenant, name string, guestPort uint16, dynamic bool) {
	// One ingress mutation at a time (see exposeMu); the state lock is still
	// taken per-section so reads elsewhere never wait on the agent call.
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

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
	srv.st.Unlock()

	nodePort, err := srv.agentExpose(agentAddr, id, guestPort, reqID(r.Context()))
	if err != nil {
		slog.Error("expose agent failed", "sandbox", id, "expose", name, "err", err, "request_id", reqID(r.Context()))
		writeJSON(w, 502, []byte(`{"error":"agent expose failed"}`))
		return
	}

	srv.st.Lock()
	sb = srv.st.FindSandbox(id)
	if sb == nil {
		// Deleted while we talked to the agent; the agent-side rule died with
		// the VM (or will at the next rebuild).
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	// Re-check the name under the re-acquired lock (concurrent expose).
	exists := false
	for _, e := range sb.Exposes {
		if e.Name == name {
			exists = e.GuestPort == guestPort
			if !exists {
				srv.st.Unlock()
				writeJSON(w, 409, []byte(`{"error":"name already exposed with a different port"}`))
				return
			}
		}
	}
	if !exists {
		sb.Exposes = append(sb.Exposes, model.Expose{Name: name, GuestPort: guestPort, NodePort: nodePort})
	}
	v := srv.exposeView(sb.ID, model.Expose{Name: name, GuestPort: guestPort, NodePort: nodePort})
	srv.st.Unlock()
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	b, _ := json.Marshal(v)
	if dynamic {
		writeJSON(w, 200, b)
	} else {
		writeJSON(w, 201, b)
	}
}

// unexposeSandbox handles DELETE /api/v1/sandboxes/{id}/expose/{name}.
// The agent rule is removed first: on agent failure the row stays (the call
// is retryable) rather than leaving an orphaned DNAT rule with no record.
func (srv *Server) unexposeSandbox(w http.ResponseWriter, r *http.Request, id, name, tenant string) {
	srv.exposeMu.Lock()
	defer srv.exposeMu.Unlock()

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	var guestPort uint16
	found := false
	for _, e := range sb.Exposes {
		if e.Name == name {
			guestPort, found = e.GuestPort, true
			break
		}
	}
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
		// A second expose row may share the guest port under another name; the
		// DNAT rule must survive in that case.
		shared := false
		srv.st.Lock()
		if sb := srv.st.FindSandbox(id); sb != nil {
			for _, e := range sb.Exposes {
				if e.Name != name && e.GuestPort == guestPort {
					shared = true
					break
				}
			}
		}
		srv.st.Unlock()
		if !shared {
			if err := srv.agentUnexpose(agentAddr, id, guestPort, reqID(r.Context())); err != nil {
				slog.Error("unexpose agent failed", "sandbox", id, "expose", name, "err", err, "request_id", reqID(r.Context()))
				writeJSON(w, 502, []byte(`{"error":"agent unexpose failed"}`))
				return
			}
		}
	}

	srv.st.Lock()
	if sb := srv.st.FindSandbox(id); sb != nil {
		kept := sb.Exposes[:0]
		for _, e := range sb.Exposes {
			if e.Name != name {
				kept = append(kept, e)
			}
		}
		sb.Exposes = kept
		if len(sb.Exposes) == 0 {
			sb.Exposes = nil
		}
	}
	srv.st.Unlock()
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	w.WriteHeader(204)
}

// routeView is one gateway routing entry: everything hearth-gw needs to take
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
