// Tests for the node-join endpoints (join.go). These live in the internal
// `server` package (unlike server_test.go's external package) so they can
// swap srv.addPeer — the wg binary is not present in the test environment.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	AgentToken      string `json:"agent_token"`
}

// joinWith posts /api/v1/nodes/join with the given token and pubkey.
func joinWith(srv *Server, token, pubkey, hostname string) *httptest.ResponseRecorder {
	body := `{"pubkey":"` + pubkey + `","hostname":"` + hostname + `"}`
	return doRequest(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer "+token)
}

// rejoinWith is joinWith carrying the node's current credential — the proof a
// re-join of an already-enrolled pubkey needs (see rejoinAuthorized).
func rejoinWith(srv *Server, token, pubkey, hostname, nodeToken string) *httptest.ResponseRecorder {
	body := `{"pubkey":"` + pubkey + `","hostname":"` + hostname + `","node_token":"` + nodeToken + `"}`
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

// A worker enrolling with a valid, operator-issued join token must not be
// turned away because somebody else was guessing. This route is pre-auth and,
// in the shipped topology, keyed on one address shared by every caller — so a
// throttle evaluated BEFORE the credential would let any anonymous client stop
// the fleet from ever growing, on the path where recovery is hardest (a worker
// that cannot join has no other way in).
func TestNodeJoinValidTokenSurvivesAnotherClientsGuessing(t *testing.T) {
	srv := newJoinTestServer(t)
	good := mintJoinToken(t, srv)

	blocked := false
	for i := 1; i <= 60; i++ {
		switch w := joinWith(srv, "hearth_jt_wrong", validPubKeyB, "attacker"); w.Code {
		case 429:
			blocked = true
		case 401:
		default:
			t.Fatalf("guess %d: expected 401 or 429, got %d body=%s", i, w.Code, w.Body)
		}
	}
	if !blocked {
		t.Fatal("60 join-token guesses from one source were never throttled")
	}

	w := joinWith(srv, good, validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("valid join token after another client's guessing: %d body=%s", w.Code, w.Body)
	}
	var resp joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("join response: %v", err)
	}
	if resp.OverlayIP == "" || resp.AgentToken == "" {
		t.Errorf("enrollment did not complete: %+v", resp)
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

	// Re-join with the same pubkey (new token, and the node's own credential
	// as proof that it IS that node): keeps its address.
	w = rejoinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1", first.AgentToken)
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

// ---- Credential-throttle mechanics ----

// Quiet time is the only thing that forgives an accumulated failure. A
// success cannot, because the key is shared by every client hearthd cannot
// tell apart; without a leak, though, an operator who mistyped a token once
// would still be paying for it an hour later.
func TestAuthThrottleDecaysWithQuietTimeOnly(t *testing.T) {
	th := newAuthThrottle()
	base := time.Unix(1_700_000_000, 0)
	const src = "198.51.100.4"

	for i := 0; i < authFailThreshold+1; i++ {
		th.failed(src, base, authBackoffMax)
	}
	if th.retryAfter(src, base) <= 0 {
		t.Fatal("a burst past the threshold left the source unthrottled")
	}

	quiet := base.Add(time.Duration(authFailThreshold+2) * authFailDecay)
	th.failed(src, quiet, authBackoffMax)
	if d := th.retryAfter(src, quiet); d != 0 {
		t.Errorf("after %v of quiet: retryAfter %v, want 0", quiet.Sub(base), d)
	}
}

// A burst must not accumulate without bound: the key it lands on stands for
// every client behind an undeclared proxy, and an unbounded count would keep
// them all penalized for as long as an attacker cared to hammer it.
func TestAuthThrottleBoundsAccumulation(t *testing.T) {
	th := newAuthThrottle()
	base := time.Unix(1_700_000_000, 0)
	const src = "198.51.100.4"

	for i := 0; i < 5000; i++ {
		th.failed(src, base, authBackoffMax)
	}
	recovered := base.Add(time.Duration(authFailCeiling+1) * authFailDecay)
	th.failed(src, recovered, authBackoffMax)
	if d := th.retryAfter(src, recovered); d != 0 {
		t.Errorf("after the bounded recovery window: retryAfter %v, want 0", d)
	}
}

// A key that stands for every client at once (hearthd behind an undeclared
// reverse proxy) is capped short: it can never refuse a valid credential —
// gateAuth evaluates that first — but it should not answer a neighbour 429 for
// a minute over someone else's guessing either.
func TestAuthThrottleCapsSharedKeysShort(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	const src = "127.0.0.1"

	burst := func(maxWait time.Duration) time.Duration {
		th := newAuthThrottle()
		for i := 0; i < 5000; i++ {
			th.failed(src, base, maxWait)
		}
		return th.retryAfter(src, base)
	}
	full, shared := burst(authBackoffMax), burst(authBackoffSharedMax)
	if full != authBackoffMax {
		t.Fatalf("attributable key waits %v, want the full cap %v", full, authBackoffMax)
	}
	// The cap has to bound the backoff in TIME, not just the number reported in
	// Retry-After — a shared key that is silently penalized for a minute while
	// being told "2s" is the same outage with better manners.
	if shared != authBackoffSharedMax {
		t.Errorf("shared key waits %v, want the short cap %v", shared, authBackoffSharedMax)
	}
}

// throttleSource decides "shared" from configuration and the peer address
// only. It must never read a header: a client that could flip itself into the
// shared bucket would be choosing its own treatment, which is the same class of
// bug as choosing its own key.
func TestThrottleSourceSharedIsNotHeaderControlled(t *testing.T) {
	req := func(srv *Server, remote string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/nodes", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	forged := map[string]string{
		"X-Forwarded-For": "203.0.113.1",
		"X-Real-IP":       "203.0.113.2",
	}

	// No proxy declared: a public peer is attributable however it decorates
	// its request, and loopback is shared however it decorates its request.
	srv := newJoinTestServer(t)
	if got := srv.throttleSource(req(srv, "203.0.113.9:5000", forged)); got.shared {
		t.Errorf("public peer with forged headers: shared=%v, want false (key %q)", got.shared, got.key)
	}
	if got := srv.throttleSource(req(srv, "127.0.0.1:41000", forged)); !got.shared || got.key != "127.0.0.1" {
		t.Errorf("loopback peer behind an undeclared proxy: %+v, want the shared peer key", got)
	}
	// Declared proxy, nothing attributable forwarded: the key is the proxy and
	// stands for everyone behind it.
	srv.trustedProxies = mustCIDRs(t, "127.0.0.1/32")
	if got := srv.throttleSource(req(srv, "127.0.0.1:41000", nil)); !got.shared {
		t.Errorf("declared proxy forwarding nothing: shared=%v, want true", got.shared)
	}
	// Same proxy, a real forwarded hop: attributable to one client.
	got := srv.throttleSource(req(srv, "127.0.0.1:41000", map[string]string{"X-Forwarded-For": "203.0.113.7"}))
	if got.shared || got.key != "203.0.113.7" {
		t.Errorf("attested forwarded hop: %+v, want key 203.0.113.7 and shared=false", got)
	}
}

// A join token authorizes AN enrollment, not a particular node — the pubkey
// naming the node is written by the caller, and a wireguard public key is
// public. So a holder of any valid token could re-join under an enrolled
// worker's key, and the rotation would hand that worker's credential slot to a
// token it never receives: hearthd 401s on every call to it and the node is off
// the control plane, killed by a credential that never established it.
func TestRejoinCannotRotateAnotherNodesCredential(t *testing.T) {
	srv := newJoinTestServer(t)

	// The victim enrolls normally.
	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "victim")
	if w.Code != 200 {
		t.Fatalf("victim join: %d %s", w.Code, w.Body)
	}
	var victim joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &victim); err != nil {
		t.Fatalf("victim join body: %v", err)
	}

	// The attacker holds a perfectly valid, operator-issued join token — its own
	// enrollment — and spends it naming the victim's (public) key.
	attack := mintJoinToken(t, srv)
	if w := joinWith(srv, attack, validPubKeyA, "attacker"); w.Code != 409 {
		t.Fatalf("re-join without proof of possession: got %d %s, want 409", w.Code, w.Body)
	}
	// A guessed proof is no better.
	if w := rejoinWith(srv, attack, validPubKeyA, "attacker", "hearth_nt_guess"); w.Code != 409 {
		t.Fatalf("re-join with a wrong node token: got %d %s, want 409", w.Code, w.Body)
	}

	// The victim's credential is untouched: hearthd still dials it with the
	// token the victim actually holds.
	if got := srv.agentTokenForHost(victim.OverlayIP); got != victim.AgentToken {
		t.Fatalf("victim credential after the attempt: %q, want it unchanged", got)
	}
	// And the refused attempts did not burn the attacker's token, so a refusal
	// cannot be used to destroy operator-issued tokens either — it is still
	// good for the enrollment it was meant for.
	if w := joinWith(srv, attack, validPubKeyB, "attacker"); w.Code != 200 {
		t.Errorf("attacker's own enrollment after two refusals: %d %s, want 200", w.Code, w.Body)
	}
}

// The proof is "the credential hearthd currently hands that node", so a fleet
// grandfathered onto the shared token can still re-join — which is the whole
// rollout path onto per-node credentials, and must not be locked out by the
// check above.
func TestRejoinOfAGrandfatheredNodeProvesWithTheSharedToken(t *testing.T) {
	srv := newJoinTestServer(t)

	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var first joinResponse
	json.Unmarshal(w.Body.Bytes(), &first)

	// Put the node back into the grandfathered state: a Legacy row, no token of
	// its own, so hearthd dials it with the shared control-plane token.
	if err := srv.db.PutNodeCred(&store.NodeCred{
		Host: nodeCredKey(first.OverlayIP, agentPort), Legacy: true, CreatedAt: 1,
	}); err != nil {
		t.Fatalf("put legacy cred: %v", err)
	}
	srv.credMu.Lock()
	srv.credCache = map[string]string{}
	srv.credMu.Unlock()
	srv.nodeIdx.put(nodeCredKey(first.OverlayIP, agentPort), "")

	if w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1"); w.Code != 409 {
		t.Fatalf("grandfathered re-join with no proof: %d %s, want 409", w.Code, w.Body)
	}
	w2 := rejoinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1", srv.cfg.Token)
	if w2.Code != 200 {
		t.Fatalf("grandfathered re-join proving with the shared token: %d %s", w2.Code, w2.Body)
	}
	var again joinResponse
	json.Unmarshal(w2.Body.Bytes(), &again)
	if !strings.HasPrefix(again.AgentToken, nodeTokenPrefix) {
		t.Errorf("rollout re-join agent_token: %q, want a fresh hearth_nt_ secret", again.AgentToken)
	}
}

