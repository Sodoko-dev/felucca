// Tests for the template + image endpoints (templates.go). Internal package
// so they can reach the validators; agent interactions run against an
// httptest server registered as the node address (the create/capture paths
// speak real HTTP, unlike the expose seam).
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// tplAgent fakes the worker HTTP API for the template flows: VM create/fork
// (JSON), rootfs capture (streaming), and pool pushes. It records the last
// create body and all pool-push bodies.
type tplAgent struct {
	mu         sync.Mutex
	lastCreate []byte
	poolPushes [][]byte
	rootfs     []byte
	rootfsCode int
}

func (a *tplAgent) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			a.lastCreate = body
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ip":"10.231.0.9"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/fork"):
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ip":"10.231.0.10"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/rootfs"):
			code := a.rootfsCode
			if code == 0 {
				code = 200
			}
			w.WriteHeader(code)
			_, _ = w.Write(a.rootfs)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/pools":
			body := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(body)
			a.poolPushes = append(a.poolPushes, body)
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}
}

// newTemplateTestServer returns a server with an images dir, one ready node
// backed by the fake agent, and the agentCall seam pointed at real HTTP (so
// pool pushes land on the httptest server too).
func newTemplateTestServer(t *testing.T) (*Server, *tplAgent, string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "hearth.db"),
		ImagesDir: filepath.Join(tmp, "images"),
	}
	if err := os.MkdirAll(cfg.ImagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(cfg, state.New(), db)

	fa := &tplAgent{}
	ts := httptest.NewServer(fa.handler())
	t.Cleanup(ts.Close)
	nodeID := srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, time.Now().Unix())
	return srv, fa, nodeID
}

