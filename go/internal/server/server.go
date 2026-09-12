// Package server implements the feluccad HTTP server, replicating main.zig
// handler behavior exactly: routing, auth, proxying, metrics, static UI.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/agentclient"
	"github.com/alpham/infra-saas/felucca/internal/config"
	"github.com/alpham/infra-saas/felucca/internal/model"
	"github.com/alpham/infra-saas/felucca/internal/state"
	"github.com/alpham/infra-saas/felucca/internal/store"
	"github.com/alpham/infra-saas/felucca/internal/wg"
)

// adminTenant is the tenant context value for the configured admin token
// (and for open mode when no token is configured): unrestricted access.
const adminTenant = ""

// maxRequestBody caps every request body. Applied at the very top of handle()
// so it also covers /api/v1/nodes/join, which is routed BEFORE the bearer
// gate: without it an unauthenticated peer holding only a "felucca_jt_" prefix
// can stream an unbounded body into a decoder. Every documented body is a
// few hundred bytes; 1 MiB is the widest of them by orders of magnitude.
const maxRequestBody = 1 << 20

// Exec argument bounds. The body cap above already bounds total bytes; these
// bound the post-decode shape so one request cannot turn a small body into a
// large slice of large strings.
const (
	maxExecArgs   = 256
	maxExecArgLen = 16 << 10
)

// reqIDKey is the context key for the per-request trace ID.
type reqIDKey struct{}

// reqID extracts the request ID from the context, returning "" when absent.
func reqID(ctx context.Context) string {
	if v, ok := ctx.Value(reqIDKey{}).(string); ok {
		return v
	}
	return ""
}

// newTraceID mints a trace identifier for one actor: "req" for API requests,
// "sweep" for lifecycle sweep actions, "bg" for background pushes. One mint
// site so the format can never diverge per actor class.
func newTraceID(prefix string) string {
	return prefix + "-" + newSecret(6)
}

// Server holds the application state and configuration.
type Server struct {
	cfg *config.Config
	st  *state.State
	db  store.Store
	// WireGuard overlay (v4 P2): feluccad's public key, set at startup when
	// the overlay is configured; joinMu serializes overlay IP allocation.
	// addPeer is the live-kernel install hook — wg.AddPeer in production,
	// swappable in tests (the binary isn't present there).
	wgPubKey string
	joinMu   sync.Mutex
	addPeer  func(pubKey, overlayIP string) error
	// agentCall is the agent-HTTP hook for the ingress path (v4 P3) —
	// agentclient.Request in production, swappable in tests.
	agentCall func(host string, port uint16, method, path string, body []byte, reqID string) (*agentclient.Response, error)
	// exposeMu serializes expose/unexpose mutations end-to-end (check +
	// agent call + row update). Without it, two racing exposes of one name
	// leak an unaccounted agent DNAT entry on the 409 path, and two racing
	// unexposes of names sharing a guest port can both skip the agent
	// removal. Ordering: exposeMu OUTER, st lock INNER.
	exposeMu sync.Mutex
	// authFails is the per-source brute-force guard on the credential gates
	// (bearer gate and node join).
	authFails *authThrottle
	// trustedProxies is cfg.TrustedProxies parsed once at startup — the peers
	// whose forwarded client address clientIP will honour. Empty means trust
	// nobody's header and key on the connection itself.
	trustedProxies []*net.IPNet
	// credCache memoizes the resolved feluccad→agent bearer per node key
	// ("host:port", see nodeCredKey). Exec is the hot path and a credential
	// changes only at enrollment, so the alternative is a synchronous sqlite
	// read per proxied request behind a single-writer pool. Misses are NOT
	// cached: a node whose enrollment is still in flight must be picked up on
	// the next call.
	credMu    sync.RWMutex
	credCache map[string]string
	// nodeIdx resolves an INBOUND node token to the node it was issued to. It
	// is an in-memory index, loaded once at startup and maintained at
	// enrollment, precisely so that authenticating an agent's heartbeat (every
	// 5s per node) costs no database read — see authenticate.
	nodeIdx *nodeTokenIndex
	// credLookups bounds how many credential lookups may be inside the store at
	// once. See credGate.
	credLookups *credGate
	// sweepSeq counts lifecycle sweeps. It exists only to rotate where each
	// sweep starts in its (deterministically ordered) action list, so the head
	// of the state order is not permanently the first served — see
	// lifecycleSweep. Atomic because tests drive lifecycleSweep directly while
	// LifecycleLoop may also be running.
	sweepSeq atomic.Uint64
}

// New creates a new Server.
func New(cfg *config.Config, st *state.State, db store.Store) *Server {
	srv := &Server{
		cfg: cfg, st: st, db: db,
		addPeer:     wg.AddPeer,
		authFails:   newAuthThrottle(),
		credCache:   make(map[string]string),
		nodeIdx:     newNodeTokenIndex(),
		credLookups: newCredGate(maxCredLookups),
	}
	// Startup validation (config.ValidateTrustedProxies) already refuses a
	// malformed entry, so a parse failure here can only mean the two parsers
	// disagree: drop that entry rather than widen who is trusted, and say so.
	for _, p := range cfg.TrustedProxies {
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			slog.Error("ignoring unparseable trusted proxy: forwarded client addresses from it will not be honoured", "cidr", p, "err", err)
			continue
		}
		srv.trustedProxies = append(srv.trustedProxies, n)
	}
	// Every feluccad→agent call carries the credential minted for THAT node,
	// never cfg.Token — see nodeDial.
	srv.agentCall = func(host string, port uint16, method, path string, body []byte, reqID string) (*agentclient.Response, error) {
		return srv.dialNodeHostPort(host, port).do(method, path, body, reqID)
	}
	srv.seedLegacyNodeCreds()
	srv.loadNodeTokenIndex()
	return srv
}

// loadNodeTokenIndex builds the inbound node-token index from the store, once,
// before the listener starts. Doing it here is what keeps agent authentication
// off the database entirely: a fleet-sized map, read under an RWMutex, instead
// of a pre-authentication SELECT on every heartbeat.
func (srv *Server) loadNodeTokenIndex() {
	creds, err := srv.db.ListNodeCreds()
	if err != nil {
		// Not fatal: the admin token still authenticates the agent routes (that
		// is the rollout path), so the fleet degrades to the pre-M3 credential
		// rather than losing the control plane. Say so loudly.
		slog.Error("could not load node credentials: agents will have to authenticate with the shared token until restart", "err", err)
		return
	}
	for _, c := range creds {
		srv.nodeIdx.put(c.Host, c.Token)
	}
}

// Credential prefixes. Each kind of secret carries its own so an operator (or a
// log line, or the bearer gate) can tell them apart at a glance.
const (
	nodeTokenPrefix = "felucca_nt_" // feluccad↔agent, per node
	apiKeyPrefix    = "felucca_sk_" // tenant API key
)

// newNodeToken mints a node's own feluccad↔agent bearer. It is presented in both
// directions: feluccad offers it dialing that node, and the node offers it on the
// agent routes (see principal).
func newNodeToken() string { return nodeTokenPrefix + newSecret(24) }

// nodeHostKey unbrackets the host part of a node address.
func nodeHostKey(host string) string { return strings.Trim(host, "[]") }

// nodeCredKey is the node-credential key for a dial target: "host:port". One
// function for the key written at enrollment and the key read at dial time, so
// the two can never diverge — the same reason validNodeAddr parses
// agentclient.SplitHostPort's own output.
//
// The PORT is part of it. Keying on the host alone made one credential stand for
// every agent on an address, which is narrower than the identity it authorizes:
// two agents on one host are two nodes, and "worth one worker" has to mean one
// worker. Rows written under the old bare-host key are migrated on first use
// (see agentTokenFor) so an upgrade does not strand an enrolled fleet.
func nodeCredKey(host string, port uint16) string {
	return net.JoinHostPort(nodeHostKey(host), strconv.Itoa(int(port)))
}

// nodeCredKeyFor is nodeCredKey for a caller holding an unsplit node address.
func nodeCredKeyFor(addr string) string {
	host, port := agentclient.SplitHostPort(addr)
	return nodeCredKey(host, port)
}

// nodeTokenIndex resolves an inbound node token to the node key it was issued
// to. It holds sha256 hashes rather than the secrets themselves: the map is
// keyed by a uniform-length digest, and a heap dump of the control plane does
// not hand over every agent credential in the fleet.
//
// It exists so that authenticating an agent costs NO database read. That is not
// an optimization: a store read reachable before authentication is a queue in
// front of the single sqlite connection that anyone can fill (see credGate), and
// the fleet's heartbeats are the highest-rate pre-authentication traffic there
// is.
type nodeTokenIndex struct {
	mu     sync.RWMutex
	byHash map[string]string // sha256(token) -> node key
	byKey  map[string]string // node key -> sha256(token), so a rotation retires the old one
}

func newNodeTokenIndex() *nodeTokenIndex {
	return &nodeTokenIndex{byHash: map[string]string{}, byKey: map[string]string{}}
}

// put records (or rotates) one node's token. An empty token — the grandfathered
// legacy rows, which carry none — indexes nothing: those nodes authenticate with
// the shared token, and an empty credential must never match an empty header.
func (x *nodeTokenIndex) put(key, token string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if old, ok := x.byKey[key]; ok {
		delete(x.byHash, old)
		delete(x.byKey, key)
	}
	if token == "" {
		return
	}
	h := hashSecret(token)
	x.byHash[h] = key
	x.byKey[key] = h
}

// lookup returns the node key a presented token belongs to.
func (x *nodeTokenIndex) lookup(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	h := hashSecret(token)
	x.mu.RLock()
	defer x.mu.RUnlock()
	key, ok := x.byHash[h]
	return key, ok
}

// maxCredLookups bounds how many credential lookups may be inside the store at
// once — see credGate.
const maxCredLookups = 4

