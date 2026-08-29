package config_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alpham/infra-saas/hearth/internal/config"
	"github.com/alpham/infra-saas/hearth/internal/model"
	"github.com/alpham/infra-saas/hearth/internal/state"
	"github.com/alpham/infra-saas/hearth/internal/store"

	_ "modernc.org/sqlite" // poison a stored row the way a driver/IO fault would
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bind != "0.0.0.0:8080" {
		t.Errorf("Bind default: got %q", cfg.Bind)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port default: got %d", cfg.Port)
	}
	if cfg.UIDir != "/usr/share/hearth/ui" {
		t.Errorf("UIDir default: got %q", cfg.UIDir)
	}
	if cfg.StatePath != "/var/lib/hearth/state.json" {
		t.Errorf("StatePath default: got %q", cfg.StatePath)
	}
	if cfg.Token != "" {
		t.Errorf("Token default: got %q", cfg.Token)
	}
}

func TestFlagPrecedence(t *testing.T) {
	t.Setenv("HEARTH_BIND", "0.0.0.0:9999")
	t.Setenv("HEARTH_TOKEN", "fromenv")

	cfg, err := config.Load([]string{"--bind", "0.0.0.0:7777", "--token", "fromflag"})
	if err != nil {
		t.Fatal(err)
	}
	// Flag beats env.
	if cfg.Bind != "0.0.0.0:7777" {
		t.Errorf("bind flag override: got %q", cfg.Bind)
	}
	if cfg.Token != "fromflag" {
		t.Errorf("token flag override: got %q", cfg.Token)
	}
	// Bind→port quirk: port extracted from bind.
	if cfg.Port != 7777 {
		t.Errorf("bind→port quirk: got %d", cfg.Port)
	}
}

func TestEnvBeatsFile(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "cfg.json")
	data, _ := json.Marshal(map[string]string{
		"token": "fromfile",
		"bind":  "0.0.0.0:5555",
	})
	os.WriteFile(cfgFile, data, 0o644)

	t.Setenv("HEARTH_TOKEN", "fromenv")
	t.Setenv("HEARTH_CONFIG", cfgFile)

	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	// Env beats file.
	if cfg.Token != "fromenv" {
		t.Errorf("env beats file for token: got %q", cfg.Token)
	}
	// File value for bind used (no env override).
	if cfg.Bind != "0.0.0.0:5555" {
		t.Errorf("file value for bind: got %q", cfg.Bind)
	}
	// Port derived from bind.
	if cfg.Port != 5555 {
		t.Errorf("bind→port from file: got %d", cfg.Port)
	}
}

func TestExplicitConfigPathMissingIsFatal(t *testing.T) {
	// A path the operator named must never be a soft miss: continuing would
	// leave Token empty and (pre-fix) open the admin API.
	missing := filepath.Join(t.TempDir(), "nope.json")

	if _, err := config.Load([]string{"--config", missing}); err == nil {
		t.Error("--config with a missing file: want error, got nil")
	}
	if _, err := config.Load([]string{"--config=" + missing}); err == nil {
		t.Error("--config=<path> with a missing file: want error, got nil")
	}

	t.Setenv("HEARTH_CONFIG", missing)
	if _, err := config.Load([]string{}); err == nil {
		t.Error("HEARTH_CONFIG with a missing file: want error, got nil")
	}
}

