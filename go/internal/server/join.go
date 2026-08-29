// Node-join endpoints for the WireGuard overlay (v4 P2): admin-issued
// one-time join tokens and the exchange that enrolls a worker as a wg peer.
// The join request authenticates with the token itself — the joining worker
// has no API key — so /api/v1/nodes/join is routed before the bearer gate.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/store"
	"github.com/alpham/infra-saas/hearth/internal/wg"
)

// rejoinAuthorized decides whether a re-join — a join naming a pubkey that is
// already enrolled — may rotate that node's credential.
//
// The proof is possession of the credential hearthd CURRENTLY hands that node:
// its own hearth_nt_ token once it has one, or the shared token while it is
// still grandfathered. One rule for both, resolved through the very function
// that decides what goes on the wire (agentTokenFor), so "prove you are that
// node" cannot drift from "this is what hearthd believes that node holds".
//
// An address with NO credential yet is authorized without proof, and that is not
// a hole: there is nothing to take away, so this cannot be the availability
// lever the check exists to close. It is also the case a legitimate retry lands
// in — the peer row is persisted before the kernel install, so a worker retrying
// after `wg add peer` failed has a peer and no credential — and the case of a
// node enrolled before per-node credentials existed, whose whole point is to
// re-join and get one.
//
// That "no credential yet" case has to be the DEFINITE one, though, and it is
// why this reads a nodeCredState rather than an empty string. agentTokenFor
// returns "" for three different things, only one of which is "there is nothing
// to prove": the other two are "the credential store could not be read" and, for
// a grandfathered node, "the shared token is itself empty". Reading either as
// authorization would mean a transient store error — or one an attacker can
// induce by loading the very store this route reads — silently switches the
// possession check OFF, and a join-token holder can rotate another node's
// credential again. Anything short of credAbsent therefore refuses: a re-join is
// retryable, an evicted worker is not.
func (srv *Server) rejoinAuthorized(overlayIP, presented string) bool {
	current, state := srv.resolveNodeCred(overlayIP, agentPort)
	switch state {
	case credAbsent:
		return true
	case credResolved:
		// An empty credential proves nothing and must never match an empty
		// (or any) presented token.
		if current == "" || presented == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(current), []byte(presented)) == 1
	default: // credUnknown
		slog.Error("re-join refused: the node credential store could not be read, so possession of this node's credential cannot be checked",
			"overlay_ip", overlayIP)
		return false
	}
}

// createJoinToken mints a one-time node-enrollment token (admin only; routing
// guards in serveAPI). The secret is shown exactly once; only its sha256 is
// stored. Tokens expire after store.JoinTokenTTL.
func (srv *Server) createJoinToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeHint string `json:"node_hint"`
	}
	// Empty body = no hint; anything else malformed is the caller's bug and
	// must 400 rather than silently dropping their label.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if len(req.NodeHint) > 64 {
		writeJSON(w, 400, []byte(`{"error":"node_hint too long (max 64)"}`))
		return
	}
	secret := "hearth_jt_" + newSecret(24)
	jt := &store.JoinToken{
		ID:        "jt-" + newSecret(4),
		TokenHash: hashSecret(secret),
		CreatedAt: time.Now().Unix(),
		NodeHint:  req.NodeHint,
	}
	if err := srv.db.CreateJoinToken(jt); err != nil {
		slog.Error("create join token", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	body, _ := json.Marshal(struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}{jt.ID, secret})
	writeJSON(w, 201, body)
}

