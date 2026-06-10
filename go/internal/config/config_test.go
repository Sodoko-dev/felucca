package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/alpham/infra-saas/hearth/internal/config"
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

func TestAuthorized(t *testing.T) {
	// No token configured → always open.
	if !config.Authorized("", "") {
		t.Error("no token: should be authorized")
	}
	if !config.Authorized("", "Bearer anything") {
		t.Error("no token: should be authorized regardless of header")
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
