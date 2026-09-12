package server_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/server"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
)

// testToken is the admin token the API tests authenticate with. An empty
// configured token no longer means "open" (C1: auth fails closed), so every
// /api/ test has to present a real credential.
const (
	testToken = "testtoken"
	testAuth  = "Bearer " + testToken
)

// newTestStore opens a throwaway SQLite store in the test's temp dir.
func newTestStore(t *testing.T, tmp string) store.Store {
	t.Helper()
	db, err := store.OpenSQLite(filepath.Join(tmp, "felucca.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newTestServer(t *testing.T, token string) (*server.Server, *state.State) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     token,
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
	}
	st := state.New()
	return server.New(cfg, st, newTestStore(t, tmp)), st
}

// newProxiedTestServer is newTestServer with declared reverse proxies — the
// deployments where a forwarded client address is honoured.
func newProxiedTestServer(t *testing.T, token string, proxies []string) *server.Server {
	t.Helper()
	return newProxiedServer(t, token, proxies, false)
}

// newProxiedTestServerXRealIP additionally opts into X-Real-IP — the setting an
// operator must name explicitly, and only for a proxy that rewrites it.
func newProxiedTestServerXRealIP(t *testing.T, token string, proxies []string) *server.Server {
	t.Helper()
	return newProxiedServer(t, token, proxies, true)
}

func newProxiedServer(t *testing.T, token string, proxies []string, trustXRealIP bool) *server.Server {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:          token,
		UIDir:          tmp,
		StatePath:      filepath.Join(tmp, "state.json"),
		DBPath:         filepath.Join(tmp, "felucca.db"),
		TrustedProxies: proxies,
		TrustXRealIP:   trustXRealIP,
	}
	return server.New(cfg, state.New(), newTestStore(t, tmp))
}

func do(h http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	return doFrom(h, method, path, body, auth, "")
}

// doFrom is do with a chosen peer address — the key the credential throttle
// counts failures under.
func doFrom(h http.Handler, method, path, body, auth, remoteAddr string) *httptest.ResponseRecorder {
	return doWith(h, method, path, body, auth, remoteAddr, nil)
}

// doWith is doFrom plus request headers — the forwarded-client headers the
// throttle keys on when the peer is a declared reverse proxy.
func doWith(h http.Handler, method, path, body, auth, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ---- Healthz ----

func TestHealthz(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/healthz", "", "")
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	var m map[string]bool
	json.Unmarshal(w.Body.Bytes(), &m)
	if !m["ok"] {
		t.Error("ok should be true")
	}
}

// ---- Bearer auth middleware ----

func TestAuthNoTokenIsClosed(t *testing.T) {
	// An unconfigured token authorizes nobody: open mode is an explicit
	// operator decision, never the fallback for a token that failed to load.
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	if w.Code != 401 {
		t.Errorf("empty token must not open the API: status %d", w.Code)
	}
	w2 := do(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer ")
	if w2.Code != 401 {
		t.Errorf("empty bearer against empty token: status %d", w2.Code)
	}
}

func TestAuthInsecureNoAuthOpensTheAPI(t *testing.T) {
	// The one path to an open API: named explicitly by the operator, never
	// inferred from a missing token.
	tmp := t.TempDir()
	cfg := &config.Config{
		InsecureNoAuth: true,
		UIDir:          tmp,
		StatePath:      filepath.Join(tmp, "state.json"),
		DBPath:         filepath.Join(tmp, "felucca.db"),
	}
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))
	if w := do(srv.Handler(), "GET", "/api/v1/nodes", "", ""); w.Code != 200 {
		t.Errorf("insecure-no-auth: status %d", w.Code)
	}
}

func TestAuthWithTokenMissing(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "")
	if w.Code != 401 {
		t.Errorf("missing auth: status %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "unauthorized" {
		t.Errorf("error body: %v", m)
	}
}

func TestAuthWithTokenWrong(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer wrongtoken")
	if w.Code != 401 {
		t.Errorf("wrong auth: status %d", w.Code)
	}
}

func TestAuthWithTokenCorrect(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken")
	if w.Code != 200 {
		t.Errorf("correct auth: status %d", w.Code)
	}
}

