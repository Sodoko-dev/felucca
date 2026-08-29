// Package config handles hearthd configuration loading.
// Precedence: flags > env vars > JSON config file > defaults.
package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config holds all hearthd runtime configuration.
type Config struct {
	Bind      string
	UIDir     string
	StatePath string
	DBPath    string
	Token     string
	Port      uint16

	// InsecureNoAuth turns the bearer gate off entirely. It is the only way to
	// run without a token, and it has to be named explicitly so that a token
	// which failed to load can never open the API by accident. Loopback-only
	// lab escape hatch.
	InsecureNoAuth bool

	// WireGuard overlay. Empty WgIP means the overlay is disabled.
	WgIP        string // hearthd's overlay address in CIDR form, e.g. "10.100.0.1/16"
	WgPort      uint16 // wg listen port
	WgKeyPath   string // private key file path
	WgEndpoint  string // public "host:port" workers dial for wg
	WgKeepalive uint16 // persistent-keepalive seconds handed to joining workers

	// Let's Encrypt TLS on the public listener. Empty TLSDomain disables it.
	TLSDomain   string // domain to obtain a certificate for (autocert host whitelist)
	TLSCacheDir string // directory where autocert caches certificates

	// Ingress (v4 P3): the gateway's wildcard zone, e.g. "sb.example.com".
	// Only used to render public URLs in expose responses; empty means URLs
	// are omitted (labels still route via the gateway's own domain config).
	IngressDomain string

	// Templates (v4 P4): where hearthd keeps template rootfs images
	// (<name>.ext4 + captured uploads). Workers pull from
	// GET /api/v1/images/{name} and cache locally.
	ImagesDir string

	// Usage-event retention in days (v4 P5.3): events older than this are
	// pruned hourly by the lifecycle loop. 0 keeps everything forever.
	UsageRetentionDays int64

	// TrustedProxies lists CIDRs whose X-Forwarded-For header is honoured when
	// hearthd resolves the client address. Empty (the default) means trust NO
	// proxy and use the connection's RemoteAddr. hearthd is documented behind a
	// Caddy reverse proxy, so every client arrives as 127.0.0.1; believing a
	// client-settable header by default would let any caller forge the
	// per-source key the auth throttle counts on, which is strictly worse than
	// throttling the proxy as one source.
	TrustedProxies []string

	// TrustXRealIP additionally honours X-Real-IP from a trusted proxy when no
	// X-Forwarded-For is present. OFF by default, and deliberately so.
	//
	// X-Forwarded-For carries a chain: hearthd walks it from the right and stops
	// at the first hop that is not one of the declared proxies, so a client's own
	// forged prefix is discarded. X-Real-IP is a bare single value with NO chain
	// of custody at all — whatever the last hop wrote (or forwarded verbatim)
	// becomes the throttle key. That is safe only with a proxy that is known to
	// rewrite it on every request, so it takes an explicit operator decision
	// rather than being the silent fallback it used to be. ValidateTrustedProxies
	// refuses the setting when no proxy is declared, so it cannot be switched on
	// in a deployment where every peer would be believed.
	TrustXRealIP bool
}

