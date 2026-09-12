// Package wg manages feluccad's WireGuard host interface (wg-felucca):
// key generation, idempotent interface bring-up, peer registration, and
// overlay IP allocation for the join endpoint.
//
// Shell-out to wg/ip with bare-then-`sudo -n` fallback, mirroring the
// run/run_once idiom in rust/agent/src/net.rs. No netlink libraries:
// the static CGO_ENABLED=0 build must stay dependency-free.
package wg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// InterfaceName is the WireGuard interface managed by feluccad.
const InterfaceName = "wg-felucca"

// run executes argv, trying unprivileged first, then `sudo -n` fallback.
// stdout/stderr are discarded. Returns true if exit code 0.
func run(argv ...string) bool {
	if runOnce(argv, false) {
		return true
	}
	return runOnce(argv, true)
}

func runOnce(argv []string, useSudo bool) bool {
	if len(argv) == 0 {
		return false
	}
	var cmd *exec.Cmd
	if useSudo {
		cmd = exec.Command("sudo", append([]string{"-n"}, argv...)...)
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// pubkeyFromPriv derives the public key from a private key by piping it to
// `wg pubkey` on stdin (never via shell interpolation). Unprivileged only:
// key derivation needs no root.
func pubkeyFromPriv(priv string) (string, error) {
	cmd := exec.Command("wg", "pubkey")
	cmd.Stdin = strings.NewReader(priv)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wg pubkey: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// EnsureKey ensures a WireGuard private key exists at path and returns the
// corresponding public key. If the file exists its key is reused; otherwise
// a fresh key is generated with `wg genkey` and written with mode 0600
// (parent directory created 0700). Key generation and pubkey derivation run
// unprivileged; only the file write can fail on permissions.
func EnsureKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		priv := strings.TrimSpace(string(data))
		if priv == "" {
			return "", fmt.Errorf("key file %s is empty", path)
		}
		return pubkeyFromPriv(priv)
	}
	// Generate ONLY when the file genuinely doesn't exist. Any other read
	// error (permissions, I/O) must surface: silently regenerating would
	// rotate feluccad's wg identity and strand every enrolled worker.
	if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read key file %s: %w", path, err)
	}

	cmd := exec.Command("wg", "genkey")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wg genkey: %w", err)
	}
	priv := strings.TrimSpace(string(out))

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create key dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(priv+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write key file: %w", err)
	}
	return pubkeyFromPriv(priv)
}

// EnsureInterface idempotently brings up wg-felucca: create the link (ignore
// "exists"), set listen port + private key, assign ipCIDR (ignore "exists"),
// and bring the link up. Only the final link-up must succeed.
func EnsureInterface(ipCIDR string, listenPort uint16, keyPath string) error {
	// Not idempotent: errors if the interface already exists, which is fine.
	run("ip", "link", "add", InterfaceName, "type", "wireguard")
	if !run("wg", "set", InterfaceName,
		"listen-port", strconv.Itoa(int(listenPort)),
		"private-key", keyPath) {
		return fmt.Errorf("wg set %s failed", InterfaceName)
	}
	// addr add errors if already present, which is fine.
	run("ip", "addr", "add", ipCIDR, "dev", InterfaceName)
	if !run("ip", "link", "set", InterfaceName, "up") {
		return fmt.Errorf("ip link set %s up failed", InterfaceName)
	}
	return nil
}

// ValidPubKey reports whether s is a WireGuard public key: 44 chars of
// standard base64, [A-Za-z0-9+/]{43}=. Exported so the join endpoint can
// reject bad keys with a 400 before consuming the one-time token.
func ValidPubKey(s string) bool {
	if len(s) != 44 || s[43] != '=' {
		return false
	}
	for i := 0; i < 43; i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z',
			c >= '0' && c <= '9', c == '+', c == '/':
		default:
			return false
		}
	}
	return true
}

// validIPv4 reports whether s parses as a plain IPv4 address.
func validIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}

// AddPeer registers a peer on wg-felucca with allowed-ips <overlayIP>/32.
// Inputs are validated before any command runs (defense-in-depth: both
// values arrive over the join API).
func AddPeer(pubKey, overlayIP string) error {
	if !ValidPubKey(pubKey) {
		return fmt.Errorf("invalid peer public key %q: validation failed (want 44-char base64)", pubKey)
	}
	if !validIPv4(overlayIP) {
		return fmt.Errorf("invalid overlay IP %q: validation failed (want IPv4)", overlayIP)
	}
	if !run("wg", "set", InterfaceName, "peer", pubKey, "allowed-ips", overlayIP+"/32") {
		return fmt.Errorf("wg set peer %s failed", pubKey)
	}
	return nil
}

// Peer is one (public key, overlay IP) pair for batch installation.
type Peer struct {
	PubKey    string
	OverlayIP string
}

// AddPeers installs all peers in ONE `wg set` invocation (wg(8) accepts
// repeated peer blocks) — startup re-add stays O(1) execs regardless of fleet
// size. Every entry is validated before anything reaches the command line.
func AddPeers(peers []Peer) error {
	if len(peers) == 0 {
		return nil
	}
	argv := []string{"wg", "set", InterfaceName}
	for _, p := range peers {
		if !ValidPubKey(p.PubKey) {
			return fmt.Errorf("invalid peer public key %q: validation failed (want 44-char base64)", p.PubKey)
		}
		if !validIPv4(p.OverlayIP) {
			return fmt.Errorf("invalid overlay IP %q: validation failed (want IPv4)", p.OverlayIP)
		}
		argv = append(argv, "peer", p.PubKey, "allowed-ips", p.OverlayIP+"/32")
	}
	if !run(argv...) {
		return fmt.Errorf("wg set %d peers failed", len(peers))
	}
	return nil
}

// AllocateOverlayIP returns the lowest free host address in serverCIDR
// (feluccad's own overlay address, e.g. "10.100.0.1/16") that is not the
// network address, the broadcast address, the server's own IP, or in taken.
// Pure function: no shell. Errors when the CIDR is invalid (or not IPv4)
// or the subnet is exhausted.
func AllocateOverlayIP(serverCIDR string, taken []string) (string, error) {
	serverIP, ipnet, err := net.ParseCIDR(serverCIDR)
	if err != nil {
		return "", fmt.Errorf("invalid server CIDR %q: %w", serverCIDR, err)
	}
	server4 := serverIP.To4()
	net4 := ipnet.IP.To4()
	if server4 == nil || net4 == nil {
		return "", fmt.Errorf("invalid server CIDR %q: not IPv4", serverCIDR)
	}

	base := binary.BigEndian.Uint32(net4)
	mask := binary.BigEndian.Uint32(net.IP(ipnet.Mask).To4())
	broadcast := base | ^mask
	server := binary.BigEndian.Uint32(server4)

	used := make(map[uint32]bool, len(taken))
	for _, t := range taken {
		if ip := net.ParseIP(t); ip != nil {
			if ip4 := ip.To4(); ip4 != nil {
				used[binary.BigEndian.Uint32(ip4)] = true
			}
		}
	}

	for addr := base + 1; addr < broadcast; addr++ {
		if addr == server || used[addr] {
			continue
		}
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], addr)
		return net.IP(buf[:]).String(), nil
	}
	return "", fmt.Errorf("overlay subnet %s exhausted", serverCIDR)
}