func TestBearerGateThrottlesGuessing(t *testing.T) {
	// Guessing the operator-chosen admin token must not run at line rate:
	// after a handful of free attempts the source's FAILURES are answered with
	// a backoff instead of a plain 401.
	srv, _ := newTestServer(t, "mytoken")
	const attacker = "203.0.113.9:5000"

	var blockedAt int
	for i := 1; i <= 40; i++ {
		w := doFrom(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer guess", attacker)
		if w.Code == 429 {
			blockedAt = i
			if w.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "too many failed attempts" {
				t.Errorf("error body: %v", m)
			}
			break
		}
		if w.Code != 401 {
			t.Fatalf("attempt %d: expected 401, got %d", i, w.Code)
		}
	}
	if blockedAt == 0 {
		t.Fatal("40 wrong-token attempts from one source were all evaluated")
	}

	// Further WRONG guesses from that source stay in backoff.
	if w := doFrom(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer guess2", attacker); w.Code != 429 {
		t.Errorf("throttled source guessing again: got %d, want 429", w.Code)
	}

	// But the RIGHT credential from that same address is served. The throttle
	// evaluates after authentication precisely so it can never be the thing
	// standing between an operator and their own control plane — an attacker
	// who cannot authenticate must not be able to deny service to one who can.
	if w := doFrom(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken", attacker); w.Code != 200 {
		t.Errorf("valid credential from a throttled address: got %d, want 200", w.Code)
	}

	// Another source is unaffected.
	if w := doFrom(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken", "198.51.100.4:5000"); w.Code != 200 {
		t.Errorf("unrelated source: got %d", w.Code)
	}
}

// The vulnerability the L6 remediation introduced, stated as a test: with the
// backoff evaluated BEFORE authentication and the shipped configuration's one
// shared 127.0.0.1 key, a single unauthenticated client sending one bad bearer
// at a time 429'd the entire control plane — every operator, the console, and
// every worker node — for as long as it cared to keep going, because each
// failure refreshed the decay clock and the backoff never expired.
func TestGuessingCannotLockOutValidCredentials(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken") // no trusted proxies: the shipped shape
	h := srv.Handler()
	const shared = "127.0.0.1:41000" // proxy peer: attacker, operator, node alike

	for i := 1; i <= 200; i++ {
		// The attacker's own attempt may be 401 or 429 — either is fine, it
		// never authenticates.
		switch w := doFrom(h, "GET", "/api/v1/nodes", "", "Bearer guess", shared); w.Code {
		case 401, 429:
		default:
			t.Fatalf("guess %d: expected 401 or 429, got %d", i, w.Code)
		}
		// Interleaved: an operator with the real token, and a tenant-facing
		// route. Neither may ever be refused for someone else's guessing.
		if w := doFrom(h, "GET", "/api/v1/nodes", "", "Bearer mytoken", shared); w.Code != 200 {
			t.Fatalf("after %d failed guesses on the shared key, a valid admin token got %d (control plane locked out)", i, w.Code)
		}
		if w := doFrom(h, "GET", "/api/v1/sandboxes", "", "Bearer mytoken", shared); w.Code != 200 {
			t.Fatalf("after %d failed guesses, a valid credential got %d on /sandboxes", i, w.Code)
		}
	}
}

// The shipped topology binds feluccad to loopback behind a reverse proxy, so
// every client arrives as one address. Wiping the record on a success made
// that a bypass: the console's own authenticated poll reset whatever a guesser
// sharing the key had accumulated, and the guard never engaged.
func TestSuccessDoesNotClearAnotherClientsFailures(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken")
	h := srv.Handler()
	const shared = "127.0.0.1:41000" // proxy peer: attacker and console alike

	blocked := false
	for i := 1; i <= 60 && !blocked; i++ {
		switch w := doFrom(h, "GET", "/api/v1/nodes", "", "Bearer guess", shared); w.Code {
		case 429:
			blocked = true
		case 401:
		default:
			t.Fatalf("guess %d: expected 401 or 429, got %d", i, w.Code)
		}
		// The console polls between guesses with a valid token. It is served
		// every time — a correct credential is never refused for someone
		// else's failures — and what must never happen is that poll clearing
		// the guesser's record.
		if w2 := doFrom(h, "GET", "/api/v1/nodes", "", "Bearer mytoken", shared); w2.Code != 200 {
			t.Fatalf("console poll %d: expected 200, got %d", i, w2.Code)
		}
	}
	if !blocked {
		t.Fatal("60 guesses interleaved with successful polls were all evaluated: a success still resets the counter")
	}
}

// With a proxy declared, the forwarded client is the key: one client's
// guessing must not throttle every other client behind the same proxy.
func TestThrottleKeysOnForwardedClientBehindTrustedProxy(t *testing.T) {
	srv := newProxiedTestServer(t, "mytoken", []string{"127.0.0.1/32"})
	h := srv.Handler()
	const proxy = "127.0.0.1:41000"
	xff := func(ip string) map[string]string { return map[string]string{"X-Forwarded-For": ip} }

	blocked := false
	for i := 1; i <= 40; i++ {
		w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", proxy, xff("203.0.113.7"))
		if w.Code == 429 {
			blocked = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("guess %d: expected 401, got %d", i, w.Code)
		}
	}
	if !blocked {
		t.Fatal("40 guesses from one forwarded client were all evaluated")
	}
	// The operator, arriving through the same proxy, is untouched.
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer mytoken", proxy, xff("198.51.100.9")); w.Code != 200 {
		t.Errorf("unrelated client behind the same proxy: got %d, want 200", w.Code)
	}
	// And the guesser keeps getting the backoff — for its guesses.
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", proxy, xff("203.0.113.7")); w.Code != 429 {
		t.Errorf("throttled client guessing again: got %d, want 429", w.Code)
	}
	// Its own valid credential, though, is still served: the guard shapes
	// failures, it never gates success.
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer mytoken", proxy, xff("203.0.113.7")); w.Code != 200 {
		t.Errorf("throttled client presenting the real token: got %d, want 200", w.Code)
	}
}

// The right-most hop is the one the proxy chain can attest to. The left-most
// entry is written by the caller, so keying on it would let a guesser rotate
// its own key (and forge someone else's) at will.
func TestThrottleUsesRightmostUntrustedForwardedHop(t *testing.T) {
	srv := newProxiedTestServer(t, "mytoken", []string{"127.0.0.1/32", "10.9.0.0/16"})
	h := srv.Handler()
	const proxy = "127.0.0.1:41000"
	// Caller-written junk, then the real client, then one of our own proxies.
	forged := func(claim string) map[string]string {
		return map[string]string{"X-Forwarded-For": claim + ", 203.0.113.7, 10.9.0.4"}
	}

	blocked := false
	for i := 1; i <= 40; i++ {
		w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", proxy, forged("198.51.100.1"))
		if w.Code == 429 {
			blocked = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("guess %d: expected 401, got %d", i, w.Code)
		}
	}
	if !blocked {
		t.Fatal("40 guesses were all evaluated")
	}
	// Rewriting the left-most entry does not buy a fresh budget.
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", proxy, forged("198.51.100.222")); w.Code != 429 {
		t.Errorf("rotated left-most XFF entry: got %d, want 429 (same real client)", w.Code)
	}
	// A genuinely different client is not blocked by it either — which is what
	// keying on the left-most entry would have caused.
	other := map[string]string{"X-Forwarded-For": "198.51.100.1, 198.51.100.9, 10.9.0.4"}
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer mytoken", proxy, other); w.Code != 200 {
		t.Errorf("different real client sharing the forged prefix: got %d, want 200", w.Code)
	}
}