// Load builds a Config from the four layers (defaults < file < env < flags).
// It replicates the Zig loadConfig exactly, including the bind→port quirk.
func Load(args []string) (*Config, error) {
	cfg := &Config{
		Bind:      "0.0.0.0:8080",
		UIDir:     "/usr/share/hearth/ui",
		StatePath: "/var/lib/hearth/state.json",
		DBPath:    "/var/lib/hearth/hearth.db",
		Token:     "",
		Port:      8080,

		InsecureNoAuth: false,

		WgIP:        "",
		WgPort:      51820,
		WgKeyPath:   "/var/lib/hearth/wg.key",
		WgEndpoint:  "",
		WgKeepalive: 25,

		TLSDomain:   "",
		TLSCacheDir: "/var/lib/hearth/autocert",

		IngressDomain: "",

		ImagesDir: "/var/lib/hearth/images",

		UsageRetentionDays: 90,

		// Trust nothing until an operator names a proxy, and never believe a
		// chainless forwarded header until one asks for it by name.
		TrustedProxies: nil,
		TrustXRealIP:   false,
	}

	// --- Layer 1: JSON config file (lowest above defaults) ---
	// Find the config path from --config flag or HEARTH_CONFIG env. Both are
	// paths the operator named, so a file that is missing or malformed is
	// fatal: warn-and-continue would drop the token the operator put in it and
	// boot with Token still "". Only a path hearthd guessed for itself would
	// be allowed to be a soft miss, and today it guesses none.
	configPath := os.Getenv("HEARTH_CONFIG")
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			configPath = args[i+1]
		}
		// flag's "--config=path" form never reaches the scan above.
		if v, ok := strings.CutPrefix(a, "--config="); ok {
			configPath = v
		}
	}
	if configPath != "" {
		if err := applyFile(cfg, configPath); err != nil {
			return nil, fmt.Errorf("config: could not load %s: %w", configPath, err)
		}
	}

	// --- Layer 2: Environment variables ---
	if v := os.Getenv("HEARTH_BIND"); v != "" {
		cfg.Bind = v
	}
	if v := os.Getenv("HEARTH_UI_DIR"); v != "" {
		cfg.UIDir = v
	}
	if v := os.Getenv("HEARTH_STATE"); v != "" {
		cfg.StatePath = v
	}
	if v := os.Getenv("HEARTH_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("HEARTH_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("HEARTH_INSECURE_NO_AUTH"); v != "" {
		// Only an affirmative value opens the gate; garbage keeps auth on.
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.InsecureNoAuth = b
		}
	}
	if v := os.Getenv("HEARTH_PORT"); v != "" {
		if p, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.Port = uint16(p)
		}
	}
	if v := os.Getenv("HEARTH_WG_IP"); v != "" {
		cfg.WgIP = v
	}
	if v := os.Getenv("HEARTH_WG_PORT"); v != "" {
		if p, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.WgPort = uint16(p)
		}
	}
	if v := os.Getenv("HEARTH_WG_KEY_PATH"); v != "" {
		cfg.WgKeyPath = v
	}
	if v := os.Getenv("HEARTH_WG_ENDPOINT"); v != "" {
		cfg.WgEndpoint = v
	}
	if v := os.Getenv("HEARTH_WG_KEEPALIVE"); v != "" {
		if p, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.WgKeepalive = uint16(p)
		}
	}
	if v := os.Getenv("HEARTH_TLS_DOMAIN"); v != "" {
		cfg.TLSDomain = v
	}
	if v := os.Getenv("HEARTH_TLS_CACHE"); v != "" {
		cfg.TLSCacheDir = v
	}
	if v := os.Getenv("HEARTH_INGRESS_DOMAIN"); v != "" {
		cfg.IngressDomain = v
	}
	if v := os.Getenv("HEARTH_IMAGES_DIR"); v != "" {
		cfg.ImagesDir = v
	}
	if v := os.Getenv("HEARTH_USAGE_RETENTION_DAYS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			cfg.UsageRetentionDays = n
		}
	}
	if v := os.Getenv("HEARTH_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = splitList(v)
	}
	if v := os.Getenv("HEARTH_TRUST_X_REAL_IP"); v != "" {
		// Only an affirmative value opts in; garbage keeps the header ignored.
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.TrustXRealIP = b
		}
	}

	// --- Layer 3: Command-line flags (highest precedence) ---
	fs := flag.NewFlagSet("hearthd", flag.ContinueOnError)
	bind := fs.String("bind", cfg.Bind, "")
	uiDir := fs.String("ui-dir", cfg.UIDir, "")
	statePath := fs.String("state", cfg.StatePath, "")
	dbPath := fs.String("db", cfg.DBPath, "")
	token := fs.String("token", cfg.Token, "")
	insecureNoAuth := fs.Bool("insecure-no-auth", cfg.InsecureNoAuth, "")
	port := fs.Uint("port", uint(cfg.Port), "")
	wgIP := fs.String("wg-ip", cfg.WgIP, "")
	wgPort := fs.Uint("wg-port", uint(cfg.WgPort), "")
	wgKeyPath := fs.String("wg-key", cfg.WgKeyPath, "")
	wgEndpoint := fs.String("wg-endpoint", cfg.WgEndpoint, "")
	wgKeepalive := fs.Uint("wg-keepalive", uint(cfg.WgKeepalive), "")
	tlsDomain := fs.String("tls-domain", cfg.TLSDomain, "")
	tlsCache := fs.String("tls-cache", cfg.TLSCacheDir, "")
	ingressDomain := fs.String("ingress-domain", cfg.IngressDomain, "")
	imagesDir := fs.String("images-dir", cfg.ImagesDir, "")
	usageRetention := fs.Int64("usage-retention-days", cfg.UsageRetentionDays, "")
	trustedProxies := fs.String("trusted-proxies", strings.Join(cfg.TrustedProxies, ","), "")
	trustXRealIP := fs.Bool("trust-x-real-ip", cfg.TrustXRealIP, "")
	// --config is consumed above; define it here so flag parsing doesn't fail.
	_ = fs.String("config", "", "")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Only override if flag was explicitly set.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "bind":
			cfg.Bind = *bind
		case "ui-dir":
			cfg.UIDir = *uiDir
		case "state":
			cfg.StatePath = *statePath
		case "db":
			cfg.DBPath = *dbPath
		case "token":
			cfg.Token = *token
		case "insecure-no-auth":
			cfg.InsecureNoAuth = *insecureNoAuth
		case "port":
			cfg.Port = uint16(*port)
		case "wg-ip":
			cfg.WgIP = *wgIP
		case "wg-port":
			// Range-checked: silent uint16 truncation would bind a random port.
			if *wgPort >= 1 && *wgPort <= 65535 {
				cfg.WgPort = uint16(*wgPort)
			}
		case "wg-key":
			cfg.WgKeyPath = *wgKeyPath
		case "wg-endpoint":
			cfg.WgEndpoint = *wgEndpoint
		case "wg-keepalive":
			if *wgKeepalive <= 65535 {
				cfg.WgKeepalive = uint16(*wgKeepalive)
			}
		case "tls-domain":
			cfg.TLSDomain = *tlsDomain
		case "tls-cache":
			cfg.TLSCacheDir = *tlsCache
		case "ingress-domain":
			cfg.IngressDomain = *ingressDomain
		case "images-dir":
			cfg.ImagesDir = *imagesDir
		case "usage-retention-days":
			if *usageRetention >= 0 {
				cfg.UsageRetentionDays = *usageRetention
			}
		case "trusted-proxies":
			// An explicit empty value is the operator asking to trust nobody.
			cfg.TrustedProxies = splitList(*trustedProxies)
		case "trust-x-real-ip":
			cfg.TrustXRealIP = *trustXRealIP
		}
	})

	// --- Bind→port quirk (replicated from Zig) ---
	// After all layers: if bind carries a parseable ":port", that overrides
	// port. The host half is honoured by ListenAddr. Split on the host:port
	// shape rather than the last colon, or an unbracketed IPv6 bind like
	// "::1" reads as port 1 — a port the operator never wrote, from a bind
	// that names no port at all.
	if _, port, err := net.SplitHostPort(cfg.Bind); err == nil {
		if p, perr := strconv.ParseUint(port, 10, 16); perr == nil {
			cfg.Port = uint16(p)
		}
	}

	// A bare IP is an unambiguous single-host proxy, so carry it in the CIDR
	// form consumers parse. Left as written it would fail ParseCIDR at request
	// time and degrade into "trust nobody" long after startup logged nothing.
	for i, p := range cfg.TrustedProxies {
		if ip := net.ParseIP(p); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			cfg.TrustedProxies[i] = ip.String() + "/" + strconv.Itoa(bits)
		}
	}

	return cfg, nil
}

