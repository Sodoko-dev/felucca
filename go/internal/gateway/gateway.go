// Package gateway implements hearth-gw (v4 P3): the public ingress reverse
// proxy. It maps Host headers of the form "<name>--<sandbox-id>.<domain>" to
// worker node ports (which the agent DNATs to the guest service) using the
// route table served by hearthd's GET /api/v1/routes. Design: ADR-0007.
//
// WebSocket upgrades pass through natively: net/http/httputil.ReverseProxy
// has handled the Upgrade/Connection hop-by-hop dance since Go 1.12 (Odoo
// longpolling and Chatwoot live updates ride on this).
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Route is one entry of hearthd's route table (wire shape of /api/v1/routes).
type Route struct {
	Hostname          string `json:"hostname"`
	SandboxID         string `json:"sandbox_id"`
	TenantID          string `json:"tenant_id"`
	Name              string `json:"name"`
	NodeHost          string `json:"node_host"`
	NodePort          uint16 `json:"node_port"`
	GuestPort         uint16 `json:"guest_port"`
	State             string `json:"state"`
	AllowDynamicPorts bool   `json:"allow_dynamic_ports"`
}

// TenantLimit is the per-tenant ingress policy (from the gateway's own
// config — ingress policy lives at the ingress, not in hearthd's store).
type TenantLimit struct {
	Enabled  *bool   `json:"enabled"`   // nil = enabled
	RPS      float64 `json:"rps"`       // 0 = unlimited
	AutoWake *bool   `json:"auto_wake"` // nil = the gateway-wide default
}

// Config is the gateway's runtime configuration.
type Config struct {
	Domain       string // wildcard zone, e.g. "sb.example.com"
	HearthdURL   string // e.g. "http://127.0.0.1:8080"
	Token        string // hearthd admin token (routes are admin-only)
	Refresh      time.Duration
	TenantLimits map[string]TenantLimit
	// AutoWake (v4 P5.2, ADR-0007 deferral): a request for a sleeping
	// sandbox wakes it and waits briefly instead of serving the 503 page.
	AutoWake bool
}

// Gateway is an http.Handler proxying sandbox ingress traffic.
type Gateway struct {
	cfg        Config
	client     *http.Client
	wakeClient *http.Client // wake calls block on the snapshot restore

	mu     sync.RWMutex
	routes map[string]Route // hostname label -> route

	lmu      sync.Mutex
	limiters map[string]*bucket

	// Sandboxes that served ingress traffic since the last activity report
	// (v4 P5.2: hearthd's idle clock counts ingress, not just exec).
	amu    sync.Mutex
	active map[string]struct{}

	// In-flight auto-wakes, keyed by sandbox id (v4 P5.2): a traffic burst to
	// one sleeping sandbox must produce ONE wake + poll, not one per request.
	wmu    sync.Mutex
	waking map[string]*wakeCall
}

// wakeCall is a single shared auto-wake operation; concurrent requests for the
// same sleeping sandbox wait on it instead of each firing their own.
type wakeCall struct {
	done  chan struct{}
	route Route
	ok    bool
}

func New(cfg Config) *Gateway {
	if cfg.Refresh <= 0 {
		cfg.Refresh = 5 * time.Second
	}
	return &Gateway{
		cfg:        cfg,
		client:     &http.Client{Timeout: 10 * time.Second},
		wakeClient: &http.Client{Timeout: 30 * time.Second},
		routes:     map[string]Route{},
		limiters:   map[string]*bucket{},
		active:     map[string]struct{}{},
		waking:     map[string]*wakeCall{},
	}
}

// Run blocks, refreshing the route table (and flushing the ingress-activity
// batch) until stop is closed.
func (g *Gateway) Run(stop <-chan struct{}) {
	t := time.NewTicker(g.cfg.Refresh)
	defer t.Stop()
	g.refresh()
	for {
		select {
		case <-t.C:
			g.reportActivity()
			g.refresh()
		case <-stop:
			return
		}
	}
}

// reportActivity flushes the batched "served traffic" sandbox ids to
// hearthd. On failure the batch is re-queued — a hearthd blip must not make
// an active sandbox look idle.
func (g *Gateway) reportActivity() {
	g.amu.Lock()
	if len(g.active) == 0 {
		g.amu.Unlock()
		return
	}
	ids := make([]string, 0, len(g.active))
	for id := range g.active {
		ids = append(ids, id)
	}
	g.active = map[string]struct{}{}
	g.amu.Unlock()

	payload, _ := json.Marshal(struct {
		SandboxIDs []string `json:"sandbox_ids"`
	}{ids})
	req, err := http.NewRequest(http.MethodPost, g.cfg.HearthdURL+"/api/v1/routes/activity", strings.NewReader(string(payload)))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "gw: activity report: %v\n", err)
		} else {
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "gw: activity report: status %d\n", resp.StatusCode)
		}
		g.amu.Lock()
		for _, id := range ids {
			g.active[id] = struct{}{}
		}
		g.amu.Unlock()
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// markActive records that a sandbox served (or is about to serve) traffic.
func (g *Gateway) markActive(sandboxID string) {
	g.amu.Lock()
	g.active[sandboxID] = struct{}{}
	g.amu.Unlock()
}

