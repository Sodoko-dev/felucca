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
	Token     string
	Port      uint16
}

// Load builds a Config from the four layers (defaults < file < env < flags).
// It replicates the Zig loadConfig exactly, including the bind→port quirk.
func Load(args []string) (*Config, error) {
	cfg := &Config{
		Bind:      "0.0.0.0:8080",
		UIDir:     "/usr/share/hearth/ui",
		StatePath: "/var/lib/hearth/state.json",
		Token:     "",
		Port:      8080,
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
	if v := os.Getenv("HEARTH_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("HEARTH_PORT"); v != "" {
		if p, err := strconv.ParseUint(v, 10, 16); err == nil {
			cfg.Port = uint16(p)
		}
	}

	// --- Layer 3: Command-line flags (highest precedence) ---
	fs := flag.NewFlagSet("hearthd", flag.ContinueOnError)
	bind := fs.String("bind", cfg.Bind, "")
	uiDir := fs.String("ui-dir", cfg.UIDir, "")
	statePath := fs.String("state", cfg.StatePath, "")
	token := fs.String("token", cfg.Token, "")
	port := fs.Uint("port", uint(cfg.Port), "")
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
		case "token":
			cfg.Token = *token
		case "port":
			cfg.Port = uint16(*port)
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
	Token     *string `json:"token"`
	Port      *int64  `json:"port"`
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
	if fc.Token != nil {
		cfg.Token = *fc.Token
	}
	if fc.Port != nil {
		cfg.Port = uint16(*fc.Port)
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