// ---- Throttle eviction ----

// The map is bounded, so eviction is unavoidable — but it must not be a RESET
// button an attacker can press. evict used to end in clear(t.fail): fill the map
// with more than authThrottleMax distinct keys inside the TTL (an IPv6 /64
// supplies them for free wherever the key is not collapsed to one address) and
// every accumulated backoff in the process was wiped, including the record for
// the source doing the spraying. Guessing then ran at line rate for as long as
// the attacker cared to keep re-filling it.
func TestThrottleSprayCannotEraseAnEstablishedRecord(t *testing.T) {
	th := newAuthThrottle()
	base := time.Unix(1_700_000_000, 0)
	const guesser = "203.0.113.9"

	// An established record: past the free attempts, serving a backoff.
	for i := 0; i < authFailThreshold+3; i++ {
		th.failed(guesser, base, authBackoffMax)
	}
	established := th.retryAfter(guesser, base)
	if established <= 0 {
		t.Fatal("setup: the guesser is not throttled")
	}

	// The spray: far more distinct keys than the map holds, none of them old
	// enough for the TTL pass to reclaim.
	for i := 0; i < authThrottleMax*3; i++ {
		th.failed(fmt.Sprintf("198.51.100.%d.%d", i/250, i%250), base, authBackoffMax)
	}

	if got := th.retryAfter(guesser, base); got <= 0 {
		t.Fatalf("after a %d-key spray the guesser's backoff was erased (retryAfter %v): eviction is a reset button",
			authThrottleMax*3, got)
	}
	// And the guard is still bounded — the spray did not grow the map either.
	th.mu.Lock()
	size := len(th.fail)
	th.mu.Unlock()
	if size > authThrottleMax {
		t.Errorf("map holds %d entries, want at most %d", size, authThrottleMax)
	}
}

// Eviction has to actually happen, or "bounded" is a comment rather than a
// property: the sprayed keys are the ones that go.
func TestThrottleEvictsTheLeastEstablishedFirst(t *testing.T) {
	th := newAuthThrottle()
	base := time.Unix(1_700_000_000, 0)

	// One record per key, one failure each: nothing established anywhere.
	for i := 0; i < authThrottleMax*2; i++ {
		th.failed(fmt.Sprintf("198.51.100.%d.%d", i/250, i%250), base, authBackoffMax)
	}
	th.mu.Lock()
	size := len(th.fail)
	th.mu.Unlock()
	if size > authThrottleMax {
		t.Fatalf("map holds %d entries after a %d-key spray, want at most %d", size, authThrottleMax*2, authThrottleMax)
	}
	if size == 0 {
		t.Fatal("eviction emptied the map: that is the clear() behaviour this replaces")
	}
}

// ---- Bounding the pre-authentication store lookup ----

// blockingKeyStore holds every credential lookup inside the store until the test
// releases them, recording how many were in there at once.
type blockingKeyStore struct {
	store.Store
	mu       sync.Mutex
	inFlight int
	peak     int
	release  chan struct{}
}

func (b *blockingKeyStore) LookupKeyByHash(hash string) (string, error) {
	b.mu.Lock()
	b.inFlight++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return b.Store.LookupKeyByHash(hash)
}

func (b *blockingKeyStore) stats() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight, b.peak
}

// bogusAPIKey is a correctly SHAPED key nobody issued — what an attacker sends
// once they have read the source, so the shape check cannot be what saves us.
func bogusAPIKey(i int) string {
	return apiKeyPrefix + fmt.Sprintf("%048x", i)
}