// splitList parses the comma-separated form env vars and flags use for list
// values, dropping surrounding space and empty entries.
func splitList(v string) []string {
	return cleanList(strings.Split(v, ","))
}

// cleanList trims each entry and drops the empty ones.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fileConfig is the shape accepted in the JSON config file.
type fileConfig struct {
	Bind      *string `json:"bind"`
	UIDir     *string `json:"ui_dir"`
	StatePath *string `json:"state_path"`
	DBPath    *string `json:"db_path"`
	Token     *string `json:"token"`
	Port      *int64  `json:"port"`

	InsecureNoAuth *bool `json:"insecure_no_auth"`

	WgIP        *string `json:"wg_ip"`
	WgPort      *int64  `json:"wg_port"`
	WgKeyPath   *string `json:"wg_key_path"`
	WgEndpoint  *string `json:"wg_endpoint"`
	WgKeepalive *int64  `json:"wg_keepalive"`

	TLSDomain   *string `json:"tls_domain"`
	TLSCacheDir *string `json:"tls_cache_dir"`

	IngressDomain *string `json:"ingress_domain"`

	ImagesDir *string `json:"images_dir"`

	UsageRetentionDays *int64 `json:"usage_retention_days"`

	TrustedProxies *[]string `json:"trusted_proxies"`
	TrustXRealIP   *bool     `json:"trust_x_real_ip"`
}

func applyFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return err
	}
	if fc.Bind != nil {
		cfg.Bind = *fc.Bind
	}
	if fc.UIDir != nil {
		cfg.UIDir = *fc.UIDir
	}
	if fc.StatePath != nil {
		cfg.StatePath = *fc.StatePath
	}
	if fc.DBPath != nil {
		cfg.DBPath = *fc.DBPath
	}
	if fc.Token != nil {
		cfg.Token = *fc.Token
	}
	if fc.InsecureNoAuth != nil {
		cfg.InsecureNoAuth = *fc.InsecureNoAuth
	}
	if fc.Port != nil {
		cfg.Port = uint16(*fc.Port)
	}
	if fc.WgIP != nil {
		cfg.WgIP = *fc.WgIP
	}
	// Range-checked: silent uint16 truncation would bind a random port.
	if fc.WgPort != nil && *fc.WgPort >= 1 && *fc.WgPort <= 65535 {
		cfg.WgPort = uint16(*fc.WgPort)
	}
	if fc.WgKeyPath != nil {
		cfg.WgKeyPath = *fc.WgKeyPath
	}
	if fc.WgEndpoint != nil {
		cfg.WgEndpoint = *fc.WgEndpoint
	}
	if fc.WgKeepalive != nil && *fc.WgKeepalive >= 0 && *fc.WgKeepalive <= 65535 {
		cfg.WgKeepalive = uint16(*fc.WgKeepalive)
	}
	if fc.TLSDomain != nil {
		cfg.TLSDomain = *fc.TLSDomain
	}
	if fc.TLSCacheDir != nil {
		cfg.TLSCacheDir = *fc.TLSCacheDir
	}
	if fc.IngressDomain != nil {
		cfg.IngressDomain = *fc.IngressDomain
	}
	if fc.ImagesDir != nil {
		cfg.ImagesDir = *fc.ImagesDir
	}
	if fc.UsageRetentionDays != nil && *fc.UsageRetentionDays >= 0 {
		cfg.UsageRetentionDays = *fc.UsageRetentionDays
	}
	if fc.TrustedProxies != nil {
		cfg.TrustedProxies = cleanList(*fc.TrustedProxies)
	}
	if fc.TrustXRealIP != nil {
		cfg.TrustXRealIP = *fc.TrustXRealIP
	}
	return nil
}

