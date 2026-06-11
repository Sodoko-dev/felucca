// Package server implements the hearthd HTTP server, replicating main.zig
// handler behavior exactly: routing, auth, proxying, metrics, static UI.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// adminTenant is the tenant context value for the configured admin token
// (and for open mode when no token is configured): unrestricted access.
const adminTenant = ""

// Server holds the application state and configuration.
type Server struct {
	cfg *config.Config
	st  *state.State
	db  store.Store
}

// New creates a new Server.
func New(cfg *config.Config, st *state.State, db store.Store) *Server {
	return &Server{cfg: cfg, st: st, db: db}
}

// persist writes the in-memory working set through to the store.
func (srv *Server) persist() error {
	return srv.db.SaveSnapshot(srv.st)
}

// authenticate resolves the Authorization header to a tenant context.
// The configured admin token (or open mode) yields adminTenant; otherwise a
// non-revoked API key (sha256 match in the store) yields its tenant id.
func (srv *Server) authenticate(authHeader string) (string, bool) {
	if config.Authorized(srv.cfg.Token, authHeader) {
		return adminTenant, true
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return "", false
	}
	key := authHeader[len(prefix):]
	if !strings.HasPrefix(key, "hearth_sk_") {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	tenantID, err := srv.db.LookupKeyByHash(hex.EncodeToString(sum[:]))
	if err != nil {
		fmt.Fprintf(os.Stderr, "key lookup: %v\n", err)
		return "", false
	}
	if tenantID == "" {
		return "", false
	}
	return tenantID, true
}

// tenantOwns reports whether the tenant context may act on the sandbox.
// Admin owns everything; tenants own only their sandboxes. Callers translate
// false into 404 so cross-tenant existence is never leaked.
func tenantOwns(tenant string, sb *model.Sandbox) bool {
	return tenant == adminTenant || sb.TenantID == tenant
}

// recordUsage appends a metering event (best-effort; metering must never
// fail the request path).
func (srv *Server) recordUsage(sb *model.Sandbox, event string) {
	if sb == nil {
		return
	}
	e := store.UsageEvent{
		TenantID:  sb.TenantID,
		SandboxID: sb.ID,
		Event:     event,
		Vcpus:     sb.VCPUs,
		MemMiB:    sb.MemMiB,
		TS:        time.Now().Unix(),
	}
	if err := srv.db.AppendUsage(e); err != nil {
		fmt.Fprintf(os.Stderr, "usage event: %v\n", err)
	}
}

// Handler returns an http.Handler for use with net/http.
func (srv *Server) Handler() http.Handler {
	return http.HandlerFunc(srv.handle)
}

func (srv *Server) handle(w http.ResponseWriter, r *http.Request) {
	srv.st.BumpRequests()

	path := r.URL.Path

	// Health — open, no auth.
	if path == "/healthz" {
		writeJSON(w, 200, []byte(`{"ok":true}`))
		return
	}

	// Metrics — open, no auth.
	if path == "/metrics" {
		srv.serveMetrics(w)
		return
	}

	// API routes — bearer-guarded when token configured. The admin token (or
	// open mode) gets the admin context; tenant API keys get their tenant.
	if strings.HasPrefix(path, "/api/") {
		authHeader := r.Header.Get("Authorization")
		tenant, ok := srv.authenticate(authHeader)
		if !ok {
			writeJSON(w, 401, []byte(`{"error":"unauthorized"}`))
			return
		}
		srv.serveAPI(w, r, tenant)
		return
	}

	// Static UI fallback.
	srv.serveStatic(w, path)
}

// ---- Routing ----

func (srv *Server) serveAPI(w http.ResponseWriter, r *http.Request, tenant string) {
	path := r.URL.Path
	method := r.Method

	// Infrastructure and tenant-administration routes are admin-only. Tenant
	// keys get 404 (not 403) so the surface doesn't advertise what exists.
	if tenant != adminTenant {
		switch {
		case path == "/api/v1/nodes",
			strings.HasPrefix(path, "/api/v1/agents/"),
			strings.HasPrefix(path, "/api/v1/tenants"),
			strings.HasPrefix(path, "/api/v1/keys/"):
			writeJSON(w, 404, []byte(`{"error":"not found"}`))
			return
		}
	}

	switch {
	case path == "/api/v1/nodes" && method == http.MethodGet:
		srv.listNodes(w)

	case path == "/api/v1/agents/register" && method == http.MethodPost:
		srv.agentRegister(w, r)

	case path == "/api/v1/agents/heartbeat" && method == http.MethodPost:
		srv.agentHeartbeat(w, r)

	case path == "/api/v1/tenants" && method == http.MethodPost:
		srv.createTenant(w, r)

	case path == "/api/v1/tenants" && method == http.MethodGet:
		srv.listTenants(w)

	case path == "/api/v1/sandboxes" && method == http.MethodGet:
		srv.listSandboxes(w, tenant)

	case path == "/api/v1/sandboxes" && method == http.MethodPost:
		srv.createSandbox(w, r, tenant)

	default:
		// Path-segment routes with {id}.
		if id, ok := matchSuffix(path, "/api/v1/tenants/", "/keys"); ok && method == http.MethodPost {
			srv.createTenantKey(w, id)
			return
		}
		if id, ok := matchExact(path, "/api/v1/keys/"); ok && method == http.MethodDelete {
			srv.revokeTenantKey(w, id)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/exec"); ok && method == http.MethodPost {
			srv.execSandbox(w, r, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/fork"); ok && method == http.MethodPost {
			srv.forkSandbox(w, r, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/sleep"); ok && method == http.MethodPost {
			srv.sleepSandbox(w, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/wake"); ok && method == http.MethodPost {
			srv.wakeSandbox(w, id, tenant)
			return
		}
		for _, action := range []string{"stop", "start", "pause", "resume"} {
			if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/"+action); ok && method == http.MethodPost {
				srv.sandboxAction(w, id, action, tenant)
				return
			}
		}
		if id, ok := matchExact(path, "/api/v1/sandboxes/"); ok {
			switch method {
			case http.MethodGet:
				srv.getSandbox(w, id, tenant)
			case http.MethodDelete:
				srv.deleteSandbox(w, id, tenant)
			default:
				writeJSON(w, 404, []byte(`{"error":"not found"}`))
			}
			return
		}
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
	}
}

// matchSuffix matches "<prefix><id><suffix>" where id has no slashes.
func matchSuffix(path, prefix, suffix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	rest := path[len(prefix):]
	if !strings.HasSuffix(rest, suffix) {
		return "", false
	}
	id := rest[:len(rest)-len(suffix)]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// matchExact matches "<prefix><id>" with no further slashes.
func matchExact(path, prefix string) (string, bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	id := path[len(prefix):]
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// ---- Node endpoints ----

func (srv *Server) listNodes(w http.ResponseWriter) {
	now := time.Now().Unix()
	srv.st.Lock()
	defer srv.st.Unlock()

	var buf bytes.Buffer
	buf.WriteString(`{"nodes":[`)
	for i, n := range srv.st.Nodes {
		if i > 0 {
			buf.WriteByte(',')
		}
		b, _ := n.MarshalWithNow(now)
		buf.Write(b)
	}
	buf.WriteString(`]}`)
	writeJSON(w, 200, buf.Bytes())
}

func (srv *Server) agentRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname string `json:"hostname"`
		Addr     string `json:"addr"`
		CPUs     uint32 `json:"cpus"`
		MemTotal uint64 `json:"mem_total_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if req.Hostname == "" {
		writeJSON(w, 400, []byte(`{"error":"hostname required"}`))
		return
	}
	now := time.Now().Unix()
	id := srv.st.RegisterNode(req.Hostname, req.Addr, req.CPUs, req.MemTotal, now)
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}
	idJSON, _ := json.Marshal(id)
	writeJSON(w, 200, []byte(`{"id":`+string(idJSON)+`}`))
}

func (srv *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string `json:"id"`
		MemFree  uint64 `json:"mem_free_mib"`
		VMCount  uint32 `json:"vm_count"`
		PoolSize uint32 `json:"pool_size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if req.ID == "" {
		writeJSON(w, 400, []byte(`{"error":"id required"}`))
		return
	}
	now := time.Now().Unix()
	if !srv.st.Heartbeat(req.ID, req.MemFree, req.VMCount, req.PoolSize, now) {
		writeJSON(w, 404, []byte(`{"error":"unknown node"}`))
		return
	}
	writeEmpty(w, 200)
}

// ---- Sandbox endpoints ----

func (srv *Server) listSandboxes(w http.ResponseWriter, tenant string) {
	srv.st.Lock()
	defer srv.st.Unlock()

	var buf bytes.Buffer
	buf.WriteString(`{"sandboxes":[`)
	first := true
	for _, sb := range srv.st.Sandboxes {
		if !tenantOwns(tenant, sb) {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		b, _ := json.Marshal(sb)
		buf.Write(b)
	}
	buf.WriteString(`]}`)
	writeJSON(w, 200, buf.Bytes())
}

func (srv *Server) getSandbox(w http.ResponseWriter, id, tenant string) {
	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	b, _ := json.Marshal(sb)
	writeJSON(w, 200, b)
}

func (srv *Server) createSandbox(w http.ResponseWriter, r *http.Request, tenant string) {
	var req struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		VCPUs     *uint32 `json:"vcpus"`
		MemMiB    *uint64 `json:"mem_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if req.Name == "" {
		writeJSON(w, 400, []byte(`{"error":"name required"}`))
		return
	}
	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}
	vcpus := uint32(1)
	if req.VCPUs != nil {
		vcpus = *req.VCPUs
	}
	memMiB := uint64(256)
	if req.MemMiB != nil {
		memMiB = *req.MemMiB
	}

	now := time.Now().Unix()

	// Tenant quota gate (admin is unmetered).
	if tenant != adminTenant {
		if msg := srv.quotaExceeded(tenant, vcpus, memMiB); msg != "" {
			writeJSON(w, 429, []byte(`{"error":"quota exceeded: `+msg+`"}`))
			return
		}
	}

	// Schedule: pick ready node with lowest vm_count. Create sandbox record in
	// "creating" state under the lock, capturing agent address.
	var agentAddr, sbID string
	srv.st.Lock()
	node := srv.st.PickNode(now)
	if node == nil {
		srv.st.Unlock()
		writeJSON(w, 503, []byte(`{"error":"no ready node"}`))
		return
	}
	agentAddr = node.Addr
	sb := srv.st.CreateSandbox(req.Name, namespace, node.ID, vcpus, memMiB, now)
	sb.TenantID = tenant
	sbID = sb.ID
	srv.st.Unlock()

	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	// Build agent create request body (tenant_id feeds nft isolation on the node).
	agentBody, _ := json.Marshal(struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		VCPUs    uint32 `json:"vcpus"`
		MemMiB   uint64 `json:"mem_mib"`
		TenantID string `json:"tenant_id,omitempty"`
	}{sbID, req.Name, vcpus, memMiB, tenant})

	host, port := agentclient.SplitHostPort(agentAddr)
	resp, err := agentclient.Request(host, port, http.MethodPost, "/v1/vms", agentBody, srv.cfg.Token)
	if err != nil {
		srv.st.SetSandboxState(sbID, model.StateError)
		if perr := srv.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "persist: %v\n", perr)
		}
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		srv.st.SetSandboxState(sbID, model.StateError)
		if perr := srv.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "persist: %v\n", perr)
		}
		writeJSON(w, 502, []byte(`{"error":"agent create failed"}`))
		return
	}

	// Capture IP from agent response.
	var agentResp struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(resp.Body, &agentResp) == nil && agentResp.IP != "" {
		srv.st.SetSandboxIP(sbID, agentResp.IP)
	}
	srv.st.SetSandboxState(sbID, model.StateRunning)
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	srv.st.Lock()
	defer srv.st.Unlock()
	sb = srv.st.FindSandbox(sbID)
	if sb == nil {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	srv.recordUsage(sb, "created")
	b, _ := json.Marshal(sb)
	writeJSON(w, 201, b)
}

func (srv *Server) sandboxAction(w http.ResponseWriter, id, action, tenant string) {
	var agentAddr string
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	if sb.NodeID == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"sandbox has no node"}`))
		return
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"node gone"}`))
		return
	}
	agentAddr = node.Addr
	srv.st.Unlock()

	host, port := agentclient.SplitHostPort(agentAddr)
	agentPath := fmt.Sprintf("/v1/vms/%s/%s", id, action)
	resp, err := agentclient.Request(host, port, http.MethodPost, agentPath, nil, srv.cfg.Token)
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status == 409 {
		// Lifecycle conflict from the agent (e.g. start on a paused VM):
		// forward status and body so the caller learns why.
		writeJSON(w, 409, resp.Body)
		return
	}
	if resp.Status >= 300 {
		writeJSON(w, 502, []byte(`{"error":"agent action failed"}`))
		return
	}

	var newState model.SandboxState
	usageEvent := map[string]string{
		"stop": "stopped", "start": "started", "pause": "paused", "resume": "resumed",
	}[action]
	switch action {
	case "stop":
		newState = model.StateStopped
	case "start":
		newState = model.StateRunning
	case "pause":
		newState = model.StatePaused
	case "resume":
		newState = model.StateRunning
	}
	if newState != "" {
		srv.st.SetSandboxState(id, newState)
		if err := srv.persist(); err != nil {
			fmt.Fprintf(os.Stderr, "persist: %v\n", err)
		}
		srv.st.Lock()
		srv.recordUsage(srv.st.FindSandbox(id), usageEvent)
		srv.st.Unlock()
	}
	writeEmpty(w, 200)
}

func (srv *Server) deleteSandbox(w http.ResponseWriter, id, tenant string) {
	var agentAddr string
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		// Unknown and foreign ids are indistinguishable: 404, per the v2
		// contract (and no cross-tenant existence leak).
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	// Capture the usage shape before removal.
	usage := *sb
	if sb.NodeID != nil {
		node := srv.st.FindNode(*sb.NodeID)
		if node != nil {
			agentAddr = node.Addr
		}
	}
	srv.st.Unlock()

	if agentAddr != "" {
		host, port := agentclient.SplitHostPort(agentAddr)
		agentPath := fmt.Sprintf("/v1/vms/%s", id)
		// Best-effort: ignore errors per Zig behavior.
		_, _ = agentclient.Request(host, port, http.MethodDelete, agentPath, nil, srv.cfg.Token)
	}

	srv.st.RemoveSandbox(id)
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}
	srv.recordUsage(&usage, "deleted")
	writeEmpty(w, 204)
}

func (srv *Server) sleepSandbox(w http.ResponseWriter, id, tenant string) {
	agentAddr, ok := srv.resolveAgent(w, id, tenant)
	if !ok {
		return
	}
	host, port := agentclient.SplitHostPort(agentAddr)
	agentPath := fmt.Sprintf("/v1/vms/%s/sleep", id)
	resp, err := agentclient.Request(host, port, http.MethodPost, agentPath, nil, srv.cfg.Token)
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		writeJSON(w, 502, []byte(`{"error":"agent sleep failed"}`))
		return
	}

	srv.st.SetSandboxState(id, model.StateSleeping)
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(id)
	if sb == nil {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	srv.recordUsage(sb, "slept")
	b, _ := json.Marshal(sb)
	writeJSON(w, 200, b)
}

func (srv *Server) wakeSandbox(w http.ResponseWriter, id, tenant string) {
	agentAddr, ok := srv.resolveAgent(w, id, tenant)
	if !ok {
		return
	}
	host, port := agentclient.SplitHostPort(agentAddr)
	agentPath := fmt.Sprintf("/v1/vms/%s/wake", id)
	resp, err := agentclient.Request(host, port, http.MethodPost, agentPath, nil, srv.cfg.Token)
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		writeJSON(w, 502, []byte(`{"error":"agent wake failed"}`))
		return
	}

	// Extract wake_ms from agent response.
	var wakeMsRaw struct {
		WakeMs int64 `json:"wake_ms"`
	}
	var wakeMs uint64
	if json.Unmarshal(resp.Body, &wakeMsRaw) == nil && wakeMsRaw.WakeMs > 0 {
		wakeMs = uint64(wakeMsRaw.WakeMs)
	}

	srv.st.SetSandboxState(id, model.StateRunning)
	srv.st.RecordWake(wakeMs)
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(id)
	if sb == nil {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	srv.recordUsage(sb, "woken")
	// Append "wake_ms" as the final key before the closing brace (Zig behavior).
	sbJSON, _ := json.Marshal(sb)
	// Drop trailing '}'
	body := make([]byte, len(sbJSON)-1, len(sbJSON)+32)
	copy(body, sbJSON[:len(sbJSON)-1])
	body = append(body, fmt.Sprintf(`,"wake_ms":%d}`, wakeMs)...)
	writeJSON(w, 200, body)
}

func (srv *Server) forkSandbox(w http.ResponseWriter, r *http.Request, parentID, tenant string) {
	// Parse optional "name" from request body.
	childName := "fork"
	var reqBody struct {
		Name string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&reqBody) == nil && reqBody.Name != "" {
		childName = reqBody.Name
	}

	now := time.Now().Unix()

	var agentAddr, childID string
	srv.st.Lock()
	parent := srv.st.FindSandbox(parentID)
	if parent == nil || !tenantOwns(tenant, parent) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	// The child counts against the owning tenant's quota (admin parents are
	// unmetered). Checked before any state is created.
	childTenant := parent.TenantID
	if childTenant != adminTenant {
		vcpus, memMiB := parent.VCPUs, parent.MemMiB
		srv.st.Unlock()
		if msg := srv.quotaExceeded(childTenant, vcpus, memMiB); msg != "" {
			writeJSON(w, 429, []byte(`{"error":"quota exceeded: `+msg+`"}`))
			return
		}
		srv.st.Lock()
		// Re-validate under the re-acquired lock.
		parent = srv.st.FindSandbox(parentID)
		if parent == nil || !tenantOwns(tenant, parent) {
			srv.st.Unlock()
			writeJSON(w, 404, []byte(`{"error":"not found"}`))
			return
		}
	}
	switch parent.State {
	case model.StateRunning, model.StatePaused, model.StateSleeping:
		// ok
	default:
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"parent must be running, paused, or sleeping"}`))
		return
	}
	if parent.NodeID == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"parent has no node"}`))
		return
	}
	node := srv.st.FindNode(*parent.NodeID)
	if node == nil {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"node gone"}`))
		return
	}
	agentAddr = node.Addr
	child := srv.st.CreateForkChild(childName, parent.Namespace, node.ID, parent.VCPUs, parent.MemMiB, parentID, now)
	child.TenantID = parent.TenantID
	childID = child.ID
	srv.st.Unlock()

	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	host, port := agentclient.SplitHostPort(agentAddr)
	agentBody, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{childID, childName})
	agentPath := fmt.Sprintf("/v1/vms/%s/fork", parentID)
	resp, err := agentclient.Request(host, port, http.MethodPost, agentPath, agentBody, srv.cfg.Token)
	if err != nil {
		srv.st.SetSandboxState(childID, model.StateError)
		if perr := srv.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "persist: %v\n", perr)
		}
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		srv.st.SetSandboxState(childID, model.StateError)
		if perr := srv.persist(); perr != nil {
			fmt.Fprintf(os.Stderr, "persist: %v\n", perr)
		}
		writeJSON(w, 502, []byte(`{"error":"agent fork failed"}`))
		return
	}

	var agentResp struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(resp.Body, &agentResp) == nil && agentResp.IP != "" {
		srv.st.SetSandboxIP(childID, agentResp.IP)
	}
	srv.st.SetSandboxState(childID, model.StateRunning)
	srv.st.RecordFork()
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(childID)
	if sb == nil {
		writeJSON(w, 500, []byte(`{"error":"lost child"}`))
		return
	}
	srv.recordUsage(sb, "forked")
	b, _ := json.Marshal(sb)
	writeJSON(w, 201, b)
}