// The bearer gate reaches LookupKeyByHash for any request whose bearer merely
// begins with "hearth_sk_" — no credential, no throttle, nothing. store/sqlite
// caps the pool at ONE connection, so without a bound an unauthenticated client
// opening N concurrent connections puts N queries in front of every other user
// of that connection: the snapshot write behind each create/sleep/delete, the
// usage append, the sweep's tenant reads, and the node-credential read on the
// exec hot path. Backpressure has to exist somewhere in front of the store.
//
// It must NOT be a refusal, though: whether the presented key is good is exactly
// what the pending lookup is about, so shedding here would be refusing a
// credential hearthd has not read yet — the lockout the previous round removed.
// So the bound is a queue, and the assertions are both halves of that.
func TestUnauthenticatedGuessingCannotMonopolizeTheCredentialStore(t *testing.T) {
	tmp := t.TempDir()
	sqlite, err := store.OpenSQLite(filepath.Join(tmp, "hearth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	blocking := &blockingKeyStore{Store: sqlite, release: make(chan struct{})}
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "hearth.db"),
	}
	srv := New(cfg, state.New(), blocking)
	h := srv.Handler()

	// A tenant with a real key, issued before the flood starts.
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"victim"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("tenant body: %v", err)
	}

	const flood = 64
	var wg sync.WaitGroup
	for i := 0; i < flood; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doRequest(h, "GET", "/api/v1/sandboxes", "", "Bearer "+bogusAPIKey(i))
		}(i)
	}
	// One legitimate tenant, arriving in the middle of it.
	legit := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		legit <- doRequest(h, "GET", "/api/v1/sandboxes", "", "Bearer "+created.APIKey).Code
	}()

	// Let the flood pile up: wait for the gate to fill, then give the rest a
	// generous window to get past it if nothing is stopping them.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := blocking.stats(); n >= maxCredLookups {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
	_, peak := blocking.stats()

	// The admin path never touches the store, so the control plane answers
	// throughout — the flood cannot make hearthd unreachable to its operator.
	if w := doRequest(h, "GET", "/api/v1/nodes", "", adminAuth); w.Code != 200 {
		t.Errorf("admin request during the flood: got %d, want 200", w.Code)
	}

	close(blocking.release)
	wg.Wait()

	if peak > maxCredLookups {
		t.Errorf("%d unauthenticated lookups were inside the store at once (cap %d): the credential store has no backpressure in front of it",
			peak, maxCredLookups)
	}
	if peak == 0 {
		t.Fatal("no lookup reached the store: the test is not exercising the path it claims to")
	}
	// Delayed, never refused: the tenant's own key is served.
	if code := <-legit; code != 200 {
		t.Errorf("valid tenant key issued during the flood: got %d, want 200 — load may delay a credential, never refuse it", code)
	}
}

// The shape check is a filter, not a defence: it must reject what was never
// issuable and accept everything hearthd ever minted.
func TestAPIKeyShapeFilter(t *testing.T) {
	real := apiKeyPrefix + newSecret(24)
	if !validAPIKeyShape(real) {
		t.Errorf("a freshly minted key was rejected by its own shape check: %q", real)
	}
	for _, bad := range []string{
		"", apiKeyPrefix, apiKeyPrefix + "short",
		apiKeyPrefix + strings.Repeat("a", 47), apiKeyPrefix + strings.Repeat("a", 49),
		apiKeyPrefix + strings.Repeat("A", 48), // hex is lowercase
		apiKeyPrefix + strings.Repeat("z", 48),
	} {
		if validAPIKeyShape(bad) {
			t.Errorf("shape check accepted %q", bad)
		}
	}
}

// ---- Forwarded-header parsing bounds ----

// X-Forwarded-For is attacker-sized and parsed on the auth path. Splitting it
// whole allocated one element per comma before anything was inspected, so a
// header full of commas turned into a huge slice per request; and a chain longer
// than any real topology is not something hearthd can attribute a client to
// anyway. Both cases end at the peer.
func TestForwardedHeaderParsingIsBounded(t *testing.T) {
	srv := newJoinTestServer(t)
	srv.trustedProxies = mustCIDRs(t, "127.0.0.1/32")
	const proxy = "127.0.0.1:41000"

	src := func(xff string) throttleSource {
		r := httptest.NewRequest("GET", "/api/v1/nodes", nil)
		r.RemoteAddr = proxy
		r.Header.Set("X-Forwarded-For", xff)
		return srv.throttleSource(r)
	}

	// A short chain still works exactly as before.
	if got := src("198.51.100.9, 203.0.113.7"); got.key != "203.0.113.7" || got.shared {
		t.Errorf("two-hop chain: %+v, want key 203.0.113.7", got)
	}
	// The client is still found through a chain of our own proxies...
	if got := src("203.0.113.7, 127.0.0.1, 127.0.0.1"); got.key != "203.0.113.7" {
		t.Errorf("client behind two of our own hops: key %q, want 203.0.113.7", got.key)
	}
	// ...but only while the walk stays inside the bound. Past it the chain is
	// longer than any real topology and attributable to nobody: key on the peer.
	hops := append([]string{"203.0.113.7"}, strings.Split(strings.Repeat("127.0.0.1 ", maxXFFHops+5), " ")...)
	if got := src(strings.Join(hops[:len(hops)-1], ", ")); got.key != "127.0.0.1" {
		t.Errorf("chain of %d hops: key %q, want the peer", len(hops)-1, got.key)
	}
	// A comma-only header: no allocation per comma, no attribution.
	if got := src(strings.Repeat(",", 200_000)); got.key != "127.0.0.1" {
		t.Errorf("comma-only header: key %q, want the peer", got.key)
	}
}

// mustCIDRs parses trusted-proxy CIDRs for the tests that set them directly.
func mustCIDRs(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

// ---- Per-node hearthd→agent credentials ----
//
// The control-plane admin token used to be the credential on every outbound
// call to a worker, and a node address is caller-supplied: registering
// "203.0.113.9:9090" made hearthd deliver `Authorization: Bearer <admin
// token>` there. These tests pin the replacement — each node gets its own
// token at enrollment, and an address nobody enrolled gets none.

// authRecorder is an httptest worker that records the Authorization header of
// every call hearthd makes to it.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
}

func (a *authRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auths = append(a.auths, r.Header.Get("Authorization"))
		a.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func (a *authRecorder) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auths...)
}

// waitForCall blocks until the worker has been called at least once (the pool
// push runs off the response path) or the deadline passes.
func (a *authRecorder) waitForCall(t *testing.T) []string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if got := a.seen(); len(got) > 0 {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker was never called")
	return nil
}

