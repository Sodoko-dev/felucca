//! Guest networking for felucca-agent: Linux bridge (`felucca0`), per-VM tap
//! devices enslaved to it, and an nftables masquerade rule for egress NAT.
//!
//! Port of backend/src/agent/net.zig — shell-out to ip/nft/sysctl with
//! bare-then-`sudo -n` fallback exactly as in the Zig code.

use crate::ipalloc::Cidr;
use std::collections::HashMap;
use std::process::Command;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::OnceLock;
use std::time::{Duration, Instant};
use tracing::{error, warn};

pub const BRIDGE_NAME: &str = "felucca0";

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

/// How long any one `nft`, `ip`, `sysctl` or helper process may run before it
/// is killed and counted as failed (FLU-8 N11). These used to have no limit:
/// one `nft` stuck on a netlink lock held the rule lock — and with it every
/// create, delete, expose and the watch — for as long as it hung.
pub(crate) const COMMAND_LIMIT: Duration = Duration::from_secs(10);

/// What became of one child process.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Ran {
    /// It exited by itself within the limit.
    Done { ok: bool, stdout: String, stderr: String },
    /// It could not be started.
    Failed,
    /// It outran the limit and was killed. Kept apart from `Failed`: a
    /// command that hung is not tried a second time (`run`, `run_nft_file`) —
    /// as root the `sudo -n` attempt is the same command, and it would hold
    /// the rule lock for a second limit.
    TimedOut,
}

/// Run `argv` (with `stdin` fed to it, if any) and give it `limit` to exit.
/// Past the limit its whole process group is killed — with `sudo -n` in front
/// the real command is a grandchild, and killing sudo alone would leave it.
/// Output is drained on threads of their own: a listing longer than a pipe
/// buffer would otherwise block the child, and it would look hung.
pub(crate) fn run_limited(argv: &[&str], stdin: Option<&str>, limit: Duration) -> Ran {
    use std::io::{Read, Write};
    use std::os::unix::process::CommandExt;
    use std::process::Stdio;
    if argv.is_empty() { return Ran::Failed; }
    let mut cmd = Command::new(argv[0]);
    cmd.args(&argv[1..])
        .stdin(if stdin.is_some() { Stdio::piped() } else { Stdio::null() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .process_group(0);
    let Ok(mut child) = cmd.spawn() else { return Ran::Failed; };
    let pid = child.id() as i32;
    let drain = |r: Option<Box<dyn Read + Send>>| {
        r.map(|mut r| {
            std::thread::spawn(move || {
                let mut s = String::new();
                let _ = r.read_to_string(&mut s);
                s
            })
        })
    };
    let out = drain(child.stdout.take().map(|r| Box::new(r) as Box<dyn Read + Send>));
    let err = drain(child.stderr.take().map(|r| Box::new(r) as Box<dyn Read + Send>));
    // The writer drops the pipe when done, so the child sees EOF.
    let writer = match (stdin, child.stdin.take()) {
        (Some(text), Some(mut w)) => {
            let text = text.to_string();
            Some(std::thread::spawn(move || w.write_all(text.as_bytes()).is_ok()))
        }
        _ => None,
    };
    let deadline = Instant::now() + limit;
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break Some(s),
            Ok(None) if Instant::now() < deadline => std::thread::sleep(Duration::from_millis(2)),
            _ => break None,
        }
    };
    let Some(status) = status else {
        // Never pid 0 or 1: `kill(-0, …)` would signal the agent's own group.
        if pid > 1 {
            unsafe { libc::kill(-pid, libc::SIGKILL); }
        }
        let _ = child.kill();
        let _ = child.wait();
        warn!(cmd = %argv.join(" "), limit_ms = limit.as_millis() as u64, "command outran its time limit and was killed");
        return Ran::TimedOut;
    };
    let wrote = writer.map(|w| w.join().unwrap_or(false)).unwrap_or(true);
    let stdout = out.and_then(|t| t.join().ok()).unwrap_or_default();
    let stderr = err.and_then(|t| t.join().ok()).unwrap_or_default();
    Ran::Done { ok: status.success() && wrote, stdout, stderr }
}

/// `argv` with the `sudo -n` prefix when `use_sudo`.
fn with_sudo<'a>(argv: &[&'a str], use_sudo: bool) -> Vec<&'a str> {
    let mut v: Vec<&str> = if use_sudo { vec!["sudo", "-n"] } else { Vec::new() };
    v.extend_from_slice(argv);
    v
}

/// Run `argv`, trying unprivileged first, then `sudo -n` fallback.
/// Returns true if exit code 0. Shared with wg.rs (same shell-out idiom).
pub(crate) fn run(argv: &[&str]) -> bool {
    run_escalating(argv, None, COMMAND_LIMIT)
}

/// `argv` bare, then with `sudo -n` — unless the bare run succeeded or
/// outran `limit`: a hung command is not run again (see `Ran::TimedOut`).
fn run_escalating(argv: &[&str], stdin: Option<&str>, limit: Duration) -> bool {
    if argv.is_empty() { return false; }
    match run_limited(argv, stdin, limit) {
        Ran::Done { ok: true, .. } => true,
        Ran::TimedOut => false,
        _ => matches!(run_limited(&with_sudo(argv, true), stdin, limit), Ran::Done { ok: true, .. }),
    }
}

fn run_once(argv: &[&str], use_sudo: bool) -> Ran {
    if argv.is_empty() { return Ran::Failed; }
    run_limited(&with_sudo(argv, use_sudo), None, COMMAND_LIMIT)
}

/// Feed a whole ruleset to `nft -f -`. nft applies a file as ONE netlink
/// transaction, so the rules either all commit or none do — no observer ever
/// sees a chain that has been flushed but not yet repopulated. `run` cannot do
/// this because it has no stdin; the bare-then-`sudo -n` fallback is the same.
fn run_nft_file(ruleset: &str) -> bool {
    run_escalating(&["nft", "-f", "-"], Some(ruleset), COMMAND_LIMIT)
}

// ---- FLU-8: markers, and what the watch looks at ----

/// FNV-1a, 64 bit. Only has to be stable within one agent run: the watch
/// compares the kernel's marker with the hash this process last applied.
fn fnv1a64(s: &str) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in s.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h
}