// bindHost splits cfg.Bind into its host half, reporting whether the value
// carried an explicit ":port" at all. A bind with no host:port shape is a bare
// host ("10.0.0.5", "::1", "[::1]") — never a licence to widen the listener,
// which is why the whole value is returned as the host rather than dropped.
func (c *Config) bindHost() (host string, hadPort bool) {
	if c.Bind == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(c.Bind); err == nil {
		return h, true
	}
	// Unbracketed IPv6 ("::1") and bracketed-without-port ("[::1]") both mean
	// "this address, settled port".
	return strings.Trim(c.Bind, "[]"), false
}

// ListenAddr is the address the HTTP listener binds: the host from bind plus
// the port the quirk above settled on. The host used to be discarded, so an
// operator who followed the docs and configured "127.0.0.1:8080" still got a
// world-reachable control plane. Only a bind that names no host at all ("" or
// ":8080") gets the 0.0.0.0 wildcard: a port-less host like "10.0.0.5" is
// honoured with the settled port, because substituting the wildcard for an
// address the operator wrote is the same silent widening in a different shape.
// ValidateBind refuses the values this cannot honour literally.
func (c *Config) ListenAddr() string {
	host, _ := c.bindHost()
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.FormatUint(uint64(c.Port), 10))
}

// ValidateBind rejects a bind hearthd cannot honour as written. Refusing to
// start is recoverable in seconds; guessing the wildcard for an address the
// operator typed exposes the admin API — which is remote root on every worker
// — to everything that can route to the host, and nothing in the logs says so.
func (c *Config) ValidateBind() error {
	host, hadPort := c.bindHost()
	if hadPort {
		if _, port, err := net.SplitHostPort(c.Bind); err == nil && port != "" {
			if _, perr := strconv.ParseUint(port, 10, 16); perr != nil {
				return fmt.Errorf("bind %q has a non-numeric port %q: hearthd would silently listen on %d instead", c.Bind, port, c.Port)
			}
		}
	}
	if host == "" {
		return nil // ":8080" / "" — the operator asked for every interface.
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	if !hadPort {
		if _, err := strconv.ParseUint(host, 10, 16); err == nil {
			return fmt.Errorf("bind %q is a bare port, not a host:port address: write %q for every interface, or \"127.0.0.1:%s\" for loopback only", c.Bind, ":"+host, host)
		}
	}
	if !isHostname(host) {
		return fmt.Errorf("bind %q has an unusable host %q: use an IP literal, a hostname, or \":%d\" for every interface", c.Bind, host, c.Port)
	}
	return nil
}

// isHostname reports whether h looks like a DNS name hearthd can resolve at
// listen time. Deliberately permissive — the point is to catch a value that is
// not an address at all, not to out-parse the resolver.
func isHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// OverlayBindProblem describes why enrolled agents cannot reach hearthd once
// the wg overlay is on, or "" when they can. Agents dial hearthd at its
// overlay IP inside the tunnel, so a loopback-only listener blackholes the
// whole fleet: every registration and heartbeat fails with a connection
// refused that reads like an agent-side fault. The shipped example config now
// binds loopback, which is right for the Caddy-fronted deployment and wrong
// for an overlay one, so say it loudly at startup instead of leaving an
// operator to debug a fleet that silently never enrolls.
func (c *Config) OverlayBindProblem() string {
	if c.WgIP == "" {
		return ""
	}
	host, _ := c.bindHost()
	if host == "" {
		return "" // wildcard: the overlay interface is covered.
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return ""
	}
	overlay, _, err := net.ParseCIDR(c.WgIP)
	dialed := c.WgIP
	if err == nil {
		dialed = overlay.String()
	}
	if isLoopbackHost(host) {
		return fmt.Sprintf("bind %q is loopback-only but the wg overlay is enabled: enrolled agents dial hearthd at %s inside the tunnel and every one of them will fail to connect — bind the overlay address or the wildcard", c.Bind, dialed)
	}
	if ip := net.ParseIP(host); ip != nil && err == nil && !ip.Equal(overlay) {
		return fmt.Sprintf("bind %q listens only on %s but the wg overlay is enabled: enrolled agents dial hearthd at %s inside the tunnel and will not be able to reach it — bind the overlay address or the wildcard", c.Bind, ip, dialed)
	}
	return ""
}

// isLoopbackHost covers both the IP literals and the name that resolves to
// them; "localhost" in a bind is a loopback-only listener like any other.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// ValidateTrustedProxies reports a trusted-proxy entry hearthd cannot parse.
// A malformed CIDR has to be a startup failure and not a skipped entry: the
// operator who wrote it believes forwarded client addresses are being
// honoured, and a silent skip leaves the deployment keying its throttles and
// logs off the proxy's own address with nothing anywhere saying why.
func (c *Config) ValidateTrustedProxies() error {
	for _, p := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil {
			return fmt.Errorf("trusted proxy %q is not a CIDR: write e.g. \"127.0.0.1/32\" for a local reverse proxy or \"10.0.0.0/8\" for a range", p)
		}
	}
	// X-Real-IP carries no chain of custody, so it is only ever read from a
	// declared proxy. Enabling it without declaring one would mean believing a
	// bare header from every peer — a client choosing its own throttle key,
	// which is the whole thing the key exists to prevent. Refuse to start
	// rather than silently ignore half of what the operator configured.
	if c.TrustXRealIP && len(c.TrustedProxies) == 0 {
		return errors.New("trust_x_real_ip is set but no trusted_proxies are declared: X-Real-IP has no chain of custody, so it is only ever read from a declared proxy — list the proxy's address in trusted_proxies, or drop trust_x_real_ip and have the proxy send X-Forwarded-For")
	}
	return nil
}

