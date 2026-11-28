// Package gateway implements felucca-gw (v4 P3): the public ingress reverse
// proxy. It maps Host headers of the form "<name>--<sandbox-id>.<domain>" to
// worker node ports (which the agent DNATs to the guest service) using the
// route table served by feluccad's GET /api/v1/routes. Design: ADR-0007.
//
// WebSocket upgrades pass through natively: net/http/httputil.ReverseProxy
// has handled the Upgrade/Connection hop-by-hop dance since Go 1.12 (Odoo
// longpolling and Chatwoot live updates ride on this).
package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alpham/infra-saas/felucca/internal/ingress"
)

// Route is one entry of feluccad's route table (wire shape of /api/v1/routes).
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
// config — ingress policy lives at the ingress, not in feluccad's store).
type TenantLimit struct {
	Enabled  *bool   `json:"enabled"`   // nil = enabled
	RPS      float64 `json:"rps"`       // 0 = unlimited
	AutoWake *bool   `json:"auto_wake"` // nil = the gateway-wide default
}

// Config is the gateway's runtime configuration.
type Config struct {
	Domain       string // wildcard zone, e.g. "sb.example.com"
	FeluccadURL  string // e.g. "http://127.0.0.1:8080"
	Token        string // feluccad admin token (routes are admin-only)
	Refresh      time.Duration
	MaxStale     time.Duration // oldest table served from; past it 503 (FLU-6 R1); 0 = the ceiling
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
	// tableAt is when the fetch of the current table STARTED (zero: no fetch
	// has succeeded yet). A table is as old as the moment feluccad could last
	// have answered it, not the moment its answer arrived.
	tableAt time.Time

	// now is the clock that stamps and ages the table (a seam for tests).
	now func() time.Time

	// ages yields the table's monotonic and wall-clock ages (clockAges; a
	// seam for tests, since a real time cannot carry a stopped monotonic
	// clock with a moving wall clock).
	ages func(now, at time.Time) (mono, wall time.Duration)

	// wake runs one auto-wake of a sandbox (doWake; a seam for tests).
	wake func(sandboxID string)

	// Upstream connections are pooled per (sandbox, guest port, node host,
	// node port), never per node:port alone (FLU-6 R9): the proxy targets a
	// synthetic authority that encodes all four (upstreamAuthority), and the
	// transport's dial decodes the node address from it.
	transport *http.Transport

	lmu      sync.Mutex
	limiters map[string]*bucket

	// Sandboxes that served ingress traffic since the last activity report
	// (v4 P5.2: feluccad's idle clock counts ingress, not just exec).
	amu    sync.Mutex
	active map[string]struct{}

	// In-flight auto-wakes, keyed by sandbox id (v4 P5.2): a traffic burst to
	// one sleeping sandbox must produce ONE wake + poll, not one per request.
	wmu    sync.Mutex
	waking map[string]*wakeCall

	// refused is when feluccad last refused a wake of a sandbox (404 or 409),
	// under wmu; see wakeRefusedHold. Pruned with the table.
	refused map[string]time.Time

	// wakeBudget bounds resending a 503-refused wake (a seam for tests).
	wakeBudget time.Duration
}

// wakeCall is a single shared auto-wake operation; concurrent requests for the
// same sleeping sandbox wait on it instead of each firing their own. It carries
// no route: every caller looks its own label up again when it is done (FLU-7).
type wakeCall struct {
	done chan struct{}
}

func New(cfg Config) *Gateway {
	if cfg.Refresh <= 0 {
		cfg.Refresh = 5 * time.Second
	}
	if cfg.MaxStale <= 0 {
		cfg.MaxStale = ingress.GatewayMaxStaleCeiling
	}
	if cfg.MaxStale > ingress.GatewayMaxStaleCeiling {
		// The port quarantine is sized for the ceiling; a caller that skipped
		// ValidateTimings must not stretch it.
		slog.Warn("gw: max-stale above the ceiling, clamped", "max_stale", cfg.MaxStale, "ceiling", ingress.GatewayMaxStaleCeiling)
		cfg.MaxStale = ingress.GatewayMaxStaleCeiling
	}
	g := &Gateway{
		cfg:        cfg,
		client:     &http.Client{Timeout: 10 * time.Second},
		wakeClient: &http.Client{Timeout: 30 * time.Second},
		routes:     map[string]Route{},
		now:        time.Now,
		ages:       clockAges,
		limiters:   map[string]*bucket{},
		active:     map[string]struct{}{},
		waking:     map[string]*wakeCall{},
		refused:    map[string]time.Time{},
		wakeBudget: wakeBudget,
	}
	g.wake = g.doWake
	g.transport = g.newTransport()
	return g
}