// A forwarded header from a peer that is not a declared proxy is just a
// caller-supplied string: honouring it would let anyone mint an unlimited
// supply of throttle keys.
func TestThrottleIgnoresForwardedHeaderFromUntrustedPeer(t *testing.T) {
	srv, _ := newTestServer(t, "mytoken") // no proxies declared
	h := srv.Handler()
	const attacker = "203.0.113.9:5000"

	blocked := false
	for i := 1; i <= 40; i++ {
		// A fresh claimed client address on every attempt.
		hdr := map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i%250+1)}
		w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", attacker, hdr)
		if w.Code == 429 {
			blocked = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("guess %d: expected 401, got %d", i, w.Code)
		}
	}
	if !blocked {
		t.Fatal("rotating X-Forwarded-For from an undeclared peer bought unlimited guesses")
	}
	// X-Real-IP is no better a forgery.
	if w := doWith(h, "GET", "/api/v1/nodes", "", "Bearer guess", attacker,
		map[string]string{"X-Real-IP": "198.51.100.77"}); w.Code != 429 {
		t.Errorf("forged X-Real-IP from an undeclared peer: got %d, want 429", w.Code)
	}
}

// X-Real-IP carries no chain of custody: feluccad cannot tell a value the proxy
// wrote from one it forwarded verbatim. nginx with a bare `proxy_pass` — a
// configuration DEPLOYMENT.md sanctions — forwards the client's own X-Real-IP
// untouched, which handed the client the very key it must not be able to
// choose. So the header is ignored unless the operator opts in by name, and the
// default deployment keys on the (shared, unforgeable) proxy address instead.
func TestXRealIPIsIgnoredUnlessOperatorOptsIn(t *testing.T) {
	const proxy = "127.0.0.1:41000"
	realIP := func(i int) map[string]string {
		return map[string]string{"X-Real-IP": fmt.Sprintf("198.51.100.%d", i%250+1)}
	}

	// Default: a proxy is declared but trust_x_real_ip is not set. A client
	// rotating X-Real-IP behind that proxy gets no fresh budget — every attempt
	// lands on the proxy's own key.
	off := newProxiedTestServer(t, "mytoken", []string{"127.0.0.1/32"})
	blocked := false
	for i := 1; i <= 40 && !blocked; i++ {
		w := doWith(off.Handler(), "GET", "/api/v1/nodes", "", "Bearer guess", proxy, realIP(i))
		switch w.Code {
		case 429:
			blocked = true
		case 401:
		default:
			t.Fatalf("guess %d: expected 401 or 429, got %d", i, w.Code)
		}
	}
	if !blocked {
		t.Fatal("rotating X-Real-IP chose its own throttle key with trust_x_real_ip off")
	}
	// And an operator behind that same proxy is still served throughout.
	if w := doWith(off.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken", proxy, realIP(1)); w.Code != 200 {
		t.Errorf("valid credential behind the throttled proxy key: got %d, want 200", w.Code)
	}

	// Opted in: the header is honoured, so each claimed client is its own key.
	// This is the operator's explicit decision, valid only for a proxy that
	// rewrites the header on every request.
	on := newProxiedTestServerXRealIP(t, "mytoken", []string{"127.0.0.1/32"})
	for i := 1; i <= 40; i++ {
		if w := doWith(on.Handler(), "GET", "/api/v1/nodes", "", "Bearer guess", proxy, realIP(i)); w.Code != 401 {
			t.Fatalf("trust_x_real_ip on, guess %d from a distinct client: got %d, want 401", i, w.Code)
		}
	}
}

func TestJoinRouteThrottlesGuessing(t *testing.T) {
	// The join route is reached before the bearer gate, so it carries its own
	// share of the same guard.
	srv, _ := newTestServer(t, "mytoken")
	const attacker = "203.0.113.10:5000"

	blocked := false
	for i := 1; i <= 40; i++ {
		w := doFrom(srv.Handler(), "POST", "/api/v1/nodes/join",
			`{"pubkey":"x","hostname":"w"}`, "Bearer nope", attacker)
		if w.Code == 429 {
			blocked = true
			break
		}
		if w.Code != 401 {
			t.Fatalf("attempt %d: expected 401, got %d body=%s", i, w.Code, w.Body)
		}
	}
	if !blocked {
		t.Fatal("40 join attempts from one source were all evaluated")
	}
	if w := doFrom(srv.Handler(), "GET", "/api/v1/nodes", "", "Bearer mytoken", "198.51.100.5:5000"); w.Code != 200 {
		t.Errorf("unrelated source: got %d", w.Code)
	}
}

// ---- 404 for unknown routes ----

func TestUnknownRoute(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	tests := []struct {
		method, path string
	}{
		{"GET", "/api/v1/unknown"},
		{"POST", "/api/v1/nodes"},      // wrong method on known path → 404 (no 405)
		{"DELETE", "/api/v1/sandboxes"}, // wrong method → 404
		{"PUT", "/api/v1/sandboxes"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			w := do(srv.Handler(), tc.method, tc.path, "", testAuth)
			if w.Code != 404 {
				t.Errorf("expected 404, got %d", w.Code)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "not found" {
				t.Errorf("error body: %v", m)
			}
		})
	}
}

