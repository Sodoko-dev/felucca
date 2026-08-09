//! Guest networking for hearth-agent: Linux bridge (`hearth0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! Port of backend/src/agent/net.zig — shell-out to ip/nft/sysctl with
//! bare-then-`sudo -n` fallback exactly as in the Zig code.

use crate::ipalloc::Cidr;
use std::collections::HashMap;
use std::process::Command;
use tracing::{error, warn};

pub const BRIDGE_NAME: &str = "hearth0";

/// Run `argv`, trying unprivileged first, then `sudo -n` fallback.
/// Returns true if exit code 0. Shared with wg.rs (same shell-out idiom).
pub(crate) fn run(argv: &[&str]) -> bool {
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
///
/// Returns false — and screams — when the sysctl could not be confirmed on:
/// without it the tenant_pairs drop rule sees no bridged traffic and
/// cross-tenant isolation is silently OFF. Under the hardened systemd unit
/// (ProtectKernelModules=true) modprobe is expected to fail; the module must
/// be preloaded via modules-load.d (install.sh ships it).
fn ensure_bridge_netfilter() -> bool {
    run(&["modprobe", "br_netfilter"]);
    if !run(&["sysctl", "-w", "net.bridge.bridge-nf-call-iptables=1"]) {
        run(&["sh", "-c", "echo 1 > /proc/sys/net/bridge/bridge-nf-call-iptables"]);
    }
    // Verify, don't assume: read the knob back. Either runner may have
    // "succeeded" without the key actually existing (module absent).
    let confirmed = std::fs::read_to_string("/proc/sys/net/bridge/bridge-nf-call-iptables")
        .map(|s| s.trim() == "1")
        .unwrap_or(false);
    if !confirmed {
        // Dual-emit ON PURPOSE: this is the security-critical diagnostic of
        // the whole agent, and tracing's RUST_LOG filter can silence error!
        // events. The raw stderr line cannot be filtered away.
        eprintln!(
            "error: br_netfilter is not active (module missing and modprobe denied?) — \
             CROSS-TENANT ISOLATION IS NOT ENFORCED on this node. \
             Preload the module (modules-load.d/hearth.conf) and restart."
        );
        error!(
            "br_netfilter is not active (module missing and modprobe denied?) — \
             CROSS-TENANT ISOLATION IS NOT ENFORCED on this node. \
             Preload the module (modules-load.d/hearth.conf) and restart."
        );
    }
    confirmed
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
    // The ruleset is still built when br_netfilter is missing (routed traffic
    // is policed either way) — the helper has already logged the loud error.
    let _ = ensure_bridge_netfilter();

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

// ---- worker-side ingress DNAT (v4 P3.2) ----

/// Render the nft argv fragments for one ingress member:
/// `(dport, dnat_target)` for `tcp dport <dport> dnat to <dnat_target>`.
/// Ports are u16 by construction (our node-port allocator / API-validated
/// guest_port) and the IP must parse as a real IPv4 address — a member that
/// fails the parse never reaches an nft argv (same invariant as isolation:
/// nothing user-controlled is ever interpolated). Pure for unit tests.
fn ingress_rule_parts(node_port: u16, guest_ip: &str, guest_port: u16) -> Option<(String, String)> {
    let ip: std::net::Ipv4Addr = guest_ip.parse().ok()?;
    Some((node_port.to_string(), format!("{}:{}", ip, guest_port)))
}

/// Rebuild the ingress DNAT ruleset from the full member list. Idempotent
/// (flush + repopulate), mirroring `rebuild_isolation`.
///
/// `members` is `(node_port, guest_ip, guest_port)` for every expose of every
/// VM that currently holds an IP. node_ports come from our own 20000-range
/// allocator and guest IPs from ipalloc — never user strings.
pub fn rebuild_ingress(members: &[(u16, String, u16)]) {
    run(&["nft", "add", "table", "ip", "hearth"]);
    run(&["nft", "add", "chain", "ip", "hearth", "ingress",
          "{ type nat hook prerouting priority -100 ; }"]);
    run(&["nft", "flush", "chain", "ip", "hearth", "ingress"]);
    for (node_port, guest_ip, guest_port) in members {
        match ingress_rule_parts(*node_port, guest_ip, *guest_port) {
            Some((dport, target)) => {
                run(&["nft", "add", "rule", "ip", "hearth", "ingress",
                      "tcp", "dport", &dport, "dnat", "to", &target]);
            }
            // Should be unreachable (IPs come from ipalloc); skipping is the
            // safe failure mode — never feed an unparsed string to nft.
            None => warn!(addr = %guest_ip, "ingress member with unparseable ip skipped"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_ingress_rule_parts_formats_dport_and_target() {
        let (dport, target) = ingress_rule_parts(20000, "10.231.0.5", 8069).expect("valid member");
        assert_eq!(dport, "20000");
        assert_eq!(target, "10.231.0.5:8069");
    }

    #[test]
    fn test_ingress_rule_parts_port_bounds() {
        let (dport, target) = ingress_rule_parts(29999, "10.231.0.2", 65535).expect("valid member");
        assert_eq!(dport, "29999");
        assert_eq!(target, "10.231.0.2:65535");
    }

    #[test]
    fn test_ingress_rule_parts_rejects_non_ip() {
        // Anything that is not a bare IPv4 literal must be dropped, never formatted.
        assert!(ingress_rule_parts(20000, "10.231.0.5; drop table", 80).is_none());
        assert!(ingress_rule_parts(20000, "evil.example.com", 80).is_none());
        assert!(ingress_rule_parts(20000, "", 80).is_none());
        assert!(ingress_rule_parts(20000, "fe80::1", 80).is_none());
    }
}
