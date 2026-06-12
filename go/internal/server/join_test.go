// Tests for the node-join endpoints (join.go). These live in the internal
// `server` package (unlike server_test.go's external package) so they can
// swap srv.addPeer — the wg binary is not present in the test environment.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"
)

const testServerPubKey = "SERVERPUBKEY" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + "="

// Syntactically valid wg public keys: 43 base64 chars + '='.
var (
	validPubKeyA = strings.Repeat("A", 43) + "="
	validPubKeyB = strings.Repeat("B", 43) + "="
)

// newJoinServerWgIP builds a Server with an admin token, a throwaway SQLite
// store, and a stubbed addPeer (success). wgIP "" disables the overlay.
func newJoinServerWgIP(t *testing.T, wgIP string) *Server {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:       "admin-tok",
		UIDir:       tmp,
		StatePath:   filepath.Join(tmp, "state.json"),
		DBPath:      filepath.Join(tmp, "hearth.db"),
		WgIP:        wgIP,
		WgEndpoint:  "1.2.3.4:51820",
		WgKeepalive: 25,
	}
	db, err := store.OpenSQLite(filepath.Join(tmp, "hearth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := New(cfg, state.New(), db)
	srv.SetWgPubKey(testServerPubKey)
	srv.addPeer = func(string, string) error { return nil }
	return srv
}

func newJoinTestServer(t *testing.T) *Server {
	t.Helper()
	return newJoinServerWgIP(t, "10.100.0.1/24")
}

func doRequest(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// mintJoinToken creates a join token via the admin endpoint and returns the
// one-time secret.
func mintJoinToken(t *testing.T, srv *Server) string {
	t.Helper()
	w := doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "", "Bearer admin-tok")
	if w.Code != 201 {
		t.Fatalf("mint join token: status %d body=%s", w.Code, w.Body)
	}
	var resp struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("mint join token: bad body %q: %v", w.Body, err)
	}
	if resp.Token == "" {
		t.Fatal("mint join token: empty token")
	}
	return resp.Token
}

type joinResponse struct {
	OverlayIP       string `json:"overlay_ip"`
	OverlayPrefix   int    `json:"overlay_prefix"`
	ServerOverlayIP string `json:"server_overlay_ip"`
	ServerPubKey    string `json:"server_pubkey"`
	ServerEndpoint  string `json:"server_endpoint"`
	KeepaliveS      int    `json:"keepalive_s"`
}

// joinWith posts /api/v1/nodes/join with the given token and pubkey.
func joinWith(srv *Server, token, pubkey, hostname string) *httptest.ResponseRecorder {
	body := `{"pubkey":"` + pubkey + `","hostname":"` + hostname + `"}`
	return doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer "+token)
}

// ---- Join-token minting ----

func TestJoinTokenAdminOnly(t *testing.T) {
	srv := newJoinTestServer(t)

	// Admin bearer mints a token.
	w := doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "", "Bearer admin-tok")
	if w.Code != 201 {
		t.Fatalf("admin mint: status %d body=%s", w.Code, w.Body)
	}
	var resp struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.ID == "" {
		t.Error("admin mint: missing id")
	}
	if !strings.HasPrefix(resp.Token, "hearth_jt_") {
		t.Errorf("admin mint: token %q lacks hearth_jt_ prefix", resp.Token)
	}

	// A tenant-shaped key (unknown hearth_sk_) must not reach the handler:
	// 401 from the bearer gate (or 404 from the admin-only route guard).
	w = doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "", "Bearer hearth_sk_bogus")
	if w.Code != 401 && w.Code != 404 {
		t.Errorf("tenant key: expected 401 or 404, got %d body=%s", w.Code, w.Body)
	}

	// No auth at all.
	w = doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "", "")
	if w.Code != 401 {
		t.Errorf("no auth: expected 401, got %d body=%s", w.Code, w.Body)
	}
}

func TestJoinTokenBadJSON(t *testing.T) {
	srv := newJoinTestServer(t)

	// Malformed body is the caller's bug: 400.
	w := doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "{bad", "Bearer admin-tok")
	if w.Code != 400 {
		t.Errorf("bad json: expected 400, got %d body=%s", w.Code, w.Body)
	}

	// Empty body (EOF) is tolerated: no hint, token still minted.
	w = doRequest(srv.Handler(), "POST", "/api/v1/join-tokens", "", "Bearer admin-tok")
	if w.Code != 201 {
		t.Errorf("empty body: expected 201, got %d body=%s", w.Code, w.Body)
	}
}

// ---- Node join ----