func writeImage(t *testing.T, srv *Server, name string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(srv.cfg.ImagesDir, name+".ext4"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestImageAndTemplateNameValidation(t *testing.T) {
	okImages := []string{"ubuntu-base", "docker-base", "odoo-v18", "a.b-c_d", "x"}
	for _, n := range okImages {
		if !validImageName(n) {
			t.Errorf("validImageName(%q) = false, want true", n)
		}
	}
	badImages := []string{"", "..", ".hidden", "-x", "UPPER", "a/b", "a;b", "a..b", strings.Repeat("a", 65)}
	for _, n := range badImages {
		if validImageName(n) {
			t.Errorf("validImageName(%q) = true, want false", n)
		}
	}
	okTpl := []string{"base", "docker-base", "odoo-v18"}
	for _, n := range okTpl {
		if !validTemplateName(n) {
			t.Errorf("validTemplateName(%q) = false, want true", n)
		}
	}
	// Template names share the expose-label grammar: "--" (separator) and
	// all-digit names (dynamic-port namespace) are reserved.
	badTpl := []string{"", "-x", "x-", "a.b", "a_b", "UP", "a--b", "8069", strings.Repeat("a", 33)}
	for _, n := range badTpl {
		if validTemplateName(n) {
			t.Errorf("validTemplateName(%q) = true, want false", n)
		}
	}
	if imageSizeGB(0) != 0 || imageSizeGB(1) != 1 || imageSizeGB(1<<30) != 1 ||
		imageSizeGB(1<<30+1) != 2 || imageSizeGB(12<<30) != 12 {
		t.Error("imageSizeGB rounding wrong")
	}
}

func TestTemplateRegisterImageFile(t *testing.T) {
	srv, fa, _ := newTemplateTestServer(t)
	h := srv.Handler()
	wantSHA := writeImage(t, srv, "base", []byte("fake-base-rootfs"))

	w := doRequest(h, "POST", "/api/v1/templates",
		`{"name":"docker-base","image_file":"base","vcpus":2,"mem_mib":512,"disk_gb":4,"pool_size":1}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create template: got %d body %s", w.Code, w.Body.String())
	}
	var tpl store.Template
	if err := json.Unmarshal(w.Body.Bytes(), &tpl); err != nil {
		t.Fatal(err)
	}
	if tpl.Image != "base" || tpl.ImageSHA256 != wantSHA || tpl.Vcpus != 2 || tpl.DiskGB != 4 || tpl.ImageSizeGB != 1 {
		t.Errorf("template row: %+v (want sha %s)", tpl, wantSHA)
	}

	// Duplicate name conflicts.
	w2 := doRequest(h, "POST", "/api/v1/templates", `{"name":"docker-base","image_file":"base"}`, adminAuth)
	if w2.Code != 409 {
		t.Errorf("duplicate template: got %d", w2.Code)
	}
	// Missing image file is a 404.
	w3 := doRequest(h, "POST", "/api/v1/templates", `{"name":"ghost","image_file":"nope"}`, adminAuth)
	if w3.Code != 404 {
		t.Errorf("missing image file: got %d", w3.Code)
	}
	// Catalog lists it.
	w4 := doRequest(h, "GET", "/api/v1/templates", "", adminAuth)
	if w4.Code != 200 || !strings.Contains(w4.Body.String(), `"docker-base"`) {
		t.Errorf("list: %d %s", w4.Code, w4.Body.String())
	}
	// pool_size > 0 pushes specs to the ready node (async, off the request
	// path — poll briefly).
	pushed := false
	for i := 0; i < 40 && !pushed; i++ {
		fa.mu.Lock()
		pushed = len(fa.poolPushes) > 0
		fa.mu.Unlock()
		if !pushed {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !pushed {
		t.Error("expected a pool push after template create")
	}
}

func TestTemplateDiskDefaultsToImageSize(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("x"))

	// disk_gb omitted → image size (1 GiB for the tiny test file).
	w := doRequest(h, "POST", "/api/v1/templates", `{"name":"auto","image_file":"base"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var tpl store.Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	if tpl.DiskGB != 1 || tpl.ImageSizeGB != 1 {
		t.Errorf("auto disk: %+v", tpl)
	}

	// Non-template create with disk_gb below the base image is rejected
	// (the agent can't shrink; quota would under-count).
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"tiny","disk_gb":1}`, adminAuth); w.Code != 400 {
		t.Errorf("sub-base disk_gb: got %d, want 400", w.Code)
	}
}

func TestTemplateValidationAndCaps(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("x"))
	bad := []string{
		`{"name":"UP","image_file":"base"}`,                  // bad name
		`{"name":"a","image_file":"base","from_sandbox":"x"}`, // both sources
		`{"name":"a"}`,                                        // no source
		`{"name":"a","image_file":"base","vcpus":99}`,         // vcpus cap
		`{"name":"a","image_file":"base","mem_mib":99999}`,    // mem cap
		`{"name":"a","image_file":"base","disk_gb":1024}`,     // disk cap
		`{"name":"a","image_file":"base","pool_size":99}`,     // pool cap
		`{"name":"a","image_file":"../etc"}`,                  // path escape
	}
	for _, body := range bad {
		if w := doRequest(h, "POST", "/api/v1/templates", body, adminAuth); w.Code != 400 {
			t.Errorf("body %s: got %d, want 400", body, w.Code)
		}
	}
}

func TestTemplateTenantScoping(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("x"))
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"t1","image_file":"base"}`, adminAuth); w.Code != 201 {
		t.Fatalf("seed template: %d", w.Code)
	}

	// Mint a tenant key (the create response carries it).
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"acme"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body.String())
	}
	var tenant struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tenant)
	auth := "Bearer " + tenant.APIKey

	// Catalog is tenant-visible.
	if w := doRequest(h, "GET", "/api/v1/templates", "", auth); w.Code != 200 || !strings.Contains(w.Body.String(), `"t1"`) {
		t.Errorf("tenant list: %d %s", w.Code, w.Body.String())
	}
	// Mutations and image pulls are not.
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"t2","image_file":"base"}`, auth); w.Code != 404 {
		t.Errorf("tenant create template: got %d, want 404", w.Code)
	}
	if w := doRequest(h, "DELETE", "/api/v1/templates/t1", "", auth); w.Code != 404 {
		t.Errorf("tenant delete template: got %d, want 404", w.Code)
	}
	if w := doRequest(h, "GET", "/api/v1/images/base", "", auth); w.Code != 404 {
		t.Errorf("tenant image pull: got %d, want 404", w.Code)
	}
}

func TestCreateSandboxFromTemplate(t *testing.T) {
	srv, fa, _ := newTemplateTestServer(t)
	h := srv.Handler()
	sha := writeImage(t, srv, "base", []byte("fake-base-rootfs"))
	if w := doRequest(h, "POST", "/api/v1/templates",
		`{"name":"big","image_file":"base","vcpus":4,"mem_mib":2048,"disk_gb":8}`, adminAuth); w.Code != 201 {
		t.Fatalf("seed template: %d %s", w.Code, w.Body.String())
	}

	// Template supplies shape + image.
	w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"s1","template":"big"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create from template: %d %s", w.Code, w.Body.String())
	}
	var sb struct {
		VCPUs    uint32 `json:"vcpus"`
		MemMiB   uint64 `json:"mem_mib"`
		Template string `json:"template"`
		DiskGB   uint32 `json:"disk_gb"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.VCPUs != 4 || sb.MemMiB != 2048 || sb.Template != "big" || sb.DiskGB != 8 {
		t.Errorf("sandbox from template: %s", w.Body.String())
	}
	fa.mu.Lock()
	created := string(fa.lastCreate)
	fa.mu.Unlock()
	if !strings.Contains(created, `"image":"base"`) || !strings.Contains(created, `"image_sha256":"`+sha+`"`) ||
		!strings.Contains(created, `"disk_gb":8`) {
		t.Errorf("agent create body: %s", created)
	}

	// Explicit fields override the template's shape.
	w = doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"s2","template":"big","vcpus":2,"disk_gb":16}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("override create: %d %s", w.Code, w.Body.String())
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.VCPUs != 2 || sb.DiskGB != 16 {
		t.Errorf("override: %s", w.Body.String())
	}

	// Unknown template and out-of-range shapes are 400s.
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"s3","template":"nope"}`, adminAuth); w.Code != 400 {
		t.Errorf("unknown template: got %d", w.Code)
	}
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"s4","vcpus":99}`, adminAuth); w.Code != 400 {
		t.Errorf("vcpus cap: got %d", w.Code)
	}
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"s5","disk_gb":1024}`, adminAuth); w.Code != 400 {
		t.Errorf("disk cap: got %d", w.Code)
	}

	// A plain create still omits the P4 fields from the agent body.
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"plain"}`, adminAuth); w.Code != 201 {
		t.Fatalf("plain create: %d", w.Code)
	}
	fa.mu.Lock()
	created = string(fa.lastCreate)
	fa.mu.Unlock()
	if strings.Contains(created, "image") || strings.Contains(created, "disk_gb") {
		t.Errorf("plain create body should omit P4 fields: %s", created)
	}
}