// The audit's payload: POST /api/v1/agents/register naming an address the
// caller chose, which hearthd then dials (PUT /v1/pools). It must not arrive
// carrying the control-plane admin token.
func TestRegisterNeverDeliversTheAdminTokenToANode(t *testing.T) {
	srv := newJoinServerWgIP(t, "") // the shipped default: overlay off
	rec := &authRecorder{}
	ts := httptest.NewServer(rec.handler())
	defer ts.Close()

	// Seed the address into the working set so the register passes
	// knownNodeAddr — an httptest listener is loopback on a random port, which
	// validNodeAddr refuses outright. This is the *friendlier* case for the
	// attacker: the address is already one hearthd dials, and it still gets no
	// admin token.
	srv.st.RegisterNode("worker-1", ts.URL, 4, 8192, 1000)

	body := `{"hostname":"worker-1","addr":` + jsonString(ts.URL) + `,"cpus":4,"mem_total_mib":8192}`
	if w := doRequest(srv.Handler(), "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}

	for _, auth := range rec.waitForCall(t) {
		if strings.Contains(auth, "admin-tok") {
			t.Fatalf("hearthd delivered the admin API token to a node address: %q", auth)
		}
		if auth != "" {
			t.Errorf("unenrolled node got a credential: %q, want none", auth)
		}
	}
}

// An address nobody enrolled resolves to no credential at all — the shared
// token is reserved for the one-time grandfathered set.
func TestUnenrolledAddressGetsNoCredential(t *testing.T) {
	srv := newJoinServerWgIP(t, "")

	body := `{"hostname":"x","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`
	if w := doRequest(srv.Handler(), "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if tok := srv.agentTokenForHost("203.0.113.9"); tok != "" {
		t.Errorf("credential for a freshly registered address: %q, want none", tok)
	}
}

// A node address is a request to speak to whatever answers there, so the port
// has to be the agent's.
func TestRegisterRefusesNonAgentPorts(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	h := srv.Handler()

	bad := []string{"203.0.113.9:80", "10.0.0.5:6379", "198.51.100.4:443", "198.51.100.4:22"}
	for _, addr := range bad {
		body := `{"hostname":"x","addr":` + jsonString(addr) + `,"cpus":1,"mem_total_mib":512}`
		if w := doRequest(h, "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 400 {
			t.Errorf("register %q: got %d %s, want 400", addr, w.Code, w.Body)
		}
	}
	body := `{"hostname":"x","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`
	if w := doRequest(h, "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 200 {
		t.Errorf("register on the agent port: got %d %s, want 200", w.Code, w.Body)
	}
}

// The overlay path: joining mints the node's own token and hands it over once.
func TestNodeJoinMintsPerNodeAgentToken(t *testing.T) {
	srv := newJoinTestServer(t)

	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var resp joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("join body: %v", err)
	}
	if !strings.HasPrefix(resp.AgentToken, "hearth_nt_") {
		t.Fatalf("agent_token: %q, want a hearth_nt_ secret", resp.AgentToken)
	}
	if resp.AgentToken == "admin-tok" {
		t.Fatal("join handed out the admin token as the node credential")
	}
	// It is the credential hearthd will present to that overlay address.
	if got := srv.agentTokenForHost(resp.OverlayIP); got != resp.AgentToken {
		t.Errorf("credential for %s: %q, want the minted token", resp.OverlayIP, got)
	}

	// Re-enrolling rotates it — the reason a stolen one has a bounded life. The
	// node proves it is itself with the credential it currently holds.
	w2 := rejoinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1", resp.AgentToken)
	if w2.Code != 200 {
		t.Fatalf("re-join: %d %s", w2.Code, w2.Body)
	}
	var again joinResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &again); err != nil {
		t.Fatalf("re-join body: %v", err)
	}
	if again.AgentToken == resp.AgentToken || again.AgentToken == "" {
		t.Errorf("re-join agent_token: %q, want a fresh secret", again.AgentToken)
	}
	if got := srv.agentTokenForHost(resp.OverlayIP); got != again.AgentToken {
		t.Errorf("credential after rotation: %q, want the new token", got)
	}
}

// The no-overlay path: a join token presented at registration is what buys a
// new node its own credential.
func TestRegisterWithJoinTokenMintsPerNodeCredential(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	h := srv.Handler()
	jt := mintJoinToken(t, srv)

	body := `{"hostname":"w1","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512,"join_token":"` + jt + `"}`
	w := doRequest(h, "POST", "/api/v1/agents/register", body, "Bearer admin-tok")
	if w.Code != 200 {
		t.Fatalf("enrolling register: %d %s", w.Code, w.Body)
	}
	var resp struct {
		ID         string `json:"id"`
		AgentToken string `json:"agent_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("register body: %v", err)
	}
	if !strings.HasPrefix(resp.AgentToken, "hearth_nt_") || resp.AgentToken == "admin-tok" {
		t.Fatalf("agent_token: %q, want a hearth_nt_ secret", resp.AgentToken)
	}
	if got := srv.agentTokenForHost("203.0.113.9"); got != resp.AgentToken {
		t.Errorf("credential for the enrolled node: %q, want the minted token", got)
	}

	// The join token is one-time.
	if w2 := doRequest(h, "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w2.Code != 401 {
		t.Errorf("replayed join token: got %d %s, want 401", w2.Code, w2.Body)
	}

	// A later plain registration of the same address never re-reveals the
	// secret: one worker must not be able to read another's credential out of
	// hearthd just by knowing its address.
	plain := `{"hostname":"w1","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`
	w3 := doRequest(h, "POST", "/api/v1/agents/register", plain, "Bearer admin-tok")
	if w3.Code != 200 {
		t.Fatalf("plain re-register: %d %s", w3.Code, w3.Body)
	}
	if strings.Contains(w3.Body.String(), "agent_token") {
		t.Errorf("plain re-register re-revealed the node credential: %s", w3.Body)
	}
	if got := srv.agentTokenForHost("203.0.113.9"); got != resp.AgentToken {
		t.Errorf("credential after a plain re-register: %q, want it unchanged", got)
	}
}

// A garbage join token is rejected outright rather than silently ignored —
// otherwise a typo'd enrollment looks like it worked and the node comes up
// with no credential.
func TestRegisterRejectsBadJoinToken(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	for _, jt := range []string{"hearth_jt_unknown", "not-a-join-token"} {
		body := `{"hostname":"w1","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512,"join_token":"` + jt + `"}`
		if w := doRequest(srv.Handler(), "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 401 {
			t.Errorf("register with %q: got %d %s, want 401", jt, w.Code, w.Body)
		}
	}
	// A rejected enrollment leaves no node behind.
	srv.st.Lock()
	n := len(srv.st.Nodes)
	srv.st.Unlock()
	if n != 0 {
		t.Errorf("rejected enrollment registered %d nodes, want 0", n)
	}
}

// ---- M3, agent side: the node principal ----

// enrolledNode joins a worker and returns (node token, overlay address).
func enrolledNode(t *testing.T, srv *Server, pubkey, hostname string) (string, string) {
	t.Helper()
	w := joinWith(srv, mintJoinToken(t, srv), pubkey, hostname)
	if w.Code != 200 {
		t.Fatalf("join %s: %d %s", hostname, w.Code, w.Body)
	}
	var resp joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("join %s body: %v", hostname, err)
	}
	return resp.AgentToken, fmt.Sprintf("%s:%d", resp.OverlayIP, agentPort)
}

func registerBody(hostname, addr string) string {
	return `{"hostname":"` + hostname + `","addr":"` + addr + `","cpus":4,"mem_total_mib":8192}`
}

// M3 is only finished end to end when a worker stops needing the FLEET ADMIN
// token to talk to hearthd. Until then every worker holds the key to every other
// worker, and "a harvested credential is worth one worker" is a claim about one
// direction of the connection only.
func TestNodeCredentialAuthenticatesTheAgentRoutes(t *testing.T) {
	srv := newJoinTestServer(t)
	h := srv.Handler()
	nodeTok, addr := enrolledNode(t, srv, validPubKeyA, "w1")

	w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addr), "Bearer "+nodeTok)
	if w.Code != 200 {
		t.Fatalf("register with the node's own credential: %d %s", w.Code, w.Body)
	}
	var reg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil || reg.ID == "" {
		t.Fatalf("register body %q: %v", w.Body, err)
	}
	if w := doRequest(h, "POST", "/api/v1/agents/heartbeat",
		`{"id":"`+reg.ID+`","mem_free_mib":4096,"vm_count":1,"pool_size":0}`, "Bearer "+nodeTok); w.Code != 200 {
		t.Fatalf("heartbeat with the node's own credential: %d %s", w.Code, w.Body)
	}

	// The admin token keeps working on the same routes — a mixed-version fleet
	// has to survive the rollout.
	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addr), adminAuth); w.Code != 200 {
		t.Errorf("register with the admin token: %d %s", w.Code, w.Body)
	}
}