// credGate bounds the number of IN-FLIGHT credential lookups (the tenant-key
// read in authenticate, the join-token read in nodeJoin). Those are the only
// store reads an unauthenticated caller can reach, and store/sqlite.go caps the
// pool at ONE connection, so without a bound a client opening N concurrent
// connections with a bogus "felucca_sk_..." bearer puts N queries in front of
// every other user of that connection: SaveSnapshot on each create/sleep/delete,
// AppendUsage, the sweep's GetTenant, and GetNodeCred on the exec hot path.
//
// It is a semaphore and NOT a refusal, deliberately. Whether a presented key is
// good is exactly what the pending lookup is about, so anything that turned a
// full gate into a 4xx would be refusing a credential it has not read yet — the
// vulnerability the previous round removed (see gateAuth). Waiting degrades:
// a caller with a real key is always served, just behind at most maxCredLookups
// queries instead of behind the whole flood, and the flood is parked in the
// scheduler where it competes for nothing.
type credGate struct{ slots chan struct{} }

func newCredGate(n int) *credGate { return &credGate{slots: make(chan struct{}, n)} }

// acquire blocks until a slot is free or ctx is done, returning the release
// func. ok is false only when the CALLER went away, never because of load.
func (g *credGate) acquire(ctx context.Context) (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// validAPIKeyShape reports whether a bearer could be a key feluccad ever issued:
// mintKey produces apiKeyPrefix + 48 lowercase hex characters, and nothing else
// writes an api_keys row. Checked before the store lookup, so garbage never
// reaches (or queues for) the database. A key that feluccad minted always passes,
// so this can never refuse a valid credential.
func validAPIKeyShape(key string) bool {
	const secretLen = 48 // hex of newSecret(24)
	if len(key) != len(apiKeyPrefix)+secretLen {
		return false
	}
	for i := len(apiKeyPrefix); i < len(key); i++ {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// seedLegacyNodeCreds grandfathers the fleet the store just loaded onto the
// shared token. The store bounds this to once per database (see
// SeedLegacyNodeCreds): "this node has no per-node credential" must not be a
// state an attacker can create on demand, or registering an arbitrary address
// would still deliver the control-plane admin key to it.
func (srv *Server) seedLegacyNodeCreds() {
	var hosts []string
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		hosts = make([]string, 0, len(srv.st.Nodes))
		for _, n := range srv.st.Nodes {
			hosts = append(hosts, nodeCredKeyFor(n.Addr))
		}
	}()

	n, err := srv.db.SeedLegacyNodeCreds(hosts, time.Now().Unix())
	if err != nil {
		// Not fatal, but the fleet now has no credential to be dialed with:
		// say so rather than let the calls fail one by one.
		slog.Error("could not grandfather existing nodes onto the shared token", "err", err)
		return
	}
	if n > 0 {
		slog.Warn("existing nodes are still using the shared control-plane token; re-enroll each with a join token so it gets its own agent credential",
			"nodes", n)
	}
}

// agentTokenFor resolves the bearer feluccad presents to one node.
//
// The admin API token is NOT it. A node address is caller-supplied (POST
// /api/v1/agents/register), so every outbound dial is a decision about where
// to deliver a bearer secret; reusing the control-plane admin key there made
// "point feluccad at a host I control" equivalent to "hand me the admin key".
// Each node gets its own token at enrollment instead, so a harvested one is
// worth one worker.
//
// An address with no credential row gets NO bearer. That is the case a forged
// registration lands in, and the reason the shared-token fallback is confined
// to the one-time grandfathered set rather than "anything unknown".
//
// agentTokenForHost is agentTokenFor for a caller that has only the host part of
// a node address; it assumes the agent's own port, which validNodeAddr is what
// makes true for every address enrolled since it existed.
func (srv *Server) agentTokenForHost(host string) string {
	return srv.agentTokenFor(host, agentPort)
}

// agentTokenFor is the DIAL path's view of the resolver: just the bearer to put
// on the wire. "" is already fail-closed here — a dial carrying no credential is
// a request the agent answers 401 — so a dial site never needs to tell the three
// outcomes below apart.
//
// A caller that decides something from the ABSENCE of a credential must use
// resolveNodeCred and read the state instead: "" is not evidence that a node has
// no credential. See rejoinAuthorized, which is where reading it as such turned a
// store error into an off switch for a possession check.
func (srv *Server) agentTokenFor(host string, port uint16) string {
	tok, _ := srv.resolveNodeCred(host, port)
	return tok
}

// nodeCredState is what resolveNodeCred managed to ESTABLISH about a node's
// credential. The three outcomes used to collapse into one empty string, and
// "the store read failed" is emphatically not "this node has no credential yet":
// only the second authorizes anything.
type nodeCredState int

const (
	// credResolved: the credential is known, and it is the returned token.
	credResolved nodeCredState = iota
	// credAbsent: the store WAS read and holds no credential for this node.
	credAbsent
	// credUnknown: nothing was established — the read itself failed.
	credUnknown
)

// resolveNodeCred is the resolver proper, keyed on "host:port" (nodeCredKey).
// Misses are deliberately not memoized: a node whose enrollment is still in
// flight must be picked up on the next call, and a failed read must not be
// cached as an answer at all.
func (srv *Server) resolveNodeCred(host string, port uint16) (string, nodeCredState) {
	key := nodeCredKey(host, port)

	srv.credMu.RLock()
	tok, ok := srv.credCache[key]
	srv.credMu.RUnlock()
	if ok {
		return tok, credResolved
	}

	cred, err := srv.lookupNodeCred(key, nodeHostKey(host))
	if err != nil {
		slog.Error("node credential lookup", "node", key, "err", err)
		return "", credUnknown
	}
	if cred == nil {
		slog.Warn("no agent credential for this node: dialing it without one — enroll it with a join token",
			"node", key)
		return "", credAbsent
	}
	if cred.Legacy || cred.Token == "" {
		// Logged once per node per process (the resolution is then memoized),
		// which is what an operator needs to finish the rollout node by node
		// without the exec path drowning the log.
		slog.Warn("node is still on the shared control-plane token (enrolled before per-node credentials) — re-enroll it with a join token",
			"node_host", key)
		tok = srv.cfg.Token
	} else {
		tok = cred.Token
	}
	srv.credMu.Lock()
	srv.credCache[key] = tok
	srv.credMu.Unlock()
	return tok, credResolved
}

// lookupNodeCred reads the credential for a node key, falling back ONCE to the
// pre-port layout (a row keyed on the bare host) and migrating what it finds.
//
// The fallback is what keeps an upgrade from stranding an enrolled fleet:
// SeedLegacyNodeCreds runs at most once per database, so a fleet grandfathered
// under the old key would otherwise resolve to nothing and be dialed with no
// credential at all. The migration REWRITES the row under "host:port" and drops
// the bare one, so the widened key is reached exactly once per node and a second
// agent on the same host cannot inherit the first one's credential afterwards.
func (srv *Server) lookupNodeCred(key, bareHost string) (*store.NodeCred, error) {
	cred, err := srv.db.GetNodeCred(key)
	if err != nil || cred != nil {
		return cred, err
	}
	legacy, err := srv.db.GetNodeCred(bareHost)
	if err != nil || legacy == nil {
		return nil, err
	}
	migrated := &store.NodeCred{Host: key, Token: legacy.Token, Legacy: legacy.Legacy, CreatedAt: legacy.CreatedAt}
	if err := srv.db.PutNodeCred(migrated); err != nil {
		// The row is still readable under the old key next time; report the
		// credential so this dial works rather than failing over a bookkeeping
		// write.
		slog.Error("could not migrate node credential to the host:port key", "node", key, "err", err)
		return migrated, nil
	}
	if _, err := srv.db.DeleteNodeCred(bareHost); err != nil {
		slog.Error("could not retire the bare-host node credential row", "host", bareHost, "err", err)
	}
	srv.nodeIdx.put(key, migrated.Token)
	slog.Info("migrated node credential to its host:port key", "node", key, "from", bareHost)
	return migrated, nil
}

// nodeDial is the ONLY way this package addresses a worker agent. It pairs the
// dial target with the bearer minted for THAT node, and its fields are
// unexported with a single producer — Server.dialNodeHostPort, which resolves
// the credential itself through agentTokenForHost.
//
// The point is structural, not procedural: a nodeDial is built from a NODE
// IDENTITY (its address), never from a token string, so there is no expression
// anywhere in the package that puts a caller-chosen credential — cfg.Token
// above all — on the wire to a node. The M3 regression is what a procedural
// rule ("remember to pass agentTokenForHost") costs: nine call sites obeyed it
// and the two in the lifecycle sweep did not, silently shipping the fleet admin
// key to every node on a 15s timer. With this type the same mistake does not
// compile. (Same shape as the H2 fix: no invalid inhabitant.)
//
// The zero value dials the empty host, i.e. nothing — a nodeDial that skipped
// the constructor fails loudly rather than quietly reaching a node unbearered.
type nodeDial struct {
	host  string
	port  uint16
	token string
}

// dialNode is the choke point for a node address ("host:port", possibly
// scheme-prefixed) — the shape state.Node carries.
func (srv *Server) dialNode(addr string) nodeDial {
	host, port := agentclient.SplitHostPort(addr)
	return srv.dialNodeHostPort(host, port)
}

// dialNodeHostPort is dialNode for callers holding an already-split address.
// It is the one and only place a nodeDial is constructed.
func (srv *Server) dialNodeHostPort(host string, port uint16) nodeDial {
	return nodeDial{host: host, port: port, token: srv.agentTokenFor(host, port)}
}

// do performs one agent request. Every agentclient.Request in this package
// goes through here (TestNoDialPathCanCarryTheAdminToken proves it).
func (d nodeDial) do(method, path string, body []byte, reqID string) (*agentclient.Response, error) {
	return agentclient.Request(d.host, d.port, method, path, body, d.token, reqID)
}

// exec is do for the buffered exec path (its own per-request timeout).
func (d nodeDial) exec(id string, body []byte, timeout time.Duration, reqID string) (*agentclient.Response, error) {
	return agentclient.ExecVM(d.host, d.port, id, body, d.token, timeout, reqID)
}

// execStream is do for the streaming exec path: the live response is returned
// unread so the caller can relay it.
func (d nodeDial) execStream(id string, body []byte, timeout time.Duration, reqID string) (*http.Response, context.CancelFunc, error) {
	return agentclient.ExecVMStream(d.host, d.port, id, body, d.token, timeout, reqID)
}

// putNodeCred records (or rotates) a node's own credential, drops the memoized
// value so the very next dial uses it, and re-points the inbound index so the
// node can authenticate to feluccad with it immediately.
func (srv *Server) putNodeCred(host string, port uint16, token string, now int64) error {
	key := nodeCredKey(host, port)
	if err := srv.db.PutNodeCred(&store.NodeCred{Host: key, Token: token, CreatedAt: now}); err != nil {
		return err
	}
	srv.credMu.Lock()
	delete(srv.credCache, key)
	srv.credMu.Unlock()
	// Rotation retires the previous token in the same call, so a credential
	// feluccad no longer holds cannot still authenticate its node.
	srv.nodeIdx.put(key, token)
	return nil
}

// SetWgPubKey records feluccad's WireGuard public key (returned to joining
// workers). Called once at startup, before the listener starts.
func (srv *Server) SetWgPubKey(pub string) { srv.wgPubKey = pub }

// persist writes the in-memory working set through to the store.
func (srv *Server) persist() error {
	return srv.db.SaveSnapshot(srv.st)
}

// principal is WHO a request is acting as. There are exactly three kinds, and
// the type is what keeps them apart:
//
//   - admin      — the configured token (or open mode): unrestricted.
//   - tenant     — an API key: its own sandboxes, nothing fleet-wide.
//   - node       — a worker's own felucca_nt_ credential: the two agent routes
//     and NOTHING else (serveAPI allowlists them by hand).
//
// A node is deliberately not expressible as a tenant string. Before this, "who
// is this" was a bare tenant id and the admin was the empty one, so any code
// path that produced "" produced the administrator; a node principal squeezed
// into that representation would have been one typo away from being a second
// admin. Here the node case has no tenant at all — tenant is meaningless when
// node is set — and isAdmin() is false for it by construction.
type principal struct {
	tenant string // adminTenant for the admin token/open mode, else a tenant id
	node   bool   // authenticated with a node credential
	key    string // the node's credential key ("host:port"); only when node
}

// isAdmin reports whether this principal holds the control-plane admin context.
func (p principal) isAdmin() bool { return !p.node && p.tenant == adminTenant }

// authenticate resolves the Authorization header to a principal.
//
// ORDERING IS A SECURITY PROPERTY here, and the order is: cheapest and most
// privileged first, store-backed last.
//
//  1. open mode / the admin token — a constant-time compare, no store.
//  2. a node credential — an in-memory index lookup, no store.
//  3. a tenant API key — the ONE store read on this path, shape-checked first
//     and bounded by credLookups so it cannot become a queue in front of the
//     single sqlite connection (see credGate).
//
// ctx bounds only the wait for a lookup slot: a caller that has gone away stops
// occupying one. Nothing here refuses a credential for load.
func (srv *Server) authenticate(ctx context.Context, authHeader string) (principal, bool) {
	// Open mode is reached only through the explicit --insecure-no-auth
	// escape hatch. An empty or unloadable token authorizes nobody.
	if srv.cfg.InsecureNoAuth {
		return principal{tenant: adminTenant}, true
	}
	if config.Authorized(srv.cfg.Token, authHeader) {
		return principal{tenant: adminTenant}, true
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return principal{}, false
	}
	key := authHeader[len(prefix):]

	// A worker presenting the credential feluccad issued it (M3, agent side).
	// Resolved from memory: see nodeTokenIndex.
	if strings.HasPrefix(key, nodeTokenPrefix) {
		if nodeKey, ok := srv.nodeIdx.lookup(key); ok {
			return principal{node: true, key: nodeKey}, true
		}
		return principal{}, false
	}

	if !strings.HasPrefix(key, apiKeyPrefix) || !validAPIKeyShape(key) {
		return principal{}, false
	}
	release, ok := srv.credLookups.acquire(ctx)
	if !ok {
		return principal{}, false // the caller hung up while waiting
	}
	// The release is DEFERRED, and scoped to the store call by the closure so
	// the slot is not held across the rest of the function. A bare
	// call-then-release leaks the slot if the store call panics: net/http
	// recovers a handler panic per connection, so the process survives and the
	// slot never comes back — four of those and every tenant-API-key
	// authentication and every node join blocks until its request context
	// expires. A permanent, silent auth outage is a strictly worse failure than
	// the panic that caused it.
	tenantID, err := func() (string, error) {
		defer release()
		return srv.db.LookupKeyByHash(hashSecret(key))
	}()
	if err != nil {
		slog.Error("key lookup", "err", err)
		return principal{}, false
	}
	if tenantID == "" {
		return principal{}, false
	}
	return principal{tenant: tenantID}, true
}

// Brute-force guard bounds. The first authFailThreshold failures from a
// source are free (an operator fat-fingering a token must not lock themselves
// out); past that each further failure doubles the wait, capped.
//
// Scope note (L6 follow-up): this guard is DEFENCE IN DEPTH and nothing more.
// What actually makes the admin token infeasible to guess is config.ValidateAuth
// — a 32-char minimum with the shipped placeholders refused, checked before the
// listener starts. The throttle therefore may never be in a position to cause an
// outage, and gateAuth evaluates the credential BEFORE consulting it precisely
// so that it cannot: see gateAuth.
const (
	authFailThreshold = 10
	authBackoffBase   = time.Second
	authBackoffMax    = time.Minute
	// authBackoffSharedMax caps the wait when the key provably stands for more
	// than one client — feluccad behind an undeclared reverse proxy, where every
	// caller arrives as the same address (see Server.throttleSource). A valid
	// credential is served regardless, so this only bounds how long an
	// unauthenticated neighbour is answered 429 instead of 401 for someone
	// else's guessing.
	authBackoffSharedMax = 2 * time.Second
	authThrottleTTL      = 15 * time.Minute
	authThrottleMax      = 4096
	// authFailDecay forgives one accumulated failure per interval of quiet.
	// It replaces the old "a success wipes the record": a wipe is reachable by
	// anyone sharing the key, so any client that can authenticate — the
	// console's own poll, an unrelated tenant behind the same NAT — could
	// reset a guesser's counter before it ever bit. Time cannot be forged, so
	// the leak forgives the operator who mistypes without handing an attacker
	// a reset button, and it bounds a sustained guesser to one attempt per
	// interval.
	authFailDecay = 15 * time.Second
	// authFailCeiling bounds how much a burst can accumulate, and with it how
	// long a shared key stays penalized once the burst stops. The backoff
	// already saturates at authBackoffMax well below this, so the cap costs
	// nothing in strength.
	authFailCeiling = authFailThreshold + 8
)

// authThrottle is the only brute-force guard on feluccad's credential gates —
// the gateway's token bucket is a different process and covers tenant ingress
// only. Keyed on the client address feluccad can actually attribute a request
// to (see Server.throttleSource): the peer, or the forwarded hop when the peer
// is a declared reverse proxy. Nothing clears a record — only quiet time decays
// it (authFailDecay) — because a clear-on-success is reachable by anyone
// sharing the key. Where the key is unavoidably shared, the wait it can produce
// is capped short (authBackoffSharedMax), and in no case is it consulted before
// a credential has already failed to authenticate.
type authThrottle struct {
	mu   sync.Mutex
	fail map[string]*authFailure
}

type authFailure struct {
	count int
	until time.Time // no attempt is evaluated before this instant
	seen  time.Time
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{fail: make(map[string]*authFailure)}
}

// retryAfter returns how long a source that has just FAILED to authenticate is
// told to wait; zero means "no backoff accumulated".
//
// It is never consulted before a credential is evaluated — a wait returned here
// shapes the response to an attempt that already failed, and can therefore
// never refuse a request that would have succeeded.
func (t *authThrottle) retryAfter(src string, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.fail[src]
	if f == nil || !now.Before(f.until) {
		return 0
	}
	return f.until.Sub(now)
}

// failed records one failed attempt. maxWait bounds the backoff this source can
// accumulate — the caller passes the shorter cap for a key that stands for many
// clients (throttleSource.maxWait), so the penalty a shared key carries is
// bounded in TIME and not merely in what the Retry-After header admits to.
func (t *authThrottle) failed(src string, now time.Time, maxWait time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evict(now)
	f := t.fail[src]
	if f == nil {
		f = &authFailure{}
		t.fail[src] = f
	} else if leak := int(now.Sub(f.seen) / authFailDecay); leak > 0 {
		// Quiet time forgives, nothing else does — see authFailDecay.
		f.count -= leak
		if f.count < 0 {
			f.count = 0
		}
	}
	f.count++
	if f.count > authFailCeiling {
		f.count = authFailCeiling
	}
	f.seen = now
	if f.count > authFailThreshold {
		d := authBackoffBase << uint(min(f.count-authFailThreshold-1, 16))
		if d > maxWait {
			d = maxWait
		}
		f.until = now.Add(d)
	}
}

// evict keeps the map bounded: a spray across forged source addresses must
// not turn the guard itself into the memory-exhaustion primitive. Caller
// holds t.mu.
//
// WHAT IT MUST NOT DO IS FORGET EVERYTHING. This used to end in clear(t.fail),
// which made the guard self-defeating: burn the free attempts on your real key,
// spray more than authThrottleMax distinct keys inside the TTL — trivial from
// any address range that is not collapsed to one key, an IPv6 /64 above all —
// and every accumulated backoff in the process was wiped, the sprayer's own
// included. Repeat, and guessing runs at line rate forever.
//
// So eviction RANKS instead. A record's rank is what it has earned:
//
//   - a source still serving a backoff is not evicted at all while any
//     unpenalized record remains — that is precisely the record an attacker
//     wants gone;
//   - among the rest, the lowest failure count goes first (a spray's entries are
//     count 1), ties broken by least recently seen.
//
// A spray therefore evicts itself. Taking out an established record costs an
// attacker more sustained failures than the record holds, on every one of
// authThrottleMax keys at once, which is a different and much worse trade than
// "send 4096 packets".
func (t *authThrottle) evict(now time.Time) {
	if len(t.fail) < authThrottleMax {
		return
	}
	for k, f := range t.fail {
		if now.Sub(f.seen) > authThrottleTTL {
			delete(t.fail, k)
		}
	}
	if len(t.fail) < authThrottleMax {
		return
	}
	// Still full: drop the least-established eighth, so the ranking sort is
	// amortized over that many further failures rather than run on each one.
	type ranked struct {
		key       string
		penalized bool
		count     int
		seen      time.Time
	}
	all := make([]ranked, 0, len(t.fail))
	for k, f := range t.fail {
		all = append(all, ranked{k, now.Before(f.until), f.count, f.seen})
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.penalized != b.penalized {
			return !a.penalized // unpenalized records go first
		}
		if a.count != b.count {
			return a.count < b.count
		}
		return a.seen.Before(b.seen)
	})
	drop := len(t.fail) - authThrottleMax + authThrottleMax/8
	for i := 0; i < drop && i < len(all); i++ {
		delete(t.fail, all[i].key)
	}
}

// maxXFFHops bounds how much of an X-Forwarded-For chain clientIP will walk.
// Real chains are one or two hops; the bound exists because the header is
// attacker-sized, not because deployments need it.
const maxXFFHops = 16

// peerIP is the connection's own address without its port.
func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return strings.Trim(host, "[]")
}