// autoWakeAllowed resolves the per-tenant toggle over the gateway default.
func (g *Gateway) autoWakeAllowed(tenantID string) bool {
	if lim, ok := g.cfg.TenantLimits[tenantID]; ok && lim.AutoWake != nil {
		return *lim.AutoWake
	}
	return g.cfg.AutoWake
}

// wakeAndWait coalesces concurrent requests for the same sleeping sandbox: the
// first caller runs the wake+poll, the rest wait on its shared result. Without
// this, a burst to one popular sleeping sandbox would fire one wake POST and
// 15 full route-table fetches PER request.
func (g *Gateway) wakeAndWait(label, sandboxID string) (Route, bool) {
	g.wmu.Lock()
	if call, ok := g.waking[sandboxID]; ok {
		g.wmu.Unlock()
		<-call.done
		return call.route, call.ok
	}
	call := &wakeCall{done: make(chan struct{})}
	g.waking[sandboxID] = call
	g.wmu.Unlock()

	call.route, call.ok = g.doWake(label, sandboxID)
	close(call.done)

	g.wmu.Lock()
	delete(g.waking, sandboxID)
	g.wmu.Unlock()
	return call.route, call.ok
}

// doWake asks hearthd to wake the sandbox, then polls the route table (≤15s)
// until the label shows running. Even a failed wake call is followed by the
// poll: a concurrent process may have won the race and woken it.
func (g *Gateway) doWake(label, sandboxID string) (Route, bool) {
	req, err := http.NewRequest(http.MethodPost, g.cfg.HearthdURL+"/api/v1/sandboxes/"+sandboxID+"/wake", nil)
	if err != nil {
		return Route{}, false
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	if resp, err := g.wakeClient.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	for i := 0; i < 15; i++ {
		routes, err := g.fetchRoutes()
		if err == nil {
			m := make(map[string]Route, len(routes))
			for _, r := range routes {
				m[r.Hostname] = r
			}
			g.mu.Lock()
			g.routes = m
			g.mu.Unlock()
			if r, ok := m[label]; ok && r.State == "running" && r.NodeHost != "" {
				return r, true
			}
		}
		time.Sleep(time.Second)
	}
	return Route{}, false
}

// refresh swaps in the latest route table; on error the old table stays (a
// hearthd blip must not take down working routes).
func (g *Gateway) refresh() {
	routes, err := g.fetchRoutes()
	if err != nil {
		fmt.Fprintf(os.Stderr, "gw: route refresh: %v\n", err)
		return
	}
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		m[r.Hostname] = r
	}
	g.mu.Lock()
	g.routes = m
	g.mu.Unlock()
}