// nodeJoin exchanges a one-time join token for a wg peer enrollment: the
// worker's overlay address plus everything it needs to configure its side of
// the tunnel. Order matters: credential shape first (uniform 401 for
// unauthenticated probes), then request validation (400s don't burn the
// token), and the token is consumed LAST — only after the peer is persisted
// and installed — so transient failures never burn it. Re-joining with an
// already-enrolled pubkey returns the same overlay address.
//
// This route is reachable without any established credential, so it carries the
// same per-source brute-force guard as the bearer gate — and the same rule: the
// guard is consulted only AFTER a credential has failed (srv.refuse). A valid
// join token is never refused because someone else guessed wrong, which matters
// most here: the throttle key in the shipped topology is one shared 127.0.0.1,
// and a worker that cannot enroll is a worker that never joins the fleet.
func (srv *Server) nodeJoin(w http.ResponseWriter, r *http.Request) {
	src := srv.throttleSource(r)

	// Credential shape check before anything is revealed or parsed.
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) || !strings.HasPrefix(auth[len(prefix):], "hearth_jt_") {
		srv.refuse(w, src, 401, `{"error":"unauthorized"}`)
		return
	}
	tokenHash := hashSecret(auth[len(prefix):])

	if srv.cfg.WgIP == "" {
		writeJSON(w, 503, []byte(`{"error":"wireguard overlay not configured"}`))
		return
	}

	var req struct {
		PubKey   string `json:"pubkey"`
		Hostname string `json:"hostname"`
		// NodeToken is the node's CURRENT hearth_nt_ credential, and it is
		// required only to RE-join: an already-enrolled pubkey. See the
		// possession check below.
		NodeToken string `json:"node_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if !wg.ValidPubKey(req.PubKey) {
		writeJSON(w, 400, []byte(`{"error":"invalid pubkey"}`))
		return
	}
	// The same cap agentRegister applies (validNodeHostname). It stays a bare
	// length check here: this hostname is a label on the wg peer row, not the
	// key a node record is appended under, and an empty one is a legal join.
	if len(req.Hostname) > maxHostnameLen {
		writeJSON(w, 400, []byte(`{"error":"hostname too long (max `+maxHostnameLenStr+`)"}`))
		return
	}

	// Startup validated cfg.WgIP parses; this also yields subnet + prefix.
	serverIP, ipnet, err := net.ParseCIDR(srv.cfg.WgIP)
	if err != nil {
		writeJSON(w, 500, []byte(`{"error":"bad server overlay config"}`))
		return
	}
	prefixLen, _ := ipnet.Mask.Size()

	// Serialize peek→allocate→persist→install→consume. In-process locking is
	// sufficient at the ADR-0002 single-hearthd scale point; the HA path moves
	// allocation into a store transaction (see ADR-0006).
	srv.joinMu.Lock()
	defer srv.joinMu.Unlock()

	now := time.Now().Unix()
	// The one store read an unauthenticated caller reaches on this route, and
	// therefore bounded like the bearer gate's: see credGate. joinMu already
	// serializes joins, so at most one slot is ever held here.
	release, ok := srv.credLookups.acquire(r.Context())
	if !ok {
		return // caller hung up
	}
	// Deferred release, scoped by the closure to the store call alone: the rest
	// of this handler does much slower work (the kernel `wg` install above all)
	// and must not sit on a lookup slot, but a panic inside CheckJoinToken must
	// not strand one either. See authenticate for what a stranded slot costs.
	tokenOK, err := func() (bool, error) {
		defer release()
		return srv.db.CheckJoinToken(tokenHash, now)
	}()
	if err != nil {
		slog.Error("check join token", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if !tokenOK {
		srv.refuse(w, src, 401, `{"error":"invalid, used, or expired join token"}`)
		return
	}

	var overlayIP string
	if existing, err := srv.db.GetWgPeerByPubKey(req.PubKey); err != nil {
		slog.Error("wg peer lookup", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	} else if existing != nil && ipnet.Contains(net.ParseIP(existing.OverlayIP)) {
		// Re-join keeps its address — but only while it's still inside the
		// configured subnet (an operator subnet change invalidates old rows).
		overlayIP = existing.OverlayIP
		// ...and it has to PROVE it is that node.
		//
		// A join token says "an operator authorized an enrollment". It does not
		// say WHICH node, and the pubkey naming the node is written by the
		// caller. So a holder of any valid token could re-join under an existing
		// worker's public key, and the credential rotation below would replace
		// that worker's token with one it never receives: hearthd then 401s on
		// every call to it and the node is off the control plane, from a
		// credential that never established it. Possession of the node's current
		// token is the cheapest thing that actually distinguishes the node from
		// someone who merely knows its (public!) key.
		//
		// A worker that lost its credential re-enrolls under a FRESH wireguard
		// key, which is a first join and needs no proof. That is the recovery
		// path, and it costs the attacker the one thing they cannot forge.
		if !srv.rejoinAuthorized(overlayIP, req.NodeToken) {
			slog.Warn("re-join refused: no proof of possession for an already-enrolled pubkey",
				"overlay_ip", overlayIP, "hostname", req.Hostname)
			srv.refuse(w, src, 409, `{"error":"pubkey already enrolled: re-join must present the node's current agent token as node_token, or enroll with a fresh wireguard key"}`)
			return
		}
	} else {
		peers, err := srv.db.ListWgPeers()
		if err != nil {
			slog.Error("list wg peers", "err", err)
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		taken := make([]string, 0, len(peers))
		for _, p := range peers {
			taken = append(taken, p.OverlayIP)
		}
		overlayIP, err = wg.AllocateOverlayIP(srv.cfg.WgIP, taken)
		if err != nil {
			slog.Error("overlay allocation", "err", err)
			writeJSON(w, 503, []byte(`{"error":"overlay exhausted"}`))
			return
		}
	}
	if err := srv.db.CreateWgPeer(&store.WgPeer{
		PubKey:    req.PubKey,
		OverlayIP: overlayIP,
		Hostname:  req.Hostname,
		CreatedAt: now,
	}); err != nil {
		slog.Error("persist wg peer", "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if err := srv.addPeer(req.PubKey, overlayIP); err != nil {
		// Token NOT consumed: the worker can retry with the same token once
		// the host-side issue (sudo, missing wg binary) is fixed.
		slog.Error("wg add peer", "err", err)
		writeJSON(w, 500, []byte(`{"error":"wg peer install failed"}`))
		return
	}
	// Mint this node's own hearthd→agent credential, keyed on the overlay
	// address it will advertise. It replaces the shared admin token on every
	// control-plane call to this worker, so a token harvested off one node
	// (or off an address someone talked hearthd into dialing) is worth that
	// node and nothing else. A re-join rotates it: the join token authorizing
	// this exchange is one-time and operator-issued, which also makes re-join
	// the recovery path for a worker that lost its copy.
	//
	// The address is keyed with the agent's own port: validNodeAddr accepts a
	// new node address on no other, so this is the endpoint this worker will
	// register and the one hearthd will dial.
	agentToken := newNodeToken()
	if err := srv.putNodeCred(overlayIP, agentPort, agentToken, now); err != nil {
		// Before the token is burned, so the worker can retry.
		slog.Error("persist node credential", "overlay_ip", overlayIP, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	// Enrollment fully succeeded — burn the token now. A consume race is
	// excluded by joinMu; a false result here means the token was somehow
	// redeemed elsewhere, which must fail the request.
	if ok, err := srv.db.ConsumeJoinToken(tokenHash, now); err != nil || !ok {
		slog.Error("consume join token", "ok", ok, "err", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}

	// AgentToken is additive: an agent from before per-node credentials
	// ignores the unknown field and keeps requiring the shared token, which
	// is what its grandfathered credential row still hands it.
	body, _ := json.Marshal(struct {
		OverlayIP       string `json:"overlay_ip"`
		OverlayPrefix   int    `json:"overlay_prefix"`
		ServerOverlayIP string `json:"server_overlay_ip"`
		ServerPubKey    string `json:"server_pubkey"`
		ServerEndpoint  string `json:"server_endpoint"`
		KeepaliveS      int    `json:"keepalive_s"`
		AgentToken      string `json:"agent_token"`
	}{overlayIP, prefixLen, serverIP.String(), srv.wgPubKey, srv.cfg.WgEndpoint, int(srv.cfg.WgKeepalive), agentToken})
	writeJSON(w, 200, body)
}
