// Tests for the tenant administration surface and the quota gate
// (tenants.go). Internal package (like expose_test.go) so the accounting
// helper's locking contract — the thing that lets check-and-insert be one
// critical section — can be asserted directly, not only through the handler.
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

// barrierStore holds every quota-row read until `want` of them have arrived,
// then releases them together. Without it the store's single sqlite
// connection staggers the callers enough that they never reach the gate at
// the same time — which is exactly the shape the audit's 50-concurrent-create
// exploit produces over the network.
type barrierStore struct {
	store.Store
	mu   sync.Mutex
	want int
	seen int
	gate chan struct{}
}

func (b *barrierStore) arm(n int) {
	b.mu.Lock()
	b.want, b.seen, b.gate = n, 0, make(chan struct{})
	b.mu.Unlock()
}

func (b *barrierStore) GetTenant(id string) (*store.Tenant, error) {
	t, err := b.Store.GetTenant(id)
	b.mu.Lock()
	gate := b.gate
	if gate != nil {
		b.seen++
		if b.seen == b.want {
			close(gate)
		}
	}
	b.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-time.After(2 * time.Second): // never hang the suite
		}
	}
	return t, err
}

// newQuotaServer wires a server to a fake agent that accepts every VM create
// and fork, registered as its one ready node.
func newQuotaServer(t *testing.T) (*Server, *barrierStore) {
	t.Helper()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ip":"10.0.0.9"}`)
	}))
	t.Cleanup(agent.Close)

	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "hearth.db"),
	}
	sqlite, err := store.OpenSQLite(cfg.DBPath)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })

	db := &barrierStore{Store: sqlite}
	srv := New(cfg, state.New(), db)
	srv.st.RegisterNode("worker-1", agent.URL, 64, 65536, time.Now().Unix())
	return srv, db
}

// newQuotaTenant creates a tenant with the given quota JSON fields and
// returns the Authorization header for its fresh API key.
func newQuotaTenant(t *testing.T, srv *Server, name, quotas string) string {
	t.Helper()
	w := doRequest(srv.Handler(), "POST", "/api/v1/tenants", `{"name":"`+name+`",`+quotas+`}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d body=%s", w.Code, w.Body)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.APIKey == "" {
		t.Fatalf("create tenant response: %s", w.Body)
	}
	return "Bearer " + created.APIKey
}

// concurrentPosts fires one request per goroutine, all released from a
// barrier, and returns the status codes.
func concurrentPosts(h http.Handler, path, auth, body string, n int) []int {
	codes := make([]int, n)
	start := make(chan struct{})
	var ready, done sync.WaitGroup
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			codes[i] = doRequest(h, "POST", path, body, auth).Code
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return codes
}

func countStatus(codes []int, status int) int {
	n := 0
	for _, c := range codes {
		if c == status {
			n++
		}
	}
	return n
}

func TestListTenantKeysNeverLeaksTheHash(t *testing.T) {
	srv, _ := newQuotaServer(t)
	h := srv.Handler()
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"keys-t"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d body=%s", w.Code, w.Body)
	}
	var created struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
		KeyID  string `json:"key_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	// A second key, this one with an expiry.
	k := doRequest(h, "POST", "/api/v1/tenants/"+created.Tenant.ID+"/keys", `{"expires_in_s":3600}`, adminAuth)
	if k.Code != 201 {
		t.Fatalf("create key: %d body=%s", k.Code, k.Body)
	}
	var minted struct {
		KeyID     string `json:"key_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	json.Unmarshal(k.Body.Bytes(), &minted)
	if minted.ExpiresAt == 0 {
		t.Errorf("expires_in_s ignored: %s", k.Body)
	}

	l := doRequest(h, "GET", "/api/v1/tenants/"+created.Tenant.ID+"/keys", "", adminAuth)
	if l.Code != 200 {
		t.Fatalf("list keys: %d body=%s", l.Code, l.Body)
	}
	var listed struct {
		Keys []struct {
			ID        string `json:"id"`
			Prefix    string `json:"prefix"`
			ExpiresAt int64  `json:"expires_at"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(l.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list body: %v (%s)", err, l.Body)
	}
	if len(listed.Keys) != 2 {
		t.Fatalf("listed %d keys, want 2", len(listed.Keys))
	}
	// The listing is the only way back to a key id after creation — it must
	// carry both, and never anything that could authenticate.
	ids := map[string]bool{}
	for _, key := range listed.Keys {
		ids[key.ID] = true
	}
	if !ids[created.KeyID] || !ids[minted.KeyID] {
		t.Errorf("listing missing a key id: %s", l.Body)
	}
	body := l.Body.String()
	for _, forbidden := range []string{hashSecret(created.APIKey), created.APIKey, "key_hash"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("listing leaks %q: %s", forbidden, body)
		}
	}
}

func TestTenantKeyRoutesAreAdminOnly(t *testing.T) {
	srv, _ := newQuotaServer(t)
	h := srv.Handler()
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"keys-scope"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d body=%s", w.Code, w.Body)
	}
	var created struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		APIKey string `json:"api_key"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	own := "Bearer " + created.APIKey

	// Its own key listing is still admin-only: a stolen tenant key must not
	// enumerate the tenant's other credentials.
	if l := doRequest(h, "GET", "/api/v1/tenants/"+created.Tenant.ID+"/keys", "", own); l.Code != 404 {
		t.Errorf("tenant key listing its own keys: %d", l.Code)
	}
	if c := doRequest(h, "POST", "/api/v1/tenants/"+created.Tenant.ID+"/keys", "", own); c.Code != 404 {
		t.Errorf("tenant key minting its own keys: %d", c.Code)
	}
}

func TestCreateTenantKeyRejectsBadExpiry(t *testing.T) {
	srv, _ := newQuotaServer(t)
	h := srv.Handler()
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"keys-ttl"}`, adminAuth)
	var created struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)

	for _, body := range []string{`{"expires_in_s":-1}`, `{"expires_in_s":31536001}`} {
		k := doRequest(h, "POST", "/api/v1/tenants/"+created.Tenant.ID+"/keys", body, adminAuth)
		if k.Code != 400 {
			t.Errorf("%s: expected 400, got %d body=%s", body, k.Code, k.Body)
			continue
		}
		var m map[string]string
		json.Unmarshal(k.Body.Bytes(), &m)
		if m["error"] != "expires_in_s out of range" {
			t.Errorf("%s: error %v", body, m)
		}
	}
}