func (g *Gateway) fetchRoutes() ([]Route, error) {
	req, err := http.NewRequest(http.MethodGet, g.cfg.HearthdURL+"/api/v1/routes", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("routes status %d", resp.StatusCode)
	}
	var body struct {
		Routes []Route `json:"routes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Routes, nil
}

// ensureDynamic asks hearthd to lazily expose a port (the port-in-hostname
// path); hearthd enforces the per-sandbox opt-in.
func (g *Gateway) ensureDynamic(sandboxID string, port uint16) (Route, int, error) {
	payload, _ := json.Marshal(struct {
		SandboxID string `json:"sandbox_id"`
		Port      uint16 `json:"port"`
	}{sandboxID, port})
	req, err := http.NewRequest(http.MethodPost, g.cfg.HearthdURL+"/api/v1/routes/ensure", strings.NewReader(string(payload)))
	if err != nil {
		return Route{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return Route{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Route{}, resp.StatusCode, fmt.Errorf("ensure status %d", resp.StatusCode)
	}
	// The ensure response is an expose view; refetch the table for the full
	// route row (node host etc.) — one extra round-trip on first hit only.
	routes, err := g.fetchRoutes()
	if err != nil {
		return Route{}, 0, err
	}
	label := fmt.Sprintf("%d--%s", port, sandboxID)
	g.mu.Lock()
	m := make(map[string]Route, len(routes))
	for _, r := range routes {
		m[r.Hostname] = r
	}
	g.routes = m
	r, ok := m[label]
	g.mu.Unlock()
	if !ok {
		return Route{}, 0, fmt.Errorf("ensured route %s missing from table", label)
	}
	return r, 200, nil
}

// splitLabel extracts the routing label from a Host header: it must be a
// single DNS label directly under the configured domain.
func (g *Gateway) splitLabel(hostHeader string) (string, bool) {
	host := hostHeader
	// Strip :port (IPv6 literals never carry our domain, so ':' is a port).
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i+1:], "]") {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + g.cfg.Domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	label := host[:len(host)-len(suffix)]
	if label == "" || strings.Contains(label, ".") {
		return "", false
	}
	return label, true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && !strings.Contains(r.Host, "--") {
		// The gateway's own health endpoint (not a sandbox path).
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
		return
	}

	label, ok := g.splitLabel(r.Host)
	if !ok {
		httpError(w, 404, "unknown host")
		return
	}
	name, sandboxID, ok := strings.Cut(label, "--")
	if !ok || name == "" || sandboxID == "" {
		httpError(w, 404, "unknown host")
		return
	}

	g.mu.RLock()
	route, found := g.routes[label]
	g.mu.RUnlock()

	if !found && allDigits(name) {
		// Dynamic port-in-hostname: lazily ensure the expose via hearthd.
		// Only the canonical decimal spelling routes ("08080" would ensure
		// 8080 but never match the table — an unauthenticated per-request
		// amplification loop against hearthd).
		port, err := strconv.ParseUint(name, 10, 16)
		if err == nil && port > 0 && strconv.FormatUint(port, 10) == name {
			rt, status, err := g.ensureDynamic(sandboxID, uint16(port))
			switch {
			case err == nil:
				route, found = rt, true
			case status == 403:
				httpError(w, 403, "dynamic ports are disabled for this sandbox")
				return
			case status == 404:
				// fall through to the shared 404 below
			default:
				fmt.Fprintf(os.Stderr, "gw: ensure %s: %v\n", label, err)
			}
		}
	}
	if !found {
		httpError(w, 404, "no such service")
		return
	}

	// Per-tenant ingress policy (gateway-local config).
	if lim, ok := g.cfg.TenantLimits[route.TenantID]; ok {
		if lim.Enabled != nil && !*lim.Enabled {
			httpError(w, 403, "ingress disabled for this tenant")
			return
		}
		if lim.RPS > 0 && !g.allow(route.TenantID, lim.RPS) {
			w.Header().Set("Retry-After", "1")
			httpError(w, 429, "rate limit exceeded")
			return
		}
	}

	if route.NodeHost == "" {
		// The sandbox's node is gone (hearthd still serves the row so we can
		// answer with state instead of a generic 404).
		httpError(w, 503, "sandbox is "+route.State)
		return
	}

	// Auto-wake (v4 P5.2): wake-on-request replaces the 503 page when
	// allowed; on success the refreshed route proxies below. Failure falls
	// through to the page exactly as before.
	if route.State == "sleeping" && g.autoWakeAllowed(route.TenantID) {
		if woken, ok := g.wakeAndWait(label, route.SandboxID); ok {
			route = woken
		}
	}

	switch route.State {
	case "running":
		// proxy below
	case "sleeping":
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(503)
		fmt.Fprintf(w, sleepingPage, route.SandboxID)
		return
	default:
		httpError(w, 503, "sandbox is "+route.State)
		return
	}

	// The proxied request is activity for hearthd's idle clock; batched and
	// reported on the refresh tick.
	g.markActive(route.SandboxID)

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", route.NodeHost, route.NodePort)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Keep the inbound Host: vhost-aware guests (Odoo) generate
			// URLs from it. SetXForwarded fills For/Host/Proto.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		// Short dial timeout: a stale route (e.g. the ≤refresh window right
		// after a sandbox sleeps) DNATs to an IP with nothing behind it —
		// without this, requests hang ~30s before the 502.
		Transport: dialBoundTransport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			httpError(w, 502, "backend unreachable")
		},
		FlushInterval: 100 * time.Millisecond, // streaming/longpolling friendly
	}
	proxy.ServeHTTP(w, r)
}

// dialBoundTransport is http.DefaultTransport with a tight connect timeout
// (backends are LAN/overlay hops away; 3s of no SYN-ACK means a dead route,
// not a slow one). Response/read timeouts stay unbounded for longpolling.
var dialBoundTransport http.RoundTripper = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	ForceAttemptHTTP2:     false,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}

const sleepingPage = `<!doctype html>
<html><head><title>Sandbox sleeping</title></head>
<body style="font-family:system-ui;text-align:center;padding-top:4rem">
<h1>This sandbox is asleep</h1>
<p>Sandbox <code>%s</code> is sleeping. Wake it via the API, then reload.</p>
</body></html>
`

// ---- per-tenant token bucket (no external deps) ----

type bucket struct {
	tokens float64
	last   time.Time
}

// allow takes one token from the tenant's bucket at the given refill rate.
// Burst equals one second of rate (minimum 1).
func (g *Gateway) allow(tenant string, rps float64) bool {
	g.lmu.Lock()
	defer g.lmu.Unlock()
	now := time.Now()
	b, ok := g.limiters[tenant]
	if !ok {
		b = &bucket{tokens: rps, last: now}
		g.limiters[tenant] = b
	}
	burst := rps
	if burst < 1 {
		burst = 1
	}
	b.tokens += now.Sub(b.last).Seconds() * rps
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