func TestDiskQuota(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()

	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"acme","max_disk_gb":5}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body.String())
	}
	var tenant struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &tenant)
	auth := "Bearer " + tenant.APIKey

	// 4 GiB fits in the 5 GiB quota.
	if w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"d1","disk_gb":4}`, auth); w.Code != 201 {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	// A second sandbox counts the base image (2 GiB effective): 4+2 > 5.
	w = doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"d2"}`, auth)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "disk_gb") {
		t.Errorf("disk quota: got %d %s, want 429 disk_gb", w.Code, w.Body.String())
	}
}

func TestCaptureFromSandbox(t *testing.T) {
	srv, fa, nodeID := newTemplateTestServer(t)
	h := srv.Handler()
	content := []byte("captured-rootfs-bytes")
	fa.rootfs = content

	srv.st.Lock()
	sb := srv.st.CreateSandbox("builder", "default", nodeID, 1, 256, time.Now().Unix())
	id := sb.ID
	srv.st.Unlock()

	// Running sandboxes can't be captured (dirty filesystem).
	srv.st.SetSandboxState(id, model.StateRunning)
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap","from_sandbox":"`+id+`"}`, adminAuth); w.Code != 409 {
		t.Errorf("capture running: got %d, want 409", w.Code)
	}

	srv.st.SetSandboxState(id, model.StateStopped)
	w := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap","from_sandbox":"`+id+`"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("capture: %d %s", w.Code, w.Body.String())
	}
	var tpl store.Template
	_ = json.Unmarshal(w.Body.Bytes(), &tpl)
	sum := sha256.Sum256(content)
	if tpl.Image != "snap" || tpl.ImageSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("captured template: %+v", tpl)
	}
	got, err := os.ReadFile(filepath.Join(srv.cfg.ImagesDir, "snap.ext4"))
	if err != nil || string(got) != string(content) {
		t.Errorf("captured file: %v %q", err, got)
	}

	// Capture refusing to overwrite an existing image file.
	writeImage(t, srv, "snap2", []byte("pre-existing"))
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap2","from_sandbox":"`+id+`"}`, adminAuth); w.Code != 409 {
		t.Errorf("capture over existing file: got %d, want 409", w.Code)
	}

	// Unknown sandbox.
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap3","from_sandbox":"sb-nope-1"}`, adminAuth); w.Code != 404 {
		t.Errorf("capture unknown sandbox: got %d, want 404", w.Code)
	}
}

