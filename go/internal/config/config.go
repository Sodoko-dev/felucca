// Package config handles hearthd configuration loading.
// Precedence: flags > env vars > JSON config file > defaults.
package config

import (
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
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

	// WireGuard overlay. Empty WgIP means the overlay is disabled.
	WgIP        string // hearthd's overlay address in CIDR form, e.g. "10.100.0.1/16"
	WgPort      uint16 // wg listen port
	WgKeyPath   string // private key file path
	WgEndpoint  string // public "host:port" workers dial for wg
	WgKeepalive uint16 // persistent-keepalive seconds handed to joining workers
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

		WgIP:        "",
		WgPort:      51820,
		WgKeyPath:   "/var/lib/hearth/wg.key",
		WgEndpoint:  "",
		WgKeepalive: 25,
	}

	// --- Layer 1: JSON config file (lowest above defaults) ---
	// Find the config path from --config flag or HEARTH_CONFIG env.
	configPath := os.Getenv("HEARTH_CONFIG")
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			configPath = args[i+1]
		}
	}
	if configPath != "" {
		if err := applyFile(cfg, configPath); err != nil {
			// Log but don't fail — mirrors Zig warn-and-continue.
			fmt.Fprintf(os.Stderr, "config: could not load %s: %v\n", configPath, err)
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

	// --- Layer 3: Command-line flags (highest precedence) ---
	fs := flag.NewFlagSet("hearthd", flag.ContinueOnError)
	bind := fs.String("bind", cfg.Bind, "")
	uiDir := fs.String("ui-dir", cfg.UIDir, "")
	statePath := fs.String("state", cfg.StatePath, "")
	dbPath := fs.String("db", cfg.DBPath, "")
	token := fs.String("token", cfg.Token, "")
	port := fs.Uint("port", uint(cfg.Port), "")
	wgIP := fs.String("wg-ip", cfg.WgIP, "")
	wgPort := fs.Uint("wg-port", uint(cfg.WgPort), "")
	wgKeyPath := fs.String("wg-key", cfg.WgKeyPath, "")
	wgEndpoint := fs.String("wg-endpoint", cfg.WgEndpoint, "")
	wgKeepalive := fs.Uint("wg-keepalive", uint(cfg.WgKeepalive), "")
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
		}
	})

	// --- Bind→port quirk (replicated exactly from Zig) ---
	// After all layers: if bind contains a parseable trailing ":port", that
	// overrides port. The listener always binds 0.0.0.0:<port>.
	if colon := strings.LastIndex(cfg.Bind, ":"); colon >= 0 {
		if p, err := strconv.ParseUint(cfg.Bind[colon+1:], 10, 16); err == nil {
			cfg.Port = uint16(p)
		}
	}

	return cfg, nil
}

// fileConfig is the shape accepted in the JSON config file.
type fileConfig struct {
	Bind      *string `json:"bind"`
	UIDir     *string `json:"ui_dir"`
	StatePath *string `json:"state_path"`
	DBPath    *string `json:"db_path"`
	Token     *string `json:"token"`
	Port      *int64  `json:"port"`

	WgIP        *string `json:"wg_ip"`
	WgPort      *int64  `json:"wg_port"`
	WgKeyPath   *string `json:"wg_key_path"`
	WgEndpoint  *string `json:"wg_endpoint"`
	WgKeepalive *int64  `json:"wg_keepalive"`
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
	return nil
}

// Authorized returns true if no token is configured (open) or if the
// Authorization header value equals "Bearer <token>" using constant-time
// comparison (replicating common.authorized in Zig).
func Authorized(token, authHeader string) bool {
	if token == "" {
		return true
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	provided := authHeader[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(token), []byte(provided)) == 1
}
