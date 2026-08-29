//! Guest networking for hearth-agent: Linux bridge (`hearth0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! Port of backend/src/agent/net.zig — shell-out to ip/nft/sysctl with
//! bare-then-`sudo -n` fallback exactly as in the Zig code.

use crate::ipalloc::Cidr;
use std::collections::HashMap;
use std::process::Command;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::OnceLock;
use tracing::{error, warn};

pub const BRIDGE_NAME: &str = "hearth0";

/// Guest CIDR in "base/prefix" form, recorded by `ensure_bridge`. Rebuilds need
/// it (conntrack scoping) but cannot take it as a parameter without changing a
/// signature the VM manager calls.
static GUEST_CIDR: OnceLock<String> = OnceLock::new();

/// Set once the isolation transaction has been applied AND br_netfilter is
/// confirmed. Read by `netfilter_ready`.
static ISOLATION_OK: AtomicBool = AtomicBool::new(false);

/// Set once every host-side fence around the bridge is confirmed: the guest
/// input filter, the L2 fence, and IPv6 off on the bridge. Read by
/// `netfilter_ready`.
static HOST_FILTER_OK: AtomicBool = AtomicBool::new(false);

/// The agent's own listen port, recorded by main before host networking comes
/// up, so the guest input filter can name it explicitly. 0 = unknown, in which
/// case the chain's default-drop still covers it.
static AGENT_PORT: AtomicU32 = AtomicU32::new(0);

/// Record the port the control API listens on. Call before `ensure_bridge`.
pub fn set_agent_port(port: u16) {
    AGENT_PORT.store(port as u32, Ordering::Relaxed);
}

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

/// Feed a whole ruleset to `nft -f -`. nft applies a file as ONE netlink
/// transaction, so the rules either all commit or none do — no observer ever
/// sees a chain that has been flushed but not yet repopulated. `run` cannot do
/// this because it has no stdin; the bare-then-`sudo -n` fallback is the same.
fn run_nft_file(ruleset: &str) -> bool {
    if run_nft_file_once(ruleset, false) { return true; }
    run_nft_file_once(ruleset, true)
}

fn run_nft_file_once(ruleset: &str, use_sudo: bool) -> bool {
    use std::io::Write;
    let mut cmd = if use_sudo {
        let mut c = Command::new("sudo");
        c.args(["-n", "nft", "-f", "-"]);
        c
    } else {
        let mut c = Command::new("nft");
        c.args(["-f", "-"]);
        c
    };
    let spawned = cmd
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn();
    let Ok(mut child) = spawned else { return false; };
    // Take + drop the pipe so nft sees EOF; holding it would deadlock wait().
    if let Some(mut stdin) = child.stdin.take() {
        if stdin.write_all(ruleset.as_bytes()).is_err() {
            drop(stdin);
            let _ = child.wait();
            return false;
        }
    }
    child.wait().map(|s| s.success()).unwrap_or(false)
}

/// Idempotently bring up the bridge with the CIDR's gateway address, enable
/// IPv4 forwarding, and install an nftables masquerade rule for egress.
/// Returns false if the bridge could not be configured (networking unusable).
pub fn ensure_bridge(cidr: Cidr) -> bool {
    let gw = crate::ipalloc::fmt_ip(cidr.gateway());
    let gw_cidr = format!("{}/{}", gw, cidr.prefix);
    let _ = GUEST_CIDR.set(format!(
        "{}/{}",
        crate::ipalloc::fmt_ip(cidr.net_base()),
        cidr.prefix
    ));

    // Create the bridge (ignore "exists"); bring it up + assign gateway IP.
    run(&["ip", "link", "add", BRIDGE_NAME, "type", "bridge"]);
    if !run(&["ip", "link", "set", BRIDGE_NAME, "up"]) { return false; }
    // addr add is idempotent-ish: errors if already present, which is fine.
    run(&["ip", "addr", "add", &gw_cidr, "dev", BRIDGE_NAME]);

    // Enable forwarding (best effort).
    enable_forwarding();

    // nftables masquerade for the CIDR.
    ensure_nat(cidr);

    // Host-side fences around the bridge. Their status is carried separately
    // from this function's return value: the bridge being usable and the bridge
    // being safe are different questions, and only `netfilter_ready` answers
    // the second one. Each is evaluated, none short-circuited — a partial
    // failure must still install everything else it can.
    let input_ok = ensure_guest_input_filter();
    let l2_ok = ensure_bridge_l2_filter();
    let v6_ok = ensure_ipv6_disabled(BRIDGE_NAME);
    if !v6_ok {
        eprintln!(
            "error: IPv6 could not be disabled on {} — the host is reachable from \
             guests over link-local IPv6, which no tenant rule covers.",
            BRIDGE_NAME
        );
        error!(
            iface = BRIDGE_NAME,
            "IPv6 could not be disabled on the guest bridge"
        );
    }
    HOST_FILTER_OK.store(input_ok && l2_ok && v6_ok, Ordering::Relaxed);
    true
}