func TestNodeJoinHappyPath(t *testing.T) {
	srv := newJoinTestServer(t)
	token := mintJoinToken(t, srv)

	w := joinWith(srv, token, validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("join: status %d body=%s", w.Code, w.Body)
	}
	var resp joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("join: bad body %q: %v", w.Body, err)
	}
	if resp.OverlayIP != "10.100.0.2" {
		t.Errorf("overlay_ip: %q, want 10.100.0.2", resp.OverlayIP)
	}
	if resp.OverlayPrefix != 24 {
		t.Errorf("overlay_prefix: %d, want 24", resp.OverlayPrefix)
	}
	if resp.ServerOverlayIP != "10.100.0.1" {
		t.Errorf("server_overlay_ip: %q, want 10.100.0.1", resp.ServerOverlayIP)
	}
	if resp.ServerPubKey != testServerPubKey {
		t.Errorf("server_pubkey: %q, want %q", resp.ServerPubKey, testServerPubKey)
	}
	if resp.ServerEndpoint != "1.2.3.4:51820" {
		t.Errorf("server_endpoint: %q, want 1.2.3.4:51820", resp.ServerEndpoint)
	}
	if resp.KeepaliveS != 25 {
		t.Errorf("keepalive_s: %d, want 25", resp.KeepaliveS)
	}

	// The token was consumed by the successful join: reuse must 401.
	w = joinWith(srv, token, validPubKeyA, "w1")
	if w.Code != 401 {
		t.Errorf("token reuse: expected 401, got %d body=%s", w.Code, w.Body)
	}
}

func TestNodeJoinUniformUnauthorized(t *testing.T) {
	srv := newJoinTestServer(t)
	body := `{"pubkey":"` + validPubKeyA + `","hostname":"w1"}`

	// No bearer at all.
	w := doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "")
	if w.Code != 401 {
		t.Errorf("no bearer: expected 401, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "unauthorized" {
		t.Errorf("no bearer: error body %v", m)
	}

	// Wrong credential shape (an API key, not a join token).
	w = doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer hearth_sk_x")
	if w.Code != 401 {
		t.Errorf("hearth_sk_ bearer: expected 401, got %d body=%s", w.Code, w.Body)
	}

	// Correct shape but unknown token: shape check passes, CheckJoinToken fails.
	w = doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer hearth_jt_unknown")
	if w.Code != 401 {
		t.Errorf("unknown join token: expected 401, got %d body=%s", w.Code, w.Body)
	}
}

func TestNodeJoinBadRequestDoesNotBurnToken(t *testing.T) {
	srv := newJoinTestServer(t)
	token := mintJoinToken(t, srv)

	// Invalid pubkey: 400, before the token is checked or consumed.
	w := joinWith(srv, token, "not-a-key", "w1")
	if w.Code != 400 {
		t.Fatalf("bad pubkey: expected 400, got %d body=%s", w.Code, w.Body)
	}

	// Same token still works with a valid request.
	w = joinWith(srv, token, validPubKeyA, "w1")
	if w.Code != 200 {
		t.Errorf("retry after 400: expected 200, got %d body=%s", w.Code, w.Body)
	}
}

func TestNodeJoinAddPeerFailureDoesNotBurnToken(t *testing.T) {
	srv := newJoinTestServer(t)
	token := mintJoinToken(t, srv)

	srv.addPeer = func(string, string) error { return errors.New("wg not installed") }
	w := joinWith(srv, token, validPubKeyA, "w1")
	if w.Code != 500 {
		t.Fatalf("addPeer failure: expected 500, got %d body=%s", w.Code, w.Body)
	}

	// Host-side issue fixed: the same token still redeems.
	srv.addPeer = func(string, string) error { return nil }
	w = joinWith(srv, token, validPubKeyA, "w1")
	if w.Code != 200 {
		t.Errorf("retry after 500: expected 200, got %d body=%s", w.Code, w.Body)
	}
}

func TestNodeJoinRejoinKeepsIP(t *testing.T) {
	srv := newJoinTestServer(t)

	// First join: pubkey P gets .2.
	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("first join: status %d body=%s", w.Code, w.Body)
	}
	var first joinResponse
	json.Unmarshal(w.Body.Bytes(), &first)
	if first.OverlayIP != "10.100.0.2" {
		t.Fatalf("first join overlay_ip: %q, want 10.100.0.2", first.OverlayIP)
	}

	// Re-join with the same pubkey (new token): keeps its address.
	w = joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("re-join: status %d body=%s", w.Code, w.Body)
	}
	var second joinResponse
	json.Unmarshal(w.Body.Bytes(), &second)
	if second.OverlayIP != first.OverlayIP {
		t.Errorf("re-join overlay_ip: %q, want %q", second.OverlayIP, first.OverlayIP)
	}

	// A different pubkey allocates the next address.
	w = joinWith(srv, mintJoinToken(t, srv), validPubKeyB, "w2")
	if w.Code != 200 {
		t.Fatalf("second node join: status %d body=%s", w.Code, w.Body)
	}
	var third joinResponse
	json.Unmarshal(w.Body.Bytes(), &third)
	if third.OverlayIP != "10.100.0.3" {
		t.Errorf("second node overlay_ip: %q, want 10.100.0.3", third.OverlayIP)
	}
}

func TestNodeJoinOverlayDisabled(t *testing.T) {
	srv := newJoinServerWgIP(t, "") // overlay disabled
	body := `{"pubkey":"` + validPubKeyA + `","hostname":"w1"}`

	// Any join-token-shaped bearer hits the 503 before token validation.
	w := doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer hearth_jt_whatever")
	if w.Code != 503 {
		t.Errorf("overlay disabled: expected 503, got %d body=%s", w.Code, w.Body)
	}
}