// trustedProxyIP reports whether ip is one of the declared reverse proxies.
func (srv *Server) trustedProxyIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range srv.trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP is the address feluccad attributes a request to — the throttle key,
// and so what keeps one client's failures off another's record. It is not what
// keeps a valid credential served: gateAuth authenticates first, so even a
// perfectly forged key cannot deny service to anyone who holds a real token.
//
// r.RemoteAddr is the sole trustworthy source unless the immediate peer is a
// declared reverse proxy (cfg.TrustedProxies): a forwarded header is
// caller-settable, so honouring it from anyone would let a client pick its own
// key, and anyone else's. The shipped topology makes this load-bearing —
// feluccad binds loopback behind Caddy, so without the proxy declared every
// client, attacker and operator alike, arrives as 127.0.0.1 and shares one
// record.
//
// When the peer IS a declared proxy, the right-most forwarded entry that is
// not itself a trusted proxy is the nearest hop the chain can attest to.
// The LEFT-most entry — the one this kind of code usually reaches for — is
// whatever the original caller wrote, and is never used.
func (srv *Server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	if len(srv.trustedProxies) == 0 || !srv.trustedProxyIP(net.ParseIP(peer)) {
		return peer
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Walked from the right, one hop at a time, and never split whole:
		// strings.Split on a header the caller controls allocates one element
		// per comma BEFORE anything is inspected, so a ~1 MiB run of commas
		// (inside Go's old 1 MiB header default — cmd/feluccad now sets a much
		// smaller MaxHeaderBytes) built a ~500k-element slice per request. Only
		// maxXFFHops are examined; no real proxy chain is anywhere near that,
		// and a longer one is not a chain feluccad can attribute anything to.
		rest := xff
		for i := 0; i < maxXFFHops; i++ {
			hop := rest
			if c := strings.LastIndexByte(rest, ','); c >= 0 {
				hop, rest = rest[c+1:], rest[:c]
			} else {
				rest = ""
			}
			ip := net.ParseIP(strings.Trim(strings.TrimSpace(hop), "[]"))
			if ip == nil {
				// An entry we cannot parse ends the chain of custody —
				// nothing further left is attributable to anyone.
				return peer
			}
			if srv.trustedProxyIP(ip) {
				if rest == "" {
					break // every hop was one of our own proxies
				}
				continue
			}
			return ip.String()
		}
		// Every hop was ours, or the chain is longer than we will parse: the
		// peer is as far as attribution goes.
		return peer
	}
	// X-Real-IP, only when the operator opted in (cfg.TrustXRealIP, off by
	// default and refused at startup unless a proxy is declared). Unlike XFF it
	// is a bare value with NO chain of custody: feluccad cannot tell a header the
	// proxy wrote from one it forwarded verbatim, so a proxy that does not
	// rewrite it (nginx with a bare proxy_pass) hands the client its own key.
	// The default is therefore to ignore it and key on the proxy — shared, but
	// never forgeable.
	if srv.cfg.TrustXRealIP {
		if xr := strings.Trim(strings.TrimSpace(r.Header.Get("X-Real-IP")), "[]"); xr != "" {
			if ip := net.ParseIP(xr); ip != nil && !srv.trustedProxyIP(ip) {
				return ip.String()
			}
		}
	}
	return peer
}