func (srv *Server) execSandbox(w http.ResponseWriter, r *http.Request, id, tenant string) {
	// Parse request body.
	var req struct {
		Cmd       []interface{} `json:"cmd"`
		TimeoutMs *int64        `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	// Validate cmd: must be non-empty array of strings.
	if len(req.Cmd) == 0 {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	cmdStrings := make([]string, len(req.Cmd))
	for i, v := range req.Cmd {
		s, ok := v.(string)
		if !ok {
			writeJSON(w, 400, []byte(`{"error":"bad json"}`))
			return
		}
		cmdStrings[i] = s
	}

	// Determine timeout_ms: default 30000, cap 300000.
	timeoutMs := int64(30000)
	if req.TimeoutMs != nil {
		timeoutMs = *req.TimeoutMs
		if timeoutMs > 300000 {
			timeoutMs = 300000
		}
	}

	// Count the attempt before proxying.
	srv.st.RecordExec()

	// Resolve sandbox and agent address.
	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	if sb.State != model.StateRunning {
		srv.st.Unlock()
		writeJSON(w, 409, []byte(`{"error":"not running"}`))
		return
	}
	if sb.NodeID == nil {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		srv.st.Unlock()
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	agentAddr := node.Addr
	srv.st.Unlock()

	// Build forwarded body.
	agentBody, _ := json.Marshal(struct {
		Cmd       []string `json:"cmd"`
		TimeoutMs int64    `json:"timeout_ms"`
	}{cmdStrings, timeoutMs})

	host, port := agentclient.SplitHostPort(agentAddr)
	reqTimeout := time.Duration(timeoutMs)*time.Millisecond + 10*time.Second
	resp, err := agentclient.ExecVM(host, port, id, agentBody, srv.cfg.Token, reqTimeout)
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent exec failed"}`))
		return
	}
	switch resp.Status {
	case 200:
		writeJSON(w, 200, resp.Body)
	case 501:
		writeJSON(w, 501, []byte(`{"error":"guest agent unavailable"}`))
	default:
		writeJSON(w, 502, []byte(`{"error":"agent exec failed"}`))
	}
}