// ---- Content-Length on responses ----

func TestContentLengthSet(t *testing.T) {
	srv, _ := newTestServer(t, "")
	w := do(srv.Handler(), "GET", "/healthz", "", "")
	cl := w.Header().Get("Content-Length")
	if cl == "" {
		t.Error("Content-Length not set")
	}
	if cl != "11" { // {"ok":true} = 11 bytes
		t.Errorf("Content-Length: %q", cl)
	}
}

// ---- Nodes ----

func TestListNodesEmpty(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "GET", "/api/v1/nodes", "", testAuth)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &m)
	var nodes []json.RawMessage
	json.Unmarshal(m["nodes"], &nodes)
	if len(nodes) != 0 {
		t.Errorf("expected empty nodes, got %d", len(nodes))
	}
}

func TestAgentRegisterAndList(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	// Register a node.
	w := do(srv.Handler(), "POST", "/api/v1/agents/register",
		`{"hostname":"h1","addr":"1.2.3.4:9090","cpus":4,"mem_total_mib":8192}`, testAuth)
	if w.Code != 200 {
		t.Fatalf("register: %d body=%s", w.Code, w.Body)
	}
	var reg map[string]string
	json.Unmarshal(w.Body.Bytes(), &reg)
	id := reg["id"]
	if id == "" {
		t.Fatal("no id in register response")
	}

	// List nodes.
	w2 := do(srv.Handler(), "GET", "/api/v1/nodes", "", testAuth)
	var nl map[string][]map[string]json.RawMessage
	json.Unmarshal(w2.Body.Bytes(), &nl)
	if len(nl["nodes"]) != 1 {
		t.Errorf("expected 1 node, got %d", len(nl["nodes"]))
	}
}

func TestAgentRegisterIdempotent(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	body := `{"hostname":"h1","addr":"10.100.0.7:9090","cpus":2,"mem_total_mib":1024}`
	w1 := do(srv.Handler(), "POST", "/api/v1/agents/register", body, testAuth)
	w2 := do(srv.Handler(), "POST", "/api/v1/agents/register", body, testAuth)

	var r1, r2 map[string]string
	json.Unmarshal(w1.Body.Bytes(), &r1)
	json.Unmarshal(w2.Body.Bytes(), &r2)
	if r1["id"] != r2["id"] {
		t.Errorf("idempotent register: got different ids %q vs %q", r1["id"], r2["id"])
	}
}

func TestAgentRegisterRejectsUnroutableAddr(t *testing.T) {
	// A registration aims feluccad's own admin-token-carrying client at the
	// given address, so names and non-routable addresses must not enrol.
	srv, st := newTestServer(t, testToken)
	for _, addr := range []string{
		"",
		"attacker.example.com:80",
		"127.0.0.1:6379",
		"169.254.169.254:80",
		"0.0.0.0:9090",
		"224.0.0.1:9090",
		"http://attacker.example.com/v1/vms",
		"[::1]:9090",
	} {
		body := fmt.Sprintf(`{"hostname":"rogue","addr":%q,"cpus":64,"mem_total_mib":999999}`, addr)
		w := do(srv.Handler(), "POST", "/api/v1/agents/register", body, testAuth)
		if w.Code != 400 {
			t.Errorf("addr %q: expected 400, got %d body=%s", addr, w.Code, w.Body)
			continue
		}
		var m map[string]string
		json.Unmarshal(w.Body.Bytes(), &m)
		if m["error"] != "invalid addr" {
			t.Errorf("addr %q error body: %v", addr, m)
		}
	}
	st.Lock()
	n := len(st.Nodes)
	st.Unlock()
	if n != 0 {
		t.Errorf("rejected registrations enrolled %d nodes", n)
	}
}

func TestAgentRegisterRequiresOverlayAddrWhenWgConfigured(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     testToken,
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
		WgIP:      "10.100.0.1/16",
	}
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))

	w := do(srv.Handler(), "POST", "/api/v1/agents/register",
		`{"hostname":"off-overlay","addr":"203.0.113.7:9090","cpus":4,"mem_total_mib":8192}`, testAuth)
	if w.Code != 400 {
		t.Errorf("off-overlay addr: expected 400, got %d body=%s", w.Code, w.Body)
	}

	w2 := do(srv.Handler(), "POST", "/api/v1/agents/register",
		`{"hostname":"on-overlay","addr":"10.100.0.5:9090","cpus":4,"mem_total_mib":8192}`, testAuth)
	if w2.Code != 200 {
		t.Errorf("overlay addr: expected 200, got %d body=%s", w2.Code, w2.Body)
	}
}

func TestHeartbeatUnknown(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "POST", "/api/v1/agents/heartbeat",
		`{"id":"node-00000000-99","mem_free_mib":1000,"vm_count":0,"pool_size":0}`, testAuth)
	if w.Code != 404 {
		t.Errorf("unknown heartbeat: %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "unknown node" {
		t.Errorf("error body: %v", m)
	}
}

// ---- Sandboxes ----

func TestListSandboxesEmpty(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "GET", "/api/v1/sandboxes", "", testAuth)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var m map[string][]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &m)
	if len(m["sandboxes"]) != 0 {
		t.Error("expected empty sandboxes")
	}
}