// ValidateTimings checks --refresh and --max-stale against the bounds the
// port quarantine is sized for (ingress.PortQuarantine): the gateway refuses
// to start outside them. max-stale must leave room for two refreshes plus 20 s
// of feluccad being slow, or one missed fetch would fail every request.
func ValidateTimings(refresh, maxStale time.Duration) error {
	if refresh < time.Second {
		return fmt.Errorf("refresh %v is below the minimum of 1s", refresh)
	}
	if refresh > ingress.GatewayRefreshCeiling {
		return fmt.Errorf("refresh %v is above the maximum of %v", refresh, ingress.GatewayRefreshCeiling)
	}
	if lo := 2*refresh + 20*time.Second; maxStale < lo {
		return fmt.Errorf("max-stale %v is below the minimum of %v (2 x refresh + 20s)", maxStale, lo)
	}
	if maxStale > ingress.GatewayMaxStaleCeiling {
		return fmt.Errorf("max-stale %v is above the maximum of %v (the port quarantine is sized for it)", maxStale, ingress.GatewayMaxStaleCeiling)
	}
	return nil
}

// Run blocks, refreshing the route table (and flushing the ingress-activity
// batch) until stop is closed. The activity report runs on its own goroutine
// and ticker: it can take up to the client timeout when feluccad is slow, and
// the refresh cadence must not wait for it, or max-stale's lower bound
// (2 x refresh + 20 s) would not hold.
func (g *Gateway) Run(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(g.cfg.Refresh)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				g.reportActivity()
			case <-stop:
				return
			}
		}
	}()
	t := time.NewTicker(g.cfg.Refresh)
	defer t.Stop()
	g.refresh()
	for {
		select {
		case <-t.C:
			g.refresh()
		case <-stop:
			return
		}
	}
}

