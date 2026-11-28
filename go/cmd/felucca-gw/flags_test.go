package main

import (
	"bytes"
	"flag"
	"io"
	"strings"
	"testing"
)

// TestUsageNeverPrintsTheToken: --help printed the admin token, because -token
// took its default from FELUCCA_TOKEN and flag prints defaults.
func TestUsageNeverPrintsTheToken(t *testing.T) {
	const marker = "SECRET-MARKER-7f3a"
	t.Setenv("FELUCCA_TOKEN", marker)
	fs := flag.NewFlagSet("felucca-gw", flag.ContinueOnError)
	defineFlags(fs)
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.PrintDefaults()
	if buf.Len() == 0 {
		t.Fatal("usage rendered nothing; the test is not looking at the help text")
	}
	if strings.Contains(buf.String(), marker) {
		t.Fatalf("usage text contains the token:\n%s", buf.String())
	}
}

func parseWith(t *testing.T, args ...string) (*options, error) {
	t.Helper()
	fs := flag.NewFlagSet("felucca-gw", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := defineFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return o, o.resolveEnv(fs)
}

// The token still comes from the environment when the flag is absent, and the
// flag still beats the environment.
func TestTokenFlagBeatsEnvBeatsDefault(t *testing.T) {
	t.Setenv("FELUCCA_TOKEN", "from-env")
	if o, err := parseWith(t); err != nil || o.token != "from-env" {
		t.Errorf("env only: token=%q err=%v, want from-env", o.token, err)
	}
	if o, err := parseWith(t, "-token", "from-flag"); err != nil || o.token != "from-flag" {
		t.Errorf("flag and env: token=%q err=%v, want from-flag", o.token, err)
	}
	t.Setenv("FELUCCA_TOKEN", "")
	if o, err := parseWith(t); err != nil || o.token != "" {
		t.Errorf("neither: token=%q err=%v, want empty", o.token, err)
	}
}