/// Put the marker rule `counter comment "felucca:<hash>"` at the head of
/// `chain` in `ruleset`, the hash taken over the whole file without it (the
/// set elements of the forward chain included). It goes in as the chain's
/// first rule, so a flushed chain, a chain re-created empty and a chain that
/// somebody rebuilt with other content all list without it.
fn with_marker(ruleset: String, family_table_chain: &str) -> String {
    let marker = format!(
        "add rule {} counter comment \"felucca:{:016x}\"\n",
        family_table_chain,
        fnv1a64(&ruleset)
    );
    let first_rule = format!("add rule {} ", family_table_chain);
    let mut out = String::with_capacity(ruleset.len() + marker.len());
    let mut placed = false;
    for line in ruleset.split_inclusive('\n') {
        if !placed && line.starts_with(&first_rule) {
            out.push_str(&marker);
            placed = true;
        }
        out.push_str(line);
    }
    if !placed {
        out.push_str(&marker);
    }
    out
}

/// The marker hash a rendered ruleset carries, or None.
pub(crate) fn marker_of(ruleset: &str) -> Option<String> {
    let at = ruleset.find("comment \"felucca:")? + "comment \"felucca:".len();
    let end = ruleset[at..].find('"')?;
    Some(ruleset[at..at + end].to_string())
}

/// A chain the agent owns, in which table it lives, and what its failure
/// puts at stake.
#[derive(Clone, Debug, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub(crate) enum Chain {
    /// Egress masquerade: `ip felucca postrouting`.
    Nat,
    /// The guest-to-host fence: `ip felucca input`.
    Input,
    /// Cross-tenant isolation: `ip felucca forward` (and `tenant_pairs`).
    Forward,
    /// Ingress DNAT: `ip felucca ingress`.
    Ingress,
    /// The L2 fence: `bridge felucca forward`.
    L2,
    /// One tap's anti-spoof chain: `netdev felucca as_<tap>`.
    Tap(String),
}

impl Chain {
    pub(crate) fn family(&self) -> &'static str {
        match self {
            Chain::L2 => "bridge",
            Chain::Tap(_) => "netdev",
            _ => "ip",
        }
    }

    pub(crate) fn name(&self) -> String {
        match self {
            Chain::Nat => "postrouting".into(),
            Chain::Input => "input".into(),
            Chain::Forward | Chain::L2 => "forward".into(),
            Chain::Ingress => "ingress".into(),
            Chain::Tap(t) => antispoof_chain(t),
        }
    }

    /// A fence between guests or between a guest and the host: when it
    /// cannot be put back, the guest bridge goes down. NAT and ingress only
    /// ever close traffic by their absence.
    pub(crate) fn fences(&self) -> bool {
        matches!(self, Chain::Input | Chain::Forward | Chain::L2)
    }
}

/// One look at a table.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Listing {
    Listed(String),
    /// nft answered that the table does not exist.
    Missing,
    /// The command could not be run, timed out, or failed some other way.
    Unknown,
}

/// What a listing shows for one chain: None when the chain is not there,
/// Some(None) when its first rule is no marker, Some(Some(hash)) otherwise.
pub(crate) fn chain_marker(listing: &str, chain: &str) -> Option<Option<String>> {
    let body = chain_body(listing, chain)?;
    let first = body.iter().find(|l| !l.starts_with("type ") && !l.starts_with("policy "));
    Some(first.and_then(|l| {
        if !l.starts_with("counter ") { return None; }
        let at = l.find("comment \"felucca:")? + "comment \"felucca:".len();
        let rest = &l[at..];
        let end = rest.find('"')?;
        // Nothing after the comment: a marker is a counter and a comment.
        if rest[end + 1..].trim().is_empty() { Some(rest[..end].to_string()) } else { None }
    }))
}

/// The lines of `chain <name> { … }` in an `nft list table` listing, trimmed,
/// or None when the table has no such chain.
fn chain_body<'a>(listing: &'a str, chain: &str) -> Option<Vec<&'a str>> {
    let head = format!("chain {} {{", chain);
    let mut depth = 0i32;
    let mut inside: Option<(i32, Vec<&str>)> = None;
    for raw in listing.lines() {
        let line = raw.trim();
        let opens = line.matches('{').count() as i32;
        let closes = line.matches('}').count() as i32;
        if let Some((d, body)) = inside.as_mut() {
            if line == "}" && depth == *d + 1 {
                return inside.map(|(_, b)| b);
            }
            if !line.is_empty() {
                body.push(line);
            }
        } else if depth == 1 && line == head {
            inside = Some((depth, Vec::new()));
        }
        depth += opens - closes;
    }
    None
}

/// Whether a listing of `inet felucca_host` holds the host fence the unit's
/// start gate requires: in chain `input`, a rule matching the gate's
/// `grep -q "iifname .felucca0. drop"`. Same match, not the whole file.
pub(crate) fn host_fence_in(listing: &str) -> bool {
    let Some(body) = chain_body(listing, "input") else { return false; };
    // "iifname " + any + "felucca0" + any + " drop", anywhere in the line.
    let name = BRIDGE_NAME.as_bytes();
    let n = 8 + 1 + name.len() + 1 + 5;
    body.iter().any(|l| {
        let b = l.as_bytes();
        (0..b.len()).any(|i| {
            b.len() >= i + n
                && &b[i..i + 8] == b"iifname "
                && &b[i + 9..i + 9 + name.len()] == name
                && &b[i + 10 + name.len()..i + n] == b" drop"
        })
    })
}

/// One port of the guest bridge.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Port {
    pub(crate) name: String,
    pub(crate) carrier: bool,
}

/// Whether a link whose `ip -o link` flags are `flags` has carrier — a
/// process holds the tap. One place, on purpose: what decides "a VMM is on
/// the other end" is a lab question, and the answer must change here only.
pub(crate) fn has_carrier(flags: &str) -> bool {
    flags.split(',').any(|f| f == "LOWER_UP") && !flags.split(',').any(|f| f == "NO-CARRIER")
}

/// Parse `ip -o link show master <bridge>`:
/// `7: hth-0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 … master felucca0 …`.
pub(crate) fn parse_bridge_ports(out: &str) -> Vec<Port> {
    out.lines()
        .filter_map(|l| {
            let mut f = l.split_whitespace();
            f.next()?;
            let name = f.next()?.trim_end_matches(':');
            let name = name.split('@').next()?.to_string();
            let flags = f.next()?.trim_start_matches('<').trim_end_matches('>');
            Some(Port { name, carrier: has_carrier(flags) })
        })
        .collect()
}

/// One read of a switch file.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Switch {
    Value(String),
    /// The file does not exist (module unloaded, device gone): broken.
    Missing,
    Unknown,
}