// throttleSource is one request's brute-force-guard key, plus whether that key
// stands for a single client. A shared key is not a bug to be fixed by trusting
// a header — it is the honest answer when nothing attributable was forwarded —
// so it is carried explicitly and given a shorter cap.
type throttleSource struct {
	key    string
	shared bool
}

// maxWait bounds the backoff this source can be told to serve.
func (s throttleSource) maxWait() time.Duration {
	if s.shared {
		return authBackoffSharedMax
	}
	return authBackoffMax
}

// throttleSource derives the guard key for a request and reports whether it
// identifies one client.
//
// "Shared" is decided from configuration and the peer address ONLY — never from
// a header — so a client cannot opt itself into the softer treatment any more
// than it can choose its key. Two cases produce a shared key:
//
//   - the peer is a declared proxy that forwarded nothing feluccad could attribute
//     (no XFF, or every hop was one of our own), so the key is the proxy itself;
//   - no proxy is declared at all and the peer is loopback or private — feluccad
//     binds loopback behind Caddy in the shipped topology, so every client in the
//     world arrives as 127.0.0.1 and shares one record, and a reverse proxy one
//     hop away on the LAN produces the same thing from a private address.
//
// The second case over-approximates: a genuine private-network client that is
// nobody's proxy is treated as shared and gets the short cap. That direction is
// chosen deliberately. Being wrong this way costs throttle strength against a
// brute-force attempt config.ValidateAuth has already made infeasible; being
// wrong the other way spends a full minute of 429s on clients feluccad cannot
// tell apart, which is the failure this whole item is about.
func (srv *Server) throttleSource(r *http.Request) throttleSource {
	peer := peerIP(r)
	key := srv.clientIP(r)
	if key != peer {
		// A hop the declared proxy chain attested to: one client.
		return throttleSource{key: key}
	}
	ip := net.ParseIP(peer)
	if srv.trustedProxyIP(ip) {
		return throttleSource{key: key, shared: true}
	}
	if len(srv.trustedProxies) == 0 && ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		return throttleSource{key: key, shared: true}
	}
	return throttleSource{key: key}
}

// refuse answers one FAILED credential attempt: it records the failure and
// writes either the caller's status or a 429 when this source has already
// accumulated backoff. Both are refusals of a request that could not have
// succeeded — no path through here can turn away a valid credential.
func (srv *Server) refuse(w http.ResponseWriter, src throttleSource, status int, body string) {
	now := time.Now()
	wait := srv.authFails.retryAfter(src.key, now)
	srv.authFails.failed(src.key, now, src.maxWait())
	if wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, 429, []byte(`{"error":"too many failed attempts"}`))
		return
	}
	writeJSON(w, status, []byte(body))
}

// gateAuth resolves the Authorization header to a tenant context, writing the
// 401/429 itself. Callers get (tenant, true) only for an accepted credential.
//
// THE CREDENTIAL IS EVALUATED FIRST, AND A CORRECT ONE IS ALWAYS SERVED. The
// throttle is consulted only on the failure path, so no accumulation of anyone
// else's failures can refuse a request that would otherwise have succeeded.
//
// Evaluating the backoff first — the shape this had after L6 — was a global
// unauthenticated denial of service. gateAuth runs on every /api/ path, the
// shipped configuration declares no trusted proxy (so every client shares the
// 127.0.0.1 key), and each failure refreshes the decay clock: one anonymous
// client sending one bad bearer every ~10s kept the whole control plane, every
// operator and every worker node included, in a 429 that never expired. The
// constant-time compare this ordering costs per attempt is the standard trade,
// and the guessing it was meant to slow is already infeasible at the config
// gate (config.ValidateAuth: 32-char minimum, placeholders refused).
func (srv *Server) gateAuth(w http.ResponseWriter, r *http.Request) (principal, bool) {
	p, ok := srv.authenticate(r.Context(), r.Header.Get("Authorization"))
	if ok {
		// Deliberately nothing recorded on success: see authFailDecay. Clearing
		// the record here was the L6 bypass — the console's own authenticated
		// poll wiped whatever a guesser sharing the key had accumulated.
		return p, true
	}
	srv.refuse(w, srv.throttleSource(r), 401, `{"error":"unauthorized"}`)
	return principal{}, false
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
		slog.Error("usage event", "err", err)
	}
}

// Handler returns an http.Handler for use with net/http.
func (srv *Server) Handler() http.Handler {
	return http.HandlerFunc(srv.handle)
}