// MinTokenLen is the shortest admin token hearthd will serve with. The
// documented recipe (`openssl rand -hex 32`) yields 64 chars; below 32 an
// operator-chosen token is guessable at API line rate.
const MinTokenLen = 32

// placeholderTokens are the literals shipped in deploy/config/*.example.json
// and hardcoded in the lab scripts. They are published in the repo, so they
// authenticate everyone who can read GitHub.
var placeholderTokens = []string{
	"replace_with_output_of__openssl_rand_-hex_32",
	"replace_with_same_token_as_hearthd",
	"hearth-lab-token",
}

// IsPlaceholderToken reports whether tok is one of the public example values.
// Compared case-insensitively, and anything still carrying "REPLACE_WITH" is
// an unedited example whatever the rest of it says.
func IsPlaceholderToken(tok string) bool {
	low := strings.ToLower(tok)
	if strings.Contains(low, "replace_with") {
		return true
	}
	for _, p := range placeholderTokens {
		if low == p {
			return true
		}
	}
	return false
}

// ValidateAuth reports why the configured credentials must not be served with.
// hearthd calls it before it listens: the admin API is remote root on every
// worker, so a missing, public or guessable token is a startup failure and not
// a warning. InsecureNoAuth is the operator's explicit opt-out.
func (c *Config) ValidateAuth() error {
	if c.InsecureNoAuth {
		return nil
	}
	if c.Token == "" {
		return errors.New("no auth token configured: set token / HEARTH_TOKEN / --token, or pass --insecure-no-auth for a loopback-only lab")
	}
	if IsPlaceholderToken(c.Token) {
		return errors.New("auth token is a shipped placeholder and is public: generate one with `openssl rand -hex 32`")
	}
	if len(c.Token) < MinTokenLen {
		return fmt.Errorf("auth token is %d chars, minimum is %d: generate one with `openssl rand -hex 32`", len(c.Token), MinTokenLen)
	}
	return nil
}

// Authorized returns true only if the Authorization header value equals
// "Bearer <token>", compared in constant time. An empty configured token
// authorizes nobody: open mode is a deliberate operator decision expressed by
// InsecureNoAuth, never a side effect of a token that failed to load.
func Authorized(token, authHeader string) bool {
	if token == "" {
		return false
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	provided := authHeader[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(provided)) == 1
}