// createTemplate echoes captureRootfs's message into the response body, so a
// failed capture must not hand an admin token holder the host's filesystem
// layout.
func TestCaptureErrorsHideHostPaths(t *testing.T) {
	srv, fa, nodeID := newTemplateTestServer(t)
	h := srv.Handler()
	fa.rootfs = []byte("captured-rootfs-bytes")

	srv.st.Lock()
	sb := srv.st.CreateSandbox("builder", "default", nodeID, 1, 256, time.Now().Unix())
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id, model.StateStopped)

	// images_dir under a regular file: MkdirAll fails with an ENOTDIR whose
	// error string names the full host path.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.cfg.ImagesDir = filepath.Join(blocker, "images")

	w := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap","from_sandbox":"`+id+`"}`, adminAuth)
	if w.Code != 500 {
		t.Fatalf("capture into a broken images dir: got %d %s, want 500", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != `{"error":"capture failed"}` {
		t.Errorf("500 body: got %s, want the fixed message", body)
	} else if strings.Contains(body, blocker) {
		t.Errorf("500 body leaks a host path: %s", body)
	}

	// The descriptive non-500 messages stay: they name only the sandbox's own
	// state and callers depend on them.
	srv.st.SetSandboxState(id, model.StateRunning)
	w2 := doRequest(h, "POST", "/api/v1/templates", `{"name":"snap2","from_sandbox":"`+id+`"}`, adminAuth)
	if w2.Code != 409 || !strings.Contains(w2.Body.String(), "must be stopped") {
		t.Errorf("409 message: got %d %s", w2.Code, w2.Body.String())
	}
}

func TestDeleteTemplate(t *testing.T) {
	srv, fa, nodeID := newTemplateTestServer(t)
	h := srv.Handler()

	// Captured template owns its file → deleted with the row.
	fa.rootfs = []byte("cap")
	srv.st.Lock()
	sb := srv.st.CreateSandbox("builder", "default", nodeID, 1, 256, time.Now().Unix())
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id, model.StateStopped)
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"cap","from_sandbox":"`+id+`"}`, adminAuth); w.Code != 201 {
		t.Fatalf("capture: %d %s", w.Code, w.Body.String())
	}
	if w := doRequest(h, "DELETE", "/api/v1/templates/cap", "", adminAuth); w.Code != 204 {
		t.Fatalf("delete captured: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.ImagesDir, "cap.ext4")); !os.IsNotExist(err) {
		t.Error("captured image file should be removed with the template")
	}

	// Registered template references a shared image → file stays.
	writeImage(t, srv, "base", []byte("shared"))
	if w := doRequest(h, "POST", "/api/v1/templates", `{"name":"reg","image_file":"base"}`, adminAuth); w.Code != 201 {
		t.Fatalf("register: %d", w.Code)
	}
	if w := doRequest(h, "DELETE", "/api/v1/templates/reg", "", adminAuth); w.Code != 204 {
		t.Fatalf("delete registered: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.ImagesDir, "base.ext4")); err != nil {
		t.Error("shared image file should survive template deletion")
	}

	if w := doRequest(h, "DELETE", "/api/v1/templates/ghost", "", adminAuth); w.Code != 404 {
		t.Errorf("delete unknown: %d", w.Code)
	}
}

func TestServeImage(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("image-bytes"))

	w := doRequest(h, "GET", "/api/v1/images/base", "", adminAuth)
	if w.Code != 200 || w.Body.String() != "image-bytes" {
		t.Errorf("serve image: %d %q", w.Code, w.Body.String())
	}
	if cl := w.Header().Get("Content-Length"); cl != "11" {
		t.Errorf("content-length: %q", cl)
	}
	if w := doRequest(h, "GET", "/api/v1/images/nope", "", adminAuth); w.Code != 404 {
		t.Errorf("missing image: %d", w.Code)
	}
	if w := doRequest(h, "GET", "/api/v1/images/..", "", adminAuth); w.Code != 400 && w.Code != 404 {
		t.Errorf("traversal name: %d", w.Code)
	}
}

func TestRegisterTriggersPoolPush(t *testing.T) {
	srv, fa, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("x"))
	if w := doRequest(h, "POST", "/api/v1/templates",
		`{"name":"warm","image_file":"base","vcpus":2,"mem_mib":512,"pool_size":2}`, adminAuth); w.Code != 201 {
		t.Fatalf("seed template: %d", w.Code)
	}
	fa.mu.Lock()
	fa.poolPushes = nil
	fa.mu.Unlock()

	// The response shape stays the frozen {"id":...}; pools arrive via an
	// async PUT /v1/pools push to the registering node's address.
	srv.st.Lock()
	agentAddr := srv.st.Nodes[0].Addr
	srv.st.Unlock()
	w := doRequest(h, "POST", "/api/v1/agents/register",
		`{"hostname":"w2","addr":"`+agentAddr+`","cpus":4,"mem_total_mib":8192}`, adminAuth)
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "pools") {
		t.Errorf("register response must stay {\"id\":...}: %s", w.Body.String())
	}
	var got []poolSpec
	for i := 0; i < 40; i++ {
		fa.mu.Lock()
		if len(fa.poolPushes) > 0 {
			_ = json.Unmarshal(fa.poolPushes[len(fa.poolPushes)-1], &got)
		}
		fa.mu.Unlock()
		if len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(got) != 1 || got[0].Image != "base" || got[0].Count != 2 {
		t.Errorf("pushed pools: %+v", got)
	}
}

func TestForkInheritsTemplateAndDisk(t *testing.T) {
	srv, _, _ := newTemplateTestServer(t)
	h := srv.Handler()
	writeImage(t, srv, "base", []byte("x"))
	if w := doRequest(h, "POST", "/api/v1/templates",
		`{"name":"big","image_file":"base","disk_gb":8}`, adminAuth); w.Code != 201 {
		t.Fatalf("seed template: %d", w.Code)
	}
	w := doRequest(h, "POST", "/api/v1/sandboxes", `{"name":"p1","template":"big"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create parent: %d %s", w.Code, w.Body.String())
	}
	var parent struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &parent)

	w = doRequest(h, "POST", "/api/v1/sandboxes/"+parent.ID+"/fork", `{"name":"c1"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("fork: %d %s", w.Code, w.Body.String())
	}
	var child struct {
		Template string `json:"template"`
		DiskGB   uint32 `json:"disk_gb"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &child)
	if child.Template != "big" || child.DiskGB != 8 {
		t.Errorf("fork inheritance: %s", w.Body.String())
	}
}