func (srv *Server) handle(w http.ResponseWriter, r *http.Request) {
	srv.st.BumpRequests()

	// Cap the body before ANY route sees it — the join route below runs
	// ahead of the bearer gate, so this is the only place that covers it.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	}

	path := r.URL.Path

	// Health — open, no auth.
	if path == "/healthz" {
		writeJSON(w, 200, []byte(`{"ok":true}`))
		return
	}

	// Metrics — bearer-gated. The scrape names every worker (hostname labels)
	// and reports live fleet counts, and it takes the global state lock, so
	// an open endpoint is both reconnaissance and a lock-contention lever for
	// anyone who can reach the port. Admin token only, same as every other
	// fleet-wide surface; Prometheus scrapes it with a bearer_token.
	if path == "/metrics" {
		p, ok := srv.gateAuth(w, r)
		if !ok {
			return
		}
		if !p.isAdmin() {
			writeJSON(w, 404, []byte(`{"error":"not found"}`))
			return
		}
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
		// Mint the per-request trace ID BEFORE auth (API-V2 §3g: EVERY /api/
		// request gets one): a 401 caused by a transient store error in key
		// lookup is exactly the case an operator needs to correlate.
		id := newTraceID("req")
		w.Header().Set(agentclient.RequestIDHeader, id)
		r = r.WithContext(context.WithValue(r.Context(), reqIDKey{}, id))

		p, ok := srv.gateAuth(w, r)
		if !ok {
			return
		}
		srv.serveAPI(w, r, p)
		return
	}

	// Static UI fallback.
	srv.serveStatic(w, path)
}

// ---- Routing ----

func (srv *Server) serveAPI(w http.ResponseWriter, r *http.Request, p principal) {
	path := r.URL.Path
	method := r.Method

	// A NODE credential reaches exactly these two routes and nothing else.
	//
	// This is an ALLOWLIST and returns before the rest of the router on every
	// path, which is the point: M3's goal is that a credential harvested off a
	// worker is worth one worker, so the reach of that credential has to be
	// enumerated rather than subtracted. A route added below is not reachable by
	// a node unless someone adds it here on purpose.
	if p.node {
		switch {
		case path == "/api/v1/agents/register" && method == http.MethodPost:
			srv.agentRegister(w, r, p)
		case path == "/api/v1/agents/heartbeat" && method == http.MethodPost:
			srv.agentHeartbeat(w, r, p)
		default:
			writeJSON(w, 404, []byte(`{"error":"not found"}`))
		}
		return
	}

	tenant := p.tenant

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
			strings.HasPrefix(path, "/api/v1/images/"),
			strings.HasPrefix(path, "/api/v1/metrics/"):
			writeJSON(w, 404, []byte(`{"error":"not found"}`))
			return
		}
	}

	switch {
	case path == "/api/v1/nodes" && method == http.MethodGet:
		srv.listNodes(w)

	case path == "/api/v1/agents/register" && method == http.MethodPost:
		srv.agentRegister(w, r, p)

	case path == "/api/v1/agents/heartbeat" && method == http.MethodPost:
		srv.agentHeartbeat(w, r, p)

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

	// Per-tenant Prometheus gauges (v4 P6; admin-gated above — the open
	// /metrics deliberately omits tenant labels, ADR-0010).
	case path == "/api/v1/metrics/tenants" && method == http.MethodGet:
		srv.serveTenantMetrics(w)

	// Template catalog: GET is tenant-visible (tenants must be able to
	// discover what they can create from); mutations are admin-only via
	// handler-level guards.
	case path == "/api/v1/templates" && method == http.MethodGet:
		srv.listTemplates(w, tenant)

	case path == "/api/v1/templates" && method == http.MethodPost:
		srv.createTemplate(w, r, tenant)

	default:
		// Path-segment routes with {id}.
		if id, ok := matchSuffix(path, "/api/v1/tenants/", "/keys"); ok {
			switch method {
			case http.MethodPost:
				srv.createTenantKey(w, r, id)
				return
			case http.MethodGet:
				srv.listTenantKeys(w, id)
				return
			}
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
				srv.unexposeSandbox(w, r, id, name, tenant)
				return
			}
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/fork"); ok && method == http.MethodPost {
			srv.forkSandbox(w, r, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/sleep"); ok && method == http.MethodPost {
			srv.sleepSandbox(w, r, id, tenant)
			return
		}
		if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/wake"); ok && method == http.MethodPost {
			srv.wakeSandbox(w, r, id, tenant)
			return
		}
		for _, action := range []string{"stop", "start", "pause", "resume"} {
			if id, ok := matchSuffix(path, "/api/v1/sandboxes/", "/"+action); ok && method == http.MethodPost {
				srv.sandboxAction(w, r, id, action, tenant)
				return
			}
		}
		if id, ok := matchExact(path, "/api/v1/sandboxes/"); ok {
			switch method {
			case http.MethodGet:
				srv.getSandbox(w, id, tenant)
			case http.MethodDelete:
				srv.deleteSandbox(w, r, id, p)
			default:
				writeJSON(w, 404, []byte(`{"error":"not found"}`))
			}
			return
		}
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
	}
}

// validDisplayName bounds the free-form names a caller may attach to a
// sandbox (name, namespace, fork child name) — the only user-supplied strings
// in the API that had neither a length nor a charset. 1..64 bytes of
// printable ASCII, matching the bounds tenant names and join hostnames
// already carry. HTML-significant bytes are refused outright: these strings
// are rendered by the operator console, and a name is not a place that ever
// needs them.
func validDisplayName(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return false
		}
		switch c {
		case '<', '>', '"', '\'', '&':
			return false
		}
	}
	return true
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

// agentPort is felucca-agent's listen port: the agent's own default, the port
// ARCHITECTURE and DEPLOYMENT document throughout, and the one the deployment
// guide's firewall rules open from the control plane to a worker. It is also
// the only port feluccad will accept a NEW node address on — the port was never
// inspected, so "10.0.0.5:6379" and "203.0.113.9:80" were valid node addresses
// and aimed feluccad's HTTP client (bearer header and all) at whatever was
// listening there. Addresses enrolled before this check keep working through
// knownNodeAddr.
const agentPort = 9090

// validNodeAddr reports whether an agent-supplied address is one feluccad may
// dial. Accepting an address is accepting to speak to whatever answers there
// with a bearer credential attached, so it has to be a literal, routable
// unicast address on the agent's port. Names are refused because what they
// resolve to is not feluccad's decision, and when the wg overlay is configured
// a worker that is not on it cannot be a worker at all.
func (srv *Server) validNodeAddr(addr string) bool {
	host, port := agentclient.SplitHostPort(addr)
	if port != agentPort {
		return false
	}
	ip := net.ParseIP(nodeHostKey(host))
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.IsLinkLocalUnicast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if srv.cfg.WgIP != "" {
		_, overlay, err := net.ParseCIDR(srv.cfg.WgIP)
		if err != nil || !overlay.Contains(ip) {
			return false
		}
	}
	return true
}

// knownNodeAddr reports whether the address is already an enrolled node's.
// Such a re-registration grants no new reach — feluccad dials that address
// already — and exempting it keeps a fleet enrolled before validNodeAddr
// existed able to re-register instead of being stranded by an upgrade.
func (srv *Server) knownNodeAddr(addr string) bool {
	if addr == "" {
		return false
	}
	srv.st.Lock()
	defer srv.st.Unlock()
	for _, n := range srv.st.Nodes {
		if n.Addr == addr {
			return true
		}
	}
	return false
}

// maxHostnameLen bounds a node hostname on every route that accepts one.
// nodeJoin has always capped it; agentRegister only checked it was non-empty,
// and its hostname is the KEY state.RegisterNode appends a node record under.
const (
	maxHostnameLen    = 128
	maxHostnameLenStr = "128" // for the 400 body; keep in step with the above
)

// validNodeHostname reports whether a hostname is one feluccad will record for a
// node: 1..maxHostnameLen bytes of [A-Za-z0-9._-], the character set a real host
// name is drawn from. The charset is not decoration — the value is a map key, a
// log field and a console label, and none of those want control bytes in them.
func validNodeHostname(s string) bool {
	if s == "" || len(s) > maxHostnameLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_':
		default:
			return false
		}
	}
	return true
}

// nodeMayRegisterLocked decides whether a NODE principal may write the
// registration it just sent. The admin is unrestricted (p.node is false there);
// this is only about what a worker can do with its own credential.
//
// Two things it must not be able to do, both of which would break "worth one
// worker" outright:
//
//   - advertise an address that is not its own — feluccad would then dial another
//     node's workloads at an address this worker controls;
//   - take over another node's RECORD — state.RegisterNode matches on hostname,
//     so registering as "worker-B" would repoint every sandbox on B here.
//
// The consequence is that a node whose address changes cannot re-register itself;
// it re-enrolls (or an operator registers it with the admin token, which still
// works). That is the safe direction: a stuck node is visible and recoverable,
// a silently redirected one is neither.
//
// It is a state.NodeAdmission, which is to say it runs INSIDE the state lock
// that state.RegisterNodeGuarded also appends under, and it therefore takes the
// node slice rather than reaching for the lock itself. That is load-bearing:
// while this was a method that locked on its own, the check and the append were
// two separate acquisitions, and N concurrent registrations from one credential
// could all pass before any of them appended. The guarantee below is only worth
// what the critical section is — see state.NodeAdmission, and quotaExceededLocked
// for the same fix applied to the tenant-quota TOCTOU.
//
// Because it holds the global state lock, it does only what is written here:
// a linear scan and string comparisons, no I/O and no second lock.
func nodeMayRegisterLocked(nodes []*model.Node, key, hostname, addr string) (string, bool) {
	if nodeCredKeyFor(addr) != key {
		return "a node credential may only register its own address", false
	}
	for _, n := range nodes {
		own := nodeCredKeyFor(n.Addr) == key
		if n.Hostname == hostname && !own {
			return "that hostname belongs to another node", false
		}
		// ONE credential, ONE node record. Pinning only the ADDRESS was not
		// enough: state.RegisterNode keys on HOSTNAME and appends a fresh record
		// for every unseen one, and reclamation only ever touches records that
		// are provably dead (state.StaleNodeTTL). So a compromised worker could
		// register unlimited hostnames against its own (correctly pinned) address
		// and grow srv.st.Nodes without bound — every one of them a full
		// SaveSnapshot, a pushPoolsTo goroutine, and another entry for this scan,
		// knownNodeAddr, FindNode and PickNode to walk under the global state lock.
		//
		// A worker's hostname is stable, and a genuine change is the same kind of
		// event as an address change: an operator act (register with the admin
		// token, or re-enroll). Same safe direction as the address pin — a stuck
		// node is visible and recoverable.
		if own && n.Hostname != hostname {
			return "this node is already registered under another hostname", false
		}
	}
	return "", true
}