// resolveAgent returns the agent address for a sandbox's node, writing a 404
// if the sandbox or its agent is not found — or if the tenant context does
// not own the sandbox (no cross-tenant existence leak).
func (srv *Server) resolveAgent(w http.ResponseWriter, id, tenant string) (string, bool) {
	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(id)
	if sb == nil || !tenantOwns(tenant, sb) {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return "", false
	}
	if sb.NodeID == nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return "", false
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return "", false
	}
	return node.Addr, true
}

// ---- Metrics ----

func (srv *Server) serveMetrics(w http.ResponseWriter) {
	now := time.Now().Unix()
	srv.st.Lock()
	defer srv.st.Unlock()

	var readyCount int
	for _, n := range srv.st.Nodes {
		if n.NodeStatus(now) == "ready" {
			readyCount++
		}
	}

	// Count sandboxes by state in enum declaration order.
	counts := make(map[model.SandboxState]int)
	for _, sb := range srv.st.Sandboxes {
		counts[sb.State]++
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# HELP hearth_nodes_ready Number of ready nodes\n# TYPE hearth_nodes_ready gauge\nhearth_nodes_ready %d\n", readyCount)
	buf.WriteString("# HELP hearth_sandboxes_total Sandboxes by state\n# TYPE hearth_sandboxes_total gauge\n")
	for _, st := range model.AllStates {
		fmt.Fprintf(&buf, "hearth_sandboxes_total{state=%q} %d\n", string(st), counts[st])
	}
	fmt.Fprintf(&buf, "# HELP hearth_api_requests_total Total API requests\n# TYPE hearth_api_requests_total counter\nhearth_api_requests_total %d\n", srv.st.RequestCount)
	fmt.Fprintf(&buf, "# HELP hearth_wake_ms_last Last wake latency in ms\n# TYPE hearth_wake_ms_last gauge\nhearth_wake_ms_last %d\n", srv.st.WakeMsLast)
	fmt.Fprintf(&buf, "# HELP hearth_wake_total Total wakes\n# TYPE hearth_wake_total counter\nhearth_wake_total %d\n", srv.st.WakeTotal)
	fmt.Fprintf(&buf, "# HELP hearth_wake_ms_sum Sum of wake latencies (ms)\n# TYPE hearth_wake_ms_sum counter\nhearth_wake_ms_sum %d\n", srv.st.WakeMsSum)
	fmt.Fprintf(&buf, "# HELP hearth_forks_total Total forks\n# TYPE hearth_forks_total counter\nhearth_forks_total %d\n", srv.st.ForksTotal)
	fmt.Fprintf(&buf, "# HELP hearth_execs_total Total exec attempts\n# TYPE hearth_execs_total counter\nhearth_execs_total %d\n", srv.st.ExecsTotal)
	buf.WriteString("# HELP hearth_pool_size Warm-pool depth per node\n# TYPE hearth_pool_size gauge\n")
	for _, n := range srv.st.Nodes {
		fmt.Fprintf(&buf, "hearth_pool_size{node=%q} %d\n", n.Hostname, n.PoolSize)
	}

	body := buf.Bytes()
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	w.Write(body) //nolint:errcheck
}

// ---- Static file serving ----

func (srv *Server) serveStatic(w http.ResponseWriter, path string) {
	rel := path
	if rel == "" || rel == "/" {
		rel = "/index.html"
	}
	// Strip leading slash.
	if len(rel) > 0 && rel[0] == '/' {
		rel = rel[1:]
	}
	// Reject traversal.
	if strings.Contains(rel, "..") {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}

	full := filepath.Join(srv.cfg.UIDir, rel)
	data, err := os.ReadFile(full)
	if err != nil {
		// SPA fallback to index.html.
		indexFull := filepath.Join(srv.cfg.UIDir, "index.html")
		index, err2 := os.ReadFile(indexFull)
		if err2 != nil {
			writeJSON(w, 404, []byte(`{"error":"ui not found"}`))
			return
		}
		writeResponse(w, 200, "text/html", index)
		return
	}
	writeResponse(w, 200, contentType(rel), data)
}

func contentType(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html"
	case strings.HasSuffix(path, ".css"):
		return "text/css"
	case strings.HasSuffix(path, ".js"):
		return "application/javascript"
	case strings.HasSuffix(path, ".json"):
		return "application/json"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// ---- Wire helpers ----

// writeJSON writes a JSON response with explicit Content-Length (never chunked).
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}

// writeResponse writes a response with explicit Content-Length.
func writeResponse(w http.ResponseWriter, status int, ct string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
}

// writeEmpty writes an empty body response (200 or 204).
func writeEmpty(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Set("Content-Length", "0")
	w.WriteHeader(status)
}
