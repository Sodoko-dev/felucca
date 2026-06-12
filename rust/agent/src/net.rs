//! Guest networking for hearth-agent: Linux bridge (`hearth0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! Port of backend/src/agent/net.zig — shell-out to ip/nft/sysctl with
//! bare-then-`sudo -n` fallback exactly as in the Zig code.

use crate::ipalloc::Cidr;
use std::collections::HashMap;
use std::process::Command;

pub const BRIDGE_NAME: &str = "hearth0";

/// Run `argv`, trying unprivileged first, then `sudo -n` fallback.
/// Returns true if exit code 0.
fn run(argv: &[&str]) -> bool {
    if run_once(argv, false) { return true; }
    run_once(argv, true)
}

fn run_once(argv: &[&str], use_sudo: bool) -> bool {
    if argv.is_empty() { return false; }
    let status = if use_sudo {
        Command::new("sudo")
            .arg("-n")
            .args(argv)
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
    } else {
        Command::new(argv[0])
            .args(&argv[1..])
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::null())
            .status()
    };
    status.map(|s| s.success()).unwrap_or(false)
}

/// Idempotently bring up the bridge with the CIDR's gateway address, enable
/// IPv4 forwarding, and install an nftables masquerade rule for egress.
/// Returns false if the bridge could not be configured (networking unusable).
pub fn ensure_bridge(cidr: Cidr) -> bool {
    let gw = crate::ipalloc::fmt_ip(cidr.gateway());
    let gw_cidr = format!("{}/{}", gw, cidr.prefix);

    // Create the bridge (ignore "exists"); bring it up + assign gateway IP.
    run(&["ip", "link", "add", BRIDGE_NAME, "type", "bridge"]);
    if !run(&["ip", "link", "set", BRIDGE_NAME, "up"]) { return false; }
    // addr add is idempotent-ish: errors if already present, which is fine.
    run(&["ip", "addr", "add", &gw_cidr, "dev", BRIDGE_NAME]);

    // Enable forwarding (best effort).
    enable_forwarding();

    // nftables masquerade for the CIDR.
    ensure_nat(cidr);
    true
}

fn enable_forwarding() {
    if run(&["sysctl", "-w", "net.ipv4.ip_forward=1"]) { return; }
    // Fallback: write via sh -c so a single sudo covers the redirect.
    run(&["sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"]);
}

fn ensure_nat(cidr: Cidr) {
    let base_ip = cidr.net_base() & cidr.mask();
    let base = crate::ipalloc::fmt_ip(base_ip);
    let src = format!("{}/{}", base, cidr.prefix);

    // Idempotent: ensure table+chain exist, flush then re-add the masquerade rule.
    run(&["nft", "add", "table", "ip", "hearth"]);
    run(&[
        "nft", "add", "chain", "ip", "hearth", "postrouting",
        "{ type nat hook postrouting priority 100 ; }",
    ]);
    run(&["nft", "flush", "chain", "ip", "hearth", "postrouting"]);
    run(&["nft", "add", "rule", "ip", "hearth", "postrouting", "ip", "saddr", &src, "masquerade"]);
}

/// Tap device name for a guest slot index, e.g. slot 3 -> "hth-3".
pub fn tap_name(idx: u32) -> String {
    format!("hth-{}", idx)
}

/// Idempotently create a tap device named `tap`, enslave to bridge, bring up.
/// Returns false on failure.
pub fn ensure_tap(tap: &str) -> bool {
    // tuntap add is not idempotent (errors if exists); ignore that error.
    run(&["ip", "tuntap", "add", "dev", tap, "mode", "tap"]);
    run(&["ip", "link", "set", tap, "master", BRIDGE_NAME]);
    run(&["ip", "link", "set", tap, "up"])
}

/// Tear down a tap device (best effort).
pub fn delete_tap(tap: &str) {
    run(&["ip", "link", "del", tap]);
}

// ---- cross-tenant isolation (v4 P1) ----