func TestGetSandboxNotFound(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "GET", "/api/v1/sandboxes/sb-00000000-99", "", testAuth)
	if w.Code != 404 {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestCreateSandboxNoNode(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "POST", "/api/v1/sandboxes",
		`{"name":"test","namespace":"default"}`, testAuth)
	if w.Code != 503 {
		t.Errorf("expected 503, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "no ready node" {
		t.Errorf("error body: %v", m)
	}
}

func TestCreateSandboxNameAndNamespaceBounds(t *testing.T) {
	// No node is registered, so a name that passes validation reaches the
	// scheduler and 503s — the two outcomes are unambiguous.
	srv, _ := newTestServer(t, testToken)
	long := strings.Repeat("n", 65)

	bad := []struct{ body, want string }{
		{`{"name":"<img src=x onerror=alert(1)>"}`, "invalid name"},
		{`{"name":"a\u0000b"}`, "invalid name"},
		{`{"name":"café"}`, "invalid name"},
		{`{"name":"` + long + `"}`, "invalid name"},
		{`{"name":"fine","namespace":"<script>"}`, "invalid namespace"},
		{`{"name":"fine","namespace":"` + long + `"}`, "invalid namespace"},
		{`{"name":"a&b"}`, "invalid name"},
	}
	for _, tc := range bad {
		w := do(srv.Handler(), "POST", "/api/v1/sandboxes", tc.body, testAuth)
		if w.Code != 400 {
			t.Errorf("%s: expected 400, got %d body=%s", tc.body, w.Code, w.Body)
			continue
		}
		var m map[string]string
		json.Unmarshal(w.Body.Bytes(), &m)
		if m["error"] != tc.want {
			t.Errorf("%s: error %v, want %q", tc.body, m, tc.want)
		}
	}

	for _, good := range []string{
		`{"name":"my sandbox-1.0_x"}`,
		`{"name":"n","namespace":"team/proj"}`,
	} {
		w := do(srv.Handler(), "POST", "/api/v1/sandboxes", good, testAuth)
		if w.Code != 503 {
			t.Errorf("%s: expected 503 (validation passed, no node), got %d body=%s", good, w.Code, w.Body)
		}
	}
}

func TestForkChildNameBounds(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent should not be called for an invalid child name")
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/fork", sbID),
		`{"name":"<img src=x onerror=alert(1)>"}`, testAuth)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "invalid name" {
		t.Errorf("error body: %v", m)
	}
}

// ---- Static serving ----

func TestStaticTraversalRejected(t *testing.T) {
	srv, _ := newTestServer(t, "")
	for _, path := range []string{"/../etc/passwd", "/foo/../bar", "/..%2Fetc"} {
		w := do(srv.Handler(), "GET", path, "", "")
		// Traversal: 404 with JSON error.
		if w.Code != 404 {
			// Note: /..%2F might not decode to ".." — only check literal ".."
			continue
		}
	}
	// Literal .. in path must be rejected.
	w := do(srv.Handler(), "GET", "/foo/../bar.html", "", "")
	if w.Code != 404 {
		t.Errorf("traversal path: expected 404, got %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not found" {
		t.Errorf("traversal error body: %v", m)
	}
}

func TestStaticIndexFallback(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>hi</html>"), 0o644)
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))

	// Unknown path should fall back to index.html (SPA routing).
	w := do(srv.Handler(), "GET", "/some/spa/route", "", "")
	if w.Code != 200 {
		t.Errorf("SPA fallback: status %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html>") {
		t.Error("SPA fallback: expected index.html content")
	}
	ct := w.Header().Get("Content-Type")
	if ct != "text/html" {
		t.Errorf("SPA fallback Content-Type: %q", ct)
	}
}

func TestStaticUINotFound(t *testing.T) {
	srv, _ := newTestServer(t, "") // UIDir is empty tmp dir, no index.html
	w := do(srv.Handler(), "GET", "/", "", "")
	if w.Code != 404 {
		t.Errorf("no ui: status %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "ui not found" {
		t.Errorf("error body: %v", m)
	}
}

func TestStaticSecurityHeaders(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{UIDir: tmp, StatePath: filepath.Join(tmp, "state.json")}
	os.WriteFile(filepath.Join(tmp, "index.html"), []byte("<html>hi</html>"), 0o644)
	os.WriteFile(filepath.Join(tmp, "app.js"), []byte("//"), 0o644)
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))

	const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; font-src 'self'; form-action 'none'; " +
		"frame-ancestors 'none'; base-uri 'none'"
	for _, path := range []string{"/", "/index.html", "/app.js", "/spa/route"} {
		w := do(srv.Handler(), "GET", path, "", "")
		h := w.Header()
		if got := h.Get("Content-Security-Policy"); got != wantCSP {
			t.Errorf("%s CSP:\n got %q\nwant %q", path, got, wantCSP)
		}
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options: %q", path, got)
		}
		if got := h.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s Referrer-Policy: %q", path, got)
		}
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s X-Frame-Options: %q", path, got)
		}
	}
}

func TestAPIResponseSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	for _, tc := range []struct{ path, auth string }{
		{"/api/v1/sandboxes", testAuth},
		{"/api/v1/sandboxes", ""}, // the 401 body is a response too
		{"/healthz", ""},
	} {
		w := do(srv.Handler(), "GET", tc.path, "", tc.auth)
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s X-Content-Type-Options: %q", tc.path, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s Referrer-Policy: %q", tc.path, got)
		}
	}
}