// A node principal is NOT a second administrator. The routes it may reach are
// allowlisted, so this is the list — everything else, tenant-shaped or
// fleet-shaped, is simply not there.
func TestNodeCredentialReachesNothingButTheAgentRoutes(t *testing.T) {
	srv := newJoinTestServer(t)
	h := srv.Handler()
	nodeTok, _ := enrolledNode(t, srv, validPubKeyA, "w1")
	auth := "Bearer " + nodeTok

	forbidden := []struct{ method, path, body string }{
		{"GET", "/api/v1/nodes", ""},
		{"GET", "/api/v1/sandboxes", ""},
		{"POST", "/api/v1/sandboxes", `{"name":"x"}`},
		{"GET", "/api/v1/tenants", ""},
		{"POST", "/api/v1/tenants", `{"name":"mine"}`},
		{"POST", "/api/v1/join-tokens", ""},
		{"GET", "/api/v1/routes", ""},
		{"POST", "/api/v1/routes/activity", `{"sandbox_ids":[]}`},
		{"GET", "/api/v1/templates", ""},
		{"GET", "/api/v1/metrics/tenants", ""},
		{"DELETE", "/api/v1/keys/key-1", ""},
	}
	for _, f := range forbidden {
		if w := doRequest(h, f.method, f.path, f.body, auth); w.Code != 404 {
			t.Errorf("%s %s with a node credential: got %d %s, want 404", f.method, f.path, w.Code, w.Body)
		}
	}
	// /metrics names every worker and takes the global state lock: admin only.
	if w := doRequest(h, "GET", "/metrics", "", auth); w.Code != 404 {
		t.Errorf("/metrics with a node credential: got %d, want 404", w.Code)
	}
}

// The two things a node credential must not be able to do on the routes it CAN
// reach: mint or rotate credentials, and speak for another node.
func TestNodeCredentialCannotActForAnotherNode(t *testing.T) {
	srv := newJoinTestServer(t)
	h := srv.Handler()
	tokA, addrA := enrolledNode(t, srv, validPubKeyA, "w1")
	tokB, addrB := enrolledNode(t, srv, validPubKeyB, "w2")

	// Both nodes are in the fleet, registered by themselves.
	for _, n := range []struct{ tok, host, addr string }{{tokA, "w1", addrA}, {tokB, "w2", addrB}} {
		if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody(n.host, n.addr), "Bearer "+n.tok); w.Code != 200 {
			t.Fatalf("register %s: %d %s", n.host, w.Code, w.Body)
		}
	}

	// A: advertising B's address would make hearthd dial B's sandboxes here.
	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addrB), "Bearer "+tokA); w.Code != 403 {
		t.Errorf("node registering another node's address: got %d %s, want 403", w.Code, w.Body)
	}
	// A: claiming B's hostname repoints B's record (RegisterNode matches on it).
	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w2", addrA), "Bearer "+tokA); w.Code != 403 {
		t.Errorf("node claiming another node's hostname: got %d %s, want 403", w.Code, w.Body)
	}
	srv.st.Lock()
	var bAddr string
	for _, n := range srv.st.Nodes {
		if n.Hostname == "w2" {
			bAddr = n.Addr
		}
	}
	srv.st.Unlock()
	if bAddr != addrB {
		t.Fatalf("w2's address is now %q, want %q — a node repointed another node's record", bAddr, addrB)
	}

	// A join token in a node's hands must not mint or rotate a credential.
	jt := mintJoinToken(t, srv)
	body := `{"hostname":"w1","addr":"` + addrA + `","cpus":4,"mem_total_mib":8192,"join_token":"` + jt + `"}`
	if w := doRequest(h, "POST", "/api/v1/agents/register", body, "Bearer "+tokA); w.Code != 403 {
		t.Errorf("node spending a join token: got %d %s, want 403", w.Code, w.Body)
	}
	if got := srv.agentTokenForHost(strings.Split(addrA, ":")[0]); got != tokA {
		t.Errorf("credential after the refused enrollment: %q, want it unchanged", got)
	}

	// B's heartbeat must not be forgeable by A: the stats it carries steer
	// PickNode, so "advertise the rival as full" would choose where sandboxes go.
	srv.st.Lock()
	var bID string
	for _, n := range srv.st.Nodes {
		if n.Hostname == "w2" {
			bID = n.ID
		}
	}
	srv.st.Unlock()
	if w := doRequest(h, "POST", "/api/v1/agents/heartbeat",
		`{"id":"`+bID+`","mem_free_mib":1,"vm_count":99,"pool_size":0}`, "Bearer "+tokA); w.Code != 404 {
		t.Errorf("node heartbeating for another node: got %d %s, want 404", w.Code, w.Body)
	}
}

// A rotation has to retire the previous secret in BOTH directions: hearthd stops
// presenting it, and it stops authenticating its node.
func TestRotatedNodeTokenStopsAuthenticating(t *testing.T) {
	srv := newJoinTestServer(t)
	h := srv.Handler()
	oldTok, addr := enrolledNode(t, srv, validPubKeyA, "w1")
	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addr), "Bearer "+oldTok); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}

	w := rejoinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1", oldTok)
	if w.Code != 200 {
		t.Fatalf("re-join: %d %s", w.Code, w.Body)
	}
	var again joinResponse
	json.Unmarshal(w.Body.Bytes(), &again)
	if again.AgentToken == oldTok || again.AgentToken == "" {
		t.Fatalf("re-join agent_token %q, want a fresh secret", again.AgentToken)
	}

	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addr), "Bearer "+oldTok); w.Code != 401 {
		t.Errorf("the retired node token still authenticates: got %d %s, want 401", w.Code, w.Body)
	}
	if w := doRequest(h, "POST", "/api/v1/agents/register", registerBody("w1", addr), "Bearer "+again.AgentToken); w.Code != 200 {
		t.Errorf("the rotated node token: got %d %s, want 200", w.Code, w.Body)
	}
}

// ---- Credential keying ----

// A credential stands for an agent ENDPOINT. Keyed on the host alone, one
// credential covered every agent on an address — narrower than the identity it
// authorizes, and "worth one worker" has to mean one worker.
func TestNodeCredentialsAreKeyedOnHostAndPort(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	now := time.Now().Unix()

	if err := srv.putNodeCred("203.0.113.9", 9090, "hearth_nt_first", now); err != nil {
		t.Fatalf("put first: %v", err)
	}
	if err := srv.putNodeCred("203.0.113.9", 9191, "hearth_nt_second", now); err != nil {
		t.Fatalf("put second: %v", err)
	}
	if got := srv.agentTokenFor("203.0.113.9", 9090); got != "hearth_nt_first" {
		t.Errorf("credential for :9090: %q", got)
	}
	if got := srv.agentTokenFor("203.0.113.9", 9191); got != "hearth_nt_second" {
		t.Errorf("credential for :9191: %q, want the second agent's own", got)
	}
	// And inbound: each token resolves to its own endpoint, never the other's.
	if key, ok := srv.nodeIdx.lookup("hearth_nt_second"); !ok || key != "203.0.113.9:9191" {
		t.Errorf("inbound lookup of the second agent's token: %q ok=%v", key, ok)
	}
}

