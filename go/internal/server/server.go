// Package server implements the hearthd HTTP server, replicating main.zig
// handler behavior exactly: routing, auth, proxying, metrics, static UI.
package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
	"github.com/alpham/infra-saas/hearth/internal/wg"
)

// adminTenant is the tenant context value for the configured admin token
// (and for open mode when no token is configured): unrestricted access.
const adminTenant = ""

// Server holds the application state and configuration.
type Server struct {
	cfg *config.Config
	st  *state.State
	db  store.Store
	// WireGuard overlay (v4 P2): hearthd's public key, set at startup when
	// the overlay is configured; joinMu serializes overlay IP allocation.
	// addPeer is the live-kernel install hook — wg.AddPeer in production,
	// swappable in tests (the binary isn't present there).
	wgPubKey string
	joinMu   sync.Mutex
	addPeer  func(pubKey, overlayIP string) error
	// agentCall is the agent-HTTP hook for the ingress path (v4 P3) —
	// agentclient.Request in production, swappable in tests.
	agentCall func(host string, port uint16, method, path string, body []byte) (*agentclient.Response, error)
	// exposeMu serializes expose/unexpose mutations end-to-end (check +
	// agent call + row update). Without it, two racing exposes of one name
	// leak an unaccounted agent DNAT entry on the 409 path, and two racing
	// unexposes of names sharing a guest port can both skip the agent
	// removal. Ordering: exposeMu OUTER, st lock INNER.
	exposeMu sync.Mutex
}

// New creates a new Server.
func New(cfg *config.Config, st *state.State, db store.Store) *Server {
	srv := &Server{cfg: cfg, st: st, db: db, addPeer: wg.AddPeer}
	srv.agentCall = func(host string, port uint16, method, path string, body []byte) (*agentclient.Response, error) {
		return agentclient.Request(host, port, method, path, body, cfg.Token)
	}
	return srv
}

// SetWgPubKey records hearthd's WireGuard public key (returned to joining
// workers). Called once at startup, before the listener starts.
func (srv *Server) SetWgPubKey(pub string) { srv.wgPubKey = pub }

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
	tenantID, err := srv.db.LookupKeyByHash(hashSecret(key))
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

// usageEventFor builds a metering event from a sandbox's current shape. The
// caller must hold srv.st (it reads sb's fields); the returned value can then
// be appended after the lock is released.
func usageEventFor(sb *model.Sandbox, event string) store.UsageEvent {
	return store.UsageEvent{
		TenantID:  sb.TenantID,
		SandboxID: sb.ID,
		Event:     event,
		Vcpus:     sb.VCPUs,
		MemMiB:    sb.MemMiB,
		DiskGB:    sb.EffectiveDiskGB(),
		TS:        time.Now().Unix(),
	}
}