func TestMIMETypes(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{UIDir: tmp, StatePath: filepath.Join(tmp, "state.json")}
	files := map[string]string{
		"app.js":   "application/javascript",
		"style.css": "text/css",
		"data.json": "application/json",
		"img.svg":  "image/svg+xml",
		"img.png":  "image/png",
		"file.bin": "application/octet-stream",
	}
	for name := range files {
		os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644)
	}
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))
	for name, wantCT := range files {
		w := do(srv.Handler(), "GET", "/"+name, "", "")
		got := w.Header().Get("Content-Type")
		if got != wantCT {
			t.Errorf("%s: Content-Type %q, want %q", name, got, wantCT)
		}
	}
}

// ---- Metrics ----

func TestMetricsRequiresAdminToken(t *testing.T) {
	// The scrape carries worker hostnames and fleet counts and takes the
	// global state lock, so it sits behind the bearer gate like every other
	// fleet-wide route: no credential 401s, a tenant key 404s, admin scrapes.
	srv, _ := newTestServer(t, testToken)
	if w := do(srv.Handler(), "GET", "/metrics", "", ""); w.Code != 401 {
		t.Errorf("unauthenticated scrape: status %d", w.Code)
	}
	if w := do(srv.Handler(), "GET", "/metrics", "", "Bearer wrong"); w.Code != 401 {
		t.Errorf("wrong token: status %d", w.Code)
	}

	tw := do(srv.Handler(), "POST", "/api/v1/tenants", `{"name":"scraper"}`, testAuth)
	if tw.Code != 201 {
		t.Fatalf("create tenant: %d body=%s", tw.Code, tw.Body)
	}
	var created struct {
		APIKey string `json:"api_key"`
	}
	json.Unmarshal(tw.Body.Bytes(), &created)
	if w := do(srv.Handler(), "GET", "/metrics", "", "Bearer "+created.APIKey); w.Code != 404 {
		t.Errorf("tenant key on fleet metrics: status %d", w.Code)
	}

	w := do(srv.Handler(), "GET", "/metrics", "", testAuth)
	if w.Code != 200 {
		t.Fatalf("admin scrape: status %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4" {
		t.Errorf("metrics Content-Type: %q", ct)
	}
}

func TestMetricsAllStates(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "GET", "/metrics", "", testAuth)
	body := w.Body.String()
	for _, st := range []string{"creating", "running", "paused", "stopped", "sleeping", "error"} {
		if !strings.Contains(body, `state="`+st+`"`) {
			t.Errorf("metrics missing state=%q", st)
		}
	}
}

func TestMetricsExecsTotal(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "GET", "/metrics", "", testAuth)
	if !strings.Contains(w.Body.String(), "felucca_execs_total") {
		t.Error("metrics missing felucca_execs_total")
	}
}

// ---- Exec endpoint ----

// seedRunningNode registers a node and injects a sandbox in the given state
// directly into the state store, returning the sandbox id and the fake agent
// server URL (so the caller can set up stub responses).
func seedSandboxWithState(t *testing.T, h http.Handler, st *state.State, sbState model.SandboxState) (srvHandler *server.Server, sbID string) {
	t.Helper()
	// We need an actual server with the real state already seeded; cast back.
	// Instead: register node via API, then inject sandbox via state directly.
	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	_ = h // unused — caller passes nil to signal "use the returned srv"
	srv := server.New(cfg, st, newTestStore(t, tmp))
	return srv, ""
}

// newServerWithRunningFakeAgent creates a test server with one registered node
// pointing at a fake agent httptest.Server, and a sandbox in the given state.
func newServerWithFakeAgent(t *testing.T, agentHandler http.HandlerFunc, sbState model.SandboxState) (*server.Server, *state.State, string, *httptest.Server) {
	t.Helper()
	fakeAgent := httptest.NewServer(agentHandler)
	t.Cleanup(fakeAgent.Close)

	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     testToken,
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	st := state.New()
	srv := server.New(cfg, st, newTestStore(t, tmp))

	// Register a node pointing at the fake agent.
	agentURL := fakeAgent.URL // e.g. http://127.0.0.1:PORT
	now := time.Now().Unix()
	nodeID := st.RegisterNode("testhost", agentURL, 4, 8192, now)

	// Create sandbox directly in state.
	st.Lock()
	sb := st.CreateSandbox("testsb", "default", nodeID, 1, 256, now)
	sbID := sb.ID
	st.Unlock()
	st.SetSandboxState(sbID, sbState)

	return srv, st, sbID, fakeAgent
}

func TestExecUnknownID(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	w := do(srv.Handler(), "POST", "/api/v1/sandboxes/sb-nonexistent/exec",
		`{"cmd":["echo","hi"]}`, testAuth)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "not found" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecNonRunning(t *testing.T) {
	for _, sbState := range []model.SandboxState{model.StateStopped, model.StatePaused, model.StateSleeping, model.StateCreating} {
		sbState := sbState
		t.Run(string(sbState), func(t *testing.T) {
			srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
				// should never be called
				t.Error("agent should not be called for non-running sandbox")
			}, sbState)
			w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
				`{"cmd":["echo","hi"]}`, testAuth)
			if w.Code != 409 {
				t.Fatalf("state=%s: expected 409, got %d body=%s", sbState, w.Code, w.Body)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "not running" {
				t.Errorf("state=%s error body: %v", sbState, m)
			}
		})
	}
}