// An upgrade must not strand a fleet whose credential rows were written under
// the old bare-host key: they are read, rewritten under "host:port", and the old
// row retired so a second agent on that host cannot inherit them.
func TestBareHostCredentialRowIsMigratedOnce(t *testing.T) {
	srv := newJoinServerWgIP(t, "")

	if err := srv.db.PutNodeCred(&store.NodeCred{Host: "203.0.113.9", Token: "hearth_nt_old", CreatedAt: 7}); err != nil {
		t.Fatalf("seed bare-host row: %v", err)
	}
	if got := srv.agentTokenFor("203.0.113.9", agentPort); got != "hearth_nt_old" {
		t.Fatalf("credential from the pre-port row: %q, want it honoured", got)
	}
	c, err := srv.db.GetNodeCred("203.0.113.9:9090")
	if err != nil || c == nil || c.Token != "hearth_nt_old" {
		t.Fatalf("row was not rewritten under the host:port key: %+v err=%v", c, err)
	}
	if old, err := srv.db.GetNodeCred("203.0.113.9"); err != nil || old != nil {
		t.Errorf("bare-host row survived the migration: %+v err=%v", old, err)
	}
	// A different agent on the same host no longer inherits it.
	if got := srv.agentTokenFor("203.0.113.9", 9191); got != "" {
		t.Errorf("second agent on the same host resolved %q, want no credential", got)
	}
	// And the migrated credential authenticates its node inbound.
	if key, ok := srv.nodeIdx.lookup("hearth_nt_old"); !ok || key != "203.0.113.9:9090" {
		t.Errorf("migrated credential inbound: %q ok=%v", key, ok)
	}
}

// Upgrading must not strand a fleet enrolled before per-node credentials: the
// nodes in the snapshot at first start are grandfathered onto the shared
// token, and nothing else is.
func TestLegacyFleetIsGrandfatheredOnceOnly(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "hearth.db")

	// Pre-upgrade hearthd: a worker enrolled, snapshot persisted, process
	// exits. No per-node credentials existed.
	db1, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	st1 := state.New()
	st1.RegisterNode("worker-legacy", "198.51.100.7:9090", 4, 8192, 1000)
	if err := db1.SaveSnapshot(st1); err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	db1.Close()

	// Post-upgrade start: load, then construct the server (which seeds).
	db2, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { db2.Close() })
	st2 := state.New()
	if err := db2.LoadInto(st2); err != nil {
		t.Fatalf("load: %v", err)
	}
	cfg := &config.Config{
		Token:     "admin-tok",
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    dbPath,
	}
	srv := New(cfg, st2, db2)

	if got := srv.agentTokenForHost("198.51.100.7"); got != "admin-tok" {
		t.Errorf("grandfathered node credential: %q, want the shared token", got)
	}

	// A node enrolled AFTER the upgrade is not grandfathered — otherwise
	// "register any address, receive the admin key" is still the exploit.
	body := `{"hostname":"new","addr":"203.0.113.9:9090","cpus":1,"mem_total_mib":512}`
	if w := doRequest(srv.Handler(), "POST", "/api/v1/agents/register", body, "Bearer admin-tok"); w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if got := srv.agentTokenForHost("203.0.113.9"); got != "" {
		t.Errorf("post-upgrade address credential: %q, want none", got)
	}

	// And a restart does not re-run the grandfathering over whatever is in the
	// snapshot by then.
	st3 := state.New()
	if err := db2.LoadInto(st3); err != nil {
		t.Fatalf("reload: %v", err)
	}
	srv2 := New(cfg, st3, db2)
	if got := srv2.agentTokenForHost("203.0.113.9"); got != "" {
		t.Errorf("credential after a restart: %q, want none", got)
	}
	if got := srv2.agentTokenForHost("198.51.100.7"); got != "admin-tok" {
		t.Errorf("grandfathered node after a restart: %q, want the shared token", got)
	}
}

// ---- Fail-closed credential resolution ----

// credErrStore fails the node-credential read the way a real store does under
// pressure: a locked sqlite file, a busy timeout, a corrupt page — or the queue
// an unauthenticated flood can put in front of the single connection, which is
// the same lever a previous round had to bound (see credGate).
type credErrStore struct {
	store.Store
	mu   sync.Mutex
	fail bool
}

func (c *credErrStore) setFail(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fail = v
}

func (c *credErrStore) GetNodeCred(host string) (*store.NodeCred, error) {
	c.mu.Lock()
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return nil, errors.New("store unavailable")
	}
	return c.Store.GetNodeCred(host)
}

// newJoinServerStore is newJoinTestServer over a caller-supplied store.
func newJoinServerStore(t *testing.T, db store.Store, tmp string) *Server {
	t.Helper()
	cfg := &config.Config{
		Token:       "admin-tok",
		UIDir:       tmp,
		StatePath:   filepath.Join(tmp, "state.json"),
		DBPath:      filepath.Join(tmp, "hearth.db"),
		WgIP:        "10.100.0.1/24",
		WgEndpoint:  "1.2.3.4:51820",
		WgKeepalive: 25,
	}
	srv := New(cfg, state.New(), db)
	srv.SetWgPubKey(testServerPubKey)
	srv.addPeer = func(string, string) error { return nil }
	return srv
}

// The re-join possession check asks "what does hearthd currently hand this
// node" and refuses a re-join that cannot present it. The resolver behind that
// question returns an empty string for THREE different things, and only one of
// them — "the store was read and this node has no credential yet" — means there
// is nothing to prove. Reading a failed READ as the same thing switches the
// whole check off: one store error, transient or induced, and a holder of any
// valid join token can again rotate an enrolled worker's credential and take it
// off the control plane. It must fail CLOSED — a re-join is retryable, an
// evicted worker is not.
func TestRejoinFailsClosedWhenTheCredentialStoreCannotBeRead(t *testing.T) {
	tmp := t.TempDir()
	sqlite, err := store.OpenSQLite(filepath.Join(tmp, "hearth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	flaky := &credErrStore{Store: sqlite}
	srv := newJoinServerStore(t, flaky, tmp)

	// The victim enrolls while the store is healthy.
	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "victim")
	if w.Code != 200 {
		t.Fatalf("victim join: %d %s", w.Code, w.Body)
	}
	var victim joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &victim); err != nil {
		t.Fatalf("victim join body: %v", err)
	}
	if victim.AgentToken == "" {
		t.Fatal("victim join returned no agent token")
	}
	attack := mintJoinToken(t, srv)

	// Drop the memoized credential so the re-join genuinely reads the store,
	// then make that read fail.
	srv.credMu.Lock()
	srv.credCache = map[string]string{}
	srv.credMu.Unlock()
	flaky.setFail(true)

	if w := joinWith(srv, attack, validPubKeyA, "attacker"); w.Code != 409 {
		t.Fatalf("re-join while the credential store is unreadable: got %d %s, want 409 — an unreadable store is not evidence that a node has no credential",
			w.Code, w.Body)
	}

	// The store recovers: the victim still holds exactly the credential it was
	// issued, so the outage handed its slot to nobody.
	flaky.setFail(false)
	if got := srv.agentTokenForHost(victim.OverlayIP); got != victim.AgentToken {
		t.Fatalf("victim credential after the attempt: %q, want it unchanged", got)
	}
	// And the refusal did not burn the attacker's operator-issued token, so
	// this cannot be turned into a way to destroy join tokens either.
	if w := joinWith(srv, attack, validPubKeyB, "attacker"); w.Code != 200 {
		t.Errorf("attacker's own enrollment after the refusal: %d %s, want 200", w.Code, w.Body)
	}
}

