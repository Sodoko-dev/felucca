// Template + image endpoints (v4 P4): the template catalog, rootfs capture
// from stopped sandboxes, image serving for worker pull-and-cache, and the
// warm-pool spec push to agents.
//
// Mutating template routes and image downloads are admin/node-token only
// (handler-level guards — the catalog GET is tenant-visible so tenants can
// discover what they may create from). Image names are path components
// interpolated into images_dir; both sides validate them against the same
// conservative class before any filesystem use.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/agentclient"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// Shape caps for sandbox/template creation (v4 P4 "bigger guests"). These are
// sanity bounds, not capacity admission — the scheduler still picks by
// vm_count and FC fails on a host that genuinely can't back the shape.
const (
	maxVcpus  = 16
	minMemMiB = 64
	maxMemMiB = 32768
	maxDiskGB = 128
	// maxPoolSize bounds the per-node warm pool one template may request.
	maxPoolSize = 8
)

// validImageName accepts a single safe path component: [a-z0-9._-], 1..=64,
// no leading '.' or '-', no "..". Mirrors the agent-side validation — the
// name is interpolated into "<images dir>/<name>.ext4" on both sides.
func validImageName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	if name[0] == '.' || name[0] == '-' {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// validTemplateName reuses the expose-label grammar: template names double
// as captured image names and may surface in hostname-shaped contexts, so
// the same reserved forms apply ("--" is the label separator, all-digit
// names are the dynamic-port namespace). One grammar, one validator.
func validTemplateName(name string) bool {
	return validExposeName(name)
}

// imageSizeGB rounds a byte count up to whole GiB (the unit disk quotas and
// the agent's resize floor are denominated in).
func imageSizeGB(bytes int64) uint32 {
	const gib = int64(1) << 30
	if bytes <= 0 {
		return 0
	}
	return uint32((bytes + gib - 1) / gib)
}

func (srv *Server) imagePath(image string) string {
	return filepath.Join(srv.cfg.ImagesDir, image+".ext4")
}

// poolSpec is the wire shape pushed to agents (register response and
// PUT /v1/pools): one warm-pool entry per template with pool_size > 0.
type poolSpec struct {
	Image       string `json:"image"`
	ImageSHA256 string `json:"image_sha256"`
	Vcpus       uint32 `json:"vcpus"`
	MemMiB      uint64 `json:"mem_mib"`
	DiskGB      uint32 `json:"disk_gb"`
	Count       uint32 `json:"count"`
}

// poolSpecs builds the current warm-pool push payload from the catalog.
// A store error is returned, not folded into "no pools" — pushing an empty
// list on a transient DB error would tear down every pool in the fleet.
func (srv *Server) poolSpecs() ([]poolSpec, error) {
	ts, err := srv.db.ListTemplates()
	if err != nil {
		return nil, err
	}
	out := []poolSpec{}
	for _, t := range ts {
		if t.PoolSize == 0 {
			continue
		}
		out = append(out, poolSpec{
			Image:       t.Image,
			ImageSHA256: t.ImageSHA256,
			Vcpus:       t.Vcpus,
			MemMiB:      t.MemMiB,
			DiskGB:      t.DiskGB,
			Count:       t.PoolSize,
		})
	}
	return out, nil
}

// readyNodeAddrs snapshots the addresses of currently-ready nodes.
func (srv *Server) readyNodeAddrs() []string {
	now := time.Now().Unix()
	srv.st.Lock()
	defer srv.st.Unlock()
	var addrs []string
	for _, n := range srv.st.Nodes {
		if n.NodeStatus(now) == "ready" {
			addrs = append(addrs, n.Addr)
		}
	}
	return addrs
}

// pushPoolsTo sends the current pool specs to one node. An empty spec list
// is still a valid push — it's how pools are torn down.
func (srv *Server) pushPoolsTo(addr string) {
	specs, err := srv.poolSpecs()
	if err != nil {
		slog.Error("pool specs", "addr", addr, "err", err)
		return
	}
	body, _ := json.Marshal(specs)
	host, port := agentclient.SplitHostPort(addr)
	// Background pool pushes get their own traceable ID.
	bgID := newTraceID("bg")
	if resp, err := srv.agentCall(host, port, http.MethodPut, "/v1/pools", body, bgID); err != nil {
		slog.Warn("pool push failed", "addr", addr, "err", err)
	} else if resp.Status >= 300 {
		slog.Warn("pool push failed", "addr", addr, "status", resp.Status)
	}
}

// pushPools fans the current pool specs out to every ready node. Best-effort
// and called off the request path (goroutine): a missed node heals on its
// next register (hearthd pushes to a node right after it registers).
func (srv *Server) pushPools() {
	for _, addr := range srv.readyNodeAddrs() {
		srv.pushPoolsTo(addr)
	}
}

// prefetchImage asks every ready node to pull an image into its cache in the
// background, so the first create from a fresh template doesn't pay the
// download inside a create request. Best-effort by design.
func (srv *Server) prefetchImage(image, sha string) {
	body, _ := json.Marshal(struct {
		Image       string `json:"image"`
		ImageSHA256 string `json:"image_sha256"`
	}{image, sha})
	for _, addr := range srv.readyNodeAddrs() {
		host, port := agentclient.SplitHostPort(addr)
		// Background prefetch calls get their own traceable ID per node.
		bgID := newTraceID("bg")
		if resp, err := srv.agentCall(host, port, http.MethodPost, "/v1/images/prefetch", body, bgID); err != nil {
			slog.Warn("prefetch push failed", "addr", addr, "err", err)
		} else if resp.Status >= 300 {
			slog.Warn("prefetch push failed", "addr", addr, "status", resp.Status)
		}
	}
}

// ---- Template CRUD ----

func (srv *Server) createTemplate(w http.ResponseWriter, r *http.Request, tenant string) {
	// Admin-only; 404 mirrors the routing-level guards (don't advertise).
	if tenant != adminTenant {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	// A from_sandbox capture streams the whole rootfs before responding —
	// minutes, not seconds. Extend THIS connection's deadline past the
	// server's global WriteTimeout (60s slow-loris guard, kept for every
	// other route).
	extendDeadline(w)
	var req struct {
		Name        string  `json:"name"`
		Vcpus       *uint32 `json:"vcpus"`
		MemMiB      *uint64 `json:"mem_mib"`
		DiskGB      *uint32 `json:"disk_gb"`
		PoolSize    *uint32 `json:"pool_size"`
		FromSandbox string  `json:"from_sandbox"`
		ImageFile   string  `json:"image_file"`
		// Tenant scoping (v4 P5.4): "" = public catalog entry; a tenant id
		// restricts visibility and create-by-template to that tenant.
		Tenant string `json:"tenant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if !validTemplateName(req.Name) {
		writeJSON(w, 400, []byte(`{"error":"invalid template name (need [a-z0-9-], 1-32 chars, no edge dash)"}`))
		return
	}
	if (req.FromSandbox == "") == (req.ImageFile == "") {
		writeJSON(w, 400, []byte(`{"error":"exactly one of from_sandbox or image_file required"}`))
		return
	}
	if req.Tenant != "" {
		owner, err := srv.db.GetTenant(req.Tenant)
		if err != nil {
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		if owner == nil {
			writeJSON(w, 400, []byte(`{"error":"unknown tenant"}`))
			return
		}
	}

	t := &store.Template{
		Name:      req.Name,
		Vcpus:     1,
		MemMiB:    256,
		TenantID:  req.Tenant,
		CreatedAt: time.Now().Unix(),
	}
	if req.Vcpus != nil {
		t.Vcpus = *req.Vcpus
	}
	if req.MemMiB != nil {
		t.MemMiB = *req.MemMiB
	}
	if req.DiskGB != nil {
		t.DiskGB = *req.DiskGB
	}
	if req.PoolSize != nil {
		t.PoolSize = *req.PoolSize
	}
	if t.Vcpus < 1 || t.Vcpus > maxVcpus || t.MemMiB < minMemMiB || t.MemMiB > maxMemMiB ||
		t.DiskGB > maxDiskGB || t.PoolSize > maxPoolSize {
		writeJSON(w, 400, []byte(`{"error":"shape out of range"}`))
		return
	}

	if existing, err := srv.db.GetTemplateByName(t.Name); err != nil {
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	} else if existing != nil {
		writeJSON(w, 409, []byte(`{"error":"template exists"}`))
		return
	}

	if req.ImageFile != "" {
		// Register a pre-provisioned image already in images_dir.
		if !validImageName(req.ImageFile) {
			writeJSON(w, 400, []byte(`{"error":"invalid image_file"}`))
			return
		}
		sha, size, err := sha256AndSizeOfFile(srv.imagePath(req.ImageFile))
		if err != nil {
			writeJSON(w, 404, []byte(`{"error":"image file not found"}`))
			return
		}
		t.Image = req.ImageFile
		t.ImageSHA256 = sha
		t.ImageSizeGB = imageSizeGB(size)
	} else {
		// Capture the rootfs of a stopped sandbox as a new image named after
		// the template.
		sha, size, status, errMsg := srv.captureRootfs(req.FromSandbox, t.Name)
		if errMsg != "" {
			writeJSON(w, status, []byte(`{"error":`+jsonString(errMsg)+`}`))
			return
		}
		t.Image = t.Name
		t.ImageSHA256 = sha
		t.ImageSizeGB = imageSizeGB(size)
	}

	// DiskGB is the real per-sandbox footprint: it can never be below the
	// image's own size (the agent refuses to shrink, so such a template
	// would fail 100% of creates — and quota would under-count it).
	if t.DiskGB == 0 {
		t.DiskGB = t.ImageSizeGB
	} else if t.DiskGB < t.ImageSizeGB {
		writeJSON(w, 400, []byte(`{"error":"disk_gb smaller than the image"}`))
		return
	}

	t.ID = "tpl-" + newSecret(6)
	if err := srv.db.CreateTemplate(t); err != nil {
		// A captured image file without a row is harmless (re-capture
		// refuses; delete cleans) but don't leave it silently.
		slog.Error("create template", "template", t.Name, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}

	// Fan out off the request path: image prefetch (so first creates don't
	// pull inside a create request) and, when pooled, the new pool specs.
	image, sha, pooled := t.Image, t.ImageSHA256, t.PoolSize > 0
	go func() {
		srv.prefetchImage(image, sha)
		if pooled {
			srv.pushPools()
		}
	}()

	b, _ := json.Marshal(t)
	writeJSON(w, 201, b)
}

func (srv *Server) listTemplates(w http.ResponseWriter, tenant string) {
	ts, err := srv.db.ListTemplates()
	if err != nil {
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	var buf strings.Builder
	buf.WriteString(`{"templates":[`)
	first := true
	for _, t := range ts {
		// Tenant scoping (v4 P5.4): tenants see public entries plus their
		// own; the admin sees everything.
		if t.TenantID != "" && tenant != adminTenant && tenant != t.TenantID {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		b, _ := json.Marshal(t)
		buf.Write(b)
	}
	buf.WriteString(`]}`)
	writeJSON(w, 200, []byte(buf.String()))
}

func (srv *Server) deleteTemplate(w http.ResponseWriter, name, tenant string) {
	if tenant != adminTenant {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	t, err := srv.db.GetTemplateByName(name)
	if err != nil {
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if t == nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	if _, err := srv.db.DeleteTemplate(name); err != nil {
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	// Captured templates own their image file (Image == Name); registered
	// ones reference a shared pre-provisioned image which stays.
	if t.Image == t.Name {
		if err := os.Remove(srv.imagePath(t.Image)); err != nil && !os.IsNotExist(err) {
			slog.Warn("delete template image", "template", name, "err", err)
		}
	}
	go srv.pushPools()
	writeEmpty(w, 204)
}

// ---- Image serving (worker pull) ----

// serveImage streams an image to a worker. Routing guarantees admin context
// (workers authenticate with the shared node token, which authenticates as
// admin; tenant keys 404 at the routing layer).
func (srv *Server) serveImage(w http.ResponseWriter, r *http.Request, name string) {
	if !validImageName(name) {
		writeJSON(w, 400, []byte(`{"error":"invalid image name"}`))
		return
	}
	// Image bodies are gigabytes and workers may pull over the wg tunnel —
	// the global 60s WriteTimeout would kill the transfer mid-stream (the
	// agent would see a sha mismatch and re-pull forever).
	extendDeadline(w)
	f, err := os.Open(srv.imagePath(name))
	if err != nil {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, 500, []byte(`{"error":"stat failed"}`))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent sets Content-Length and handles ranges — the agent's
	// plain curl download depends on an exact length.
	http.ServeContent(w, r, name+".ext4", fi.ModTime(), f)
}

// ---- Rootfs capture ----

// captureRootfs streams a stopped sandbox's rootfs from its agent into
// images_dir/<image>.ext4, hashing in flight. Returns the sha256 hex and
// byte size, or an HTTP status + message describing why it can't.
func (srv *Server) captureRootfs(sandboxID, image string) (sha string, size int64, status int, errMsg string) {
	srv.st.Lock()
	sb := srv.st.FindSandbox(sandboxID)
	if sb == nil {
		srv.st.Unlock()
		return "", 0, 404, "sandbox not found"
	}
	if sb.State != model.StateStopped {
		srv.st.Unlock()
		return "", 0, 409, "sandbox must be stopped (clean filesystem) for capture"
	}
	if sb.NodeID == nil {
		srv.st.Unlock()
		return "", 0, 409, "sandbox has no node"
	}
	node := srv.st.FindNode(*sb.NodeID)
	if node == nil {
		srv.st.Unlock()
		return "", 0, 409, "node gone"
	}
	agentAddr := node.Addr
	srv.st.Unlock()

	dst := srv.imagePath(image)
	if _, err := os.Stat(dst); err == nil {
		return "", 0, 409, "image file already exists"
	}
	if err := os.MkdirAll(srv.cfg.ImagesDir, 0o755); err != nil {
		return "", 0, 500, "images dir: " + err.Error()
	}

	// O_EXCL on the fixed .partial path doubles as the per-image capture
	// mutex: a concurrent capture of the same name gets EEXIST instead of
	// interleaving writes (which would publish a sha that matches neither).
	// The lock has process-lifetime semantics but lives on disk — a crash
	// mid-stream would otherwise 409 the name forever, so a .partial older
	// than the maximum capture duration is treated as stale and reclaimed.
	tmp := dst + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		if fi, statErr := os.Stat(tmp); statErr == nil && time.Since(fi.ModTime()) > 30*time.Minute {
			os.Remove(tmp)
			f, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		}
	}
	if err != nil {
		if os.IsExist(err) {
			return "", 0, 409, "capture already in progress"
		}
		return "", 0, 500, "tmp file: " + err.Error()
	}

	host, port := agentclient.SplitHostPort(agentAddr)
	url := fmt.Sprintf("http://%s:%d/v1/vms/%s/rootfs", host, port, sandboxID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", 0, 500, "request: " + err.Error()
	}
	if srv.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+srv.cfg.Token)
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return "", 0, 502, "agent unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		f.Close()
		os.Remove(tmp)
		return "", 0, 502, fmt.Sprintf("agent rootfs fetch: status %d", resp.StatusCode)
	}

	h := sha256.New()
	n, copyErr := io.Copy(f, io.TeeReader(resp.Body, h))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(tmp)
		return "", 0, 502, "image transfer failed"
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", 0, 500, "rename: " + err.Error()
	}
	return hex.EncodeToString(h.Sum(nil)), n, 0, ""
}

// imageClient is the long-transfer HTTP client for rootfs capture: fail fast
// on dial (stale node addr) but allow a multi-GB streaming body. Package
// level so connections are pooled across captures (same pattern as the
// gateway's dial-bound transport).
var imageClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	},
	Timeout: 30 * time.Minute,
}

// ---- helpers ----

// extendDeadline lifts the server's global read/write timeouts for one
// long-transfer connection (rootfs capture, image serving — admin/node-only
// routes). Errors are ignored: a transport that doesn't support deadlines
// (tests' recorders) just keeps its defaults.
func extendDeadline(w http.ResponseWriter) {
	deadlineFor(w, 30*time.Minute)
}

// deadlineFor sets this connection's read/write deadlines `d` from now —
// used by tenant-reachable routes that must size the extension to the
// request instead of granting the full capture budget.
func deadlineFor(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(d)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

func sha256AndSizeOfFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