func TestExecHappyPath(t *testing.T) {
	agentResp := `{"ok":true,"exit_code":0,"stdout":"hello\n","stderr":""}`
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("agent: unexpected method %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(agentResp)))
		w.WriteHeader(200)
		io.WriteString(w, agentResp)
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["echo","hello"]}`, testAuth)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body)
	}
	// Body must be passed through verbatim.
	if w.Body.String() != agentResp {
		t.Errorf("body mismatch: got %q want %q", w.Body.String(), agentResp)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: %q", ct)
	}
	if cl := w.Header().Get("Content-Length"); cl == "" {
		t.Error("Content-Length not set")
	}
}

func TestExecAgent501(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		body := `{"error":"guest agent unavailable"}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(501)
		io.WriteString(w, body)
	}, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["ls"]}`, testAuth)
	if w.Code != 501 {
		t.Fatalf("expected 501, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "guest agent unavailable" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecAgentDown(t *testing.T) {
	// Start a fake agent and immediately close it so the connection is refused.
	fakeAgent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	fakeAgent.Close() // closed before the request arrives

	tmp := t.TempDir()
	cfg := &config.Config{
		Token:     testToken,
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
	}
	st := state.New()
	srv := server.New(cfg, st, newTestStore(t, tmp))
	now := time.Now().Unix()
	nodeID := st.RegisterNode("testhost", fakeAgent.URL, 4, 8192, now)
	st.Lock()
	sb := st.CreateSandbox("testsb", "default", nodeID, 1, 256, now)
	sbID := sb.ID
	st.Unlock()
	st.SetSandboxState(sbID, model.StateRunning)

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["ls"]}`, testAuth)
	if w.Code != 502 {
		t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "agent exec failed" {
		t.Errorf("error body: %v", m)
	}
}

func TestExecBadJSON(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent should not be called for bad json")
	}, model.StateRunning)

	for _, body := range []string{
		`not json`,
		`{"cmd":[]}`,      // empty cmd
		`{"cmd":123}`,     // cmd not array
		`{"cmd":[1,2,3]}`, // cmd elements not strings
		`{"cmd":[null]}`,  // null decodes to "" — not a program name
	} {
		body := body
		t.Run(body, func(t *testing.T) {
			w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID), body, testAuth)
			if w.Code != 400 {
				t.Fatalf("body=%q: expected 400, got %d resp=%s", body, w.Code, w.Body)
			}
			var m map[string]string
			json.Unmarshal(w.Body.Bytes(), &m)
			if m["error"] != "bad json" {
				t.Errorf("body=%q error: %v", body, m)
			}
		})
	}
}

// ---- Request body cap ----

func TestBodyCapRejectsOversizedAPIBody(t *testing.T) {
	srv, _ := newTestServer(t, testToken)
	big := `{"name":"` + strings.Repeat("a", 2<<20) + `"}`
	w := do(srv.Handler(), "POST", "/api/v1/sandboxes", big, testAuth)
	if w.Code != 400 {
		t.Fatalf("oversized create body: expected 400, got %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "bad json" {
		t.Errorf("error body: %v", m)
	}
}

func TestBodyCapCoversPreAuthJoinRoute(t *testing.T) {
	// The join route runs before the bearer gate, so the cap must already be
	// in place: an oversized body must die in the reader, not in join's own
	// field validation (which would mean the whole body was buffered first).
	tmp := t.TempDir()
	cfg := &config.Config{
		UIDir:     tmp,
		StatePath: filepath.Join(tmp, "state.json"),
		DBPath:    filepath.Join(tmp, "felucca.db"),
		WgIP:      "10.100.0.1/16",
	}
	srv := server.New(cfg, state.New(), newTestStore(t, tmp))

	body := `{"pubkey":"` + strings.Repeat("A", 43) + `=","hostname":"` + strings.Repeat("x", 2<<20) + `"}`
	w := do(srv.Handler(), "POST", "/api/v1/nodes/join", body, "Bearer felucca_jt_x")
	if w.Code != 400 {
		t.Fatalf("oversized join body: expected 400, got %d", w.Code)
	}
	var m map[string]string
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["error"] != "bad json" {
		t.Errorf("error body: %v (body was read past the cap)", m)
	}
}

func TestExecCmdBounds(t *testing.T) {
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("agent should not be called for an out-of-bounds cmd")
	}, model.StateRunning)

	args := make([]string, 300)
	for i := range args {
		args[i] = `"x"`
	}
	tooMany := `{"cmd":[` + strings.Join(args, ",") + `]}`
	tooLong := `{"cmd":["sh","-c","` + strings.Repeat("a", 17<<10) + `"]}`

	for _, tc := range []struct{ body, want string }{
		{tooMany, "cmd too long"},
		{tooLong, "cmd argument too long"},
	} {
		w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID), tc.body, testAuth)
		if w.Code != 400 {
			t.Fatalf("want=%q: expected 400, got %d body=%s", tc.want, w.Code, w.Body)
		}
		var m map[string]string
		json.Unmarshal(w.Body.Bytes(), &m)
		if m["error"] != tc.want {
			t.Errorf("error body: %v, want %q", m, tc.want)
		}
	}
}