// A peer with no credential at all is still authorized without proof — that is
// the legitimate retry path (the peer row is persisted before the kernel
// install, so a worker retrying after `wg add peer` failed is in exactly this
// state) and the rollout path for a pre-credential fleet. Failing closed on the
// UNKNOWN case must not have closed the ABSENT one.
func TestRejoinWithNoCredentialYetIsStillAuthorized(t *testing.T) {
	srv := newJoinTestServer(t)

	w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1")
	if w.Code != 200 {
		t.Fatalf("join: %d %s", w.Code, w.Body)
	}
	var first joinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("join body: %v", err)
	}

	if _, err := srv.db.DeleteNodeCred(nodeCredKey(first.OverlayIP, agentPort)); err != nil {
		t.Fatalf("delete node cred: %v", err)
	}
	srv.credMu.Lock()
	srv.credCache = map[string]string{}
	srv.credMu.Unlock()

	if w := joinWith(srv, mintJoinToken(t, srv), validPubKeyA, "w1"); w.Code != 200 {
		t.Fatalf("re-join of a peer with no credential yet: got %d %s, want 200 — there is nothing to prove", w.Code, w.Body)
	}
}

// ---- credGate slot accounting ----

// panicStore makes the two GATED credential reads panic. A store call can panic
// for reasons that have nothing to do with this package: a driver bug, a nil map
// inside a wrapper, an assertion in the sqlite shim.
type panicStore struct {
	store.Store
	mu         sync.Mutex
	keyPanics  int
	joinPanic  int
	usagePanic int
}

func (p *panicStore) armKey(n int)   { p.mu.Lock(); p.keyPanics = n; p.mu.Unlock() }
func (p *panicStore) armJoin(n int)  { p.mu.Lock(); p.joinPanic = n; p.mu.Unlock() }
func (p *panicStore) armUsage(n int) { p.mu.Lock(); p.usagePanic = n; p.mu.Unlock() }

// AppendUsage is reached from recordUsage, which several handlers call WHILE
// HOLDING the state lock (the metering event is built from the sandbox's live
// fields). It is therefore the store call that can panic inside a state-lock
// region — see TestAPanicUnderTheStateLockDoesNotDeadlockTheControlPlane.
func (p *panicStore) AppendUsage(e store.UsageEvent) error {
	p.mu.Lock()
	if p.usagePanic > 0 {
		p.usagePanic--
		p.mu.Unlock()
		panic("store: usage append blew up")
	}
	p.mu.Unlock()
	return p.Store.AppendUsage(e)
}

func (p *panicStore) LookupKeyByHash(hash string) (string, error) {
	p.mu.Lock()
	if p.keyPanics > 0 {
		p.keyPanics--
		p.mu.Unlock()
		panic("store: key lookup blew up")
	}
	p.mu.Unlock()
	return p.Store.LookupKeyByHash(hash)
}

func (p *panicStore) CheckJoinToken(hash string, now int64) (bool, error) {
	p.mu.Lock()
	if p.joinPanic > 0 {
		p.joinPanic--
		p.mu.Unlock()
		panic("store: join-token check blew up")
	}
	p.mu.Unlock()
	return p.Store.CheckJoinToken(hash, now)
}

// A credGate slot released without defer is LOST when the store call panics.
// net/http recovers a handler panic per connection, so the process survives and
// nothing looks broken — but the slot never comes back, and maxCredLookups of
// them leaves the gate permanently full. Every tenant-API-key authentication and
// every node join then blocks until its own request context expires: a silent,
// permanent, total auth outage for everyone except the admin token and the
// node-token index. The panic is a bug; turning it into a fleet-wide lockout is
// a much worse one.
func TestPanickingCredentialLookupDoesNotStrandItsGateSlot(t *testing.T) {
	tmp := t.TempDir()
	sqlite, err := store.OpenSQLite(filepath.Join(tmp, "hearth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	ps := &panicStore{Store: sqlite}
	srv := newJoinServerStore(t, ps, tmp)
	h := srv.Handler()

	// A tenant with a real key, so the final assertion is about the GATE and
	// not about whether the credential is any good.
	w := doRequest(h, "POST", "/api/v1/tenants", `{"name":"victim"}`, adminAuth)
	if w.Code != 201 {
		t.Fatalf("create tenant: %d %s", w.Code, w.Body)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("tenant body: %v", err)
	}

	panicked := func(call func()) bool {
		recovered := false
		func() {
			defer func() {
				if r := recover(); r != nil {
					recovered = true
				}
			}()
			call()
		}()
		return recovered
	}

	// Enough panics to exhaust the gate, on each of the two gated sites.
	ps.armKey(maxCredLookups)
	for i := 0; i < maxCredLookups; i++ {
		if !panicked(func() { doRequest(h, "GET", "/api/v1/sandboxes", "", "Bearer "+bogusAPIKey(i)) }) {
			t.Fatalf("bearer-gate lookup %d never reached the panicking store: the test is not exercising the path it claims to", i)
		}
	}
	if held := len(srv.credLookups.slots); held != 0 {
		t.Fatalf("%d of %d credential-lookup slots never came back after a panicking bearer-gate lookup: the gate is permanently narrowed",
			held, maxCredLookups)
	}

	ps.armJoin(maxCredLookups)
	for i := 0; i < maxCredLookups; i++ {
		if !panicked(func() { joinWith(srv, "hearth_jt_"+strings.Repeat("a", 48), validPubKeyA, "w1") }) {
			t.Fatalf("join-token lookup %d never reached the panicking store: the test is not exercising the path it claims to", i)
		}
	}
	if held := len(srv.credLookups.slots); held != 0 {
		t.Fatalf("%d of %d credential-lookup slots never came back after a panicking join-token lookup: the gate is permanently narrowed",
			held, maxCredLookups)
	}

	// And the gate still admits: a real key authenticates without waiting out
	// its context. (A leak makes acquire block, which surfaces here as ok=false
	// rather than as a hung test.)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, ok := srv.authenticate(ctx, "Bearer "+created.APIKey); !ok {
		t.Fatal("a valid tenant key could not be authenticated after the panics: the credential gate is full of stranded slots")
	}
}

// ---- Node-record growth ----

// agentRegister validated the hostname only as non-empty, and RegisterNode KEYS
// on hostname: every unseen one appends a node record, and nothing anywhere
// prunes them. Pinning the ADDRESS to the caller's own credential was not
// enough, because the hostname is the other half of the key — so one compromised
// worker could grow srv.st.Nodes without bound, each record a full SaveSnapshot,
// a pushPoolsTo goroutine, and another entry every registration check, FindNode
// and PickNode scans linearly under the global state lock.
func TestOneNodeCredentialCannotAccumulateNodeRecords(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	h := srv.Handler()
	const addr = "203.0.113.9:9090"

	// Enroll the worker with the admin token, the way an operator does.
	body := `{"hostname":"worker-1","addr":"` + addr + `","cpus":1,"mem_total_mib":512,"join_token":"` + mintJoinToken(t, srv) + `"}`
	w := doRequest(h, "POST", "/api/v1/agents/register", body, adminAuth)
	if w.Code != 200 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body)
	}
	var enrolled struct {
		AgentToken string `json:"agent_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &enrolled); err != nil {
		t.Fatalf("enroll body: %v", err)
	}
	nodeAuth := "Bearer " + enrolled.AgentToken

	// Its own re-registration is idempotent and must keep working.
	same := `{"hostname":"worker-1","addr":"` + addr + `","cpus":1,"mem_total_mib":512}`
	if w := doRequest(h, "POST", "/api/v1/agents/register", same, nodeAuth); w.Code != 200 {
		t.Fatalf("node re-registering itself: %d %s, want 200", w.Code, w.Body)
	}

	// Fresh hostnames against its own (correctly pinned) address must not
	// append records.
	for i := 0; i < 25; i++ {
		fresh := fmt.Sprintf(`{"hostname":"filler-%d","addr":"%s","cpus":1,"mem_total_mib":512}`, i, addr)
		if w := doRequest(h, "POST", "/api/v1/agents/register", fresh, nodeAuth); w.Code != 403 {
			t.Fatalf("registering hostname filler-%d against its own address: got %d %s, want 403", i, w.Code, w.Body)
		}
	}
	srv.st.Lock()
	n := len(srv.st.Nodes)
	srv.st.Unlock()
	if n != 1 {
		t.Errorf("node records after 25 fresh hostnames from one credential: %d, want 1", n)
	}
}

// The pin above is only worth the critical section it is checked in, and the
// test that shipped with it was SEQUENTIAL — which is exactly why the race
// survived review. The check took the state lock, released it, and RegisterNode
// then took it again to append: two acquisitions, so N concurrent registrations
// from one credential could all pass the check before any of them appended, and
// the table permanently gained a record per compromised credential.
//
// The precondition below is the one the scan cannot refuse from: a credential
// that exists with NO node record yet. It is not contrived — /api/v1/nodes/join
// mints a worker's credential and creates no node record at all, so every
// overlay-enrolled worker's first registration starts here.
//
// Sequential registration is already covered above; this asserts the property
// under concurrency, where it actually had to hold. Real handler, real auth, real
// store — and, being a race probe, it needs the scheduler's cooperation: measured
// against the two-acquisition shape it reproduces in ~4 of 5 runs at 200 rounds
// under `go test -race`, which is in the shipped verification suite, and rarely
// without it (an uninstrumented Go mutex lets the releasing goroutine barge back
// in, and the gap here is one branch wide). -race is the detector for this class,
// and 200 rounds is what makes it a guard rather than a coin flip.
func TestOneNodeCredentialCannotRaceInASecondNodeRecord(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	h := srv.Handler()
	const addr = "203.0.113.9:9090"

	// Enroll once with the admin token so this address has its own credential.
	body := `{"hostname":"seed","addr":"` + addr + `","cpus":1,"mem_total_mib":512,"join_token":"` + mintJoinToken(t, srv) + `"}`
	w := doRequest(h, "POST", "/api/v1/agents/register", body, adminAuth)
	if w.Code != 200 {
		t.Fatalf("enroll: %d %s", w.Code, w.Body)
	}
	var enrolled struct {
		AgentToken string `json:"agent_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &enrolled); err != nil {
		t.Fatalf("enroll body: %v", err)
	}
	nodeAuth := "Bearer " + enrolled.AgentToken

	const rounds = 200
	const racers = 32
	for round := 0; round < rounds; round++ {
		// Back to "credential, no record" — the wg-join starting state.
		srv.st.Lock()
		srv.st.Nodes = nil
		srv.st.Unlock()

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b := fmt.Sprintf(`{"hostname":"r%d-w%d","addr":%q,"cpus":1,"mem_total_mib":512}`, round, i, addr)
				<-start
				doRequest(h, "POST", "/api/v1/agents/register", b, nodeAuth)
			}(i)
		}
		close(start) // release them together, into the same window
		wg.Wait()

		srv.st.Lock()
		n := len(srv.st.Nodes)
		hostnames := make([]string, 0, n)
		for _, node := range srv.st.Nodes {
			hostnames = append(hostnames, node.Hostname)
		}
		srv.st.Unlock()
		if n != 1 {
			t.Fatalf("round %d: %d node records from ONE node credential after %d concurrent registrations (%v); want exactly 1 — the pin and the append are not in the same critical section",
				round, n, racers, hostnames)
		}
	}
}