func (srv *Server) agentRegister(w http.ResponseWriter, r *http.Request, p principal) {
	var req struct {
		Hostname string `json:"hostname"`
		Addr     string `json:"addr"`
		CPUs     uint32 `json:"cpus"`
		MemTotal uint64 `json:"mem_total_mib"`
		// JoinToken is optional and turns a bare registration into an
		// operator-authorized enrollment: it is the one-time, admin-issued
		// credential from POST /api/v1/join-tokens, and presenting it is what
		// buys this address its own feluccad→agent token. Deployments running
		// the wg overlay spend the token at /api/v1/nodes/join instead and
		// leave this empty.
		JoinToken string `json:"join_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if !validNodeHostname(req.Hostname) {
		writeJSON(w, 400, []byte(`{"error":"hostname must be 1-`+maxHostnameLenStr+` characters of [A-Za-z0-9._-]"}`))
		return
	}
	if !srv.validNodeAddr(req.Addr) && !srv.knownNodeAddr(req.Addr) {
		writeJSON(w, 400, []byte(`{"error":"invalid addr"}`))
		return
	}
	// A node credential can never MINT or ROTATE one: spending a join token is an
	// enrollment, and enrollment is the operator's act. Without this, one
	// compromised worker plus any join token could rotate another node's
	// credential and take it off the control plane.
	//
	// It also means the registration pin below cannot burn a token: for a node
	// principal the enrollment block is unreachable, so a refusal there never
	// costs an operator anything.
	if p.node && req.JoinToken != "" {
		writeJSON(w, 403, []byte(`{"error":"a node credential cannot spend a join token: enroll with the admin token"}`))
		return
	}
	// The pin a node principal is held to, as an admission check the state store
	// runs under the very lock it appends under. Nil for the admin, who is
	// unrestricted. See nodeMayRegisterLocked.
	var admit state.NodeAdmission
	if p.node {
		admit = func(nodes []*model.Node) (string, bool) {
			return nodeMayRegisterLocked(nodes, p.key, req.Hostname, req.Addr)
		}
	}
	now := time.Now().Unix()

	// Enrollment before any state change: the address checks above have run,
	// so a 400 never burns the token, and a rejected enrollment leaves no node
	// row behind. The token is consumed here — one token, one node.
	agentToken := ""
	if req.JoinToken != "" {
		if !strings.HasPrefix(req.JoinToken, "felucca_jt_") {
			writeJSON(w, 401, []byte(`{"error":"invalid, used, or expired join token"}`))
			return
		}
		ok, err := srv.db.ConsumeJoinToken(hashSecret(req.JoinToken), now)
		if err != nil {
			slog.Error("consume join token", "err", err)
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		if !ok {
			writeJSON(w, 401, []byte(`{"error":"invalid, used, or expired join token"}`))
			return
		}
		agentToken = newNodeToken()
		host, port := agentclient.SplitHostPort(req.Addr)
		if err := srv.putNodeCred(host, port, agentToken, now); err != nil {
			slog.Error("persist node credential", "node", nodeCredKey(host, port), "err", err)
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		slog.Info("node enrolled with its own agent credential", "node", nodeCredKey(host, port), "hostname", req.Hostname)
	}

	reg := srv.st.RegisterNodeGuarded(req.Hostname, req.Addr, req.CPUs, req.MemTotal, now, admit)
	if reg.Refused != "" {
		slog.Warn("node registration refused", "node", p.key, "hostname", req.Hostname, "addr", req.Addr, "reason", reg.Refused)
		writeJSON(w, 403, []byte(`{"error":"`+reg.Refused+`"}`))
		return
	}
	if len(reg.Reclaimed) > 0 {
		// The ceiling was hit and records that had been silent for
		// state.StaleNodeTTL with no sandbox on them were dropped to make room.
		// Loud, because it is the only path that ever removes a node record.
		slog.Warn("node table was full: reclaimed node records that had not heartbeated for a very long time and held no sandboxes",
			"reclaimed", reg.Reclaimed, "stale_after_s", state.StaleNodeTTL, "max", state.MaxNodes)
	}
	id := reg.ID
	if id == "" {
		// state.MaxNodes reached, this hostname is not already a record, and
		// nothing in the table was provably dead enough to reclaim.
		slog.Error("node registration refused: the node table is full and no record was stale enough to reclaim",
			"hostname", req.Hostname, "addr", req.Addr, "max", state.MaxNodes, "stale_after_s", state.StaleNodeTTL)
		writeJSON(w, 503, []byte(`{"error":"node limit reached"}`))
		return
	}
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	// Push the current warm-pool specs to the (re)registering node off the
	// response path (v4 P4). Pools have exactly one delivery channel
	// (PUT /v1/pools), so empty-list teardown semantics are unambiguous.
	go srv.pushPoolsTo(req.Addr)
	idJSON, _ := json.Marshal(id)
	body := `{"id":` + string(idJSON) + `}`
	if agentToken != "" {
		// Shown exactly once, and only to the enrollment that minted it: a
		// later plain registration of the same address never re-reveals it, so
		// one worker cannot read another's credential out of feluccad. A worker
		// that lost its copy re-enrolls with a fresh join token, which rotates.
		tokJSON, _ := json.Marshal(agentToken)
		body = `{"id":` + string(idJSON) + `,"agent_token":` + string(tokJSON) + `}`
	}
	writeJSON(w, 200, []byte(body))
}

// nodeOwnsNodeID reports whether the node id belongs to the principal's own node.
func (srv *Server) nodeOwnsNodeID(p principal, id string) bool {
	srv.st.Lock()
	defer srv.st.Unlock()
	n := srv.st.FindNode(id)
	return n != nil && nodeCredKeyFor(n.Addr) == p.key
}

func (srv *Server) agentHeartbeat(w http.ResponseWriter, r *http.Request, p principal) {
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
	// A node credential heartbeats for ITS node and no other. The stats a
	// heartbeat carries steer PickNode, so an unbound one would let a worker
	// advertise a rival as full (or itself as empty) and choose where every new
	// sandbox lands. Same 404 as an unknown id: no existence leak.
	if p.node && !srv.nodeOwnsNodeID(p, req.ID) {
		writeJSON(w, 404, []byte(`{"error":"unknown node"}`))
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
	// Create-to-201 latency histogram (v4 P6): started at entry so template
	// resolution, scheduling, and the agent round-trip are all included —
	// the number a caller experiences.
	createStart := time.Now()
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
	if !validDisplayName(req.Name) {
		writeJSON(w, 400, []byte(`{"error":"invalid name"}`))
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
	if !validDisplayName(namespace) {
		writeJSON(w, 400, []byte(`{"error":"invalid namespace"}`))
		return
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

	// Tenant quota gate (admin is unmetered). The limits are read here, off
	// the lock; the usage summation happens inside the critical section that
	// also does the insert, so concurrent creates cannot all admit against
	// the same pre-request totals.
	var quota *store.Tenant
	if tenant != adminTenant {
		quota = srv.quotaRow(tenant)
	}

	// Schedule: pick ready node with lowest vm_count. Create sandbox record in
	// "creating" state under the lock, capturing agent address.
	var agentAddr, sbID string
	if status, body := func() (int, string) {
		srv.st.Lock()
		defer srv.st.Unlock()
		if msg := srv.quotaExceededLocked(quota, vcpus, memMiB, diskGB); msg != "" {
			return 429, `{"error":"quota exceeded: ` + msg + `"}`
		}
		node := srv.st.PickNode(now)
		if node == nil {
			return 503, `{"error":"no ready node"}`
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
		return 0, ""
	}(); status != 0 {
		writeJSON(w, status, []byte(body))
		return
	}

	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
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

	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, "/v1/vms", agentBody, reqID(r.Context()))
	if err != nil {
		srv.st.SetSandboxState(sbID, model.StateError)
		if perr := srv.persist(); perr != nil {
			slog.Error("persist failed", "err", perr)
		}
		slog.Error("agent unreachable", "sandbox", sbID, "request_id", reqID(r.Context()), "err", err)
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		srv.st.SetSandboxState(sbID, model.StateError)
		if perr := srv.persist(); perr != nil {
			slog.Error("persist failed", "err", perr)
		}
		slog.Error("agent create failed", "sandbox", sbID, "request_id", reqID(r.Context()), "status", resp.Status)
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
		slog.Error("persist failed", "err", err)
	}

	srv.st.Lock()
	defer srv.st.Unlock()
	sb := srv.st.FindSandbox(sbID)
	if sb == nil {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	srv.recordUsage(sb, "created")
	observeCreateMs(uint64(time.Since(createStart).Milliseconds()))
	b, _ := json.Marshal(sb)
	writeJSON(w, 201, b)
}

func (srv *Server) sandboxAction(w http.ResponseWriter, r *http.Request, id, action, tenant string) {
	var agentAddr string
	if status, body := func() (int, string) {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || !tenantOwns(tenant, sb) {
			return 404, `{"error":"not found"}`
		}
		if sb.NodeID == nil {
			return 409, `{"error":"sandbox has no node"}`
		}
		node := srv.st.FindNode(*sb.NodeID)
		if node == nil {
			return 409, `{"error":"node gone"}`
		}
		agentAddr = node.Addr
		return 0, ""
	}(); status != 0 {
		writeJSON(w, status, []byte(body))
		return
	}

	agentPath := fmt.Sprintf("/v1/vms/%s/%s", id, action)
	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, agentPath, nil, reqID(r.Context()))
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
			slog.Error("persist failed", "err", err)
		}
		func() {
			srv.st.Lock()
			defer srv.st.Unlock()
			srv.recordUsage(srv.st.FindSandbox(id), usageEvent)
		}()
	}
	writeEmpty(w, 200)
}

func (srv *Server) deleteSandbox(w http.ResponseWriter, r *http.Request, id string, p principal) {
	var agentAddr string
	var usage model.Sandbox
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || !tenantOwns(p.tenant, sb) {
			return false
		}
		// Capture the usage shape before removal.
		usage = *sb
		if sb.NodeID != nil {
			node := srv.st.FindNode(*sb.NodeID)
			if node != nil {
				agentAddr = node.Addr
			}
		}
		return true
	}() {
		// Unknown and foreign ids are indistinguishable: 404, per the v2
		// contract (and no cross-tenant existence leak).
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}

	// The agent call is NOT best-effort. Dropping the record while the microVM
	// is still running on the worker orphans it: it keeps its vCPUs, its memory
	// and its nft DNAT rules, it is unbillable, and no control-plane path can
	// ever reach it again — the operator has to go find it by hand on the node.
	// So a failed delete keeps the record and says so, and the caller retries.
	//
	// ?force=1 is the escape hatch for the case that would otherwise be
	// unresolvable — a node that is permanently gone — because a record that can
	// never be removed is its own kind of stuck. It is an explicit operator
	// decision, logged as one, and it is the only way a live VM can be
	// abandoned here.
	//
	// ADMIN ONLY, and not for tidiness: forcing is a decision to leave a VM
	// running that nothing will ever bill or reap, and only whoever can go look
	// at the node is in a position to take it. A tenant gets the 502 and the
	// sandbox stays theirs, which is the honest answer — their VM really is
	// still there.
	force := p.isAdmin() &&
		(r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true")
	if agentAddr != "" {
		agentPath := fmt.Sprintf("/v1/vms/%s", id)
		resp, err := srv.dialNode(agentAddr).do(http.MethodDelete, agentPath, nil, reqID(r.Context()))
		// 404 from the agent means the VM is already gone: the record should go
		// with it.
		gone := err == nil && (resp.Status < 300 || resp.Status == 404)
		if !gone {
			status := 0
			if resp != nil {
				status = resp.Status
			}
			if !force {
				slog.Error("delete: agent refused, keeping the sandbox record so the VM is not orphaned",
					"sandbox", id, "request_id", reqID(r.Context()), "status", status, "err", err)
				writeJSON(w, 502, []byte(`{"error":"agent delete failed: the sandbox is kept so its VM is not orphaned — retry once the node is reachable; an operator can drop the record anyway with ?force=1"}`))
				return
			}
			slog.Warn("delete: forced past a failed agent call — the VM may still be running on the node and is now unreachable from the control plane",
				"sandbox", id, "node", agentAddr, "request_id", reqID(r.Context()), "status", status, "err", err)
		}
	}

	srv.st.RemoveSandbox(id)
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	srv.recordUsage(&usage, "deleted")
	writeEmpty(w, 204)
}

func (srv *Server) sleepSandbox(w http.ResponseWriter, r *http.Request, id, tenant string) {
	agentAddr, ok := srv.resolveAgent(w, r, id, tenant)
	if !ok {
		return
	}
	agentPath := fmt.Sprintf("/v1/vms/%s/sleep", id)
	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, agentPath, nil, reqID(r.Context()))
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		writeJSON(w, 502, []byte(`{"error":"agent sleep failed"}`))
		return
	}

	srv.st.SetSandboxState(id, model.StateSleeping)

	var b []byte
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil {
			return false
		}
		sb.SleptAt = time.Now().Unix() // the auto-delete TTL clock (v4 P5.2)
		srv.recordUsage(sb, "slept")
		b, _ = json.Marshal(sb)
		return true
	}() {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}
	writeJSON(w, 200, b)
}

func (srv *Server) wakeSandbox(w http.ResponseWriter, r *http.Request, id, tenant string) {
	agentAddr, ok := srv.resolveAgent(w, r, id, tenant)
	if !ok {
		return
	}
	agentPath := fmt.Sprintf("/v1/vms/%s/wake", id)
	wakeStart := time.Now()
	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, agentPath, nil, reqID(r.Context()))
	if err != nil {
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		writeJSON(w, 502, []byte(`{"error":"agent wake failed"}`))
		return
	}
	// Successful wake: histogram gets feluccad's wall-clock (v4 P6) — the
	// agent-reported wake_ms below stays the wire/legacy-counter value.
	observeWakeMs(uint64(time.Since(wakeStart).Milliseconds()))

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

	var sbJSON []byte
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil {
			return false
		}
		// A wake is activity: restart the idle clock, stop the TTL clock (v4 P5.2).
		sb.LastActivity = time.Now().Unix()
		sb.SleptAt = 0
		srv.recordUsage(sb, "woken")
		// Append "wake_ms" as the final key before the closing brace (Zig behavior).
		sbJSON, _ = json.Marshal(sb)
		return true
	}() {
		writeJSON(w, 500, []byte(`{"error":"lost sandbox"}`))
		return
	}
	// Persist AFTER releasing the lock (SaveSnapshot takes it) so the
	// snapshot carries the fresh activity clocks.
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
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
	if !validDisplayName(childName) {
		writeJSON(w, 400, []byte(`{"error":"invalid name"}`))
		return
	}

	now := time.Now().Unix()

	// The child counts against the owning tenant's quota (admin parents are
	// unmetered). Resolve which tenant that is under a short lock so its
	// quota row can be fetched off the lock; the summation and the insert
	// then share one unbroken critical section below.
	var childTenant string
	if !func() bool {
		srv.st.Lock()
		defer srv.st.Unlock()
		parent := srv.st.FindSandbox(parentID)
		if parent == nil || !tenantOwns(tenant, parent) {
			return false
		}
		childTenant = parent.TenantID
		return true
	}() {
		writeJSON(w, 404, []byte(`{"error":"not found"}`))
		return
	}

	var quota *store.Tenant
	if childTenant != adminTenant {
		quota = srv.quotaRow(childTenant)
	}

	var agentAddr, childID string
	var parentExposes []model.Expose
	if status, body := func() (int, string) {
		srv.st.Lock()
		defer srv.st.Unlock()
		// Re-validate under the re-acquired lock.
		parent := srv.st.FindSandbox(parentID)
		if parent == nil || !tenantOwns(tenant, parent) {
			return 404, `{"error":"not found"}`
		}
		if msg := srv.quotaExceededLocked(quota, parent.VCPUs, parent.MemMiB, parent.DiskGB); msg != "" {
			return 429, `{"error":"quota exceeded: ` + msg + `"}`
		}
		switch parent.State {
		case model.StateRunning, model.StatePaused, model.StateSleeping:
			// ok
		default:
			return 409, `{"error":"parent must be running, paused, or sleeping"}`
		}
		if parent.NodeID == nil {
			return 409, `{"error":"parent has no node"}`
		}
		node := srv.st.FindNode(*parent.NodeID)
		if node == nil {
			return 409, `{"error":"node gone"}`
		}
		agentAddr = node.Addr
		// Snapshot ingress config to re-create on the child: same names/guest
		// ports, fresh node ports (the smoke contract — a fork's services are
		// reachable on the child's own URLs).
		parentExposes = append([]model.Expose(nil), parent.Exposes...)
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
		return 0, ""
	}(); status != 0 {
		writeJSON(w, status, []byte(body))
		return
	}

	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
	}

	agentBody, _ := json.Marshal(struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{childID, childName})
	agentPath := fmt.Sprintf("/v1/vms/%s/fork", parentID)
	resp, err := srv.dialNode(agentAddr).do(http.MethodPost, agentPath, agentBody, reqID(r.Context()))
	if err != nil {
		srv.st.SetSandboxState(childID, model.StateError)
		if perr := srv.persist(); perr != nil {
			slog.Error("persist failed", "err", perr)
		}
		slog.Error("agent unreachable", "sandbox", childID, "request_id", reqID(r.Context()), "err", err)
		writeJSON(w, 502, []byte(`{"error":"agent unreachable"}`))
		return
	}
	if resp.Status >= 300 {
		srv.st.SetSandboxState(childID, model.StateError)
		if perr := srv.persist(); perr != nil {
			slog.Error("persist failed", "err", perr)
		}
		slog.Error("agent fork failed", "sandbox", childID, "request_id", reqID(r.Context()), "status", resp.Status)
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

	// Re-expose the parent's services on the child (fresh node ports) under the
	// ingress lock and the ingress caps — see reExposeFork. Best-effort: a
	// failed re-expose degrades that one URL, not the fork — the operator can
	// retry via the expose API.
	srv.reExposeFork(agentAddr, childID, parentExposes, reqID(r.Context()))
	if err := srv.persist(); err != nil {
		slog.Error("persist failed", "err", err)
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
	// Parse request body. cmd decodes straight into []string: a []interface{}
	// of boxed strings costs ~15-20x the wire bytes and is then copied again,
	// and a non-string element is rejected by the decoder with the same 400
	// the hand-rolled type switch used to produce.
	var req struct {
		Cmd       []string `json:"cmd"`
		TimeoutMs *int64   `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	// Validate cmd: must be a non-empty, bounded array of bounded strings.
	if len(req.Cmd) == 0 {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	// argv[0] is the program name; a JSON null decodes to an empty string
	// rather than failing the decode, so it is refused here instead.
	if req.Cmd[0] == "" {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if len(req.Cmd) > maxExecArgs {
		writeJSON(w, 400, []byte(`{"error":"cmd too long"}`))
		return
	}
	for _, s := range req.Cmd {
		if len(s) > maxExecArgLen {
			writeJSON(w, 400, []byte(`{"error":"cmd argument too long"}`))
			return
		}
	}
	cmdStrings := req.Cmd

	// Determine timeout_ms: default 30000, bounded 1..300000. The bound is
	// two-sided and rejecting rather than clamping: a negative value
	// overflows time.Duration(timeoutMs)*time.Millisecond into a ~290-year
	// POSITIVE duration, and that value feeds both the connection deadline
	// below and the agent request context — inverting the slow-loris guard
	// they exist to provide. Validated here, before the ?stream=1 branch, so
	// both arms are covered by the one check.
	timeoutMs := int64(30000)
	if req.TimeoutMs != nil {
		timeoutMs = *req.TimeoutMs
		if timeoutMs < 1 || timeoutMs > 300000 {
			writeJSON(w, 400, []byte(`{"error":"timeout_ms out of range"}`))
			return
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
	var agentAddr string
	var execEvent store.UsageEvent
	if status, body := func() (int, string) {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || !tenantOwns(tenant, sb) {
			return 404, `{"error":"not found"}`
		}
		if sb.State != model.StateRunning {
			return 409, `{"error":"not running"}`
		}
		if sb.NodeID == nil {
			return 404, `{"error":"not found"}`
		}
		node := srv.st.FindNode(*sb.NodeID)
		if node == nil {
			return 404, `{"error":"not found"}`
		}
		agentAddr = node.Addr
		// v4 P5: an exec restarts the idle clock and leaves a metering event
		// (the usage endpoint counts execs per tenant per window). Build the
		// event under the lock but append it AFTER unlocking — exec is the hot
		// path and the global state lock must not gate a synchronous sqlite
		// write, which is why the append is outside this closure and not merely
		// after a `defer`.
		sb.LastActivity = time.Now().Unix()
		execEvent = usageEventFor(sb, "exec")
		return 0, ""
	}(); status != 0 {
		writeJSON(w, status, []byte(body))
		return
	}
	if err := srv.db.AppendUsage(execEvent); err != nil {
		slog.Error("usage event", "err", err)
	}

	// Build forwarded body.
	agentBody, _ := json.Marshal(struct {
		Cmd       []string `json:"cmd"`
		TimeoutMs int64    `json:"timeout_ms"`
	}{cmdStrings, timeoutMs})

	reqTimeout := time.Duration(timeoutMs)*time.Millisecond + 10*time.Second
	execStart := time.Now()
	resp, err := srv.dialNode(agentAddr).exec(id, agentBody, reqTimeout, reqID(r.Context()))
	if err != nil {
		slog.Error("agent exec failed", "sandbox", id, "request_id", reqID(r.Context()), "err", err)
		writeJSON(w, 502, []byte(`{"error":"agent exec failed"}`))
		return
	}
	switch resp.Status {
	case 200:
		// Histogram counts only completed execs (v4 P6): a 501/502 is an
		// availability failure, not exec latency.
		observeExecMs(uint64(time.Since(execStart).Milliseconds()))
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
	var agentAddr string
	var execEvent store.UsageEvent
	if status, body := func() (int, string) {
		srv.st.Lock()
		defer srv.st.Unlock()
		sb := srv.st.FindSandbox(id)
		if sb == nil || !tenantOwns(tenant, sb) {
			return 404, `{"error":"not found"}`
		}
		if sb.State != model.StateRunning {
			return 409, `{"error":"not running"}`
		}
		if sb.NodeID == nil {
			return 404, `{"error":"not found"}`
		}
		node := srv.st.FindNode(*sb.NodeID)
		if node == nil {
			return 404, `{"error":"not found"}`
		}
		agentAddr = node.Addr
		// v4 P5: an exec restarts the idle clock and leaves a metering event
		// (same as the buffered arm — append after unlocking, off the hot path).
		sb.LastActivity = time.Now().Unix()
		execEvent = usageEventFor(sb, "exec")
		return 0, ""
	}(); status != 0 {
		writeJSON(w, status, []byte(body))
		return
	}
	if err := srv.db.AppendUsage(execEvent); err != nil {
		slog.Error("usage event", "err", err)
	}

	// Build forwarded body: the buffered shape plus "stream":true.
	agentBody, _ := json.Marshal(struct {
		Cmd       []string `json:"cmd"`
		TimeoutMs int64    `json:"timeout_ms"`
		Stream    bool     `json:"stream,omitempty"`
	}{cmd, timeoutMs, true})

	reqTimeout := time.Duration(timeoutMs)*time.Millisecond + 70*time.Second
	resp, cancel, err := srv.dialNode(agentAddr).execStream(id, agentBody, reqTimeout, reqID(r.Context()))
	if err != nil {
		slog.Error("agent exec stream failed", "sandbox", id, "request_id", reqID(r.Context()), "err", err)
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
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
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
func (srv *Server) resolveAgent(w http.ResponseWriter, r *http.Request, id, tenant string) (string, bool) {
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

	// Snapshot under the lock, format outside it. Every sandbox operation
	// needs this lock; a scrape must not hold it for the length of a response
	// build.
	type poolGauge struct {
		hostname string
		size     uint32
	}
	var readyCount int
	var pools []poolGauge
	counts := make(map[model.SandboxState]int)
	var requestCount, wakeMsLast, wakeTotal, wakeMsSum, forksTotal, execsTotal uint64
	func() {
		srv.st.Lock()
		defer srv.st.Unlock()
		for _, n := range srv.st.Nodes {
			if n.NodeStatus(now) == "ready" {
				readyCount++
			}
			pools = append(pools, poolGauge{n.Hostname, n.PoolSize})
		}
		// Count sandboxes by state in enum declaration order.
		for _, sb := range srv.st.Sandboxes {
			counts[sb.State]++
		}
		requestCount = srv.st.RequestCount
		wakeMsLast, wakeTotal, wakeMsSum = srv.st.WakeMsLast, srv.st.WakeTotal, srv.st.WakeMsSum
		forksTotal, execsTotal = srv.st.ForksTotal, srv.st.ExecsTotal
	}()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# HELP felucca_nodes_ready Number of ready nodes\n# TYPE felucca_nodes_ready gauge\nfelucca_nodes_ready %d\n", readyCount)
	buf.WriteString("# HELP felucca_sandboxes_total Sandboxes by state\n# TYPE felucca_sandboxes_total gauge\n")
	for _, st := range model.AllStates {
		fmt.Fprintf(&buf, "felucca_sandboxes_total{state=%q} %d\n", string(st), counts[st])
	}
	fmt.Fprintf(&buf, "# HELP felucca_api_requests_total Total API requests\n# TYPE felucca_api_requests_total counter\nfelucca_api_requests_total %d\n", requestCount)
	fmt.Fprintf(&buf, "# HELP felucca_wake_ms_last Last wake latency in ms\n# TYPE felucca_wake_ms_last gauge\nfelucca_wake_ms_last %d\n", wakeMsLast)
	fmt.Fprintf(&buf, "# HELP felucca_wake_total Total wakes\n# TYPE felucca_wake_total counter\nfelucca_wake_total %d\n", wakeTotal)
	fmt.Fprintf(&buf, "# HELP felucca_wake_ms_sum Sum of wake latencies (ms)\n# TYPE felucca_wake_ms_sum counter\nfelucca_wake_ms_sum %d\n", wakeMsSum)
	fmt.Fprintf(&buf, "# HELP felucca_forks_total Total forks\n# TYPE felucca_forks_total counter\nfelucca_forks_total %d\n", forksTotal)
	fmt.Fprintf(&buf, "# HELP felucca_execs_total Total exec attempts\n# TYPE felucca_execs_total counter\nfelucca_execs_total %d\n", execsTotal)
	buf.WriteString("# HELP felucca_pool_size Warm-pool depth per node\n# TYPE felucca_pool_size gauge\n")
	for _, p := range pools {
		fmt.Fprintf(&buf, "felucca_pool_size{node=%q} %d\n", p.hostname, p.size)
	}

	// Latency histograms (v4 P6, ADR-0010): always emitted, in-memory only.
	appendHistograms(&buf)

	// NB (v4 P5): per-tenant gauges with the tenant_id as a label are
	// deliberately NOT emitted here. /metrics is admin-gated now, but the
	// split still holds: this scrape is the fleet view, and per-tenant
	// inventory + resource footprint belong on the surfaces scoped to a
	// tenant — GET /api/v1/tenants/{id}/usage and the admin-only
	// GET /api/v1/metrics/tenants.

	body := buf.Bytes()
	h := w.Header()
	h.Set("Content-Type", "text/plain; version=0.0.4")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	w.Write(body) //nolint:errcheck
}

// ---- Static file serving ----

// uiCSP is the console's Content-Security-Policy. script-src 'self' is the
// half that matters: it is what stops an injected string from executing as an
// inline handler, and default-src/connect-src 'none'/'self' is what stops a
// payload that does run from posting the admin token off-origin. 'unsafe-inline'
// stays on style-src only — style attributes cannot exfiltrate.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self'; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setUIHeaders applies the console's security headers. frame-ancestors and
// X-Frame-Options both deny framing because the console exposes one-click
// destructive actions (Delete/Stop).
func setUIHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func (srv *Server) serveStatic(w http.ResponseWriter, path string) {
	setUIHeaders(w)
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
// API responses carry nosniff (a JSON body must never be interpreted as
// anything else) and no-referrer (URLs on this API carry sandbox and tenant
// ids).
func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
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