func TestExecTimeoutRange(t *testing.T) {
	// timeout_ms outside 1..300000 is refused, not clamped: a negative value
	// used to survive the one-sided cap and overflow into a ~290-year
	// deadline. The upper bound is still accepted at its exact value.
	var receivedTimeoutMs int64
	srv, _, sbID, _ := newServerWithFakeAgent(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TimeoutMs int64 `json:"timeout_ms"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		receivedTimeoutMs = req.TimeoutMs
		body := `{"ok":true,"exit_code":0,"stdout":"","stderr":""}`
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(200)
		io.WriteString(w, body)
	}, model.StateRunning)

	for _, bad := range []string{"-9300000000000", "-1", "0", "999999"} {
		receivedTimeoutMs = 0
		w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
			`{"cmd":["true"],"timeout_ms":`+bad+`}`, testAuth)
		if w.Code != 400 {
			t.Fatalf("timeout_ms=%s: expected 400, got %d body=%s", bad, w.Code, w.Body)
		}
		var m map[string]string
		json.Unmarshal(w.Body.Bytes(), &m)
		if m["error"] != "timeout_ms out of range" {
			t.Errorf("timeout_ms=%s error body: %v", bad, m)
		}
		if receivedTimeoutMs != 0 {
			t.Errorf("timeout_ms=%s reached the agent as %d", bad, receivedTimeoutMs)
		}
	}

	w := do(srv.Handler(), "POST", fmt.Sprintf("/api/v1/sandboxes/%s/exec", sbID),
		`{"cmd":["true"],"timeout_ms":300000}`, testAuth)
	if w.Code != 200 {
		t.Fatalf("timeout_ms at the cap: expected 200, got %d body=%s", w.Code, w.Body)
	}
	if receivedTimeoutMs != 300000 {
		t.Errorf("agent received timeout_ms=%d, want 300000", receivedTimeoutMs)
	}
}

// ---- M3: every feluccad→agent dial, audited structurally ----

// funcKey names a declaration for the audit below: "Recv.Name" for methods,
// "Name" for plain functions.
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

// isCallOfSel reports whether n is a call to something ending in ".<sel>",
// e.g. srv.agentTokenFor(...) or req.Header.Set(...).
func isCallOfSel(n ast.Node, sel string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	s, ok := call.Fun.(*ast.SelectorExpr)
	return ok && s.Sel.Name == sel
}

// nodeCredResolvers are the entry points to a node's own credential:
// resolveNodeCred (keyed on host:port, and the only one that can tell "no
// credential" from "the read failed"), plus the agentTokenFor / agentTokenForHost
// shims that discard the state for the dial paths.
var nodeCredResolvers = []string{"resolveNodeCred", "agentTokenFor", "agentTokenForHost"}

// TestNoDialPathCanCarryTheAdminToken audits the SOURCE of the whole package
// instead of the handful of paths a behavioural test happens to drive. That
// gap is exactly how M3 survived a green suite: the tests covered the resolver
// (agentTokenForHost) and never the call sites, so nine dials carried the
// per-node credential, the two in the lifecycle sweep carried cfg.Token, and
// nothing noticed the fleet admin key going out to every worker on a 15-second
// timer.
//
// Three invariants, each of which the pre-fix tree violated:
//
//  1. agentclient.Request / ExecVM / ExecVMStream — the only functions that put
//     an Authorization header on the wire to a node — are called ONLY from the
//     nodeDial methods, which take a node identity and resolve the credential
//     themselves. No call site can supply a token, so none can supply the wrong
//     one.
//  2. cfg.Token is read ONLY where the control-plane credential belongs:
//     verifying an INBOUND Authorization header, and the grandfathered
//     legacy-node fallback inside the resolver.
//  3. any hand-rolled outbound Authorization header (the rootfs-capture client
//     does not go through agentclient) sits in a function that resolves the
//     node's own credential.
func TestNoDialPathCanCarryTheAdminToken(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	dialFuncs := []string{"Request", "ExecVM", "ExecVMStream"}
	// The choke point, and nothing else.
	allowedDial := map[string]bool{
		"nodeDial.do": true, "nodeDial.exec": true, "nodeDial.execStream": true,
	}
	allowedToken := map[string]bool{
		// The inbound check, and the grandfathered legacy-node fallback inside
		// the resolver proper (agentTokenFor is now a shim over it and must not
		// reach cfg.Token itself).
		"Server.authenticate": true, "Server.resolveNodeCred": true,
	}

	fset := gotoken.NewFileSet()
	scanned, dialSites := 0, 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned++
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			key := funcKey(fd)
			var setsAuthHeader, resolvesNodeCred bool
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, isCall := n.(*ast.CallExpr)
				// (1) agent dial helpers.
				if isCall {
					if s, ok := call.Fun.(*ast.SelectorExpr); ok {
						if x, ok := s.X.(*ast.Ident); ok && x.Name == "agentclient" {
							for _, d := range dialFuncs {
								if s.Sel.Name != d {
									continue
								}
								dialSites++
								if !allowedDial[key] {
									t.Errorf("%s: %s calls agentclient.%s directly — every feluccad→agent dial must go through the nodeDial choke point, which resolves the node's own credential (see nodeDial in server.go)",
										fset.Position(call.Pos()), key, d)
								}
							}
						}
					}
				}
				for _, r := range nodeCredResolvers {
					if isCallOfSel(n, r) {
						resolvesNodeCred = true
					}
				}
				// (3) hand-rolled outbound Authorization header.
				if isCall && isCallOfSel(n, "Set") && len(call.Args) > 0 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Value == `"Authorization"` {
						setsAuthHeader = true
					}
				}
				// (2) cfg.Token reads.
				if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "Token" {
					if inner, ok := s.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "cfg" && !allowedToken[key] {
						t.Errorf("%s: %s reads cfg.Token — the control-plane admin credential must not be reachable outside inbound auth and the legacy-node fallback",
							fset.Position(s.Pos()), key)
					}
				}
				return true
			})
			if setsAuthHeader && !resolvesNodeCred {
				t.Errorf("%s: %s builds an outbound Authorization header without resolving the node's own credential (%v)",
					fset.Position(fd.Pos()), key, nodeCredResolvers)
			}
		}
	}
	// Guard the audit itself: a glob that matched nothing would pass silently.
	if scanned < 5 {
		t.Fatalf("audited only %d package sources; the glob is wrong", scanned)
	}
	if dialSites < 3 {
		t.Fatalf("found only %d agentclient dial sites; the audit is not seeing them", dialSites)
	}
}