func TestQuotaAccountingRunsUnderTheCallersLock(t *testing.T) {
	// The whole point of the split: the accounting helper must NOT take the
	// state lock itself, so the caller can hold it across the summation and
	// the insert it guards. A helper that locks internally deadlocks here.
	srv, _ := newQuotaServer(t)
	tn := &store.Tenant{ID: "tn-lock", Name: "lock", MaxSandboxes: 1}

	done := make(chan string, 1)
	go func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		done <- srv.quotaExceededLocked(tn, 1, 256, 0)
	}()
	select {
	case msg := <-done:
		if msg != "" {
			t.Errorf("empty fleet against max_sandboxes=1: %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota accounting deadlocked under the caller's lock: it takes the state lock itself")
	}
}

func TestConcurrentCreatesCannotExceedSandboxQuota(t *testing.T) {
	srv, db := newQuotaServer(t)
	auth := newQuotaTenant(t, srv, "quota-race", `"max_sandboxes":1`)

	const n = 16
	db.arm(n)
	codes := concurrentPosts(srv.Handler(), "/api/v1/sandboxes", auth, `{"name":"race"}`, n)
	for i, c := range codes {
		if c != 201 && c != 429 {
			t.Errorf("create %d: unexpected status %d", i, c)
		}
	}
	if admitted := countStatus(codes, 201); admitted != 1 {
		t.Fatalf("max_sandboxes=1 admitted %d concurrent creates, want 1", admitted)
	}

	srv.st.Lock()
	held := len(srv.st.Sandboxes)
	srv.st.Unlock()
	if held != 1 {
		t.Errorf("fleet holds %d sandboxes, want 1", held)
	}
}

func TestConcurrentCreatesCannotExceedVcpuQuota(t *testing.T) {
	// Shape quotas race the same way: 8 vCPUs of headroom must admit at most
	// two 4-vCPU sandboxes no matter how many callers ask at once.
	srv, db := newQuotaServer(t)
	auth := newQuotaTenant(t, srv, "quota-race-vcpu", `"max_vcpus":8`)

	const n = 16
	db.arm(n)
	codes := concurrentPosts(srv.Handler(), "/api/v1/sandboxes", auth, `{"name":"race","vcpus":4}`, n)
	if admitted := countStatus(codes, 201); admitted != 2 {
		t.Fatalf("max_vcpus=8 admitted %d concurrent 4-vcpu creates, want 2", admitted)
	}
}

func TestConcurrentForksCannotExceedSandboxQuota(t *testing.T) {
	// The fork path used to release the state lock for its quota check and
	// re-validate only the parent afterwards, so the child insert landed in a
	// different critical section than the count it was checked against.
	srv, db := newQuotaServer(t)
	auth := newQuotaTenant(t, srv, "quota-race-fork", `"max_sandboxes":2`)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes", `{"name":"parent"}`, auth)
	if w.Code != 201 {
		t.Fatalf("create parent: %d body=%s", w.Code, w.Body)
	}
	var parent struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &parent)

	const n = 16
	db.arm(n)
	codes := concurrentPosts(srv.Handler(), "/api/v1/sandboxes/"+parent.ID+"/fork", auth, `{"name":"child"}`, n)
	if admitted := countStatus(codes, 201); admitted != 1 {
		t.Fatalf("one slot of headroom admitted %d concurrent forks, want 1", admitted)
	}
}

func TestForkCountsAgainstQuota(t *testing.T) {
	srv, _ := newQuotaServer(t)
	auth := newQuotaTenant(t, srv, "quota-fork", `"max_sandboxes":1`)

	w := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes", `{"name":"parent"}`, auth)
	if w.Code != 201 {
		t.Fatalf("create parent: %d body=%s", w.Code, w.Body)
	}
	var parent struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &parent)

	f := doRequest(srv.Handler(), "POST", "/api/v1/sandboxes/"+parent.ID+"/fork", `{"name":"child"}`, auth)
	if f.Code != 429 {
		t.Fatalf("fork beyond quota: got %d body=%s", f.Code, f.Body)
	}
	var m map[string]string
	json.Unmarshal(f.Body.Bytes(), &m)
	if m["error"] != "quota exceeded: sandboxes" {
		t.Errorf("error body: %v", m)
	}
}