/// The `br_netfilter` switch for IPv4 (`ensure_bridge_netfilter`).
pub(crate) const BR_NF_SWITCH: &str = "/proc/sys/net/bridge/bridge-nf-call-iptables";
/// Present only on a kernel with IPv6 (`ensure_ipv6_disabled`).
pub(crate) const IPV6_ROOT: &str = "/proc/sys/net/ipv6";

/// IPv6 off on the guest bridge (`ensure_ipv6_disabled(BRIDGE_NAME)`).
pub(crate) fn bridge_v6_switch() -> String {
    format!("/proc/sys/net/ipv6/conf/{}/disable_ipv6", BRIDGE_NAME)
}

/// What the rule rebuilds and the watch need from the host. `HostKernel` is
/// the real one; unit tests use an in-memory stand-in (`vm::rule_fake`).
pub(crate) trait Kernel: Send + Sync {
    /// Apply one ruleset as one `nft -f -` transaction.
    fn apply(&self, ruleset: &str) -> bool;
    fn list_table(&self, family: &str, table: &str) -> Listing;
    /// The ports of the guest bridge, or None when they could not be listed.
    fn bridge_ports(&self) -> Option<Vec<Port>>;
    fn read_switch(&self, path: &str) -> Switch;
    fn write_switch(&self, path: &str, value: &str) -> bool;
    fn path_exists(&self, path: &str) -> bool;
    fn set_link(&self, dev: &str, up: bool) -> bool;
    /// Whether `dev` exists; None when that could not be found out.
    fn link_exists(&self, dev: &str) -> Option<bool>;
    /// The permanent blackhole route for the guest network, worst metric.
    fn blackhole_route(&self, cidr: &str) -> bool;
}

/// `Kernel` on the host: nft, ip and /proc, every child under `COMMAND_LIMIT`.
pub(crate) struct HostKernel;

impl HostKernel {
    /// Run bare, then with `sudo -n` unless the bare run gave a definite
    /// answer (`definite` decides from stderr) or timed out.
    fn ask(argv: &[&str], definite: impl Fn(&str) -> bool) -> Ran {
        let bare = run_limited(argv, None, COMMAND_LIMIT);
        match &bare {
            Ran::Done { ok: true, .. } | Ran::TimedOut => return bare,
            Ran::Done { ok: false, stderr, .. } if definite(stderr) => return bare,
            _ => {}
        }
        run_limited(&with_sudo(argv, true), None, COMMAND_LIMIT)
    }
}

/// The answer nft gives for a table that does not exist (lab: exit 1,
/// `Error: No such file or directory`).
fn no_such_table(stderr: &str) -> bool {
    stderr.contains("No such file or directory")
}

/// What one `nft list table` run says. Only nft's own "no such table" is
/// Missing; a command that could not run, timed out or failed any other way
/// is Unknown — never taken for missing.
fn listing_of(ran: Ran) -> Listing {
    match ran {
        Ran::Done { ok: true, stdout, .. } => Listing::Listed(stdout),
        Ran::Done { ok: false, stderr, .. } if no_such_table(&stderr) => Listing::Missing,
        _ => Listing::Unknown,
    }
}