/// Install the intra-bridge L2 fence. Returns false when it is not in force.
fn ensure_bridge_l2_filter() -> bool {
    let ok = run_nft_file(&bridge_l2_ruleset());
    if !ok {
        eprintln!(
            "error: the nft bridge-family L2 fence was rejected — GUESTS CAN REACH \
             EACH OTHER OVER IPv6 AND OTHER NON-IPv4 PROTOCOLS on this node."
        );
        error!(
            "the nft bridge-family L2 fence was rejected — GUESTS CAN REACH EACH \
             OTHER OVER IPv6 AND OTHER NON-IPv4 PROTOCOLS on this node."
        );
    }
    ok
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

/// Render the guest→host fence as one `nft -f -` file.
///
/// Guests route to the world through the bridge gateway, so every host service
/// bound to a wildcard address is one hop from inside any sandbox — and this
/// traffic lands on the INPUT hook, which the forward-chain tenant isolation
/// never sees. Default-drop everything arriving from the bridge and permit only
/// what a guest legitimately needs.
///
/// The chain policy stays `accept` deliberately: this is a shared hook, and a
/// drop policy here would cut the host's own SSH and control-plane traffic. The
/// fence is the trailing `iifname <bridge> drop` instead, which fails closed for
/// guest traffic only. No DHCP or DNS holes: guests get a static address from
/// the kernel command line and resolve through NAT'd egress, never from the
/// host. Pure for unit tests.
fn guest_input_ruleset(agent_port: Option<u16>) -> String {
    let mut s = String::new();
    s.push_str("add table ip hearth\n");
    s.push_str("add chain ip hearth input { type filter hook input priority 0 ; policy accept ; }\n");
    s.push_str("flush chain ip hearth input\n");
    s.push_str(&format!(
        "add rule ip hearth input iifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    // Named explicitly so the rule reads as the deny it is, and so it can never
    // be shadowed by a hole opened below it.
    if let Some(port) = agent_port {
        s.push_str(&format!(
            "add rule ip hearth input iifname \"{}\" tcp dport {} drop\n",
            BRIDGE_NAME, port
        ));
    }
    // ICMP stays open: guests ping the gateway to prove egress works, and path
    // MTU discovery depends on the error types.
    s.push_str(&format!(
        "add rule ip hearth input iifname \"{}\" icmp type {{ echo-request, echo-reply, destination-unreachable, time-exceeded, parameter-problem }} accept\n",
        BRIDGE_NAME
    ));
    // Replies to flows the host itself opened. A guest cannot forge this: a
    // dropped SYN never confirms a conntrack entry.
    s.push_str(&format!(
        "add rule ip hearth input iifname \"{}\" ct state established,related accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule ip hearth input iifname \"{}\" drop\n",
        BRIDGE_NAME
    ));
    s
}

/// Render the intra-bridge L2 fence as one `nft -f -` file.
///
/// The tenant ruleset lives in the `ip` family, so it only ever sees IPv4. Two
/// guests on one bridge exchange every other ethertype by pure L2 switching
/// with no `ip` hook involved at all — most importantly IPv6, which any guest
/// kernel autoconfigures on eth0 as a link-local address that no tenant rule
/// covers. Accept ARP (same-tenant IPv4 needs it) and IPv4 (the `ip` forward
/// chain decides those), drop the rest between bridge ports.
///
/// Policy stays `accept` for the same reason as the input chain — other bridges
/// on the host are not ours to police — and the fence is the trailing `drop`.
/// Pure for unit tests.
fn bridge_l2_ruleset() -> String {
    let mut s = String::new();
    s.push_str("add table bridge hearth\n");
    s.push_str("add chain bridge hearth forward { type filter hook forward priority -200 ; policy accept ; }\n");
    s.push_str("flush chain bridge hearth forward\n");
    s.push_str(&format!(
        "add rule bridge hearth forward meta ibrname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule bridge hearth forward meta obrname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str("add rule bridge hearth forward ether type arp accept\n");
    s.push_str("add rule bridge hearth forward ether type ip accept\n");
    s.push_str("add rule bridge hearth forward drop\n");
    s
}

/// Interface names we will interpolate into a command: `[A-Za-z0-9_-]`, within
/// the kernel's IFNAMSIZ. Nothing user-controlled reaches these helpers today
/// (the bridge is a constant, taps are built from a slot index), so this is the
/// same defense-in-depth as `valid_tenant` rather than the primary guard.
fn valid_ifname(name: &str) -> bool {
    !name.is_empty()
        && name.len() < 16
        && name.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

/// Set a sysctl to 1 and confirm it from /proc, escalating until it lands.
///
/// `sysctl -w` exits 0 on a key it could not write ("permission denied on key
/// …, ignoring"), so `run`'s bare-then-`sudo -n` escalation never fires on the
/// exit code alone — the unprivileged attempt "succeeds" and the sudo attempt
/// is never made. Only the read-back can tell whether the write landed, so each
/// attempt is checked against /proc rather than against its status.
fn set_sysctl_verified(key: &str, proc_path: &str) -> bool {
    let confirmed = || {
        std::fs::read_to_string(proc_path)
            .map(|s| s.trim() == "1")
            .unwrap_or(false)
    };
    if confirmed() { return true; }
    let arg = format!("{}=1", key);
    run_once(&["sysctl", "-w", &arg], false);
    if confirmed() { return true; }
    run_once(&["sysctl", "-w", &arg], true);
    if confirmed() { return true; }
    // Last resort for units with no sysctl binary; sh -c so one sudo covers
    // the redirect.
    run(&["sh", "-c", &format!("echo 1 > {}", proc_path)]);
    confirmed()
}

/// Turn IPv6 off on one interface, verifying the result rather than trusting
/// the write — same discipline as `ensure_bridge_netfilter`. Only this
/// interface: `all`/`default` would disable IPv6 host-wide and can take the
/// operator's own connectivity with it.
///
/// A kernel with no IPv6 at all reports success: there is nothing to disable
/// and no guest can speak it.
fn ensure_ipv6_disabled(iface: &str) -> bool {
    if !valid_ifname(iface) { return false; }
    if !std::path::Path::new("/proc/sys/net/ipv6").exists() { return true; }
    set_sysctl_verified(
        &format!("net.ipv6.conf.{}.disable_ipv6", iface),
        &format!("/proc/sys/net/ipv6/conf/{}/disable_ipv6", iface),
    )
}

/// Install the guest→host fence. Returns false when it is not in force.
fn ensure_guest_input_filter() -> bool {
    let port = match AGENT_PORT.load(Ordering::Relaxed) {
        0 => None,
        p => Some(p as u16),
    };
    let ok = run_nft_file(&guest_input_ruleset(port));
    if !ok {
        // Dual-emit: with this chain missing, the root control API is reachable
        // from inside every sandbox, and RUST_LOG can silence error!.
        eprintln!(
            "error: the nft guest input filter was rejected — HOST SERVICES ARE \
             REACHABLE FROM INSIDE EVERY SANDBOX on this node."
        );
        error!(
            "the nft guest input filter was rejected — HOST SERVICES ARE \
             REACHABLE FROM INSIDE EVERY SANDBOX on this node."
        );
    }
    ok
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
    // Before the link comes up, so the host never autoconfigures a link-local
    // address on a port a tenant's guest is on the other end of.
    ensure_ipv6_disabled(tap);
    run(&["ip", "link", "set", tap, "up"])
}

/// Tear down a tap device (best effort).
pub fn delete_tap(tap: &str) {
    run(&["ip", "link", "del", tap]);
}

// ---- per-tap anti-spoofing ----

/// A guest's L2 identity: six hex octets, lowercased, never empty.
///
/// This used to be a `&str` that callers were told to leave empty when they
/// "could not set a MAC", and two of them did — so the rendered chain had no
/// `ether saddr` rule at all and the guest behind that tap could emit frames
/// carrying ANOTHER tenant's MAC while still passing the IP and ARP pins. The
/// bridge then learned the victim's address on the attacker's port and
/// delivered its inbound traffic there. The type has no empty inhabitant, so
/// that call site cannot be written again: a path that has no MAC to pin has
/// no value of this type to pass, and the compiler says so.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct GuestMac(String);

impl GuestMac {
    /// Parse six hex octets into a MAC, or None.
    ///
    /// No path uses it today — every MAC in the agent is derived from a slot —
    /// but it is the only door from text into this type, so it stays here,
    /// validating and tested, rather than being re-invented as a cast at
    /// whichever call site first reads a MAC from disk or an API.
    #[allow(dead_code)]
    pub fn parse(mac: &str) -> Option<GuestMac> {
        if !valid_mac(mac) {
            return None;
        }
        Some(GuestMac(mac.to_ascii_lowercase()))
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl std::fmt::Display for GuestMac {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

/// Deterministic MAC for a guest slot: locally-administered (0x02) and unicast,
/// with 0x48 0x54 ("HT") marking it as a hearth tap and the slot index in the
/// low three octets.
///
/// Deterministic because the tenant ruleset keys on `ip saddr . ip daddr`, and
/// both of those are addresses the guest itself configures. A MAC the host
/// assigns and the tap enforces is a second key the guest does not choose. It
/// is derived from the slot rather than stored so that every path — cold boot,
/// start, wake, fork, startup adoption — pins the same value without having to
/// remember one across a snapshot.
pub fn guest_mac_for_slot(idx: u32) -> GuestMac {
    GuestMac(format!(
        "02:48:54:{:02x}:{:02x}:{:02x}",
        (idx >> 16) & 0xff,
        (idx >> 8) & 0xff,
        idx & 0xff
    ))
}

fn valid_mac(mac: &str) -> bool {
    let parts: Vec<&str> = mac.split(':').collect();
    parts.len() == 6
        && parts
            .iter()
            .all(|p| p.len() == 2 && p.bytes().all(|b| b.is_ascii_hexdigit()))
}

/// nft chain name for one tap's ingress filter. `valid_ifname` has already
/// bounded the input; this only maps it into the identifier charset.
fn antispoof_chain(tap: &str) -> String {
    let sanitized: String = tap
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '_' })
        .collect();
    format!("as_{}", sanitized)
}

/// Render one tap's ingress filter as an `nft -f -` file, or None if any input
/// fails validation — a half-specified pin is worse than none, because the
/// caller would read success.
///
/// The chain hangs off the tap itself, so it sees exactly the frames one guest
/// emits, before the bridge can learn anything from them. Policy `drop` with
/// explicit permits: the guest may send ARP that claims its own address and
/// IPv4 from its own address, nothing else. That closes both spoofing routes —
/// forging a same-tenant pair to reach another tenant, and ARPing for another
/// tenant's address to intercept their DNAT'd ingress.
///
/// Three pins, not two: an L3 identity alone is not enough. A frame whose
/// `ip saddr` is this guest's own address may still carry a VICTIM's MAC as its
/// `ether saddr` — the bridge learns from that field, moves the victim's FDB
/// entry onto this port, and unicast traffic destined for the victim (its
/// DNAT'd ingress included) is delivered here instead. The same forgery fits in
/// an ARP payload's sender hardware address, which is what every receiver
/// caches, so `arp saddr ether` is pinned alongside `arp saddr ip`. Pure for
/// unit tests.
fn antispoof_ruleset(tap: &str, guest_ip: &str, guest_mac: &GuestMac) -> Option<String> {
    if !valid_ifname(tap) { return None; }
    let ip: std::net::Ipv4Addr = guest_ip.parse().ok()?;
    // Validated and lowercased at construction; there is no empty case.
    let mac = guest_mac.as_str();

    let chain = antispoof_chain(tap);
    let mut s = String::new();
    s.push_str("add table netdev hearth\n");
    s.push_str(&format!(
        "add chain netdev hearth {} {{ type filter hook ingress device \"{}\" priority 0 ; policy drop ; }}\n",
        chain, tap
    ));
    s.push_str(&format!("flush chain netdev hearth {}\n", chain));
    s.push_str(&format!(
        "add rule netdev hearth {} ether saddr != {} drop\n",
        chain, mac
    ));
    s.push_str(&format!(
        "add rule netdev hearth {} arp operation {{ request, reply }} arp saddr ether != {} drop\n",
        chain, mac
    ));
    s.push_str(&format!(
        "add rule netdev hearth {} arp operation {{ request, reply }} arp saddr ip != {} drop\n",
        chain, ip
    ));
    s.push_str(&format!("add rule netdev hearth {} ether type arp accept\n", chain));
    s.push_str(&format!("add rule netdev hearth {} ip saddr {} accept\n", chain, ip));
    Some(s)
}

/// Pin `tap` to its allocated address and to its L2 identity, so the guest
/// behind it cannot claim another tenant's IP or MAC. Idempotent; one atomic
/// nft transaction.
///
/// `guest_mac` must be the MAC the NIC is actually configured with — a NIC on
/// any other address has every frame it sends dropped by its own tap. That is
/// the deliberate failure direction: an unpinnable guest goes dark rather than
/// reaching the bridge unfiltered. Returns false when the pin is NOT in force.
///
/// PRECONDITION: `tap` must already exist. The chain hangs off the device
/// (`hook ingress device "<tap>"`), and nft resolves that at load time, so
/// against a missing device it fails the WHOLE transaction with ENOENT rather
/// than just the chain line — this returns false and nothing is pinned. Call
/// it only via `vm::ensure_pinned_tap`, which creates the tap first; that is
/// the sole call site, and a unit test
/// (`test_tap_is_created_before_it_is_pinned_on_every_path`) fails if a second
/// one appears or the order inside the funnel is swapped.
pub fn ensure_tap_antispoof(tap: &str, guest_ip: &str, guest_mac: &GuestMac) -> bool {
    let Some(ruleset) = antispoof_ruleset(tap, guest_ip, guest_mac) else {
        warn!(tap = %tap, addr = %guest_ip, "tap antispoof skipped: tap, ip or mac failed validation");
        return false;
    };
    let ok = run_nft_file(&ruleset);
    if !ok {
        error!(
            tap = %tap,
            addr = %guest_ip,
            "tap antispoof not installed — this guest can claim another tenant's address"
        );
    }
    ok
}

/// Drop a tap's ingress filter (best effort), for when the slot is released.
pub fn clear_tap_antispoof(tap: &str) {
    if !valid_ifname(tap) { return; }
    run(&["nft", "delete", "chain", "netdev", "hearth", &antispoof_chain(tap)]);
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
    // Verify, don't assume: read the knob back. Either runner may have
    // "succeeded" without the key actually existing (module absent).
    let confirmed = set_sysctl_verified(
        "net.bridge.bridge-nf-call-iptables",
        "/proc/sys/net/bridge/bridge-nf-call-iptables",
    );
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

/// Allowed ordered-pair elements for the `tenant_pairs` set: every (a, b) where
/// a and b are distinct guests of the same valid tenant.
///
/// Both halves are re-rendered from a parsed `Ipv4Addr`, so a member whose IP
/// is not a bare IPv4 literal is dropped rather than concatenated into the
/// ruleset text — the same invariant `ingress_rule_parts` holds, and it matters
/// more here because the text is fed to `nft -f -` where a newline would be a
/// new command. Pure for unit tests.
fn tenant_pair_elements(members: &[(String, String)]) -> Vec<String> {
    let mut by_tenant: HashMap<&str, Vec<std::net::Ipv4Addr>> = HashMap::new();
    for (t, ip) in members {
        match (valid_tenant(t), ip.parse::<std::net::Ipv4Addr>()) {
            (true, Ok(addr)) => by_tenant.entry(t.as_str()).or_default().push(addr),
            (true, Err(_)) => warn!(addr = %ip, "isolation member with unparseable ip skipped"),
            // No/invalid tenant → joins no pair → isolated from every peer.
            (false, _) => {}
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
    elems
}

/// Render the whole cross-tenant isolation transaction as one `nft -f -` file.
///
/// Creation, both flushes, the pair elements and every rule live in one batch
/// on purpose: nft commits a file atomically, so the forward chain is never
/// observable in the flushed-but-not-yet-repopulated state that used to accept
/// all guest-to-guest traffic for the duration of seven `nft` processes.
///
/// The chain policy is `drop`: if the rules ever fail to load, the failure must
/// deny rather than permit. There is deliberately no
/// `ct state established,related accept` at the head — it would let a flow that
/// slipped through an older ruleset keep running for the life of its conntrack
/// entry (TCP established defaults to 5 days), and nothing here needs it: every
/// flow that is not guest-to-guest is accepted outright by the two `iifname` /
/// `oifname` rules. Pure for unit tests.
fn isolation_ruleset(members: &[(String, String)]) -> String {
    let mut s = String::new();
    s.push_str("add table ip hearth\n");
    s.push_str("add set ip hearth tenant_pairs { type ipv4_addr . ipv4_addr ; }\n");
    s.push_str("add chain ip hearth forward { type filter hook forward priority 0 ; policy drop ; }\n");
    s.push_str("flush chain ip hearth forward\n");
    s.push_str("flush set ip hearth tenant_pairs\n");
    let elems = tenant_pair_elements(members);
    if !elems.is_empty() {
        s.push_str(&format!(
            "add element ip hearth tenant_pairs {{ {} }}\n",
            elems.join(", ")
        ));
    }
    s.push_str(&format!(
        "add rule ip hearth forward iifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule ip hearth forward oifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str("add rule ip hearth forward ip saddr . ip daddr @tenant_pairs accept\n");
    s.push_str(&format!(
        "add rule ip hearth forward iifname \"{}\" oifname \"{}\" drop\n",
        BRIDGE_NAME, BRIDGE_NAME
    ));
    s
}

/// Drop conntrack entries for guest-to-guest flows, so nothing established
/// under an older ruleset outlives it. Scoped to guest↔guest: egress NAT state
/// is untouched, and a still-allowed same-tenant session is unaffected because
/// the pair rule is stateless and simply re-creates the entry. Best effort —
/// conntrack-tools is not a hard dependency of the agent.
fn flush_guest_conntrack() {
    let Some(cidr) = GUEST_CIDR.get() else { return; };
    run(&["conntrack", "-D", "-s", cidr, "-d", cidr]);
}

/// Whether cross-tenant isolation is actually in force on this node: the two
/// nft transactions applied, IPv6 is off on the bridge, and br_netfilter is
/// confirmed. Reads two atomics — cheap and safe to call repeatedly, including
/// from a health handler or a heartbeat.
///
/// False means every sandbox on this worker shares one flat network. That is a
/// reason to stop accepting placements, not a log line: the failure modes are
/// silent from the tenant's side and total from the victim's.
pub fn netfilter_ready() -> bool {
    HOST_FILTER_OK.load(Ordering::Relaxed) && ISOLATION_OK.load(Ordering::Relaxed)
}

/// Rebuild cross-tenant network isolation from the full member list. Idempotent,
/// and applied as a single atomic nft transaction.
///
/// `members` is `(tenant_id, guest_ip)` for every VM that currently holds an IP.
/// Guest-to-guest traffic on `hearth0` is dropped by default; only ordered IP
/// pairs that share a (valid) tenant are allowed, via a single concatenated
/// `ipv4_addr . ipv4_addr` set. VMs with no/invalid tenant join no pair and are
/// thus isolated from every peer. Egress and host↔guest traffic are unaffected.
///
/// Returns false when isolation is NOT in force — either the transaction failed
/// or br_netfilter is missing — so the caller can stop serving tenants instead
/// of running an unisolated node. See `netfilter_ready`.
pub fn rebuild_isolation(members: &[(String, String)]) -> bool {
    let br_ok = ensure_bridge_netfilter();
    let applied = run_nft_file(&isolation_ruleset(members));
    if !applied {
        // Dual-emit for the same reason as the br_netfilter diagnostic: this
        // is the rule that separates tenants, and RUST_LOG can silence error!.
        eprintln!(
            "error: the nft transaction for cross-tenant isolation was rejected — \
             CROSS-TENANT ISOLATION IS NOT ENFORCED on this node."
        );
        error!(
            "the nft transaction for cross-tenant isolation was rejected — \
             CROSS-TENANT ISOLATION IS NOT ENFORCED on this node."
        );
    } else {
        flush_guest_conntrack();
    }
    let ok = br_ok && applied;
    ISOLATION_OK.store(ok, Ordering::Relaxed);
    ok
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

    fn member(t: &str, ip: &str) -> (String, String) {
        (t.to_string(), ip.to_string())
    }

    #[test]
    fn test_isolation_ruleset_is_one_transaction_with_the_drop_rule() {
        // Was: flush, then five separate `nft add rule` processes with the drop
        // rule last — every guest-to-guest packet was accepted in between.
        // The flush and the drop rule must now be in the same batch.
        let rs = isolation_ruleset(&[member("tn-a", "10.231.0.2")]);
        let flush = rs.find("flush chain ip hearth forward").expect("chain flushed");
        let drop = rs
            .find(&format!(
                "add rule ip hearth forward iifname \"{}\" oifname \"{}\" drop",
                BRIDGE_NAME, BRIDGE_NAME
            ))
            .expect("drop rule present");
        assert!(flush < drop);
        assert!(rs.contains("flush set ip hearth tenant_pairs"));
    }

    #[test]
    fn test_isolation_ruleset_chain_policy_is_drop() {
        // Partial application must deny, not permit.
        let rs = isolation_ruleset(&[]);
        assert!(rs.contains("type filter hook forward priority 0 ; policy drop ;"));
        assert!(!rs.contains("policy accept"));
    }

    #[test]
    fn test_isolation_ruleset_has_no_conntrack_shortcut() {
        // A `ct state established,related accept` at the head of the chain kept
        // flows opened under an older ruleset alive for the life of their
        // conntrack entry.
        let rs = isolation_ruleset(&[member("tn-a", "10.231.0.2"), member("tn-a", "10.231.0.3")]);
        assert!(!rs.contains("ct state"));
    }

    #[test]
    fn test_isolation_ruleset_accepts_traffic_that_is_not_guest_to_guest() {
        let rs = isolation_ruleset(&[]);
        assert!(rs.contains(&format!("iifname != \"{}\" accept", BRIDGE_NAME)));
        assert!(rs.contains(&format!("oifname != \"{}\" accept", BRIDGE_NAME)));
        // With no members there is no set element to add at all.
        assert!(!rs.contains("add element"));
    }

    #[test]
    fn test_guest_input_ruleset_default_drops_guest_to_host() {
        // Was: no input chain existed at all, so a guest reached every host
        // service bound to the wildcard — the agent's own API included.
        let rs = guest_input_ruleset(Some(9090));
        let drop_all = rs
            .find(&format!("add rule ip hearth input iifname \"{}\" drop", BRIDGE_NAME))
            .expect("guest default-drop present");
        // Every permit must sit above the default-drop to have any effect.
        let icmp = rs.find("icmp type").expect("icmp permitted");
        let established = rs.find("ct state established,related").expect("replies permitted");
        assert!(icmp < drop_all);
        assert!(established < drop_all);
    }

    #[test]
    fn test_guest_input_ruleset_drops_the_agent_port_explicitly() {
        let rs = guest_input_ruleset(Some(9090));
        let port_drop = rs
            .find(&format!(
                "add rule ip hearth input iifname \"{}\" tcp dport 9090 drop",
                BRIDGE_NAME
            ))
            .expect("agent port dropped");
        // Ahead of every permit, so no later hole can shadow it.
        assert!(port_drop < rs.find("icmp type").unwrap());
        assert!(port_drop < rs.find("ct state established,related").unwrap());
        // A non-default port is honoured.
        assert!(guest_input_ruleset(Some(9443)).contains("tcp dport 9443 drop"));
        // Unknown port: the default-drop still fences the guest off.
        let unknown = guest_input_ruleset(None);
        assert!(!unknown.contains("tcp dport"));
        assert!(unknown.contains(&format!("input iifname \"{}\" drop", BRIDGE_NAME)));
    }

    #[test]
    fn test_guest_input_ruleset_leaves_non_guest_traffic_alone() {
        // A drop policy on the shared input hook would cut the host's own SSH
        // and control-plane traffic; the fence must be scoped to the bridge.
        let rs = guest_input_ruleset(Some(9090));
        assert!(rs.contains("type filter hook input priority 0 ; policy accept ;"));
        let exempt = rs
            .find(&format!("input iifname != \"{}\" accept", BRIDGE_NAME))
            .expect("non-guest traffic exempted");
        assert!(exempt < rs.find("tcp dport 9090 drop").unwrap());
        assert!(rs.contains("flush chain ip hearth input"));
    }

    #[test]
    fn test_bridge_l2_ruleset_drops_non_ipv4_between_guests() {
        // Was: the whole ruleset was IPv4-only, so guests reached each other
        // over autoconfigured link-local IPv6 with no netfilter hook involved.
        let rs = bridge_l2_ruleset();
        let drop_all = rs
            .find("add rule bridge hearth forward drop")
            .expect("L2 default-drop present");
        // ARP and IPv4 must still pass — same-tenant IPv4 depends on both, and
        // the ip forward chain is what polices IPv4.
        let arp = rs.find("ether type arp accept").expect("arp permitted");
        let ipv4 = rs.find("ether type ip accept").expect("ipv4 permitted");
        assert!(arp < drop_all);
        assert!(ipv4 < drop_all);
        // Nothing permits IPv6, so it falls to the drop.
        assert!(!rs.contains("ip6"));
    }

    #[test]
    fn test_bridge_l2_ruleset_only_polices_our_bridge() {
        let rs = bridge_l2_ruleset();
        assert!(rs.contains("policy accept"));
        let ibr = rs
            .find(&format!("meta ibrname != \"{}\" accept", BRIDGE_NAME))
            .expect("other bridges exempted");
        let obr = rs
            .find(&format!("meta obrname != \"{}\" accept", BRIDGE_NAME))
            .expect("other bridges exempted");
        let drop_all = rs.find("add rule bridge hearth forward drop").unwrap();
        assert!(ibr < drop_all);
        assert!(obr < drop_all);
        assert!(rs.contains("flush chain bridge hearth forward"));
    }

    #[test]
    fn test_guest_mac_for_slot_is_deterministic_and_locally_administered() {
        assert_eq!(guest_mac_for_slot(3).as_str(), "02:48:54:00:00:03");
        assert_eq!(guest_mac_for_slot(3), guest_mac_for_slot(3));
        assert_eq!(guest_mac_for_slot(258).as_str(), "02:48:54:00:01:02");
        assert_ne!(guest_mac_for_slot(1), guest_mac_for_slot(2));
        for idx in [0u32, 1, 253, 65_535, 16_777_215] {
            let mac = guest_mac_for_slot(idx);
            assert!(valid_mac(mac.as_str()), "{mac}");
            let first = u8::from_str_radix(&mac.as_str()[..2], 16).unwrap();
            assert_eq!(first & 0x02, 0x02, "locally administered");
            assert_eq!(first & 0x01, 0x00, "unicast");
        }
    }

    #[test]
    fn test_guest_mac_has_no_empty_inhabitant() {
        // Was: the pin took a `&str` and documented "" as the way to say "no
        // MAC" — wake_vm and fork both passed it, and the chain they rendered
        // had no `ether saddr` rule, so those guests could carry a victim's
        // MAC. There is no longer a value of this type that renders no rule:
        // `ensure_tap_antispoof(tap, ip, "")` does not compile, and the only
        // fallible constructor rejects the empty string outright.
        assert!(GuestMac::parse("").is_none());
        assert!(GuestMac::parse("not-a-mac").is_none());
        assert!(GuestMac::parse("02:48:54:00:00").is_none());
        assert!(GuestMac::parse("02:48:54:00:00:zz").is_none());
        assert!(GuestMac::parse("02:48:54:00:00:03\naccept").is_none());
        // Round-trips, lowercased, and never empty however it was built.
        assert_eq!(
            GuestMac::parse("02:48:54:AA:BB:CC").map(|m| m.as_str().to_string()),
            Some("02:48:54:aa:bb:cc".to_string())
        );
        for idx in [0u32, 1, 7, 253, 16_777_215] {
            assert!(!guest_mac_for_slot(idx).as_str().is_empty());
        }
    }

    #[test]
    fn test_antispoof_ruleset_pins_ip_mac_and_arp() {
        // Was: tap setup was create + enslave + up, and the pair rule keyed on
        // addresses the guest configures itself.
        let mac = guest_mac_for_slot(3);
        let rs = antispoof_ruleset("hth-3", "10.231.0.5", &mac).expect("valid tap pin");
        assert!(rs.contains("hook ingress device \"hth-3\" priority 0 ; policy drop ;"));
        assert!(rs.contains("ether saddr != 02:48:54:00:00:03 drop"));
        assert!(rs.contains("arp operation { request, reply } arp saddr ip != 10.231.0.5 drop"));
        assert!(rs.contains("ip saddr 10.231.0.5 accept"));
        // Only this guest's own ARP and IPv4 are permitted; the policy denies
        // everything else, IPv6 included.
        assert!(rs.contains("flush chain netdev hearth as_hth_3"));
        assert!(!rs.contains("accept\nadd rule netdev hearth as_hth_3 ip saddr != "));
    }

    #[test]
    fn test_antispoof_ruleset_always_pins_the_l2_identity() {
        // The L2 half of the pin used to be conditional, and the wake and fork
        // paths took the branch that omitted it. Every rendering must carry it:
        // with `ip saddr` pinned but `ether saddr` free, a guest still sends
        // frames under a victim's MAC, and the bridge moves that MAC's FDB
        // entry — and the victim's inbound traffic — onto this port.
        for (slot, ip) in [(0u32, "10.231.0.2"), (3, "10.231.0.5"), (253, "10.231.0.255")] {
            let mac = guest_mac_for_slot(slot);
            let rs = antispoof_ruleset(&tap_name(slot), ip, &mac).expect("valid tap pin");
            let l2 = rs
                .find(&format!("ether saddr != {} drop", mac))
                .unwrap_or_else(|| panic!("slot {slot} has no L2 pin: {rs}"));
            // Above the accepts, or it could never fire.
            assert!(l2 < rs.find("ether type arp accept").expect("arp accept"));
            assert!(l2 < rs.find(&format!("ip saddr {} accept", ip)).expect("ip accept"));
        }
    }

    #[test]
    fn test_antispoof_ruleset_pins_the_arp_sender_hardware_address() {
        // `ether saddr` alone leaves the ARP payload free: the sender hardware
        // address is the field every receiver caches, so a request claiming the
        // guest's own IP with a victim's MAC would still poison them.
        let mac = guest_mac_for_slot(3);
        let rs = antispoof_ruleset("hth-3", "10.231.0.5", &mac).expect("valid tap pin");
        let arp_l2 = rs
            .find("arp operation { request, reply } arp saddr ether != 02:48:54:00:00:03 drop")
            .expect("arp sender hardware address pinned");
        // The permit for ARP sits below both ARP pins, so neither can be
        // shadowed by it.
        let arp_accept = rs.find("ether type arp accept").expect("arp accept");
        let arp_l3 = rs.find("arp saddr ip != 10.231.0.5 drop").expect("arp ip pin");
        assert!(arp_l2 < arp_accept);
        assert!(arp_l3 < arp_accept);
    }

    #[test]
    fn test_antispoof_ruleset_rejects_bad_inputs() {
        // Nothing half-specified: the caller must not read success from a pin
        // that would not actually constrain the guest.
        let mac = guest_mac_for_slot(3);
        assert!(antispoof_ruleset("hth-3", "not-an-ip", &mac).is_none());
        assert!(antispoof_ruleset("hth-3", "10.231.0.5\naccept", &mac).is_none());
        assert!(antispoof_ruleset("hth-3; nft flush ruleset", "10.231.0.5", &mac).is_none());
        assert!(antispoof_ruleset("", "10.231.0.5", &mac).is_none());
        assert!(antispoof_ruleset("hth-3", "fe80::1", &mac).is_none());
        // A malformed MAC can no longer reach the renderer at all — it is
        // rejected where the value is built.
        assert!(GuestMac::parse("not-a-mac").is_none());
        assert!(GuestMac::parse("02:48:54:00:00:zz").is_none());
    }

    #[test]
    fn test_antispoof_chain_is_per_tap() {
        assert_eq!(antispoof_chain("hth-3"), "as_hth_3");
        assert_ne!(antispoof_chain("hth-3"), antispoof_chain("hth-4"));
    }

    #[test]
    fn test_ensure_ipv6_disabled_rejects_bad_ifname() {
        // The name reaches a sysctl key and a /proc path, so nothing outside
        // the interface charset may get that far.
        assert!(!ensure_ipv6_disabled("hearth0; rm -rf /"));
        assert!(!ensure_ipv6_disabled("../../../proc/sys/net/ipv4/ip_forward"));
        assert!(!ensure_ipv6_disabled(""));
    }

    #[test]
    fn test_valid_ifname() {
        assert!(valid_ifname("hearth0"));
        assert!(valid_ifname("hth-3"));
        assert!(!valid_ifname(""));
        assert!(!valid_ifname("hth-3; rm -rf /"));
        assert!(!valid_ifname("hth 3"));
        assert!(!valid_ifname("../../proc/sys"));
        assert!(!valid_ifname("aaaaaaaaaaaaaaaa"));
    }

    #[test]
    fn test_tenant_pair_elements_pairs_same_tenant_both_ways() {
        let elems = tenant_pair_elements(&[
            member("tn-a", "10.231.0.2"),
            member("tn-a", "10.231.0.3"),
            member("tn-b", "10.231.0.4"),
        ]);
        assert_eq!(elems.len(), 2);
        assert!(elems.contains(&"10.231.0.2 . 10.231.0.3".to_string()));
        assert!(elems.contains(&"10.231.0.3 . 10.231.0.2".to_string()));
        // Nothing crossing tenants, in either direction.
        assert!(!elems.iter().any(|e| e.contains("10.231.0.4")));
    }

    #[test]
    fn test_tenant_pair_elements_rejects_unparseable_ip() {
        // The elements are concatenated into an `nft -f -` file, where a
        // newline would start a new command.
        let elems = tenant_pair_elements(&[
            member("tn-a", "10.231.0.2"),
            member("tn-a", "10.231.0.3\nadd rule ip hearth forward accept"),
        ]);
        assert!(elems.is_empty());
        let elems2 = tenant_pair_elements(&[
            member("tn-a", "10.231.0.2"),
            member("tn-a", "not-an-ip"),
        ]);
        assert!(elems2.is_empty());
    }

    #[test]
    fn test_tenant_pair_elements_isolates_invalid_tenant() {
        let elems = tenant_pair_elements(&[
            member("", "10.231.0.2"),
            member("", "10.231.0.3"),
            member("tn a", "10.231.0.4"),
        ]);
        assert!(elems.is_empty());
    }

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
