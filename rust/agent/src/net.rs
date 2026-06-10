//! Guest networking for hearth-agent: Linux bridge (`hearth0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! Port of backend/src/agent/net.zig — shell-out to ip/nft/sysctl with
//! bare-then-`sudo -n` fallback exactly as in the Zig code.

use crate::ipalloc::Cidr;
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