// reportActivity flushes the batched "served traffic" sandbox ids to
// feluccad. On failure the batch is re-queued — a feluccad blip must not make
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
	req, err := http.NewRequest(http.MethodPost, g.cfg.FeluccadURL+"/api/v1/routes/activity", strings.NewReader(string(payload)))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil || resp.StatusCode >= 300 {
		if err != nil {
			slog.Error("gw: activity report", "err", err)
		} else {
			resp.Body.Close()
			slog.Warn("gw: activity report", "status", resp.StatusCode)
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
// first caller runs the wake+poll, the rest wait for it to finish. Without
// this, a burst to one popular sleeping sandbox would fire one wake POST and
// 15 full route-table fetches PER request. It hands back no route: a sandbox
// has one route per exposed port, so each caller (the first one too) looks its
// OWN label up again afterwards (FLU-7 W1).
func (g *Gateway) wakeAndWait(sandboxID string) {
	// A wake feluccad just refused is not sent again for a while: the caller
	// goes straight to its second resolution (and the page).
	if g.wakeRefused(sandboxID) {
		return
	}
	g.wmu.Lock()
	if call, ok := g.waking[sandboxID]; ok {
		g.wmu.Unlock()
		<-call.done
		return
	}
	call := &wakeCall{done: make(chan struct{})}
	g.waking[sandboxID] = call
	g.wmu.Unlock()

	// Completed in a defer so that a panic in the wake path cannot leave the
	// entry behind and park every later request for this sandbox (FLU-7 W3).
	defer func() {
		g.wmu.Lock()
		delete(g.waking, sandboxID)
		g.wmu.Unlock()
		close(call.done)
	}()
	g.wake(sandboxID)
}

// Wake pacing (FLU-7 W2).
const (
	wakeBudget   = 15 * time.Second       // resending a 503-refused wake stops here
	wakeRetryCap = 2 * time.Second        // longest wait a Retry-After can ask for
	wakeRetryMin = 250 * time.Millisecond // shortest: "Retry-After: 0" must not spin
	wakePolls    = 15                     // table fetches after an accepted wake
	// wakeRefusedHold: after a 404 or 409, requests for that sandbox skip the
	// wake this long (the sleeping page says Retry-After: 5). Without it every
	// request would cost feluccad a wake, a worker call and a full table fetch,
	// in milliseconds, for anyone who can send requests.
	wakeRefusedHold = 5 * time.Second
)

// doWake asks feluccad to wake the sandbox and reads the answer:
//   - 2xx: poll the table until any route of the sandbox shows running;
//   - 503 with Retry-After: feluccad's store is failing (the wake was refused,
//     or applied on the worker but not saved); look at the table, and send
//     the wake again after that delay while the budget lasts;
//   - 404 (no such sandbox) or 409 (busy: another lifecycle operation, e.g. a
//     fork of it, is running): nothing to wait for; fetch the table once so the
//     waiters see the state feluccad now has, and hold off further wakes;
//   - anything else (transport error, 502, other 5xx): poll as before, since a
//     concurrent caller may have won the race and woken it.
//
// A failed table fetch is logged at Warn once per wake and at Debug after
// that: an outage would otherwise log 15 lines per wake.
func (g *Gateway) doWake(sandboxID string) {
	warned := false
	fetch := func() ([]Route, bool) {
		routes, err := g.fetchTable()
		if err != nil {
			lvl := slog.LevelDebug
			if !warned {
				lvl, warned = slog.LevelWarn, true
			}
			slog.Log(context.Background(), lvl, "gw: route fetch during wake", "sandbox", sandboxID, "err", err)
			return nil, false
		}
		return routes, true
	}
	start := time.Now()
	for {
		status, retry := g.sendWake(sandboxID)
		switch {
		case status >= 200 && status < 300:
			g.pollRunning(sandboxID, fetch)
			return
		case status == 503 && retry >= 0:
			// When the wake WAS applied but not saved ("applied but not
			// saved"), the table already shows the sandbox running. Look
			// before waiting and resending.
			if routes, ok := fetch(); ok && sandboxRunning(routes, sandboxID) {
				return
			}
			d := min(max(retry, wakeRetryMin), wakeRetryCap)
			if time.Since(start)+d >= g.wakeBudget {
				return // the table was fetched just above
			}
			time.Sleep(d)
		case status == 404 || status == 409:
			fetch()
			g.wmu.Lock()
			g.refused[sandboxID] = g.now()
			g.wmu.Unlock()
			return
		default:
			g.pollRunning(sandboxID, fetch)
			return
		}
	}
}

// wakeRefused reports whether feluccad refused a wake of the sandbox (404 or
// 409) less than wakeRefusedHold ago.
func (g *Gateway) wakeRefused(sandboxID string) bool {
	g.wmu.Lock()
	at, ok := g.refused[sandboxID]
	g.wmu.Unlock()
	return ok && g.now().Sub(at) < wakeRefusedHold
}

// sendWake posts one wake. It returns the status (0 on a transport error) and
// the Retry-After delay (-1 when absent or not a number of seconds).
func (g *Gateway) sendWake(sandboxID string) (int, time.Duration) {
	req, err := http.NewRequest(http.MethodPost, g.cfg.FeluccadURL+"/api/v1/sandboxes/"+sandboxID+"/wake", nil)
	if err != nil {
		return 0, -1
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	resp, err := g.wakeClient.Do(req)
	if err != nil {
		slog.Warn("gw: wake", "sandbox", sandboxID, "err", err)
		return 0, -1
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	retry := time.Duration(-1)
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
		retry = time.Duration(s) * time.Second
	}
	return resp.StatusCode, retry
}

// pollRunning fetches the table (at most wakePolls times, 1 s apart) until any
// route of the sandbox shows running with a node: the sandbox is up, and every
// waiter's own label is in that same table.
func (g *Gateway) pollRunning(sandboxID string, fetch func() ([]Route, bool)) {
	for i := 0; i < wakePolls; i++ {
		if i > 0 {
			time.Sleep(time.Second)
		}
		if routes, ok := fetch(); ok && sandboxRunning(routes, sandboxID) {
			return
		}
	}
}

// sandboxRunning reports whether any route of the sandbox shows it running
// with a node: it is up, and every waiter's own label is in that table.
func sandboxRunning(routes []Route, sandboxID string) bool {
	for _, r := range routes {
		if r.SandboxID == sandboxID && r.State == "running" && r.NodeHost != "" {
			return true
		}
	}
	return false
}

// refresh swaps in the latest route table; on error the old table stays (a
// feluccad blip must not take down working routes) until it is older than
// MaxStale, when requests fail closed (ServeHTTP).
func (g *Gateway) refresh() {
	if _, err := g.fetchTable(); err != nil {
		slog.Warn("gw: route refresh", "err", err)
	}
}

// fetchTable fetches the route table and installs it. It returns the fetched
// routes (even when a newer table was already in place); the caller logs a
// failure at the level its loop can afford.
func (g *Gateway) fetchTable() ([]Route, error) {
	started := g.now()
	routes, err := g.fetchRoutes()
	if err != nil {
		return nil, err
	}
	g.installTable(routes, started)
	return routes, nil
}

// installTable is the one place the table is swapped. started is when its
// fetch STARTED: a fetch that started earlier than the current table's never
// replaces it, however late it finishes (the background loop, a wake poll and
// a dynamic ensure fetch concurrently, and a slow answer is an old answer).
// Refused-wake marks of sandboxes that left the table, or have expired, are
// dropped with it.
func (g *Gateway) installTable(routes []Route, started time.Time) bool {
	m := make(map[string]Route, len(routes))
	sandboxes := make(map[string]struct{}, len(routes))
	for _, r := range routes {
		m[r.Hostname] = r
		sandboxes[r.SandboxID] = struct{}{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.tableAt.IsZero() && started.Before(g.tableAt) {
		return false
	}
	g.routes = m
	g.tableAt = started
	// Under g.mu so that two installs prune in the order they installed.
	g.wmu.Lock()
	for id, at := range g.refused {
		if _, ok := sandboxes[id]; !ok || started.Sub(at) >= wakeRefusedHold {
			delete(g.refused, id)
		}
	}
	g.wmu.Unlock()
	return true
}

// tableFresh reports whether the table may be served from: a fetch has
// succeeded, and the current table's fetch started at most MaxStale ago.
//
// The age is the LARGER of the monotonic and the wall-clock difference. On
// Linux the monotonic clock stops while the host is suspended or the VM is
// paused, so after a freeze it alone would call an hour-old table young and
// serve it for up to MaxStale more — longer in real time than the port
// quarantine covers. The wall clock keeps counting through a freeze. A wall
// clock stepped backwards loses to the monotonic age; one that jumped forward
// fails closed until the next successful fetch, at most one refresh away.
func (g *Gateway) tableFresh() bool {
	g.mu.RLock()
	at := g.tableAt
	g.mu.RUnlock()
	if at.IsZero() {
		return false
	}
	return tableAge(g.ages(g.now(), at)) <= g.cfg.MaxStale
}

// clockAges is the monotonic and the wall-clock difference between two
// times (Round(0) strips the monotonic reading).
func clockAges(now, at time.Time) (mono, wall time.Duration) {
	return now.Sub(at), now.Round(0).Sub(at.Round(0))
}

// tableAge is the age tableFresh judges by: the larger of the monotonic and
// the wall-clock difference (see tableFresh for why).
func tableAge(mono, wall time.Duration) time.Duration {
	return max(mono, wall)
}

func (g *Gateway) fetchRoutes() ([]Route, error) {
	req, err := http.NewRequest(http.MethodGet, g.cfg.FeluccadURL+"/api/v1/routes", nil)
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

// ensureDynamic asks feluccad to lazily expose a port (the port-in-hostname
// path); feluccad enforces the per-sandbox opt-in. It returns feluccad's
// status (0 when there was no usable answer) and its Retry-After header.
func (g *Gateway) ensureDynamic(sandboxID string, port uint16) (Route, int, string, error) {
	payload, _ := json.Marshal(struct {
		SandboxID string `json:"sandbox_id"`
		Port      uint16 `json:"port"`
	}{sandboxID, port})
	req, err := http.NewRequest(http.MethodPost, g.cfg.FeluccadURL+"/api/v1/routes/ensure", strings.NewReader(string(payload)))
	if err != nil {
		return Route{}, 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return Route{}, 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Route{}, resp.StatusCode, resp.Header.Get("Retry-After"), fmt.Errorf("ensure status %d", resp.StatusCode)
	}
	// The ensure response is an expose view; refetch the table for the full
	// route row (node host etc.) — one extra round-trip on first hit only.
	// The row is taken from this fetch even when a newer table won the
	// install: this fetch started after the ensure, so it has the row.
	routes, err := g.fetchTable()
	if err != nil {
		return Route{}, 0, "", fmt.Errorf("route table fetch after ensure: %w", err)
	}
	label := fmt.Sprintf("%d--%s", port, sandboxID)
	for _, r := range routes {
		if r.Hostname == label {
			return r, 200, "", nil
		}
	}
	return Route{}, 0, "", fmt.Errorf("ensured route %s missing from table", label)
}

// retryAfterOr passes a Retry-After of whole seconds through and falls back
// to def for anything else (absent, a date, garbage).
func retryAfterOr(v, def string) string {
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return strconv.Itoa(s)
	}
	return def
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

// resolve looks a label up the way a fresh request does: the table's age, the
// table, the lazy ensure of an all-digit label, the tenant policy and the node.
// It answers the client itself and returns false when the request ends here.
// It runs a second time after an auto-wake (FLU-7 W1); that pass does not take
// another rate-limit token, since it is the same client request.
func (g *Gateway) resolve(w http.ResponseWriter, label, name, sandboxID string, again bool) (Route, bool) {
	// An old table may name a node port that has since gone to another
	// sandbox; past MaxStale the gateway fails closed (FLU-6 R1).
	if !g.tableFresh() {
		tableUnavailable(w)
		return Route{}, false
	}

	g.mu.RLock()
	route, found := g.routes[label]
	g.mu.RUnlock()

	if !found && allDigits(name) {
		// Dynamic port-in-hostname: lazily ensure the expose via feluccad.
		// Only the canonical decimal spelling routes ("08080" would ensure
		// 8080 but never match the table — an unauthenticated per-request
		// amplification loop against feluccad).
		port, err := strconv.ParseUint(name, 10, 16)
		if err == nil && port > 0 && strconv.FormatUint(port, 10) == name {
			rt, status, retry, err := g.ensureDynamic(sandboxID, uint16(port))
			switch {
			case err == nil:
				route, found = rt, true
			case status == 403:
				httpError(w, 403, "dynamic ports are disabled for this sandbox")
				return Route{}, false
			case status == 404:
				// fall through to the shared 404 below
			case status == 429:
				// The tenant's expose churn cap: feluccad says when to retry.
				w.Header().Set("Retry-After", retryAfterOr(retry, "5"))
				httpError(w, 429, "too many new dynamic ports for this tenant")
				return Route{}, false
			default:
				// feluccad could not expose it right now: a 503 (e.g. no free
				// node port, or its store failing), no answer, a 409 (no
				// address yet), a 502, or a table fetch after the ensure that
				// failed. All retryable, none "no such service".
				if status != 503 {
					slog.Warn("gw: ensure failed", "label", label, "status", status, "err", err)
				}
				ra := "5"
				if status == 503 {
					ra = retryAfterOr(retry, "5")
				}
				w.Header().Set("Retry-After", ra)
				httpError(w, 503, "dynamic port temporarily unavailable")
				return Route{}, false
			}
		}
	}
	if !found {
		httpError(w, 404, "no such service")
		return Route{}, false
	}

	// Per-tenant ingress policy (gateway-local config).
	if lim, ok := g.cfg.TenantLimits[route.TenantID]; ok {
		if lim.Enabled != nil && !*lim.Enabled {
			httpError(w, 403, "ingress disabled for this tenant")
			return Route{}, false
		}
		if !again && lim.RPS > 0 && !g.allow(route.TenantID, lim.RPS) {
			w.Header().Set("Retry-After", "1")
			httpError(w, 429, "rate limit exceeded")
			return Route{}, false
		}
	}

	if route.NodeHost == "" {
		// The sandbox's node is gone (feluccad still serves the row so we can
		// answer with state instead of a generic 404).
		httpError(w, 503, "sandbox is "+route.State)
		return Route{}, false
	}
	return route, true
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && !strings.Contains(r.Host, "--") {
		// The gateway's own health endpoint (not a sandbox path).
		w.WriteHeader(200)
		fmt.Fprintln(w, "ok")
		return
	}
	if r.URL.Path == "/readyz" && !strings.Contains(r.Host, "--") {
		// Readiness: /healthz says the process is up, /readyz says it may
		// route (its table is fresh enough to serve from).
		if !g.tableFresh() {
			tableUnavailable(w)
			return
		}
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

	route, ok := g.resolve(w, label, name, sandboxID, false)
	if !ok {
		return
	}

	// Auto-wake (v4 P5.2): wake-on-request replaces the 503 page when
	// allowed. Whatever the wake did, the label is then resolved again from
	// the top, once, with no second wake: the table now says where THIS
	// label goes (or what state the sandbox is in, for the page below).
	if route.State == "sleeping" && g.autoWakeAllowed(route.TenantID) {
		g.wakeAndWait(route.SandboxID)
		if route, ok = g.resolve(w, label, name, sandboxID, true); !ok {
			return
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

	// The proxied request is activity for feluccad's idle clock; batched and
	// reported on the refresh tick.
	g.markActive(route.SandboxID)

	// The URL names the sandbox and guest port as well as the node address;
	// the transport dials the node address it encodes (see newTransport).
	target := &url.URL{Scheme: "http", Host: upstreamAuthority(route)}
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
		Transport: g.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The client only gets "unreachable"; the cause (a refused or
			// timed-out dial, an undecodable upstream) goes to the log.
			slog.Warn("gw: upstream", "label", label, "err", err)
			httpError(w, 502, "backend unreachable")
		},
		FlushInterval: 100 * time.Millisecond, // streaming/longpolling friendly
	}
	proxy.ServeHTTP(w, r)
}

// upstreamSuffix ends every synthetic upstream authority. ".invalid" can never
// resolve, should a name escape the dial hook.
const upstreamSuffix = ".sandbox.invalid:80"

// upstreamAuthority is the synthetic "host:port" the proxy targets for a
// route: one per (sandbox, guest port, node host, node port). The transport
// pools connections by it, so a pooled connection is only ever reused for the
// sandbox it was opened for — after a node port moves to another sandbox, a
// connection the worker still pins to the OLD sandbox must not carry the new
// one's request (FLU-6 R9) — and only for the node address it was opened to:
// when a sandbox's node address changes, its old idle connections are never
// picked again.
//
// Encoding: "<hex sandbox id>-<guest port>-<hex node host>-<node port>" +
// upstreamSuffix. Hex keeps any byte of an id or a host (IPv6 colons and
// brackets, dots, upper case) inside one DNS-safe label, and lowercase hex
// survives anything that lowercases a host name.
func upstreamAuthority(r Route) string {
	return fmt.Sprintf("%x-%d-%x-%d%s", r.SandboxID, r.GuestPort, r.NodeHost, r.NodePort, upstreamSuffix)
}

// upstreamDialAddr decodes the node address an upstream authority names.
func upstreamDialAddr(authority string) (string, error) {
	name, ok := strings.CutSuffix(authority, upstreamSuffix)
	parts := strings.Split(name, "-")
	if !ok || len(parts) != 4 {
		return "", fmt.Errorf("not a gateway upstream: %s", authority)
	}
	host, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("upstream %s: node host: %w", authority, err)
	}
	port, err := strconv.ParseUint(parts[3], 10, 16)
	if err != nil {
		return "", fmt.Errorf("upstream %s: node port: %w", authority, err)
	}
	return nodeAddr(string(host), uint16(port)), nil
}

// nodeAddr joins a node host and port for dialing. feluccad names an IPv6
// worker "[addr]" already; net.JoinHostPort on that would bracket it twice.
func nodeAddr(host string, port uint16) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

// newTransport is http.DefaultTransport with a tight connect timeout
// (backends are LAN/overlay hops away; 3s of no SYN-ACK means a dead route,
// not a slow one), and a dial that decodes the synthetic upstream authority
// to its node address. Response/read timeouts stay unbounded for longpolling.
// The idle timeout is part of the port quarantine's arithmetic (package
// ingress).
func (g *Gateway) newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			node, err := upstreamDialAddr(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, node)
		},
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       ingress.UpstreamIdleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}

// tableUnavailable is the fail-closed answer while the route table is too old
// to trust or has never been fetched (FLU-6 R1).
func tableUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	httpError(w, 503, "route table unavailable")
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
