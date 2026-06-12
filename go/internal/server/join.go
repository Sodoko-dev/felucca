// Node-join endpoints for the WireGuard overlay (v4 P2): admin-issued
// one-time join tokens and the exchange that enrolls a worker as a wg peer.
// The join request authenticates with the token itself — the joining worker
// has no API key — so /api/v1/nodes/join is routed before the bearer gate.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/store"
	"github.com/alpham/infra-saas/hearth/internal/wg"
)

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
		fmt.Fprintf(os.Stderr, "create join token: %v\n", err)
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
func (srv *Server) nodeJoin(w http.ResponseWriter, r *http.Request) {
	// Credential shape check before anything is revealed or parsed.
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) || !strings.HasPrefix(auth[len(prefix):], "hearth_jt_") {
		writeJSON(w, 401, []byte(`{"error":"unauthorized"}`))
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, []byte(`{"error":"bad json"}`))
		return
	}
	if !wg.ValidPubKey(req.PubKey) {
		writeJSON(w, 400, []byte(`{"error":"invalid pubkey"}`))
		return
	}
	if len(req.Hostname) > 128 {
		writeJSON(w, 400, []byte(`{"error":"hostname too long (max 128)"}`))
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
	if ok, err := srv.db.CheckJoinToken(tokenHash, now); err != nil {
		fmt.Fprintf(os.Stderr, "check join token: %v\n", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	} else if !ok {
		writeJSON(w, 401, []byte(`{"error":"invalid, used, or expired join token"}`))
		return
	}

	var overlayIP string
	if existing, err := srv.db.GetWgPeerByPubKey(req.PubKey); err != nil {
		fmt.Fprintf(os.Stderr, "wg peer lookup: %v\n", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	} else if existing != nil && ipnet.Contains(net.ParseIP(existing.OverlayIP)) {
		// Re-join keeps its address — but only while it's still inside the
		// configured subnet (an operator subnet change invalidates old rows).
		overlayIP = existing.OverlayIP
	} else {
		peers, err := srv.db.ListWgPeers()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list wg peers: %v\n", err)
			writeJSON(w, 500, []byte(`{"error":"store error"}`))
			return
		}
		taken := make([]string, 0, len(peers))
		for _, p := range peers {
			taken = append(taken, p.OverlayIP)
		}
		overlayIP, err = wg.AllocateOverlayIP(srv.cfg.WgIP, taken)
		if err != nil {
			fmt.Fprintf(os.Stderr, "overlay allocation: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "persist wg peer: %v\n", err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}
	if err := srv.addPeer(req.PubKey, overlayIP); err != nil {
		// Token NOT consumed: the worker can retry with the same token once
		// the host-side issue (sudo, missing wg binary) is fixed.
		fmt.Fprintf(os.Stderr, "wg add peer: %v\n", err)
		writeJSON(w, 500, []byte(`{"error":"wg peer install failed"}`))
		return
	}
	// Enrollment fully succeeded — burn the token now. A consume race is
	// excluded by joinMu; a false result here means the token was somehow
	// redeemed elsewhere, which must fail the request.
	if ok, err := srv.db.ConsumeJoinToken(tokenHash, now); err != nil || !ok {
		fmt.Fprintf(os.Stderr, "consume join token: ok=%v err=%v\n", ok, err)
		writeJSON(w, 500, []byte(`{"error":"store error"}`))
		return
	}

	body, _ := json.Marshal(struct {
		OverlayIP       string `json:"overlay_ip"`
		OverlayPrefix   int    `json:"overlay_prefix"`
		ServerOverlayIP string `json:"server_overlay_ip"`
		ServerPubKey    string `json:"server_pubkey"`
		ServerEndpoint  string `json:"server_endpoint"`
		KeepaliveS      int    `json:"keepalive_s"`
	}{overlayIP, prefixLen, serverIP.String(), srv.wgPubKey, srv.cfg.WgEndpoint, int(srv.cfg.WgKeepalive)})
	writeJSON(w, 200, body)
}