/// A tenant id is valid for grouping only if it is short and alphanumeric
/// (`[A-Za-z0-9_-]`, 1..=64). Note: tenant ids are NEVER interpolated into an
/// nft command — they are only Rust-side grouping keys (see `rebuild_isolation`),
/// so this check is defense-in-depth, not the primary injection guard. An
/// invalid id is treated as "no tenant" → the VM is isolated from all peers.
fn valid_tenant(t: &str) -> bool {
    !t.is_empty()
        && t.len() <= 64
        && t.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

/// Same-subnet guests on one bridge talk via L2 switching, which bypasses the
/// ip `forward` hook entirely. br_netfilter routes bridged IPv4 frames through
/// netfilter so the forward chain can police guest-to-guest traffic.
fn ensure_bridge_netfilter() {
    run(&["modprobe", "br_netfilter"]);
    if !run(&["sysctl", "-w", "net.bridge.bridge-nf-call-iptables=1"]) {
        run(&["sh", "-c", "echo 1 > /proc/sys/net/bridge/bridge-nf-call-iptables"]);
    }
}

/// Rebuild cross-tenant network isolation from the full member list. Idempotent
/// (flush + repopulate), mirroring `ensure_nat`'s flush-and-re-add idiom.
///
/// `members` is `(tenant_id, guest_ip)` for every VM that currently holds an IP.
/// Guest-to-guest traffic on `hearth0` is dropped by default; only ordered IP
/// pairs that share a (valid) tenant are allowed, via a single concatenated
/// `ipv4_addr . ipv4_addr` set. VMs with no/invalid tenant join no pair and are
/// thus isolated from every peer. Egress and host↔guest traffic are unaffected.
pub fn rebuild_isolation(members: &[(String, String)]) {
    ensure_bridge_netfilter();

    // Table/set/chain exist (idempotent), then flush the parts we own.
    run(&["nft", "add", "table", "ip", "hearth"]);
    run(&["nft", "add", "set", "ip", "hearth", "tenant_pairs",
          "{ type ipv4_addr . ipv4_addr ; }"]);
    run(&["nft", "add", "chain", "ip", "hearth", "forward",
          "{ type filter hook forward priority 0 ; policy accept ; }"]);
    run(&["nft", "flush", "chain", "ip", "hearth", "forward"]);
    run(&["nft", "flush", "set", "ip", "hearth", "tenant_pairs"]);

    // Group IPs by valid tenant; build the allowed ordered-pair elements.
    let mut by_tenant: HashMap<&str, Vec<&str>> = HashMap::new();
    for (t, ip) in members {
        if valid_tenant(t) {
            by_tenant.entry(t.as_str()).or_default().push(ip.as_str());
        }
    }
    let mut elems: Vec<String> = Vec::new();
    for ips in by_tenant.values() {
        for a in ips {
            for b in ips {
                if a != b {
                    elems.push(format!("{} . {}", a, b));
                }
            }
        }
    }
    if !elems.is_empty() {
        let set = format!("{{ {} }}", elems.join(", "));
        run(&["nft", "add", "element", "ip", "hearth", "tenant_pairs", &set]);
    }

    // Forward chain (policy accept; first match wins). Only intra-bridge
    // guest-to-guest is policed — everything else is accepted early.
    run(&["nft", "add", "rule", "ip", "hearth", "forward",
          "ct", "state", "established,related", "accept"]);
    run(&["nft", "add", "rule", "ip", "hearth", "forward",
          "iifname", "!=", BRIDGE_NAME, "accept"]);
    run(&["nft", "add", "rule", "ip", "hearth", "forward",
          "oifname", "!=", BRIDGE_NAME, "accept"]);
    run(&["nft", "add", "rule", "ip", "hearth", "forward",
          "ip", "saddr", ".", "ip", "daddr", "@tenant_pairs", "accept"]);
    run(&["nft", "add", "rule", "ip", "hearth", "forward",
          "iifname", BRIDGE_NAME, "oifname", BRIDGE_NAME, "drop"]);
}