// The hostname is a map key, a log field and a console label. agentRegister
// bounded neither its length nor its charset, unlike nodeJoin's cap.
func TestAgentRegisterBoundsTheHostname(t *testing.T) {
	srv := newJoinServerWgIP(t, "")
	h := srv.Handler()
	for _, bad := range []string{
		"",
		strings.Repeat("a", maxHostnameLen+1),
		"worker one",       // space
		"worker\n1",        // control byte: log injection
		"worker/../../etc", // path-ish
		"worker\x00",
	} {
		body, err := json.Marshal(map[string]any{
			"hostname": bad, "addr": "203.0.113.9:9090", "cpus": 1, "mem_total_mib": 512,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if w := doRequest(h, "POST", "/api/v1/agents/register", string(body), adminAuth); w.Code != 400 {
			t.Errorf("register hostname %q: got %d %s, want 400", bad, w.Code, w.Body)
		}
	}
	// The bound must accept what a real host name looks like.
	for _, good := range []string{"w", "worker-1", "kata_lab.1", strings.Repeat("a", maxHostnameLen)} {
		if !validNodeHostname(good) {
			t.Errorf("validNodeHostname(%q) = false, want true", good)
		}
	}
}

// ---- The state lock and panics ----

// The credGate lesson, applied to the lock that actually matters. A slot
// released without defer is lost when the code in between panics; the GLOBAL
// STATE LOCK released without defer is worse, because everything needs it.
//
// net/http recovers a handler panic and keeps the process alive, so hearthd goes
// on answering its listener and passing a TCP health check while srv.st is held
// forever: no create, no exec, no delete, no sweep, no /metrics. A silent total
// control-plane outage from one panic in one request.
//
// The panic here is induced where a real one could land: recordUsage is called
// UNDER the state lock (it builds the event from the sandbox's live fields), so a
// store that blows up inside AppendUsage panics inside the critical section — the
// same class of thing as the driver bug the credGate test stands in for.
//
// lockaudit_test.go is the structural half of this: it keeps new bare regions out.
// This is the behavioural half — it shows what such a region actually costs.
func TestAPanicUnderTheStateLockDoesNotDeadlockTheControlPlane(t *testing.T) {
	tmp := t.TempDir()
	sqlite, err := store.OpenSQLite(filepath.Join(tmp, "hearth.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { sqlite.Close() })
	ps := &panicStore{Store: sqlite}
	srv := newJoinServerStore(t, ps, tmp)
	h := srv.Handler()

	// A running sandbox on an agent that says yes to everything, so the request
	// below reaches the state-lock region rather than failing before it.
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(agent.Close)
	now := time.Now().Unix()
	nodeID := srv.st.RegisterNode("worker-1", agent.URL, 4, 8192, now)
	srv.st.Lock()
	sb := srv.st.CreateSandbox("victim", "default", nodeID, 1, 256, now)
	id := sb.ID
	srv.st.Unlock()
	srv.st.SetSandboxState(id, model.StateRunning)

	// Sleep stamps SleptAt and records the "slept" event under the lock.
	ps.armUsage(1)
	blew := false
	func() {
		defer func() {
			if recover() != nil {
				blew = true
			}
		}()
		doRequest(h, "POST", "/api/v1/sandboxes/"+id+"/sleep", "", adminAuth)
	}()
	if !blew {
		t.Fatal("the store call never panicked: this test is not exercising the path it claims to")
	}

	// The control plane must still be able to take the state lock. Run it off
	// the test goroutine so a deadlock is a FAILURE and not a hung test binary.
	done := make(chan int, 1)
	go func() {
		done <- doRequest(h, "GET", "/api/v1/sandboxes", "", adminAuth).Code
	}()
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("list sandboxes after the panic: %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a request that needs the state lock never completed after a panic inside a locked region: srv.st is still held, and every request from here on blocks forever while the process stays up and healthy-looking")
	}
}