func TestExplicitConfigPathMalformedIsFatal(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(cfgFile, []byte(`{"token": "abc",}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load([]string{"--config", cfgFile})
	if err == nil {
		t.Fatalf("malformed config: want error, got cfg with token %q", cfg.Token)
	}
}

func TestConfigPathEqualsFormApplies(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "cfg.json")
	data, _ := json.Marshal(map[string]string{"token": "0123456789abcdef0123456789abcdef"})
	if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load([]string{"--config=" + cfgFile})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "0123456789abcdef0123456789abcdef" {
		t.Errorf("--config=<path> should apply the file: token %q", cfg.Token)
	}
}

func TestBindPortQuirk(t *testing.T) {
	tests := []struct {
		bind     string
		wantPort uint16
	}{
		{"0.0.0.0:8080", 8080},
		{"0.0.0.0:9001", 9001},
		{"0.0.0.0:notaport", 8080}, // parse fails, port stays at default
		{"localhost:1234", 1234},
		{"nocolon", 8080},
		{"[::1]:1234", 1234},
		// An unbracketed IPv6 literal names no port: the trailing ":1" is part
		// of the address, so the settled port must stay the default.
		{"::1", 8080},
	}
	for _, tc := range tests {
		t.Run(tc.bind, func(t *testing.T) {
			cfg, err := config.Load([]string{"--bind", tc.bind})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Port != tc.wantPort {
				t.Errorf("bind=%q → port=%d, want %d", tc.bind, cfg.Port, tc.wantPort)
			}
		})
	}
}

func TestListenAddrHonoursBindHost(t *testing.T) {
	tests := []struct {
		bind string
		want string
	}{
		// A loopback bind must actually listen on loopback.
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"localhost:1234", "localhost:1234"},
		{"10.100.0.1:8080", "10.100.0.1:8080"},
		{"[::1]:8080", "[::1]:8080"},
		// Wildcard cases: only a bind that names no host at all.
		{"0.0.0.0:8080", "0.0.0.0:8080"},
		{":9000", "0.0.0.0:9000"},
		// Port-less host: honoured with the settled port. Substituting the
		// wildcard here is the same silent widening in a different shape —
		// "10.0.0.5" is an address the operator wrote, not a blank.
		{"10.0.0.5", "10.0.0.5:8080"},
		{"127.0.0.1", "127.0.0.1:8080"},
		{"::1", "[::1]:8080"},
		{"[::1]", "[::1]:8080"},
		{"nocolon", "nocolon:8080"},
		// Unparseable port half: host still honoured, port stays default.
		{"127.0.0.1:notaport", "127.0.0.1:8080"},
	}
	for _, tc := range tests {
		t.Run(tc.bind, func(t *testing.T) {
			cfg, err := config.Load([]string{"--bind", tc.bind})
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.ListenAddr(); got != tc.want {
				t.Errorf("bind=%q → ListenAddr()=%q, want %q", tc.bind, got, tc.want)
			}
		})
	}
}

func TestPortLessBindIsNeverWidenedToWildcard(t *testing.T) {
	// The port-less form used to be dropped on the floor: SplitHostPort fails,
	// the host is reset to "", and 0.0.0.0 takes its place with nothing said
	// anywhere. An operator who wrote a single management address got a
	// world-reachable admin API — remote root on every worker — instead.
	for _, bind := range []string{"10.0.0.5", "127.0.0.1", "192.168.1.10", "::1"} {
		t.Run(bind, func(t *testing.T) {
			cfg, err := config.Load([]string{"--bind", bind})
			if err != nil {
				t.Fatal(err)
			}
			addr := cfg.ListenAddr()
			if strings.HasPrefix(addr, "0.0.0.0:") || strings.HasPrefix(addr, "[::]:") {
				t.Fatalf("bind=%q widened to the wildcard: ListenAddr()=%q", bind, addr)
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("ListenAddr()=%q is not a host:port: %v", addr, err)
			}
			if want := strings.Trim(bind, "[]"); host != want {
				t.Errorf("bind=%q → listen host %q, want %q", bind, host, want)
			}
		})
	}
}

func TestValidateBind(t *testing.T) {
	// The values hearthd cannot honour literally must stop startup rather than
	// resolve to something wider than what was written.
	ok := []string{"", "0.0.0.0:8080", ":8080", "127.0.0.1:8080", "10.0.0.5",
		"localhost:1234", "[::1]:8080", "::1", "hearth.example.com:8080"}
	for _, bind := range ok {
		t.Run("ok/"+bind, func(t *testing.T) {
			cfg := &config.Config{Bind: bind, Port: 8080}
			if err := cfg.ValidateBind(); err != nil {
				t.Errorf("bind=%q: want nil, got %v", bind, err)
			}
		})
	}

	bad := []string{
		"8080",               // a port typed into a host:port field
		"127.0.0.1:notaport", // would silently listen on the default port
		"10.0.0.5:80:90",     // not an address at all
		"http://10.0.0.5:8080",
	}
	for _, bind := range bad {
		t.Run("bad/"+bind, func(t *testing.T) {
			cfg := &config.Config{Bind: bind, Port: 8080}
			if err := cfg.ValidateBind(); err == nil {
				t.Errorf("bind=%q: want a startup error, got nil (ListenAddr()=%q)", bind, cfg.ListenAddr())
			}
		})
	}
}

func TestOverlayBindProblem(t *testing.T) {
	const wgIP = "10.100.0.1/16"
	tests := []struct {
		name     string
		bind     string
		wgIP     string
		wantWarn bool
	}{
		// The shipped example config binds loopback, which is right behind a
		// local reverse proxy and cuts off the whole fleet on an overlay
		// deployment: agents dial hearthd's overlay IP inside the tunnel.
		{"loopback with overlay", "127.0.0.1:8080", wgIP, true},
		{"localhost with overlay", "localhost:8080", wgIP, true},
		{"loopback port-less with overlay", "127.0.0.1", wgIP, true},
		{"unrelated address with overlay", "203.0.113.5:8080", wgIP, true},
		// Reachable: the wildcard covers the overlay interface, and so does
		// binding the overlay address itself.
		{"wildcard with overlay", "0.0.0.0:8080", wgIP, false},
		{"empty host with overlay", ":8080", wgIP, false},
		{"overlay address itself", "10.100.0.1:8080", wgIP, false},
		// No overlay: a loopback bind is exactly what the docs ask for.
		{"loopback without overlay", "127.0.0.1:8080", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Bind: tc.bind, Port: 8080, WgIP: tc.wgIP}
			problem := cfg.OverlayBindProblem()
			if tc.wantWarn && problem == "" {
				t.Fatalf("bind=%q wg_ip=%q: want a startup warning, got none", tc.bind, tc.wgIP)
			}
			if !tc.wantWarn && problem != "" {
				t.Fatalf("bind=%q wg_ip=%q: want no warning, got %q", tc.bind, tc.wgIP, problem)
			}
			if tc.wantWarn && !strings.Contains(problem, "10.100.0.1") {
				t.Errorf("warning should name the address agents dial: %q", problem)
			}
		})
	}
}

func TestTrustedProxiesDefaultIsTrustNothing(t *testing.T) {
	// An unconfigured deployment must never honour a client-supplied
	// X-Forwarded-For: it is the only thing keeping a caller from forging the
	// per-source key the auth throttle counts on.
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies default: got %v, want empty", cfg.TrustedProxies)
	}
	if err := cfg.ValidateTrustedProxies(); err != nil {
		t.Errorf("empty trusted proxies: want nil, got %v", err)
	}
}

func TestTrustedProxiesSources(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "cfg.json")
		data, _ := json.Marshal(map[string]any{
			"trusted_proxies": []string{"127.0.0.1/32", " 10.0.0.0/8 ", ""},
		})
		if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HEARTH_CONFIG", cfgFile)
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cfg.TrustedProxies, ","); got != "127.0.0.1/32,10.0.0.0/8" {
			t.Errorf("trusted_proxies from file: got %q", got)
		}
	})

	t.Run("env-beats-file", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "cfg.json")
		data, _ := json.Marshal(map[string]any{"trusted_proxies": []string{"10.0.0.0/8"}})
		if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HEARTH_CONFIG", cfgFile)
		t.Setenv("HEARTH_TRUSTED_PROXIES", "192.168.0.0/16, 172.16.0.0/12")
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cfg.TrustedProxies, ","); got != "192.168.0.0/16,172.16.0.0/12" {
			t.Errorf("HEARTH_TRUSTED_PROXIES: got %q", got)
		}
	})

	t.Run("flag-beats-env", func(t *testing.T) {
		t.Setenv("HEARTH_TRUSTED_PROXIES", "192.168.0.0/16")
		cfg, err := config.Load([]string{"--trusted-proxies", "10.1.2.0/24"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cfg.TrustedProxies, ","); got != "10.1.2.0/24" {
			t.Errorf("--trusted-proxies: got %q", got)
		}
	})

	t.Run("flag-can-clear", func(t *testing.T) {
		// Naming the flag with an empty value is the operator asking to trust
		// nobody, and must beat a proxy list inherited from env or file.
		t.Setenv("HEARTH_TRUSTED_PROXIES", "192.168.0.0/16")
		cfg, err := config.Load([]string{"--trusted-proxies", ""})
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.TrustedProxies) != 0 {
			t.Errorf("--trusted-proxies=\"\": got %v, want empty", cfg.TrustedProxies)
		}
	})

	t.Run("bare-ip-normalized-to-cidr", func(t *testing.T) {
		// Consumers parse these with ParseCIDR. A bare IP left as written
		// would fail there and degrade into "trust nobody" at request time,
		// long after startup had a chance to say so.
		cfg, err := config.Load([]string{"--trusted-proxies", "127.0.0.1,::1"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(cfg.TrustedProxies, ","); got != "127.0.0.1/32,::1/128" {
			t.Errorf("bare IP normalization: got %q", got)
		}
		if err := cfg.ValidateTrustedProxies(); err != nil {
			t.Errorf("normalized entries must validate: %v", err)
		}
	})
}

func TestValidateTrustedProxies(t *testing.T) {
	// A malformed entry is a startup failure: skipping it silently leaves the
	// operator believing forwarded addresses are honoured when they are not.
	bad := []string{"10.0.0.0/33", "not-an-ip/24", "10.0.0.0/8extra", "10.0.0.0-10.0.0.255"}
	for _, p := range bad {
		cfg := &config.Config{TrustedProxies: []string{p}}
		if err := cfg.ValidateTrustedProxies(); err == nil {
			t.Errorf("trusted proxy %q: want error, got nil", p)
		}
	}

	good := &config.Config{TrustedProxies: []string{"127.0.0.1/32", "10.0.0.0/8", "::1/128", "fd00::/8"}}
	if err := good.ValidateTrustedProxies(); err != nil {
		t.Errorf("valid CIDRs: want nil, got %v", err)
	}

	// One bad entry among good ones still stops startup.
	mixed := &config.Config{TrustedProxies: []string{"127.0.0.1/32", "10.0.0.0/99"}}
	if err := mixed.ValidateTrustedProxies(); err == nil {
		t.Error("mixed valid/invalid: want error, got nil")
	}
}

// X-Real-IP has no chain of custody — hearthd cannot tell a value the proxy
// wrote from one it forwarded verbatim — so honouring it is an explicit
// operator decision that is OFF by default and cannot be reached by accident.
func TestTrustXRealIPDefaultsOffAndNeedsADeclaredProxy(t *testing.T) {
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TrustXRealIP {
		t.Error("trust_x_real_ip default: got true, want false")
	}

	// On without a declared proxy would mean believing a bare header from every
	// peer — a client choosing its own throttle key. Refuse to start.
	orphan := &config.Config{TrustXRealIP: true}
	err = orphan.ValidateTrustedProxies()
	if err == nil {
		t.Fatal("trust_x_real_ip with no trusted_proxies: want a startup error, got nil")
	}
	if !strings.Contains(err.Error(), "trusted_proxies") {
		t.Errorf("error should name the missing setting: %q", err)
	}

	// With a proxy declared it is a legitimate configuration.
	paired := &config.Config{TrustXRealIP: true, TrustedProxies: []string{"127.0.0.1/32"}}
	if err := paired.ValidateTrustedProxies(); err != nil {
		t.Errorf("trust_x_real_ip with a declared proxy: want nil, got %v", err)
	}
}

func TestTrustXRealIPSources(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "cfg.json")
		data, _ := json.Marshal(map[string]any{
			"trusted_proxies": []string{"127.0.0.1/32"},
			"trust_x_real_ip": true,
		})
		if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HEARTH_CONFIG", cfgFile)
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.TrustXRealIP {
			t.Error("trust_x_real_ip from file: got false, want true")
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("HEARTH_TRUST_X_REAL_IP", "true")
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.TrustXRealIP {
			t.Error("HEARTH_TRUST_X_REAL_IP=true: got false, want true")
		}
	})

	t.Run("env-garbage-keeps-it-off", func(t *testing.T) {
		// Only an affirmative value opts in; a typo must not open the header.
		t.Setenv("HEARTH_TRUST_X_REAL_IP", "yes-please")
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TrustXRealIP {
			t.Error("unparseable HEARTH_TRUST_X_REAL_IP opted in")
		}
	})

	t.Run("flag-beats-env", func(t *testing.T) {
		t.Setenv("HEARTH_TRUST_X_REAL_IP", "true")
		cfg, err := config.Load([]string{"--trust-x-real-ip=false"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TrustXRealIP {
			t.Error("--trust-x-real-ip=false did not override the env var")
		}
	})
}

func TestLegacyPortFlag(t *testing.T) {
	// --port sets port when bind has no parseable port. But if bind also has a
	// port, bind wins (quirk). Here bind is default "0.0.0.0:8080", so bind port
	// (8080) overwrites the --port value. This replicates Zig behavior where
	// bind colon-port is extracted after all layers.
	cfg, err := config.Load([]string{"--port", "9999"})
	if err != nil {
		t.Fatal(err)
	}
	// The bind quirk runs last: bind="0.0.0.0:8080" → port=8080 wins.
	if cfg.Port != 8080 {
		t.Errorf("bind overrides --port: got %d", cfg.Port)
	}
}

func TestWgDefaults(t *testing.T) {
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WgIP != "" {
		t.Errorf("WgIP default: got %q", cfg.WgIP)
	}
	if cfg.WgPort != 51820 {
		t.Errorf("WgPort default: got %d", cfg.WgPort)
	}
	if cfg.WgKeyPath != "/var/lib/hearth/wg.key" {
		t.Errorf("WgKeyPath default: got %q", cfg.WgKeyPath)
	}
	if cfg.WgEndpoint != "" {
		t.Errorf("WgEndpoint default: got %q", cfg.WgEndpoint)
	}
}

func TestWgEnv(t *testing.T) {
	t.Setenv("HEARTH_WG_IP", "10.100.0.1/16")
	t.Setenv("HEARTH_WG_PORT", "51999")
	t.Setenv("HEARTH_WG_KEY_PATH", "/tmp/wg.key")
	t.Setenv("HEARTH_WG_ENDPOINT", "hearth.example.com:51999")

	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WgIP != "10.100.0.1/16" {
		t.Errorf("WgIP from env: got %q", cfg.WgIP)
	}
	if cfg.WgPort != 51999 {
		t.Errorf("WgPort from env: got %d", cfg.WgPort)
	}
	if cfg.WgKeyPath != "/tmp/wg.key" {
		t.Errorf("WgKeyPath from env: got %q", cfg.WgKeyPath)
	}
	if cfg.WgEndpoint != "hearth.example.com:51999" {
		t.Errorf("WgEndpoint from env: got %q", cfg.WgEndpoint)
	}
}

func TestWgFlagPrecedence(t *testing.T) {
	t.Setenv("HEARTH_WG_IP", "10.100.0.1/16")
	t.Setenv("HEARTH_WG_PORT", "51999")
	t.Setenv("HEARTH_WG_KEY_PATH", "/tmp/env-wg.key")
	t.Setenv("HEARTH_WG_ENDPOINT", "env.example.com:51999")

	cfg, err := config.Load([]string{
		"--wg-ip", "10.200.0.1/16",
		"--wg-port", "52000",
		"--wg-key", "/tmp/flag-wg.key",
		"--wg-endpoint", "flag.example.com:52000",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flags beat env.
	if cfg.WgIP != "10.200.0.1/16" {
		t.Errorf("wg-ip flag override: got %q", cfg.WgIP)
	}
	if cfg.WgPort != 52000 {
		t.Errorf("wg-port flag override: got %d", cfg.WgPort)
	}
	if cfg.WgKeyPath != "/tmp/flag-wg.key" {
		t.Errorf("wg-key flag override: got %q", cfg.WgKeyPath)
	}
	if cfg.WgEndpoint != "flag.example.com:52000" {
		t.Errorf("wg-endpoint flag override: got %q", cfg.WgEndpoint)
	}
}

func TestWgFile(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "cfg.json")
	data, _ := json.Marshal(map[string]any{
		"wg_ip":       "10.100.0.1/16",
		"wg_port":     51888,
		"wg_key_path": "/tmp/file-wg.key",
		"wg_endpoint": "file.example.com:51888",
	})
	os.WriteFile(cfgFile, data, 0o644)

	t.Setenv("HEARTH_CONFIG", cfgFile)

	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	// File values apply when env/flags are absent.
	if cfg.WgIP != "10.100.0.1/16" {
		t.Errorf("wg_ip from file: got %q", cfg.WgIP)
	}
	if cfg.WgPort != 51888 {
		t.Errorf("wg_port from file: got %d", cfg.WgPort)
	}
	if cfg.WgKeyPath != "/tmp/file-wg.key" {
		t.Errorf("wg_key_path from file: got %q", cfg.WgKeyPath)
	}
	if cfg.WgEndpoint != "file.example.com:51888" {
		t.Errorf("wg_endpoint from file: got %q", cfg.WgEndpoint)
	}
}

func TestTLSDefaults(t *testing.T) {
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSDomain != "" {
		t.Errorf("TLSDomain default: got %q", cfg.TLSDomain)
	}
	if cfg.TLSCacheDir != "/var/lib/hearth/autocert" {
		t.Errorf("TLSCacheDir default: got %q", cfg.TLSCacheDir)
	}
}

func TestTLSEnv(t *testing.T) {
	t.Setenv("HEARTH_TLS_DOMAIN", "hearth.example.com")
	t.Setenv("HEARTH_TLS_CACHE", "/tmp/env-autocert")

	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSDomain != "hearth.example.com" {
		t.Errorf("TLSDomain from env: got %q", cfg.TLSDomain)
	}
	if cfg.TLSCacheDir != "/tmp/env-autocert" {
		t.Errorf("TLSCacheDir from env: got %q", cfg.TLSCacheDir)
	}
}

func TestTLSFlagPrecedence(t *testing.T) {
	t.Setenv("HEARTH_TLS_DOMAIN", "env.example.com")
	t.Setenv("HEARTH_TLS_CACHE", "/tmp/env-autocert")

	cfg, err := config.Load([]string{
		"--tls-domain", "flag.example.com",
		"--tls-cache", "/tmp/flag-autocert",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Flags beat env.
	if cfg.TLSDomain != "flag.example.com" {
		t.Errorf("tls-domain flag override: got %q", cfg.TLSDomain)
	}
	if cfg.TLSCacheDir != "/tmp/flag-autocert" {
		t.Errorf("tls-cache flag override: got %q", cfg.TLSCacheDir)
	}
}

func TestValidateAuthRejectsOpenMode(t *testing.T) {
	cfg, err := config.Load([]string{})
	if err != nil {
		t.Fatal(err)
	}
	// The default config has no token: hearthd must refuse to serve with it.
	if err := cfg.ValidateAuth(); err == nil {
		t.Error("empty token: want startup error, got nil")
	}

	cfg, err = config.Load([]string{"--insecure-no-auth"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureNoAuth {
		t.Fatal("--insecure-no-auth: flag not applied")
	}
	if err := cfg.ValidateAuth(); err != nil {
		t.Errorf("explicit opt-out: want nil, got %v", err)
	}
}

func TestInsecureNoAuthSources(t *testing.T) {
	// Env and file must be able to express the opt-out too, and only an
	// affirmative value may turn it on.
	t.Run("env", func(t *testing.T) {
		t.Setenv("HEARTH_INSECURE_NO_AUTH", "1")
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.InsecureNoAuth {
			t.Error("HEARTH_INSECURE_NO_AUTH=1: not applied")
		}
	})
	t.Run("env-garbage", func(t *testing.T) {
		t.Setenv("HEARTH_INSECURE_NO_AUTH", "maybe")
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.InsecureNoAuth {
			t.Error("HEARTH_INSECURE_NO_AUTH=maybe: should not open the gate")
		}
	})
	t.Run("file", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "cfg.json")
		data, _ := json.Marshal(map[string]any{"insecure_no_auth": true})
		if err := os.WriteFile(cfgFile, data, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HEARTH_CONFIG", cfgFile)
		cfg, err := config.Load([]string{})
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.InsecureNoAuth {
			t.Error("insecure_no_auth from file: not applied")
		}
	})
}

func TestValidateAuthTokenLength(t *testing.T) {
	short := &config.Config{Token: "0123456789abcdef"} // 16 chars
	if err := short.ValidateAuth(); err == nil {
		t.Error("16-char token: want error, got nil")
	}
	exact := &config.Config{Token: strings.Repeat("a", config.MinTokenLen)}
	if err := exact.ValidateAuth(); err != nil {
		t.Errorf("%d-char token: want nil, got %v", config.MinTokenLen, err)
	}
	// The documented `openssl rand -hex 32` output.
	hex64 := &config.Config{Token: strings.Repeat("0123456789abcdef", 4)}
	if err := hex64.ValidateAuth(); err != nil {
		t.Errorf("64-char hex token: want nil, got %v", err)
	}
}

func TestValidateAuthRejectsPlaceholders(t *testing.T) {
	placeholders := []string{
		"REPLACE_WITH_OUTPUT_OF__openssl_rand_-hex_32",
		"REPLACE_WITH_SAME_TOKEN_AS_HEARTHD",
		"replace_with_output_of__openssl_rand_-hex_32",
		"hearth-lab-token",
		"HEARTH-LAB-TOKEN",
		// Long enough to pass the length floor, still an unedited example.
		"prefix-REPLACE_WITH_ANYTHING-suffix-padding-padding",
	}
	for _, tok := range placeholders {
		cfg := &config.Config{Token: tok}
		if err := cfg.ValidateAuth(); err == nil {
			t.Errorf("placeholder %q: want error, got nil", tok)
		}
		if !config.IsPlaceholderToken(tok) {
			t.Errorf("IsPlaceholderToken(%q) = false", tok)
		}
	}

	real := strings.Repeat("7f3a9c1e", 8)
	if config.IsPlaceholderToken(real) {
		t.Errorf("IsPlaceholderToken(%q) = true for a real token", real)
	}
	if err := (&config.Config{Token: real}).ValidateAuth(); err != nil {
		t.Errorf("real token: want nil, got %v", err)
	}
}

// ---- hearthd startup behaviour ----
//
// These drive the real binary because the properties under test are decisions
// main() makes about the config above — refusing to serve, and warning about a
// listener the fleet cannot reach — and there is no seam short of the process
// that can observe "hearthd did not start".

// hearthdBinary builds cmd/hearthd once per test run and returns its path.
func hearthdBinary(t *testing.T) string {
	t.Helper()
	hearthdBuild.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			hearthdNoToolchain = true
			return
		}
		dir, err := os.MkdirTemp("", "hearthd-bin")
		if err != nil {
			hearthdBuildErr = err
			return
		}
		bin := filepath.Join(dir, "hearthd")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/hearthd")
		cmd.Dir = filepath.Join("..", "..") // module root
		if out, err := cmd.CombinedOutput(); err != nil {
			hearthdBuildErr = fmt.Errorf("go build cmd/hearthd: %v\n%s", err, out)
			return
		}
		hearthdBin = bin
	})
	if hearthdNoToolchain {
		t.Skip("no go toolchain: cannot exercise hearthd startup")
	}
	if hearthdBuildErr != nil {
		// A build failure is a real failure, never a skip: skipping here would
		// quietly stop checking that hearthd refuses to start on a bad load.
		t.Fatalf("cannot build hearthd: %v", hearthdBuildErr)
	}
	return hearthdBin
}

var (
	hearthdBuild       sync.Once
	hearthdBin         string
	hearthdBuildErr    error
	hearthdNoToolchain bool
)

// runHearthd starts the daemon with a clean environment and returns its stderr
// plus whether the process exited on its own before the deadline.
func runHearthd(t *testing.T, timeout time.Duration, args ...string) (stderr string, exited bool, exitErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, hearthdBinary(t), args...)
	cmd.Stderr = &buf
	// Clean env: an inherited HEARTH_* would silently change the run.
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	err := cmd.Run()
	return buf.String(), ctx.Err() == nil, err
}

// realToken passes ValidateAuth so these runs reach the code under test.
const realToken = "9f2c7a1e4b6d8035af19c3e7d24b0f6a9f2c7a1e4b6d8035af19c3e7d24b0f6a"

func TestHearthdRefusesToServeAPartiallyLoadedState(t *testing.T) {
	// LoadInto appends rows as it scans and never clears what it appended, so
	// a load that fails part-way leaves a truncated working set in memory. The
	// first mutation persists it, and SaveSnapshot's DELETE-then-reinsert makes
	// the truncation permanent. A WARN here means the daemon serves happily
	// while the next write destroys every row the scan did not reach.
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "hearth.db")

	db, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Nodes = append(st.Nodes,
		&model.Node{ID: "n1", Hostname: "w1", Addr: "10.0.0.1:9090", CPUs: 4, MemTotalMiB: 8192, LastHB: 1},
		&model.Node{ID: "n2", Hostname: "w2", Addr: "10.0.0.2:9090", CPUs: 8, MemTotalMiB: 16384, LastHB: 2},
	)
	st.Sandboxes = append(st.Sandboxes,
		&model.Sandbox{ID: "sb1", Name: "keepme", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 512, CreatedAt: 1},
		&model.Sandbox{ID: "sb2", Name: "keepmetoo", Namespace: "default", State: model.StateRunning, VCPUs: 1, MemMiB: 512, CreatedAt: 2},
	)
	if err := db.SaveSnapshot(st); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Poison one node row so the nodes scan fails after the first row: cpus is
	// scanned into a uint32, and SQLite keeps the text as written.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE nodes SET cpus = 'not-a-number' WHERE id = 'n2'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	stderr, exited, exitErr := runHearthd(t, 60*time.Second,
		"--db", dbPath,
		"--state", filepath.Join(tmp, "state.json"),
		"--ui-dir", tmp,
		"--images-dir", filepath.Join(tmp, "images"),
		"--bind", "127.0.0.1:0",
		"--token", realToken,
	)
	if !exited {
		t.Fatalf("hearthd kept serving after a failed state load — the next write would delete every row it could not read.\nstderr:\n%s", stderr)
	}
	if exitErr == nil {
		t.Fatalf("hearthd exited 0 after a failed state load, want a non-zero status.\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "refusing to start") {
		t.Errorf("startup failure should say why it refused; stderr:\n%s", stderr)
	}

	// And the rows it could not read are still there: nothing was persisted.
	raw, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, tc := range []struct{ table string }{{"nodes"}, {"sandboxes"}} {
		var n int
		if err := raw.QueryRow(`SELECT COUNT(*) FROM ` + tc.table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("%s: %d rows left, want 2 — the refused start still destroyed data", tc.table, n)
		}
	}
}

func TestHearthdWarnsWhenTheOverlayCannotReachTheListener(t *testing.T) {
	// The shipped example config binds loopback, which is correct behind the
	// documented reverse proxy and cuts off every enrolled agent on an overlay
	// deployment — they dial hearthd's wg address inside the tunnel. Without
	// this line the symptom is a fleet that silently never registers.
	tmp := t.TempDir()

	// The wg key path is deliberately unopenable (its parent is a regular
	// file), so EnsureKey fails and the daemon exits before it reaches
	// EnsureInterface. That is load-bearing, not incidental: wg.run() retries
	// every ip/wg command under sudo, so a hearthd that got that far would
	// reconfigure the host's real wg-hearth interface — replacing its private
	// key and stranding every enrolled agent on the machine running the tests.
	// Keep this path broken. The warning under test is printed well before any
	// of that, which is the whole point of putting it there.
	blocked := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	stderr, exited, _ := runHearthd(t, 30*time.Second,
		"--db", filepath.Join(tmp, "hearth.db"),
		"--state", filepath.Join(tmp, "state.json"),
		"--ui-dir", tmp,
		"--images-dir", filepath.Join(tmp, "images"),
		"--bind", "127.0.0.1:0",
		"--token", realToken,
		"--wg-ip", "10.100.0.1/16",
		"--wg-endpoint", "hearth.example.com:51820",
		"--wg-key", filepath.Join(blocked, "wg.key"),
	)
	if !exited {
		t.Fatalf("hearthd did not stop at the unopenable wg key — it may have reconfigured the host's wg interface.\nstderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "OVERLAY UNREACHABLE") {
		t.Errorf("loopback bind + wg overlay should warn loudly at startup; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "10.100.0.1") {
		t.Errorf("the warning should name the address agents dial; stderr:\n%s", stderr)
	}
}

func TestHearthdRefusesAMalformedTrustedProxy(t *testing.T) {
	tmp := t.TempDir()
	stderr, exited, exitErr := runHearthd(t, 30*time.Second,
		"--db", filepath.Join(tmp, "hearth.db"),
		"--state", filepath.Join(tmp, "state.json"),
		"--ui-dir", tmp,
		"--images-dir", filepath.Join(tmp, "images"),
		"--bind", "127.0.0.1:0",
		"--token", realToken,
		"--trusted-proxies", "10.0.0.0/8,garbage",
	)
	if !exited || exitErr == nil {
		t.Fatalf("hearthd started with an unparseable trusted proxy; stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "trusted") {
		t.Errorf("startup failure should name the bad setting; stderr:\n%s", stderr)
	}
}

func TestAuthorized(t *testing.T) {
	// No token configured → nobody is authorized. A token that never loaded
	// must not turn the admin API into an open one.
	if config.Authorized("", "") {
		t.Error("no token: should be unauthorized")
	}
	if config.Authorized("", "Bearer anything") {
		t.Error("no token: should be unauthorized regardless of header")
	}
	if config.Authorized("", "Bearer ") {
		t.Error("no token: empty bearer value should be unauthorized")
	}

	// Token configured.
	if config.Authorized("secret", "") {
		t.Error("token configured, empty header: should be unauthorized")
	}
	if config.Authorized("secret", "secret") {
		t.Error("no Bearer prefix: should be unauthorized")
	}
	if config.Authorized("secret", "Bearer wrong") {
		t.Error("wrong token: should be unauthorized")
	}
	if !config.Authorized("secret", "Bearer secret") {
		t.Error("correct token: should be authorized")
	}
	// Length mismatch must not panic (constant-time).
	if config.Authorized("short", "Bearer muchlongertoken") {
		t.Error("length mismatch: should be unauthorized")
	}
}