impl Kernel for HostKernel {
    fn apply(&self, ruleset: &str) -> bool {
        run_nft_file(ruleset)
    }
    fn list_table(&self, family: &str, table: &str) -> Listing {
        listing_of(HostKernel::ask(&["nft", "list", "table", family, table], no_such_table))
    }
    fn bridge_ports(&self) -> Option<Vec<Port>> {
        match HostKernel::ask(&["ip", "-o", "link", "show", "master", BRIDGE_NAME], |_| false) {
            Ran::Done { ok: true, stdout, .. } => Some(parse_bridge_ports(&stdout)),
            _ => None,
        }
    }
    fn read_switch(&self, path: &str) -> Switch {
        match std::fs::read_to_string(path) {
            Ok(s) => Switch::Value(s.trim().to_string()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Switch::Missing,
            Err(_) => Switch::Unknown,
        }
    }
    fn write_switch(&self, path: &str, value: &str) -> bool {
        if std::fs::write(path, value).is_ok() { return true; }
        // As `set_sysctl_verified`'s last resort: one sudo covers the redirect.
        run(&["sh", "-c", &format!("echo {} > {}", value, path)])
    }
    fn path_exists(&self, path: &str) -> bool {
        std::path::Path::new(path).exists()
    }
    fn set_link(&self, dev: &str, up: bool) -> bool {
        if !valid_ifname(dev) { return false; }
        run(&["ip", "link", "set", dev, if up { "up" } else { "down" }])
    }
    fn link_exists(&self, dev: &str) -> Option<bool> {
        if !valid_ifname(dev) { return Some(false); }
        let gone = |e: &str| e.contains("does not exist");
        match HostKernel::ask(&["ip", "link", "show", "dev", dev], gone) {
            Ran::Done { ok: true, .. } => Some(true),
            Ran::Done { ok: false, stderr, .. } if gone(&stderr) => Some(false),
            _ => None,
        }
    }
    fn blackhole_route(&self, cidr: &str) -> bool {
        // `replace`, so a restart finds it in place. The worst metric: the
        // bridge's own connected route always wins while the bridge is up.
        run(&["ip", "route", "replace", "blackhole", cidr, "metric", "4294967295"])
    }
}

/// Idempotently create the bridge with the CIDR's gateway address, enable
/// IPv4 forwarding and turn IPv6 off on it. Returns (the bridge is usable,
/// IPv6 is off on it). NAT and the two host-side fences are built next, by
/// the manager, one transaction each (`vm::Manager::setup_host`), so that
/// it knows the marker of each.
///
/// This does NOT bring the bridge up — `raise_bridge` does, and the caller
/// runs it only once `netfilter_ready` holds. On an ordinary restart the
/// bridge is up already and stays up, so guests notice nothing. But a bridge
/// that a previous start took down because the fences could not be loaded
/// (`quarantine_bridge`) has to stay down until they are: raising it here put
/// every guest back on an unfenced network for the length of each start-up
/// attempt, once every restart interval.
pub fn ensure_bridge(cidr: Cidr) -> (bool, bool) {
    let gw = crate::ipalloc::fmt_ip(cidr.gateway());
    let gw_cidr = format!("{}/{}", gw, cidr.prefix);
    let _ = GUEST_CIDR.set(guest_net(cidr));

    // Create the bridge (ignore "exists") and assign the gateway IP.
    run(&["ip", "link", "add", BRIDGE_NAME, "type", "bridge"]);
    if !run(&["ip", "link", "show", "dev", BRIDGE_NAME]) { return (false, false); }
    // addr add is idempotent-ish: errors if already present, which is fine.
    run(&["ip", "addr", "add", &gw_cidr, "dev", BRIDGE_NAME]);

    // Enable forwarding (best effort).
    enable_forwarding();

    // The fences' status is carried separately from the bridge being usable:
    // the bridge being usable and the bridge being safe are different
    // questions, and only `netfilter_ready` answers the second one.
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
    (true, v6_ok)
}

/// Record start-up's verdict on the host-side fences (guest input filter, L2
/// fence, IPv6 off on the bridge). Read by `netfilter_ready`.
pub fn set_host_filter_ok(ok: bool) {
    HOST_FILTER_OK.store(ok, Ordering::Relaxed);
}

/// The guest network, "base/prefix".
pub(crate) fn guest_net(cidr: Cidr) -> String {
    format!("{}/{}", crate::ipalloc::fmt_ip(cidr.net_base() & cidr.mask()), cidr.prefix)
}

/// The ruleset of one of the three chains host set-up builds (NAT, the guest
/// input filter, the L2 fence), or None for any other chain.
pub(crate) fn host_chain_ruleset(chain: &Chain, cidr: Cidr) -> Option<String> {
    match chain {
        Chain::Nat => Some(nat_ruleset(cidr)),
        Chain::Input => Some(guest_input_ruleset(match AGENT_PORT.load(Ordering::Relaxed) {
            0 => None,
            p => Some(p as u16),
        })),
        Chain::L2 => Some(bridge_l2_ruleset()),
        _ => None,
    }
}

/// Say, loudly, that one of the host set-up chains was rejected.
pub(crate) fn report_rejected(chain: &Chain) {
    let what = match chain {
        // Dual-emit: with this chain missing, the root control API is
        // reachable from inside every sandbox, and RUST_LOG can silence error!.
        Chain::Input => "the nft guest input filter was rejected — HOST SERVICES ARE \
                         REACHABLE FROM INSIDE EVERY SANDBOX on this node.",
        Chain::L2 => "the nft bridge-family L2 fence was rejected — GUESTS CAN REACH \
                      EACH OTHER OVER IPv6 AND OTHER NON-IPv4 PROTOCOLS on this node.",
        Chain::Nat => "the nft transaction for egress NAT was rejected — guests have no \
                       egress on this node.",
        _ => return,
    };
    eprintln!("error: {}", what);
    error!("{}", what);
}

fn enable_forwarding() {
    if run(&["sysctl", "-w", "net.ipv4.ip_forward=1"]) { return; }
    // Fallback: write via sh -c so a single sudo covers the redirect.
    run(&["sh", "-c", "echo 1 > /proc/sys/net/ipv4/ip_forward"]);
}

/// Render egress NAT as one `nft -f -` file. It used to be four `nft`
/// processes whose results were all ignored: between the flush and the add
/// guests had no egress, and a rejected add left none at all, unnoticed.
/// Pure for unit tests.
fn nat_ruleset(cidr: Cidr) -> String {
    let mut s = String::new();
    s.push_str("add table ip felucca\n");
    s.push_str("add chain ip felucca postrouting { type nat hook postrouting priority 100 ; }\n");
    s.push_str("flush chain ip felucca postrouting\n");
    s.push_str(&format!(
        "add rule ip felucca postrouting ip saddr {} masquerade\n",
        guest_net(cidr)
    ));
    with_marker(s, "ip felucca postrouting")
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
    s.push_str("add table ip felucca\n");
    s.push_str("add chain ip felucca input { type filter hook input priority 0 ; policy accept ; }\n");
    s.push_str("flush chain ip felucca input\n");
    s.push_str(&format!(
        "add rule ip felucca input iifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    // Named explicitly so the rule reads as the deny it is, and so it can never
    // be shadowed by a hole opened below it.
    if let Some(port) = agent_port {
        s.push_str(&format!(
            "add rule ip felucca input iifname \"{}\" tcp dport {} drop\n",
            BRIDGE_NAME, port
        ));
    }
    // ICMP stays open: guests ping the gateway to prove egress works, and path
    // MTU discovery depends on the error types.
    s.push_str(&format!(
        "add rule ip felucca input iifname \"{}\" icmp type {{ echo-request, echo-reply, destination-unreachable, time-exceeded, parameter-problem }} accept\n",
        BRIDGE_NAME
    ));
    // Replies to flows the host itself opened. A guest cannot forge this: a
    // dropped SYN never confirms a conntrack entry.
    s.push_str(&format!(
        "add rule ip felucca input iifname \"{}\" ct state established,related accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule ip felucca input iifname \"{}\" drop\n",
        BRIDGE_NAME
    ));
    with_marker(s, "ip felucca input")
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
    s.push_str("add table bridge felucca\n");
    s.push_str("add chain bridge felucca forward { type filter hook forward priority -200 ; policy accept ; }\n");
    s.push_str("flush chain bridge felucca forward\n");
    s.push_str(&format!(
        "add rule bridge felucca forward meta ibrname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule bridge felucca forward meta obrname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str("add rule bridge felucca forward ether type arp accept\n");
    s.push_str("add rule bridge felucca forward ether type ip accept\n");
    s.push_str("add rule bridge felucca forward drop\n");
    with_marker(s, "bridge felucca forward")
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
    // A hung sysctl is not run again (`Ran::TimedOut`).
    if run_once(&["sysctl", "-w", &arg], false) == Ran::TimedOut { return confirmed(); }
    if confirmed() { return true; }
    if run_once(&["sysctl", "-w", &arg], true) == Ran::TimedOut { return confirmed(); }
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

/// Take a link down without removing it (best effort). A down port forwards
/// nothing, and — unlike a deleted one — whatever holds it open keeps a device
/// that `ip link set <dev> up` makes work again.
pub fn set_link_down(dev: &str) {
    run(&["ip", "link", "set", dev, "down"]);
}

/// Bring the guest bridge up. Only ever called once the fences around it are
/// in force (`netfilter_ready`); see `ensure_bridge`. A no-op on a bridge that
/// is up already.
pub fn raise_bridge() -> bool {
    run(&["ip", "link", "set", BRIDGE_NAME, "up"])
}

/// Stop all guest traffic on this node, reversibly: take the guest bridge
/// down. A down bridge forwards nothing between its ports and answers nothing
/// on the gateway address; `raise_bridge` brings it back up at the first agent
/// start that gets the fences loaded.
///
/// This is what "refusing to serve tenants" has to do now that Firecrackers
/// outlive the agent (ADR-0011). When the agent exits because isolation is not
/// in force, the guests that were already running keep running; before, the
/// same exit killed them. They must not stay on a network the agent could not
/// fence.
pub fn quarantine_bridge() {
    set_link_down(BRIDGE_NAME);
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
/// with 0x48 0x54 ("HT") marking it as a felucca tap and the slot index in the
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
pub(crate) fn antispoof_ruleset(tap: &str, guest_ip: &str, guest_mac: &GuestMac) -> Option<String> {
    if !valid_ifname(tap) { return None; }
    let ip: std::net::Ipv4Addr = guest_ip.parse().ok()?;
    // Validated and lowercased at construction; there is no empty case.
    let mac = guest_mac.as_str();

    let chain = antispoof_chain(tap);
    let mut s = String::new();
    s.push_str("add table netdev felucca\n");
    s.push_str(&format!(
        "add chain netdev felucca {} {{ type filter hook ingress device \"{}\" priority 0 ; policy drop ; }}\n",
        chain, tap
    ));
    s.push_str(&format!("flush chain netdev felucca {}\n", chain));
    s.push_str(&format!(
        "add rule netdev felucca {} ether saddr != {} drop\n",
        chain, mac
    ));
    s.push_str(&format!(
        "add rule netdev felucca {} arp operation {{ request, reply }} arp saddr ether != {} drop\n",
        chain, mac
    ));
    s.push_str(&format!(
        "add rule netdev felucca {} arp operation {{ request, reply }} arp saddr ip != {} drop\n",
        chain, ip
    ));
    s.push_str(&format!("add rule netdev felucca {} ether type arp accept\n", chain));
    s.push_str(&format!("add rule netdev felucca {} ip saddr {} accept\n", chain, ip));
    let at = format!("netdev felucca {}", chain);
    Some(with_marker(s, &at))
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
    run(&["nft", "delete", "chain", "netdev", "felucca", &antispoof_chain(tap)]);
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
             Preload the module (modules-load.d/felucca.conf) and restart."
        );
        error!(
            "br_netfilter is not active (module missing and modprobe denied?) — \
             CROSS-TENANT ISOLATION IS NOT ENFORCED on this node. \
             Preload the module (modules-load.d/felucca.conf) and restart."
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
    let mut pairs: Vec<(std::net::Ipv4Addr, std::net::Ipv4Addr)> = Vec::new();
    for ips in by_tenant.values() {
        for a in ips {
            for b in ips {
                if a != b {
                    pairs.push((*a, *b));
                }
            }
        }
    }
    // Sorted: the members come out of a HashMap, and the forward chain's
    // marker is a hash over this text — it must not depend on that order.
    pairs.sort();
    pairs.into_iter().map(|(a, b)| format!("{} . {}", a, b)).collect()
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
pub(crate) fn isolation_ruleset(members: &[(String, String)]) -> String {
    let mut s = String::new();
    s.push_str("add table ip felucca\n");
    s.push_str("add set ip felucca tenant_pairs { type ipv4_addr . ipv4_addr ; }\n");
    s.push_str("add chain ip felucca forward { type filter hook forward priority 0 ; policy drop ; }\n");
    s.push_str("flush chain ip felucca forward\n");
    s.push_str("flush set ip felucca tenant_pairs\n");
    let elems = tenant_pair_elements(members);
    if !elems.is_empty() {
        s.push_str(&format!(
            "add element ip felucca tenant_pairs {{ {} }}\n",
            elems.join(", ")
        ));
    }
    s.push_str(&format!(
        "add rule ip felucca forward iifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str(&format!(
        "add rule ip felucca forward oifname != \"{}\" accept\n",
        BRIDGE_NAME
    ));
    s.push_str("add rule ip felucca forward ip saddr . ip daddr @tenant_pairs accept\n");
    s.push_str(&format!(
        "add rule ip felucca forward iifname \"{}\" oifname \"{}\" drop\n",
        BRIDGE_NAME, BRIDGE_NAME
    ));
    with_marker(s, "ip felucca forward")
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
/// Guest-to-guest traffic on `felucca0` is dropped by default; only ordered IP
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

/// Render the whole ingress DNAT chain as one `nft -f -` file: creation, the
/// flush and one rule per member in a single batch.
///
/// It used to be a flush and then one `nft add rule` process per expose, with
/// every result ignored: between the flush and the last add some node ports
/// had no rule, and a rule that failed to load was simply missing while the
/// expose that asked for it was answered with success. nft commits a file
/// atomically, so the chain now goes from the old rules to the new ones in one
/// step, or stays as it was. Pure for unit tests.
pub(crate) fn ingress_ruleset(members: &[(u16, String, u16)]) -> String {
    let mut s = String::new();
    s.push_str("add table ip felucca\n");
    s.push_str("add chain ip felucca ingress { type nat hook prerouting priority -100 ; }\n");
    s.push_str("flush chain ip felucca ingress\n");
    for (node_port, guest_ip, guest_port) in members {
        match ingress_rule_parts(*node_port, guest_ip, *guest_port) {
            Some((dport, target)) => s.push_str(&format!(
                "add rule ip felucca ingress tcp dport {} dnat to {}\n",
                dport, target
            )),
            // Should be unreachable (IPs come from ipalloc); skipping is the
            // safe failure mode — never feed an unparsed string to nft.
            None => warn!(addr = %guest_ip, "ingress member with unparseable ip skipped"),
        }
    }
    with_marker(s, "ip felucca ingress")
}

/// Rebuild the ingress DNAT ruleset from the full member list. Idempotent
/// (flush + repopulate), applied as a single atomic nft transaction like
/// `rebuild_isolation`.
///
/// `members` is `(node_port, guest_ip, guest_port)` for every expose of every
/// VM that currently holds an IP. node_ports come from our own 20000-range
/// allocator and guest IPs from ipalloc — never user strings.
///
/// Returns false when the transaction was rejected: the kernel then still has
/// the chain as it was before this call.
pub fn rebuild_ingress(members: &[(u16, String, u16)]) -> bool {
    let ok = run_nft_file(&ingress_ruleset(members));
    if !ok {
        error!("the nft transaction for ingress DNAT was rejected — the node ports keep their previous rules");
    }
    ok
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
        let flush = rs.find("flush chain ip felucca forward").expect("chain flushed");
        let drop = rs
            .find(&format!(
                "add rule ip felucca forward iifname \"{}\" oifname \"{}\" drop",
                BRIDGE_NAME, BRIDGE_NAME
            ))
            .expect("drop rule present");
        assert!(flush < drop);
        assert!(rs.contains("flush set ip felucca tenant_pairs"));
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
            .find(&format!("add rule ip felucca input iifname \"{}\" drop", BRIDGE_NAME))
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
                "add rule ip felucca input iifname \"{}\" tcp dport 9090 drop",
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
        assert!(rs.contains("flush chain ip felucca input"));
    }

    #[test]
    fn test_bridge_l2_ruleset_drops_non_ipv4_between_guests() {
        // Was: the whole ruleset was IPv4-only, so guests reached each other
        // over autoconfigured link-local IPv6 with no netfilter hook involved.
        let rs = bridge_l2_ruleset();
        let drop_all = rs
            .find("add rule bridge felucca forward drop")
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
        let drop_all = rs.find("add rule bridge felucca forward drop").unwrap();
        assert!(ibr < drop_all);
        assert!(obr < drop_all);
        assert!(rs.contains("flush chain bridge felucca forward"));
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
        assert!(rs.contains("flush chain netdev felucca as_hth_3"));
        assert!(!rs.contains("accept\nadd rule netdev felucca as_hth_3 ip saddr != "));
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
        assert!(!ensure_ipv6_disabled("felucca0; rm -rf /"));
        assert!(!ensure_ipv6_disabled("../../../proc/sys/net/ipv4/ip_forward"));
        assert!(!ensure_ipv6_disabled(""));
    }

    #[test]
    fn test_valid_ifname() {
        assert!(valid_ifname("felucca0"));
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
            member("tn-a", "10.231.0.3\nadd rule ip felucca forward accept"),
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

    /// `rs` without its marker rule for `chain` ("<family> <table> <chain>"),
    /// after checking that the marker is that chain's first rule and that its
    /// hash is the one over the rest of the file.
    fn unmarked(rs: &str, chain: &str) -> String {
        let first = rs
            .lines()
            .find(|l| l.starts_with(&format!("add rule {} ", chain)))
            .unwrap_or_else(|| panic!("no rule for {chain}: {rs}"));
        let hash = marker_of(rs).expect("a marker");
        assert_eq!(first, format!("add rule {} counter comment \"felucca:{}\"", chain, hash), "{rs}");
        assert_eq!(rs.matches("comment \"felucca:").count(), 1, "{rs}");
        let rest = rs.replacen(&format!("{}\n", first), "", 1);
        assert_eq!(hash, format!("{:016x}", fnv1a64(&rest)), "the hash is over the rest of the file");
        rest
    }

    #[test]
    fn test_ingress_ruleset_with_no_members_is_one_transaction_that_empties_the_chain() {
        // Was: `nft flush chain` and then one `nft add rule` process per
        // expose, each result ignored. With nothing exposed the file must
        // still create the chain and flush it, so stale rules go away.
        let rs = ingress_ruleset(&[]);
        assert_eq!(
            unmarked(&rs, "ip felucca ingress"),
            "add table ip felucca\n\
             add chain ip felucca ingress { type nat hook prerouting priority -100 ; }\n\
             flush chain ip felucca ingress\n"
        );
        // FLU-8: an empty chain still carries its marker, last.
        assert!(rs.lines().last().unwrap().starts_with("add rule ip felucca ingress counter comment \"felucca:"));
    }

    #[test]
    fn test_ingress_ruleset_with_one_member() {
        assert_eq!(
            unmarked(&ingress_ruleset(&[(20000, "10.231.0.5".to_string(), 8069)]), "ip felucca ingress"),
            "add table ip felucca\n\
             add chain ip felucca ingress { type nat hook prerouting priority -100 ; }\n\
             flush chain ip felucca ingress\n\
             add rule ip felucca ingress tcp dport 20000 dnat to 10.231.0.5:8069\n"
        );
    }

    #[test]
    fn test_ingress_ruleset_with_several_members_keeps_the_flush_inside_the_batch() {
        let rs = ingress_ruleset(&[
            (20000, "10.231.0.5".to_string(), 8069),
            (24711, "10.231.0.6".to_string(), 80),
            (29999, "10.231.0.5".to_string(), 65535),
        ]);
        assert_eq!(
            unmarked(&rs, "ip felucca ingress"),
            "add table ip felucca\n\
             add chain ip felucca ingress { type nat hook prerouting priority -100 ; }\n\
             flush chain ip felucca ingress\n\
             add rule ip felucca ingress tcp dport 20000 dnat to 10.231.0.5:8069\n\
             add rule ip felucca ingress tcp dport 24711 dnat to 10.231.0.6:80\n\
             add rule ip felucca ingress tcp dport 29999 dnat to 10.231.0.5:65535\n"
        );
        // One flush, ahead of every rule: the file is applied as one
        // transaction, so no observer sees the chain flushed but not refilled.
        assert_eq!(rs.matches("flush").count(), 1);
        let flush = rs.find("flush chain ip felucca ingress").unwrap();
        assert!(rs.match_indices("add rule").all(|(i, _)| i > flush));
    }

    #[test]
    fn test_ingress_ruleset_skips_a_member_whose_ip_does_not_parse() {
        // The text goes to `nft -f -`, where a newline starts a new command.
        let rs = ingress_ruleset(&[
            (20000, "10.231.0.5\nflush ruleset".to_string(), 80),
            (20001, "10.231.0.6".to_string(), 80),
        ]);
        assert!(!rs.contains("flush ruleset"));
        assert!(!rs.contains("dport 20000"));
        assert!(rs.contains("add rule ip felucca ingress tcp dport 20001 dnat to 10.231.0.6:80\n"));
    }

    // ---- FLU-8: markers, NAT, what the watch reads ----

    fn cidr24() -> Cidr {
        Cidr { base: 0x0AE7_0000, prefix: 24 }
    }

    #[test]
    fn test_every_builder_starts_its_chain_with_the_marker() {
        // `unmarked` checks: the marker is the chain's first rule, there is
        // exactly one, and its hash is over the rest of the file.
        unmarked(&nat_ruleset(cidr24()), "ip felucca postrouting");
        unmarked(&guest_input_ruleset(Some(9090)), "ip felucca input");
        unmarked(&guest_input_ruleset(None), "ip felucca input");
        unmarked(&isolation_ruleset(&[member("tn-a", "10.231.0.2"), member("tn-a", "10.231.0.3")]), "ip felucca forward");
        unmarked(&isolation_ruleset(&[]), "ip felucca forward");
        unmarked(&ingress_ruleset(&[(20000, "10.231.0.5".into(), 80)]), "ip felucca ingress");
        unmarked(&bridge_l2_ruleset(), "bridge felucca forward");
        let mac = guest_mac_for_slot(3);
        unmarked(&antispoof_ruleset("hth-3", "10.231.0.5", &mac).unwrap(), "netdev felucca as_hth_3");
    }

    #[test]
    fn test_nat_is_one_transaction() {
        // Was: four `nft` processes (add table, add chain, flush, add rule)
        // with every result ignored, so a rejected masquerade went unnoticed
        // and guests had no egress between the flush and the add.
        let rs = nat_ruleset(cidr24());
        assert_eq!(
            unmarked(&rs, "ip felucca postrouting"),
            "add table ip felucca\n\
             add chain ip felucca postrouting { type nat hook postrouting priority 100 ; }\n\
             flush chain ip felucca postrouting\n\
             add rule ip felucca postrouting ip saddr 10.231.0.0/24 masquerade\n"
        );
        assert_eq!(host_chain_ruleset(&Chain::Nat, cidr24()), Some(rs));
        assert!(host_chain_ruleset(&Chain::Forward, cidr24()).is_none());
    }

    #[test]
    fn test_the_forward_marker_does_not_depend_on_member_order() {
        let a = [member("tn-a", "10.231.0.2"), member("tn-a", "10.231.0.3"), member("tn-b", "10.231.0.4"), member("tn-b", "10.231.0.10")];
        let b = [member("tn-b", "10.231.0.10"), member("tn-a", "10.231.0.3"), member("tn-b", "10.231.0.4"), member("tn-a", "10.231.0.2")];
        // Many orders, so a HashMap's iteration order cannot pass by luck.
        for _ in 0..20 {
            assert_eq!(isolation_ruleset(&a), isolation_ruleset(&b));
        }
        let c = [member("tn-a", "10.231.0.2"), member("tn-a", "10.231.0.5")];
        assert_ne!(marker_of(&isolation_ruleset(&a)), marker_of(&isolation_ruleset(&c)));
        // A member that joins no pair still changes nothing the kernel holds…
        let mut d = a.to_vec();
        d.push(member("tn-c", "10.231.0.9"));
        assert_eq!(marker_of(&isolation_ruleset(&a)), marker_of(&isolation_ruleset(&d)));
        // …and one that does, changes the hash.
        d.push(member("tn-c", "10.231.0.11"));
        assert_ne!(marker_of(&isolation_ruleset(&a)), marker_of(&isolation_ruleset(&d)));
    }

    /// A listing as nft 1.0.9 prints it (lab-2 E5, lab-3 G).
    fn listing(marker: &str) -> String {
        format!(
            "table ip felucca {{\n\
             \tset tenant_pairs {{\n\
             \t\ttype ipv4_addr . ipv4_addr\n\
             \t\telements = {{ 10.231.0.2 . 10.231.0.3,\n\
             \t\t\t     10.231.0.3 . 10.231.0.2 }}\n\
             \t}}\n\
             \n\
             \tchain postrouting {{\n\
             \t\ttype nat hook postrouting priority srcnat; policy accept;\n\
             \t\tcounter packets 12 bytes 900 comment \"felucca:aaaa\"\n\
             \t\tip saddr 10.231.0.0/24 masquerade\n\
             \t}}\n\
             \n\
             \tchain forward {{\n\
             \t\ttype filter hook forward priority filter; policy drop;\n\
             \t\t{}\n\
             \t\ticmp type {{ echo-request, echo-reply }} accept\n\
             \t}}\n\
             \n\
             \tchain input {{\n\
             \t\ttype filter hook input priority filter; policy accept;\n\
             \t\tiifname != \"felucca0\" accept\n\
             \t}}\n\
             }}\n",
            marker
        )
    }

    #[test]
    fn test_chain_marker_reads_a_kernel_listing() {
        let l = listing("counter packets 0 bytes 0 comment \"felucca:3f2a9c1e\"");
        assert_eq!(chain_marker(&l, "forward"), Some(Some("3f2a9c1e".into())));
        assert_eq!(chain_marker(&l, "postrouting"), Some(Some("aaaa".into())));
        // A chain without the marker as its first rule (flushed and refilled
        // by someone else, or a rule inserted at the head).
        assert_eq!(chain_marker(&l, "input"), Some(None));
        assert_eq!(chain_marker(&listing("ip saddr 10.0.0.1 accept"), "forward"), Some(None));
        // A marker that is not the first rule does not count.
        let late = l.replace("\t\ticmp type", "\t\tcounter packets 0 bytes 0 comment \"felucca:late\"\n\t\ticmp type");
        assert_eq!(chain_marker(&late.replace("counter packets 0 bytes 0 comment \"felucca:3f2a9c1e\"", "drop"), "forward"), Some(None));
        // No such chain; the set's block is not a chain.
        assert_eq!(chain_marker(&l, "ingress"), None);
        assert_eq!(chain_marker(&l, "tenant_pairs"), None);
        // `-a` adds a handle: then it is no bare marker any more, and the
        // watch never lists with `-a`.
        assert_eq!(chain_marker(&listing("counter packets 0 bytes 0 comment \"felucca:3f2a9c1e\" # handle 2"), "forward"), Some(None));
    }

    #[test]
    fn test_host_fence_matches_what_the_start_gate_greps() {
        let t = |rule: &str| {
            format!(
                "table inet felucca_host {{\n\tchain input {{\n\t\ttype filter hook input priority filter; policy accept;\n\t\t{}\n\t}}\n}}\n",
                rule
            )
        };
        assert!(host_fence_in(&t("iifname \"felucca0\" drop")));
        assert!(host_fence_in(&t("iifname \"felucca0\" drop comment \"x\"")));
        assert!(!host_fence_in(&t("iifname \"felucca0\" counter packets 0 bytes 0 drop")));
        assert!(!host_fence_in(&t("iifname \"felucca1\" drop")));
        assert!(!host_fence_in(&t("")));
        // Only chain input counts, as in the gate's `nft list chain … input`.
        assert!(!host_fence_in(&t("iifname \"felucca0\" drop").replace("chain input", "chain other")));
    }

    #[test]
    fn test_bridge_ports_and_carrier() {
        let out = "7: hth-0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc fq_codel master felucca0 state UP mode DEFAULT\n\
                   8: hth-1: <NO-CARRIER,BROADCAST,MULTICAST,UP> mtu 1500 qdisc fq_codel master felucca0 state DOWN\n\
                   9: hth-2: <BROADCAST,MULTICAST> mtu 1500 qdisc noop master felucca0 state DOWN\n";
        assert_eq!(
            parse_bridge_ports(out),
            vec![
                Port { name: "hth-0".into(), carrier: true },
                Port { name: "hth-1".into(), carrier: false },
                Port { name: "hth-2".into(), carrier: false },
            ]
        );
        assert!(has_carrier("BROADCAST,MULTICAST,UP,LOWER_UP"));
        assert!(!has_carrier("NO-CARRIER,BROADCAST,MULTICAST,UP"));
        assert!(!has_carrier("NO-CARRIER,LOWER_UP"));
    }

    #[test]
    fn test_could_not_look_is_never_missing() {
        let done = |ok: bool, out: &str, err: &str| Ran::Done { ok, stdout: out.into(), stderr: err.into() };
        assert_eq!(listing_of(done(true, "table ip felucca {\n}\n", "")), Listing::Listed("table ip felucca {\n}\n".into()));
        // lab-3 G: a missing table answers exit 1 and this message.
        let missing = "Error: No such file or directory\nlist table ip felucca\n              ^^^^^^^\n";
        assert_eq!(listing_of(done(false, "", missing)), Listing::Missing);
        // Timed out, could not be started, or any other failure.
        assert_eq!(listing_of(Ran::Failed), Listing::Unknown);
        assert_eq!(listing_of(Ran::TimedOut), Listing::Unknown);
        assert_eq!(listing_of(done(false, "", "Error: Operation not permitted")), Listing::Unknown);
        assert_eq!(listing_of(done(false, "", "")), Listing::Unknown);
    }

    #[test]
    fn test_a_command_past_its_limit_is_killed() {
        let dir = tempfile::tempdir().unwrap();
        let pidfile = dir.path().join("pid");
        let script = format!("echo $$ > {}; exec sleep 30", pidfile.display());
        let t = Instant::now();
        assert_eq!(run_limited(&["sh", "-c", &script], None, Duration::from_millis(300)), Ran::TimedOut);
        assert!(t.elapsed() < Duration::from_secs(5), "took {:?}", t.elapsed());
        let pid: i32 = std::fs::read_to_string(&pidfile).unwrap().trim().parse().unwrap();
        // Killed and reaped: nothing answers to the pid any more.
        assert_eq!(unsafe { libc::kill(pid, 0) }, -1, "pid {pid} is still alive");
        // Within the limit: the answer, stdin and output included.
        assert_eq!(
            run_limited(&["cat"], Some("hello"), Duration::from_secs(5)),
            Ran::Done { ok: true, stdout: "hello".into(), stderr: String::new() }
        );
        assert!(matches!(run_limited(&["false"], None, Duration::from_secs(5)), Ran::Done { ok: false, .. }));
        assert_eq!(run_limited(&["/nonexistent/felucca-test"], None, Duration::from_secs(5)), Ran::Failed);
    }

    #[test]
    fn test_a_command_that_timed_out_is_not_tried_again_with_sudo() {
        // Was: any failure, a time-out included, ran the command a second
        // time with `sudo -n` — as root the same command, so a hung `nft`
        // held the rule lock for two limits. (On the build VM `sudo -n`
        // works, so the second attempt would really hang a second time.)
        let limit = Duration::from_secs(1);
        let t = Instant::now();
        assert!(!run_escalating(&["sh", "-c", "sleep 30"], None, limit));
        assert!(t.elapsed() < limit + limit / 2, "took {:?}: tried twice", t.elapsed());
        let t = Instant::now();
        assert!(!run_escalating(&["sh", "-c", "sleep 30"], Some("x"), limit));
        assert!(t.elapsed() < limit + limit / 2, "took {:?}: tried twice", t.elapsed());
        // A plain failure still escalates (and fails here too), quickly.
        assert!(!run_escalating(&["false"], None, limit));
        assert!(run_escalating(&["true"], None, limit));
    }

    #[test]
    fn test_the_bridge_is_only_raised_once_the_fences_are_checked() {
        // A bridge that an earlier start took down because the fences could
        // not be loaded has to stay down until they are. `ensure_bridge` runs
        // before any of that is known, on every start-up attempt.
        let src = include_str!("net.rs");
        let prod = src.split("#[cfg(test)]").next().unwrap();
        let start = prod.find("pub fn ensure_bridge(").expect("ensure_bridge");
        let end = start + prod[start..].find("\n}\n").expect("end of ensure_bridge");
        assert!(
            !prod[start..end].contains("\"up\""),
            "ensure_bridge must not bring the bridge up"
        );
        // The one place that does comes after the check, in main.
        let main = include_str!("main.rs");
        let check = main.find("!net::netfilter_ready()").expect("the fence check in main");
        let raise = main.find("net::raise_bridge()").expect("the raise_bridge call in main");
        assert!(raise > check, "main raises the bridge before it has checked the fences");
        assert_eq!(main.matches("net::raise_bridge()").count(), 1);
        assert_eq!(prod.matches("BRIDGE_NAME, \"up\"").count(), 1, "one place raises the bridge: raise_bridge");
        // FLU-8, changed on purpose: the isolation watch raises again a
        // bridge IT took down, and only through `raise_after_repair`, which
        // refuses unless the tick confirmed every fence and switch in place
        // (behaviour pinned in `vm::watch::tests`). Nothing else in the
        // agent brings the bridge up.
        let watch = include_str!("vm/watch.rs");
        let watch_prod = watch.split("#[cfg(test)]").next().unwrap();
        assert_eq!(watch_prod.matches("set_link(net::BRIDGE_NAME, true)").count(), 1);
        let gate = watch_prod.find("fn raise_after_repair(").expect("raise_after_repair");
        let refuse = gate + watch_prod[gate..].find("if !confirmed {").expect("the confirmation check");
        let raise = watch_prod.find("set_link(net::BRIDGE_NAME, true)").unwrap();
        assert!(gate < refuse && refuse < raise, "the watch raises the bridge without checking the fences");
        for src in [include_str!("vm/mod.rs"), include_str!("main.rs"), include_str!("server.rs")] {
            let p = src.split("#[cfg(test)]").next().unwrap();
            assert!(!p.contains("BRIDGE_NAME, true"), "a second place raises the bridge");
        }
    }
}
