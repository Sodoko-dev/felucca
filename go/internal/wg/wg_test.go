package wg

import (
	"strings"
	"testing"
)

// ---- AllocateOverlayIP (pure, no shell) ----

func TestAllocateFirstFree(t *testing.T) {
	got, err := AllocateOverlayIP("10.100.0.1/24", nil)
	if err != nil {
		t.Fatalf("AllocateOverlayIP: %v", err)
	}
	if got != "10.100.0.2" {
		t.Fatalf("first free = %q, want 10.100.0.2", got)
	}
}

func TestAllocateSkipsTaken(t *testing.T) {
	got, err := AllocateOverlayIP("10.100.0.1/24", []string{"10.100.0.2", "10.100.0.3"})
	if err != nil {
		t.Fatalf("AllocateOverlayIP: %v", err)
	}
	if got != "10.100.0.4" {
		t.Fatalf("got %q, want 10.100.0.4", got)
	}
}

func TestAllocateSkipsServerIP(t *testing.T) {
	// Server sits at .3; .1 and .2 are free and lower, .3 must be skipped
	// when .1/.2 are taken.
	got, err := AllocateOverlayIP("10.100.0.3/24", []string{"10.100.0.1", "10.100.0.2"})
	if err != nil {
		t.Fatalf("AllocateOverlayIP: %v", err)
	}
	if got != "10.100.0.4" {
		t.Fatalf("got %q, want 10.100.0.4 (server .3 skipped)", got)
	}
}

func TestAllocateExhausted(t *testing.T) {
	// /30: hosts are .1 (server) and .2; taking .2 exhausts the subnet.
	_, err := AllocateOverlayIP("10.100.0.1/30", []string{"10.100.0.2"})
	if err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("error %q does not mention exhaustion", err)
	}
}

func TestAllocateInvalidCIDR(t *testing.T) {
	for _, cidr := range []string{"not-a-cidr", "10.100.0.1", "10.100.0.1/33", ""} {
		if _, err := AllocateOverlayIP(cidr, nil); err == nil {
			t.Errorf("AllocateOverlayIP(%q): expected error, got nil", cidr)
		}
	}
}

// ---- AddPeer validation (must fail before any exec) ----

const goodKey = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOP0=" // 43 base64 chars + '='

func TestAddPeerRejectsBadPubKey(t *testing.T) {
	bad := []string{
		"",
		"short=",
		strings.Repeat("a", 44),                                  // no trailing '='
		strings.Repeat("a", 43) + "==",                           // wrong length (45)
		strings.Repeat("a", 20) + " " + strings.Repeat("a", 22) + "=", // space (shell-unsafe), 44 chars
		strings.Repeat("a", 20) + ";" + strings.Repeat("a", 22) + "=", // semicolon, 44 chars
	}
	for _, k := range bad {
		err := AddPeer(k, "10.100.0.2")
		if err == nil {
			t.Errorf("AddPeer(%q): expected validation error, got nil", k)
			continue
		}
		if !strings.Contains(err.Error(), "validation") {
			t.Errorf("AddPeer(%q): error %q does not mention validation (exec may have been attempted)", k, err)
		}
	}
}

func TestAddPeerRejectsBadOverlayIP(t *testing.T) {
	bad := []string{"", "not-an-ip", "10.100.0.999", "fd00::1", "10.0.0.1; rm -rf /"}
	for _, ip := range bad {
		err := AddPeer(goodKey, ip)
		if err == nil {
			t.Errorf("AddPeer(ip=%q): expected validation error, got nil", ip)
			continue
		}
		if !strings.Contains(err.Error(), "validation") {
			t.Errorf("AddPeer(ip=%q): error %q does not mention validation (exec may have been attempted)", ip, err)
		}
	}
}

func TestValidPubKeyAcceptsWellFormed(t *testing.T) {
	if !ValidPubKey(goodKey) {
		t.Fatalf("ValidPubKey(%q) = false, want true", goodKey)
	}
}