// recordUsage appends a metering event (best-effort; metering must never
// fail the request path). Used by the rare lifecycle transitions, which
// already hold the lock for other state writes; the per-request exec path
// builds the event with usageEventFor and appends AFTER unlocking instead, so
// a synchronous sqlite write never gates the global state lock.
func (srv *Server) recordUsage(sb *model.Sandbox, event string) {
	if sb == nil {
		return
	}
	if err := srv.db.AppendUsage(usageEventFor(sb, event)); err != nil {
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

	// Node join — authenticated by the one-time join token itself (the
	// joining worker has no API key yet), so it bypasses the bearer gate.
	if path == "/api/v1/nodes/join" && r.Method == http.MethodPost {
		srv.nodeJoin(w, r)
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
		// The one tenant-reachable corner of /api/v1/tenants: a tenant may
		// read ITS OWN usage (v4 P5.3). Anything else 404s below.
		if id, ok := matchSuffix(path, "/api/v1/tenants/", "/usage"); ok && method == http.MethodGet && id == tenant {
			srv.tenantUsage(w, r, id)
			return
		}
		switch {
		case path == "/api/v1/nodes",
			strings.HasPrefix(path, "/api/v1/agents/"),
			strings.HasPrefix(path, "/api/v1/tenants"),
			strings.HasPrefix(path, "/api/v1/keys/"),
			strings.HasPrefix(path, "/api/v1/join-tokens"),
			path == "/api/v1/routes",
			strings.HasPrefix(path, "/api/v1/routes/"),
			strings.HasPrefix(path, "/api/v1/images/"):
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

	case path == "/api/v1/join-tokens" && method == http.MethodPost:
		srv.createJoinToken(w, r)

	case path == "/api/v1/tenants" && method == http.MethodPost:
		srv.createTenant(w, r)

	case path == "/api/v1/tenants" && method == http.MethodGet:
		srv.listTenants(w)

	case path == "/api/v1/sandboxes" && method == http.MethodGet:
		srv.listSandboxes(w, tenant)

	case path == "/api/v1/sandboxes" && method == http.MethodPost:
		srv.createSandbox(w, r, tenant)

	case path == "/api/v1/routes" && method == http.MethodGet:
		srv.listRoutes(w)

	case path == "/api/v1/routes/ensure" && method == http.MethodPost:
		srv.ensureRoute(w, r)

	// Gateway ingress-activity reports (v4 P5.2; admin-gated above).
	case path == "/api/v1/routes/activity" && method == http.MethodPost:
		srv.routesActivity(w, r)

	// Template catalog: GET is tenant-visible (tenants must be able to
	// discover what they can create from); mutations are admin-only via
	// handler-level guards.
	case path == "/api/v1/templates" && method == http.MethodGet:
		srv.listTemplates(w, tenant)

	case path == "/api/v1/templates" && method == http.MethodPost:
		srv.createTemplate(w, r, tenant)

	default:
		// Path-segment routes with {id}.
		if id, ok := matchSuffix(path, "/api/v1/tenants/", "/keys"); ok && method == http.MethodPost {
			srv.createTenantKey(w, id)
			return
		}
		// Admin reach of the usage endpoint (tenant self-access is granted
		// before the admin gate above).
		if id, ok := matchSuffix(path, "/api/v1/tenants/", "/usage"); ok && method == http.MethodGet {
			srv.tenantUsage(w, r, id)
			return
		}
		if id, ok := matchExact(path, "/api/v1/keys/"); ok && method == http.MethodDelete {
			srv.revokeTenantKey(w, id)
			return
		}
		if name, ok := matchExact(path, "/api/v1/templates/"); ok && method == http.MethodDelete {
			srv.deleteTemplate(w, name, tenant)
			return
		}
		if name, ok := matchExact(path, "/api/v1/images/"); ok && method == http.MethodGet {
			srv.serveImage(w, r, name)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/exec"); ok && method == http.MethodPost {
			srv.execSandbox(w, r, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/expose"); ok && method == http.MethodPost {
			srv.exposeSandbox(w, r, id, tenant)
			return
		}
		// DELETE /api/v1/sandboxes/{id}/expose/{name}
		if rest, ok := strings.CutPrefix(path, "/api/v1/sandboxes/"); ok && method == http.MethodDelete {
			if id, name, ok2 := strings.Cut(rest, "/expose/"); ok2 &&
				id != "" && name != "" &&
				!strings.Contains(id, "/") && !strings.Contains(name, "/") {
				srv.unexposeSandbox(w, id, name, tenant)
				return
			}
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
	// Push the current warm-pool specs to the (re)registering node off the
	// response path (v4 P4). The response shape itself stays the frozen
	// {"id":...} — pools have exactly one delivery channel (PUT /v1/pools),
	// so empty-list teardown semantics are unambiguous.
	go srv.pushPoolsTo(req.Addr)
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
		Name              string  `json:"name"`
		Namespace         string  `json:"namespace"`
		VCPUs             *uint32 `json:"vcpus"`
		MemMiB            *uint64 `json:"mem_mib"`
		AllowDynamicPorts bool    `json:"allow_dynamic_ports"`
		Template          string  `json:"template"`
		DiskGB            *uint32 `json:"disk_gb"`
		IdleSleepS        int64   `json:"idle_sleep_s"`
		AsleepDeleteS     int64   `json:"asleep_delete_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if req.Name == "" {
		writeJSON(w, 400, []byte(`{"error":"name required"}`))
		return
	}
	// Lifecycle overrides (v4 P5.2): 0 inherit, -1 disabled, else bounded.
	if !validPolicy(req.IdleSleepS, minIdleSleepS) || !validPolicy(req.AsleepDeleteS, minAsleepDeleteS) {
		writeJSON(w, 400, []byte(`{"error":"bad lifecycle policy: 0, -1, or seconds within bounds"}`))
		return
	}
	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Template resolution (v4 P4): the template supplies the image and the
	// default shape; explicit request fields override the shape. diskFloor
	// is the image's size — the agent cannot shrink a rootfs, so a smaller
	// disk_gb could never boot (and would under-count quota).
	vcpus := uint32(1)
	memMiB := uint64(256)
	var diskGB uint32
	var image, imageSHA string
	diskFloor := uint32(model.BaseImageDiskGB)
	if req.Template != "" {
		tpl, err := srv.db.GetTemplateByName(req.Template)
		if err != nil {
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		if tpl == nil {
			writeJSON(w, 400, []byte(`{"error":"unknown template"}`))
			return
		}
		// Tenant-scoped templates (v4 P5.4): a foreign tenant gets the same
		// error as a missing template — no existence leak.
		if tpl.TenantID != "" && tenant != adminTenant && tenant != tpl.TenantID {
			writeJSON(w, 400, []byte(`{"error":"unknown template"}`))
			return
		}
		vcpus, memMiB, diskGB = tpl.Vcpus, tpl.MemMiB, tpl.DiskGB
		image, imageSHA = tpl.Image, tpl.ImageSHA256
		diskFloor = tpl.ImageSizeGB
	}
	if req.VCPUs != nil {
		vcpus = *req.VCPUs
	}
	if req.MemMiB != nil {
		memMiB = *req.MemMiB
	}
	// disk_gb: 0 (or absent) means "the template's default / the unresized
	// base image", never "override to zero".
	if req.DiskGB != nil && *req.DiskGB > 0 {
		if *req.DiskGB < diskFloor {
			writeJSON(w, 400, []byte(`{"error":"disk_gb smaller than the image"}`))
			return
		}
		diskGB = *req.DiskGB
	}
	if vcpus < 1 || vcpus > maxVcpus || memMiB < minMemMiB || memMiB > maxMemMiB || diskGB > maxDiskGB {
		writeJSON(w, 400, []byte(`{"error":"shape out of range"}`))
		return
	}

	now := time.Now().Unix()

	// Tenant quota gate (admin is unmetered).
	if tenant != adminTenant {
		if msg := srv.quotaExceeded(tenant, vcpus, memMiB, diskGB); msg != "" {
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
	sb.AllowDynamicPorts = req.AllowDynamicPorts
	sb.Template = req.Template
	sb.DiskGB = diskGB
	sb.IdleSleepS = req.IdleSleepS
	sb.AsleepDeleteS = req.AsleepDeleteS
	sb.LastActivity = now
	sbID = sb.ID
	srv.st.Unlock()

	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}

	// Build agent create request body (tenant_id feeds nft isolation on the
	// node; image/sha/disk are omitted for plain base-image sandboxes so the
	// pre-P4 wire bytes are unchanged).
	agentBody, _ := json.Marshal(struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		VCPUs       uint32 `json:"vcpus"`
		MemMiB      uint64 `json:"mem_mib"`
		TenantID    string `json:"tenant_id,omitempty"`
		Image       string `json:"image,omitempty"`
		ImageSHA256 string `json:"image_sha256,omitempty"`
		DiskGB      uint32 `json:"disk_gb,omitempty"`
	}{sbID, req.Name, vcpus, memMiB, tenant, image, imageSHA, diskGB})

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

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil {
		srv.st.Unlock()
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	sb.SleptAt = time.Now().Unix() // the auto-delete TTL clock (v4 P5.2)
	srv.recordUsage(sb, "slept")
	b, _ := json.Marshal(sb)
	srv.st.Unlock()
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}
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

	srv.st.Lock()
	sb := srv.st.FindSandbox(id)
	if sb == nil {
		srv.st.Unlock()
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	// A wake is activity: restart the idle clock, stop the TTL clock (v4 P5.2).
	sb.LastActivity = time.Now().Unix()
	sb.SleptAt = 0
	srv.recordUsage(sb, "woken")
	// Append "wake_ms" as the final key before the closing brace (Zig behavior).
	sbJSON, _ := json.Marshal(sb)
	srv.st.Unlock()
	// Persist AFTER releasing the lock (SaveSnapshot takes it) so the
	// snapshot carries the fresh activity clocks.
	if err := srv.persist(); err != nil {
		fmt.Fprintf(os.Stderr, "persist: %v\n", err)
	}
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
		vcpus, memMiB, diskGB := parent.VCPUs, parent.MemMiB, parent.DiskGB
		srv.st.Unlock()
		if msg := srv.quotaExceeded(childTenant, vcpus, memMiB, diskGB); msg != "" {
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
	// Snapshot ingress config to re-create on the child: same names/guest
	// ports, fresh node ports (the smoke contract — a fork's services are
	// reachable on the child's own URLs).
	parentExposes := append([]model.Expose(nil), parent.Exposes...)
	child := srv.st.CreateForkChild(childName, parent.Namespace, node.ID, parent.VCPUs, parent.MemMiB, parentID, now)
	child.TenantID = parent.TenantID
	child.AllowDynamicPorts = parent.AllowDynamicPorts
	// The child runs on a reflink of the parent's (possibly resized,
	// template-built) rootfs — inherit both for accounting.
	child.Template = parent.Template
	child.DiskGB = parent.DiskGB
	// Lifecycle overrides travel with the fork (v4 P5.2); the fork itself
	// starts the child's idle clock.
	child.IdleSleepS = parent.IdleSleepS
	child.AsleepDeleteS = parent.AsleepDeleteS
	child.LastActivity = now
	childID = child.ID
	// A fork reads the parent's live disk+memory: strong evidence the parent
	// is in use, so it counts as parent activity (v4 P5.2). For a RUNNING
	// parent this defers its idle auto-sleep; a SLEEPING fork-base parent's
	// auto-delete TTL is also pushed out, so a regularly-forked golden image
	// is never reaped out from under its children.
	parent.LastActivity = now
	if parent.State == model.StateSleeping {
		parent.SleptAt = now
	}
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

	// Re-expose the parent's services on the child (fresh node ports).
	// Best-effort: a failed re-expose degrades that one URL, not the fork —
	// the operator can retry via the expose API.
	for _, e := range parentExposes {
		nodePort, err := srv.agentExpose(agentAddr, childID, e.GuestPort)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fork %s: re-expose %s: %v\n", childID, e.Name, err)
			continue
		}
		srv.st.Lock()
		if c := srv.st.FindSandbox(childID); c != nil {
			c.Exposes = append(c.Exposes, model.Expose{Name: e.Name, GuestPort: e.GuestPort, NodePort: nodePort})
		}
		srv.st.Unlock()
	}
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

	// v4 P5.1: ?stream=1 selects the SSE relay arm — same body, same
	// validation/resolution, but the agent's NDJSON exec stream is forwarded
	// live instead of buffered.
	if r.URL.Query().Get("stream") == "1" {
		srv.execSandboxStream(w, r, id, tenant, cmdStrings, timeoutMs)
		return
	}

	// Exec's cap (300s) outlives the server's global 60s WriteTimeout —
	// without a per-connection extension, any guest command over ~60s gets
	// its connection killed mid-wait (latent since the P2.3 hardening,
	// surfaced by P4 template provisioning). Sized to THIS request's
	// timeout (body already parsed), not the capture-sized 30 minutes:
	// this route is tenant-reachable and must stay slow-loris-resistant.
	deadlineFor(w, time.Duration(timeoutMs)*time.Millisecond+60*time.Second)

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
	// v4 P5: an exec restarts the idle clock and leaves a metering event
	// (the usage endpoint counts execs per tenant per window). Build the
	// event under the lock but append it AFTER unlocking — exec is the hot
	// path and the global state lock must not gate a synchronous sqlite write.
	sb.LastActivity = time.Now().Unix()
	execEvent := usageEventFor(sb, "exec")
	srv.st.Unlock()
	if err := srv.db.AppendUsage(execEvent); err != nil {
		fmt.Fprintf(os.Stderr, "usage event: %v\n", err)
	}

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

// execSandboxStream is the ?stream=1 arm of execSandbox (v4 P5.1): identical
// validation/resolution to the buffered path, but the agent's NDJSON exec
// stream is relayed to the client as SSE events as the frames arrive.
// cmd/timeoutMs were already parsed and clamped by execSandbox.
func (srv *Server) execSandboxStream(w http.ResponseWriter, r *http.Request, id, tenant string, cmd []string, timeoutMs int64) {
	// Slow-loris bound sized to cover the whole stream: the agent's own
	// data deadline is timeout+60s, plus 30s margin for relay slack.
	deadlineFor(w, time.Duration(timeoutMs)*time.Millisecond+90*time.Second)

	// Count the attempt before proxying.
	srv.st.RecordExec()

	// Resolve sandbox and agent address (same flow as the buffered path).
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
	// v4 P5: an exec restarts the idle clock and leaves a metering event
	// (same as the buffered arm — append after unlocking, off the hot path).
	sb.LastActivity = time.Now().Unix()
	execEvent := usageEventFor(sb, "exec")
	srv.st.Unlock()
	if err := srv.db.AppendUsage(execEvent); err != nil {
		fmt.Fprintf(os.Stderr, "usage event: %v\n", err)
	}

	// Build forwarded body: the buffered shape plus "stream":true.
	agentBody, _ := json.Marshal(struct {
		Cmd       []string `json:"cmd"`
		TimeoutMs int64    `json:"timeout_ms"`
		Stream    bool     `json:"stream,omitempty"`
	}{cmd, timeoutMs, true})

	host, port := agentclient.SplitHostPort(agentAddr)
	reqTimeout := time.Duration(timeoutMs)*time.Millisecond + 70*time.Second
	resp, cancel, err := agentclient.ExecVMStream(host, port, id, agentBody, srv.cfg.Token, reqTimeout)
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent exec failed"}`))
		return
	}
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		// Drain a bounded slice of the error body so the connection can be
		// reused/closed cleanly, then map exactly like the buffered path.
		_, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == 501 {
			writeJSON(w, 501, []byte(`{"error":"guest agent unavailable"}`))
			return
		}
		writeJSON(w, 502, []byte(`{"error":"agent exec failed"}`))
		return
	}

	// 200: commit to SSE and relay each NDJSON line as one event, flushed
	// immediately so output appears as the guest produces it.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)

	sawDone := false
	sc := bufio.NewScanner(resp.Body)
	// Guest chunks are ≤8KiB raw, but JSON string escaping can inflate a
	// line well past that.
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		select {
		case <-r.Context().Done():
			// Client hung up — nothing left to relay to.
			return
		default:
		}
		line := sc.Bytes()
		// A frame is terminal iff it unmarshals with done:true (unmarshal
		// errors → not done).
		var frame struct {
			Done bool `json:"done"`
		}
		if json.Unmarshal(line, &frame) == nil && frame.Done {
			sawDone = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", line); err != nil {
			return
		}
		_ = rc.Flush()
	}
	if !sawDone {
		// Agent stream ended without a terminal frame (EOF or read error
		// mid-stream): tell the client explicitly instead of going silent.
		_, _ = w.Write([]byte("data: " + `{"done":true,"ok":false,"error":"stream interrupted"}` + "\n\n"))
		_ = rc.Flush()
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

	// NB (v4 P5): per-tenant gauges with the tenant_id as a label were
	// considered here but deliberately NOT emitted — /metrics is
	// unauthenticated (see handle(): "Metrics — open, no auth"), so labeling
	// by tenant would let any reachable client enumerate the full tenant
	// inventory and per-tenant resource footprint (an existence + sizing
	// leak that the rest of the API is careful to avoid). Per-tenant
	// observability is served by the authenticated GET /api/v1/tenants/{id}/usage
	// endpoint instead; an authenticated metrics surface is a P6 item.

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
