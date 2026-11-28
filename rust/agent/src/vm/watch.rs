//! FLU-8: the isolation watch.
//!
//! The rules were built at start-up and on membership changes, and nothing
//! ever looked at the kernel again. With `ip felucca` and `netdev felucca`
//! gone (`nft flush ruleset`, a distribution `nftables.service` restart,
//! another tool) another tenant's guest was reachable at once, egress broke,
//! `/healthz` kept saying `"isolation":true`, and only an agent restart put
//! everything back.
//!
//! Every chain the agent builds starts with a marker rule whose hash is over
//! what was applied (`net::with_marker`), and the manager remembers, per
//! chain, the hash it last applied (`Record::built`). Every `TICK` the watch
//! lists each table once and compares. It never recomputes what the rules
//! should be from the membership — that is the rebuilds' job when membership
//! changes — so a change path that has not run yet is not drift.
//!
//! Three outcomes per look, kept apart: in place; missing or different
//! (drift: warn, rebuild with the current membership, look again, count);
//! could not look (never treated as missing). A chain whose last rebuild
//! failed is rebuilt every tick, quietly. Something is taken down only when
//! the kernel positively shows the fence is not there AND it could not be put
//! back: the guest bridge for a fence between guests or towards the host, or
//! a `br_netfilter`/IPv6 switch; one tap for its anti-spoof chain; nothing for
//! NAT or ingress (their absence closes traffic, it does not open it).

use super::{lock_std, Manager};
use crate::config::IsolationWatch;
use crate::net::{self, Chain, Listing};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use tokio::time::{Duration, Instant};
use tracing::{error, info, warn};

/// How often the watch looks.
pub(crate) const TICK: Duration = Duration::from_secs(5);
/// How long a tick waits for the rule lock before it is skipped (N12): a
/// tick queued behind a long rule change would only look at stale news.
pub(crate) const LOCK_WAIT: Duration = Duration::from_secs(5);
/// A verdict older than this is no verdict (N9').
pub(crate) const STALE: Duration = Duration::from_secs(20);
/// The host fence's warning, at most this often.
const FENCE_WARN_EVERY: Duration = Duration::from_secs(60);

/// The outcome of the last rebuild of one chain.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Built {
    /// Applied, with this marker hash.
    Applied(String),
    Failed,
}

/// The watch's counters (logged in one line whenever one changes; the agent
/// has no metrics endpoint).
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Counters {
    pub(crate) repairs: u64,
    pub(crate) failed_repairs: u64,
    pub(crate) skipped_ticks: u64,
}

/// What the manager knows about its chains, and what the watch last saw.
#[derive(Debug, Default)]
pub(crate) struct Record {
    chains: BTreeMap<Chain, Built>,
    /// Chains whose failing rebuild has been logged (once, not every tick).
    retry_logged: BTreeSet<Chain>,
    /// Taps the watch took down, until a tick pins them again.
    taps_down: BTreeSet<String>,
    /// The watch took the guest bridge down.
    bridge_down: bool,
    last_complete: Option<Instant>,
    /// The last completed tick (and any incomplete one since) left a fence
    /// or a switch unrepaired.
    unrepaired: bool,
    host_fence: bool,
    fence_warned: Option<Instant>,
    pub(crate) counters: Counters,
}

impl Record {
    /// Remember the outcome of one rebuild: its marker hash, or None when it
    /// failed. Called under the rule lock.
    pub(crate) fn built(&mut self, chain: Chain, hash: Option<String>) {
        match hash {
            Some(h) => {
                self.retry_logged.remove(&chain);
                self.chains.insert(chain, Built::Applied(h));
            }
            None => {
                self.chains.insert(chain, Built::Failed);
            }
        }
    }

    /// Whether `chain`'s latest rebuild succeeded.
    pub(crate) fn applied(&self, chain: &Chain) -> bool {
        matches!(self.chains.get(chain), Some(Built::Applied(_)))
    }

    /// `isolation` once a tick has completed, None before (and with the
    /// watch off): then start-up's and the last rule change's verdict stands.
    /// NAT and ingress do not enter it.
    pub(crate) fn verdict(&self, now: Instant) -> Option<bool> {
        let t = self.last_complete?;
        let fence_failed = self.chains.iter().any(|(c, b)| c.fences() && *b == Built::Failed);
        Some(now.saturating_duration_since(t) <= STALE && !self.unrepaired && self.host_fence && !fence_failed)
    }
}

/// What one tick did — for the tests, and nothing else reads it.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub(crate) struct Tick {
    /// Every look succeeded.
    pub(crate) complete: bool,
    /// Drift warnings, by "<family> <table> <chain>" or "switch <path>".
    pub(crate) drift: Vec<String>,
    /// Drift repaired and confirmed by a second look.
    pub(crate) repaired: Vec<String>,
    /// Drift that could not be repaired or confirmed.
    pub(crate) failed: Vec<String>,
    /// Chains rebuilt because their last rebuild had failed, and whether
    /// that worked and was confirmed.
    pub(crate) retried: Vec<(String, bool)>,
    /// Taps with no carrier, not looked at.
    pub(crate) skipped_taps: Vec<String>,
    pub(crate) taps_down: Vec<String>,
    pub(crate) taps_raised: Vec<String>,
    pub(crate) bridge_down: bool,
    pub(crate) bridge_raised: bool,
    /// The host fence's warning was logged this tick.
    pub(crate) fence_warning: bool,
}

/// What a listing shows for one chain.
#[derive(Debug, Clone, PartialEq, Eq)]
enum Seen {
    Marker(String),
    /// No table, no chain, or no marker at its head.
    Absent,
    Unknown,
}

fn seen(l: &Listing, chain: &Chain) -> Seen {
    match l {
        Listing::Unknown => Seen::Unknown,
        Listing::Missing => Seen::Absent,
        Listing::Listed(text) => match net::chain_marker(text, &chain.name()) {
            Some(Some(h)) => Seen::Marker(h),
            _ => Seen::Absent,
        },
    }
}

fn label(chain: &Chain) -> String {
    format!("{} felucca {}", chain.family(), chain.name())
}

/// The slot a tap name stands for (`net::tap_name`), or None.
fn tap_slot(name: &str) -> Option<u32> {
    let n = name.strip_prefix("hth-")?;
    let slot: u32 = n.parse().ok()?;
    (net::tap_name(slot) == name).then_some(slot)
}

/// The watch's one way to bring the guest bridge back up: only when this
/// tick confirmed every fence and switch in place. Start-up's `raise_bridge`
/// in main is the other, and the only one before the watch exists.
fn raise_after_repair(k: &dyn net::Kernel, confirmed: bool) -> bool {
    if !confirmed {
        return false;
    }
    k.set_link(net::BRIDGE_NAME, true)
}

/// Run the watch until the process ends: one tick every `TICK`, a tick
/// never starting before the previous one ended. `off` starts nothing.
pub async fn watch_loop(mgr: Arc<Manager>, mode: IsolationWatch) {
    if mode == IsolationWatch::Off {
        return;
    }
    let mut every = tokio::time::interval(TICK);
    every.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        every.tick().await;
        mgr.watch_tick(mode).await;
    }
}

impl Manager {
    /// One tick. None when it was not run: the network is off, the watch is
    /// off, or the rule lock was busy for `LOCK_WAIT` (skipped, counted).
    ///
    /// The rule lock is held for the look and the repairs only, across the
    /// await of the blocking task that does them — the commands must not
    /// run on a runtime thread.
    pub(crate) async fn watch_tick(self: &Arc<Self>, mode: IsolationWatch) -> Option<Tick> {
        self.watch_tick_within(mode, LOCK_WAIT).await
    }

    /// `watch_tick`, waiting `lock_wait` for the rule lock (a seam for tests).
    async fn watch_tick_within(self: &Arc<Self>, mode: IsolationWatch, lock_wait: Duration) -> Option<Tick> {
        if mode == IsolationWatch::Off || !self.rules_on() {
            return None;
        }
        let Ok(rules) = tokio::time::timeout(lock_wait, self.ruleset_lock.lock()).await else {
            let c = {
                let mut w = lock_std(&self.watch);
                w.counters.skipped_ticks += 1;
                w.counters
            };
            warn!(wait_ms = lock_wait.as_millis() as u64, "isolation watch: the rule lock stayed busy; this tick is skipped");
            log_counters(c);
            return None;
        };
        let me = Arc::clone(self);
        let tick = tokio::task::spawn_blocking(move || me.tick_work(mode, Instant::now())).await;
        drop(rules);
        match tick {
            Ok(t) => Some(t),
            Err(e) => {
                error!(err = %e, "isolation watch: the tick failed");
                None
            }
        }
    }

    /// Rebuild one of the five fixed chains with the builder start-up and
    /// the change paths use, and the CURRENT membership, and remember the
    /// outcome. Under the rule lock, on a blocking thread.
    fn rebuild_chain(&self, chain: &Chain) -> bool {
        let (ok, hash) = match chain {
            Chain::Forward => {
                let members = Self::isolation_members(&self.inner.blocking_lock());
                let ok = self.rules.rebuild_isolation(&members);
                (ok, net::marker_of(&net::isolation_ruleset(&members)))
            }
            Chain::Ingress => {
                let members = Self::ingress_members(&self.inner.blocking_lock());
                let ok = self.rules.rebuild_ingress(&members);
                (ok, net::marker_of(&net::ingress_ruleset(&members)))
            }
            _ => {
                let Some(rs) = net::host_chain_ruleset(chain, self.cidr) else { return false };
                let ok = self.rules.kernel().apply(&rs);
                if !ok {
                    net::report_rejected(chain);
                }
                (ok, net::marker_of(&rs))
            }
        };
        lock_std(&self.watch).built(chain.clone(), if ok { hash } else { None });
        ok
    }

    /// The tick's work: look, repair, look again, decide. Synchronous, on a
    /// blocking thread, with the rule lock held by the caller.
    fn tick_work(&self, mode: IsolationWatch, now: Instant) -> Tick {
        let k = self.rules.kernel();
        let enforce = mode == IsolationWatch::Enforce;
        let before = lock_std(&self.watch).counters;
        let mut t = Tick { complete: true, ..Default::default() };
        // A fence or a switch is not in place after this tick.
        let mut unrepaired = false;
        // A fence or a switch is positively not there AND could not be put back.
        let mut bridge_broken = false;
        // Every fence and switch was seen (or confirmed) in place this tick.
        let mut confirmed = true;
        let mut repairs = 0u64;
        let mut failed_repairs = 0u64;

        // ---- the five fixed chains ----
        let ip = k.list_table("ip", "felucca");
        let br = k.list_table("bridge", "felucca");
        // (chain, it was drift, what the first look saw)
        let mut rebuilt: Vec<(Chain, bool, Seen)> = Vec::new();
        for chain in [Chain::Nat, Chain::Input, Chain::Forward, Chain::Ingress, Chain::L2] {
            let listing = if chain == Chain::L2 { &br } else { &ip };
            let look = seen(listing, &chain);
            let state = lock_std(&self.watch).chains.get(&chain).cloned();
            match (state, &look) {
                // The last rebuild failed (or none ran): again, quietly,
                // whatever the look says.
                (None | Some(Built::Failed), _) => {
                    if look == Seen::Unknown {
                        t.complete = false;
                    }
                    let ok = self.rebuild_chain(&chain);
                    if ok {
                        rebuilt.push((chain, false, look));
                    } else {
                        let first = lock_std(&self.watch).retry_logged.insert(chain.clone());
                        if first {
                            error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                                "isolation watch: the chain's last rebuild failed and rebuilding it failed again; \
                                 it is retried every tick");
                        }
                        t.retried.push((label(&chain), false));
                        if chain.fences() {
                            unrepaired = true;
                            confirmed = false;
                            // Taken down only when the kernel positively
                            // has no marker: an older, complete ruleset is
                            // still a fence.
                            if look == Seen::Absent {
                                bridge_broken = true;
                            }
                        }
                    }
                }
                (Some(Built::Applied(_)), Seen::Unknown) => {
                    t.complete = false;
                    if chain.fences() {
                        confirmed = false;
                    }
                }
                (Some(Built::Applied(want)), Seen::Marker(h)) if *h == want => {}
                (Some(Built::Applied(want)), _) => {
                    let found = match &look {
                        Seen::Marker(h) => format!("marker {h}, expected {want}"),
                        _ => "no table, no chain, or no marker at its head".to_string(),
                    };
                    warn!(table = %format!("{} felucca", chain.family()), chain = %chain.name(), found = %found,
                        "isolation watch: drift — the chain is not what this agent last applied; rebuilding it");
                    t.drift.push(label(&chain));
                    if self.rebuild_chain(&chain) {
                        rebuilt.push((chain, true, look));
                    } else {
                        error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                            "isolation watch: the repair failed");
                        t.failed.push(label(&chain));
                        failed_repairs += 1;
                        if chain.fences() {
                            unrepaired = true;
                            confirmed = false;
                            bridge_broken = true;
                        }
                    }
                }
            }
        }
        // The second look, for whatever was rebuilt.
        if !rebuilt.is_empty() {
            let ip2 = k.list_table("ip", "felucca");
            let br2 = k.list_table("bridge", "felucca");
            for (chain, drift, first) in rebuilt {
                let look = seen(if chain == Chain::L2 { &br2 } else { &ip2 }, &chain);
                let want = match lock_std(&self.watch).chains.get(&chain) {
                    Some(Built::Applied(h)) => Some(h.clone()),
                    _ => None,
                };
                let ok = matches!((&look, &want), (Seen::Marker(h), Some(w)) if h == w);
                if look == Seen::Unknown {
                    t.complete = false;
                }
                if drift {
                    if ok {
                        info!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                            "isolation watch: chain repaired and confirmed");
                        t.repaired.push(label(&chain));
                        repairs += 1;
                    } else {
                        error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                            "isolation watch: the repair could not be confirmed");
                        t.failed.push(label(&chain));
                        failed_repairs += 1;
                    }
                } else {
                    t.retried.push((label(&chain), ok));
                }
                if !ok {
                    // Not in the kernel after all: retried quietly next tick.
                    lock_std(&self.watch).built(chain.clone(), None);
                    if chain.fences() {
                        unrepaired = true;
                        confirmed = false;
                        if drift || first == Seen::Absent {
                            bridge_broken = true;
                        }
                    }
                }
            }
        }

        // ---- the switches ----
        let mut switches = vec![net::BR_NF_SWITCH.to_string()];
        if k.path_exists(net::IPV6_ROOT) {
            switches.push(net::bridge_v6_switch());
        }
        for path in switches {
            match k.read_switch(&path) {
                net::Switch::Value(v) if v == "1" => {}
                net::Switch::Unknown => {
                    t.complete = false;
                    confirmed = false;
                }
                net::Switch::Missing => {
                    error!(switch = %path,
                        "isolation watch: the switch file is missing (module unloaded or device gone) — it cannot be set");
                    t.failed.push(format!("switch {path}"));
                    unrepaired = true;
                    confirmed = false;
                    bridge_broken = true;
                }
                net::Switch::Value(v) => {
                    warn!(switch = %path, value = %v, "isolation watch: drift — a switch this agent set to 1 is not; setting it again");
                    t.drift.push(format!("switch {path}"));
                    let ok = k.write_switch(&path, "1") && k.read_switch(&path) == net::Switch::Value("1".into());
                    if ok {
                        info!(switch = %path, "isolation watch: switch repaired and confirmed");
                        t.repaired.push(format!("switch {path}"));
                        repairs += 1;
                    } else {
                        error!(switch = %path, "isolation watch: the switch could not be set again");
                        t.failed.push(format!("switch {path}"));
                        failed_repairs += 1;
                        unrepaired = true;
                        confirmed = false;
                        bridge_broken = true;
                    }
                }
            }
        }

        // ---- the taps ----
        match k.bridge_ports() {
            None => t.complete = false,
            Some(ports) => {
                let nd = k.list_table("netdev", "felucca");
                let pins: Vec<(String, bool, Option<String>, net::GuestMac)> = {
                    let g = self.inner.blocking_lock();
                    ports
                        .iter()
                        .filter_map(|p| {
                            let slot = tap_slot(&p.name)?;
                            let (tap, ip, mac) = self.tap_pin(&g, slot);
                            Some((tap, p.carrier, ip, mac))
                        })
                        .collect()
                };
                let mut pending: Vec<(String, String, bool)> = Vec::new(); // (tap, want, was down)
                for (tap, carrier, ip, mac) in pins {
                    let was_down = lock_std(&self.watch).taps_down.contains(&tap);
                    // No carrier: no process holds the tap, it carries
                    // nothing. That also skips a tap `ensure_pinned_tap` has
                    // just raised and not yet pinned.
                    if !carrier && !was_down {
                        t.skipped_taps.push(tap);
                        continue;
                    }
                    // A record holds the slot but has no address: nothing to
                    // pin to (a sandbox being deleted).
                    let Some(rs) = ip.as_deref().and_then(|ip| net::antispoof_ruleset(&tap, ip, &mac)) else { continue };
                    let Some(want) = net::marker_of(&rs) else { continue };
                    let chain = Chain::Tap(tap.clone());
                    match seen(&nd, &chain) {
                        Seen::Unknown => {
                            t.complete = false;
                            continue;
                        }
                        Seen::Marker(h) if h == want => {
                            if was_down {
                                pending.push((tap, want, true));
                            }
                            continue;
                        }
                        look => {
                            if !was_down {
                                warn!(table = "netdev felucca", chain = %chain.name(),
                                    found = %match &look { Seen::Marker(h) => format!("marker {h}, expected {want}"), _ => "no table, no chain, or no marker at its head".into() },
                                    "isolation watch: drift — the tap's anti-spoof chain is not what this agent pins; rebuilding it");
                                t.drift.push(label(&chain));
                            }
                        }
                    }
                    if k.apply(&rs) {
                        pending.push((tap, want, was_down));
                        continue;
                    }
                    // Deleted between the port list and the repair: that is
                    // not a failure.
                    if k.link_exists(&tap) == Some(false) {
                        lock_std(&self.watch).taps_down.remove(&tap);
                        continue;
                    }
                    self.tap_unrepaired(k, &tap, was_down, enforce, &mut t, &mut unrepaired, &mut failed_repairs);
                }
                if !pending.is_empty() {
                    let nd2 = k.list_table("netdev", "felucca");
                    for (tap, want, was_down) in pending {
                        let chain = Chain::Tap(tap.clone());
                        let look = seen(&nd2, &chain);
                        if look == Seen::Marker(want) {
                            if !was_down {
                                info!(table = "netdev felucca", chain = %chain.name(), "isolation watch: chain repaired and confirmed");
                                t.repaired.push(label(&chain));
                                repairs += 1;
                            } else if k.set_link(&tap, true) {
                                lock_std(&self.watch).taps_down.remove(&tap);
                                info!(tap = %tap, "isolation watch: the tap is pinned again and back up");
                                t.taps_raised.push(tap);
                            }
                        } else {
                            if look == Seen::Unknown {
                                t.complete = false;
                            }
                            if k.link_exists(&tap) == Some(false) {
                                lock_std(&self.watch).taps_down.remove(&tap);
                                continue;
                            }
                            self.tap_unrepaired(k, &tap, was_down, enforce, &mut t, &mut unrepaired, &mut failed_repairs);
                        }
                    }
                }
                // Taps that are gone are no longer the watch's to raise.
                let names: BTreeSet<&str> = ports.iter().map(|p| p.name.as_str()).collect();
                lock_std(&self.watch).taps_down.retain(|t| names.contains(t.as_str()));
            }
        }

        // ---- the host fence (felucca-firewall's; reported, never repaired) ----
        let fence = match k.list_table("inet", "felucca_host") {
            Listing::Listed(l) => Some(net::host_fence_in(&l)),
            Listing::Missing => Some(false),
            Listing::Unknown => {
                t.complete = false;
                None
            }
        };

        // ---- the bridge ----
        let bridge_was_down = lock_std(&self.watch).bridge_down;
        if bridge_broken {
            if !enforce {
                error!("isolation watch (report mode): a fence or switch could not be put back; the guest bridge is \
                        left up — GUESTS ARE NOT FENCED on this node");
            } else if !bridge_was_down {
                if k.set_link(net::BRIDGE_NAME, false) {
                    lock_std(&self.watch).bridge_down = true;
                    t.bridge_down = true;
                    eprintln!(
                        "error: isolation watch: a fence or switch could not be put back — the guest bridge {} has \
                         been taken down; guests keep running without network until a tick repairs it",
                        net::BRIDGE_NAME
                    );
                    error!(bridge = net::BRIDGE_NAME,
                        "isolation watch: a fence or switch could not be put back — the guest bridge has been taken \
                         down; guests keep running without network until a tick repairs it");
                } else {
                    error!(bridge = net::BRIDGE_NAME, "isolation watch: the guest bridge could not be taken down");
                }
            }
        } else if bridge_was_down && raise_after_repair(k, confirmed && t.complete) {
            lock_std(&self.watch).bridge_down = false;
            t.bridge_raised = true;
            info!(bridge = net::BRIDGE_NAME, "isolation watch: every fence is confirmed again — the guest bridge is up again");
        }

        // ---- the verdict ----
        let after = {
            let mut w = lock_std(&self.watch);
            if t.complete {
                w.last_complete = Some(now);
                w.unrepaired = unrepaired;
            } else {
                // Only what was positively seen may change it: a look that
                // failed changes nothing for up to `STALE`.
                w.unrepaired |= unrepaired;
            }
            if let Some(f) = fence {
                w.host_fence = f;
                let due = match w.fence_warned {
                    None => true,
                    Some(at) => now.saturating_duration_since(at) >= FENCE_WARN_EVERY,
                };
                if !f && due {
                    w.fence_warned = Some(now);
                    t.fence_warning = true;
                    warn!("isolation watch: the host fence (iifname \"felucca0\" drop, chain input of table inet \
                           felucca_host) is missing; it belongs to felucca-firewall.service, which this agent does not \
                           repair — isolation is reported false until it is back");
                }
                if f {
                    w.fence_warned = None;
                }
            }
            w.counters.repairs += repairs;
            w.counters.failed_repairs += failed_repairs;
            w.counters
        };
        if after != before {
            log_counters(after);
        }
        // Rebuilds may have made it safe to hand out held addresses (A, B).
        self.settle_slots(&mut self.inner.blocking_lock());
        t
    }

    /// A tap whose anti-spoof chain could not be put back: taken down in
    /// `enforce` (once), left up and reported in `report`.
    #[allow(clippy::too_many_arguments)]
    fn tap_unrepaired(
        &self,
        k: &dyn net::Kernel,
        tap: &str,
        was_down: bool,
        enforce: bool,
        t: &mut Tick,
        unrepaired: &mut bool,
        failed_repairs: &mut u64,
    ) {
        if was_down {
            // Still down; tried again next tick.
            return;
        }
        error!(table = "netdev felucca", chain = %net::Chain::Tap(tap.to_string()).name(), "isolation watch: the repair failed");
        t.failed.push(label(&Chain::Tap(tap.to_string())));
        *failed_repairs += 1;
        if enforce && k.set_link(tap, false) {
            lock_std(&self.watch).taps_down.insert(tap.to_string());
            t.taps_down.push(tap.to_string());
            error!(tap = %tap, "isolation watch: the tap's anti-spoof chain could not be put back — the tap has been \
                                taken down until a tick pins it again");
        } else {
            // Left up without its pin: this guest can claim another's address.
            *unrepaired = true;
        }
    }
}

fn log_counters(c: Counters) {
    info!(repairs = c.repairs, failed_repairs = c.failed_repairs, skipped_ticks = c.skipped_ticks, "isolation watch counters");
}

#[cfg(test)]
pub(crate) mod tests {
    use super::super::rule_fake::{add_vm, hold_rule_lock, install, FakeRules, RuleCall};
    use super::*;
    use crate::ipalloc::Cidr;
    use std::collections::HashSet;
    use std::sync::atomic::Ordering;

    const ENFORCE: IsolationWatch = IsolationWatch::Enforce;

    pub(crate) struct Worker {
        _tmp: tempfile::TempDir,
        pub(crate) mgr: Arc<Manager>,
        pub(crate) rules: Arc<FakeRules>,
    }

    /// A manager on the fake kernel, as after start-up: host set-up, two
    /// sandboxes of two tenants with their taps held by a VMM, one expose.
    pub(crate) async fn worker() -> Worker {
        let tmp = tempfile::tempdir().unwrap();
        let mut mgr = Manager::new(
            tmp.path().to_str().unwrap().to_string(),
            false,
            Cidr { base: 0x0AE7_0000, prefix: 24 },
            0,
            "http://cp".into(),
            "tok".into(),
        );
        let rules = install(&mut mgr);
        mgr.setup_host().await;
        let w = Worker { _tmp: tmp, mgr, rules };
        plug(&w, "vm-a", 0, "t1").await;
        plug(&w, "vm-b", 1, "t2").await;
        w.mgr.expose("vm-a", 80, &HashSet::new(), None).await.unwrap();
        w
    }

    /// A sandbox in slot `slot`, published in the rules and with its tap
    /// pinned and held by its VMM — as a create leaves one.
    async fn plug(w: &Worker, id: &str, slot: u32, tenant: &str) {
        let ip = add_vm(&w.mgr, id, slot, tenant).await;
        assert!(w.mgr.refresh_isolation().await);
        let (tap, mac) = super::super::tap_pin_for_slot(slot);
        assert!(w.mgr.rules.pin_tap(&tap, Some(&ip), &mac));
        w.rules.k().links.get_mut(&tap).unwrap().held = true;
    }

    async fn tick(w: &Worker) -> Tick {
        w.mgr.watch_tick(ENFORCE).await.expect("the tick ran")
    }

    fn verdict(w: &Worker) -> Option<bool> {
        lock_std(&w.mgr.watch).verdict(Instant::now())
    }

    fn counters(w: &Worker) -> Counters {
        lock_std(&w.mgr.watch).counters
    }

    fn quiet(t: &Tick) -> bool {
        t.complete && t.drift.is_empty() && t.repaired.is_empty() && t.failed.is_empty() && t.retried.is_empty()
            && !t.bridge_down && t.taps_down.is_empty()
    }

    /// Every chain the agent owns on the worker above.
    fn owned() -> Vec<(&'static str, &'static str, &'static str)> {
        vec![
            ("ip", "felucca", "postrouting"),
            ("ip", "felucca", "input"),
            ("ip", "felucca", "forward"),
            ("ip", "felucca", "ingress"),
            ("bridge", "felucca", "forward"),
            ("netdev", "felucca", "as_hth_0"),
            ("netdev", "felucca", "as_hth_1"),
        ]
    }

    #[tokio::test]
    async fn test_a_healthy_worker_is_quiet_and_isolated() {
        let w = worker().await;
        assert_eq!(verdict(&w), None, "before the first tick: start-up's verdict");
        assert!(w.mgr.isolation_ok());
        let t = tick(&w).await;
        assert!(quiet(&t), "{t:?}");
        assert_eq!(verdict(&w), Some(true));
        // Every owned chain carries a marker at its head.
        for (f, tb, c) in owned() {
            assert!(matches!(w.rules.marker(f, tb, c), Some(Some(_))), "{f} {tb} {c}");
        }
    }

    #[derive(Clone, Copy, Debug)]
    enum Break {
        DeleteTable,
        DeleteChain,
        Flush,
        Marker,
    }

    fn break_it(rules: &FakeRules, how: Break, f: &str, tb: &str, c: &str) {
        let mut k = rules.k();
        match how {
            Break::DeleteTable => {
                k.tables.remove(&(f.to_string(), tb.to_string()));
            }
            Break::DeleteChain => {
                k.tables.get_mut(&(f.to_string(), tb.to_string())).unwrap().remove(c);
            }
            Break::Flush => k.chain_mut(f, tb, c).unwrap().rules.clear(),
            Break::Marker => {
                k.chain_mut(f, tb, c).unwrap().rules[0] = "counter packets 0 bytes 0 comment \"felucca:0000000000000000\"".into()
            }
        }
    }

    #[tokio::test]
    async fn test_drift_in_every_owned_chain_is_warned_rebuilt_confirmed_and_counted() {
        for (f, tb, c) in owned() {
            for how in [Break::DeleteTable, Break::DeleteChain, Break::Flush, Break::Marker] {
                let w = worker().await;
                assert!(quiet(&tick(&w).await));
                // With the table gone, every chain in it drifted.
                let expect: Vec<String> = match how {
                    Break::DeleteTable => owned()
                        .into_iter()
                        .filter(|o| o.0 == f && o.1 == tb)
                        .map(|o| format!("{} {} {}", o.0, o.1, o.2))
                        .collect(),
                    _ => vec![format!("{f} {tb} {c}")],
                };
                break_it(&w.rules, how, f, tb, c);
                let t = tick(&w).await;
                let mut drift = t.drift.clone();
                drift.sort();
                let mut want = expect.clone();
                want.sort();
                assert_eq!(drift, want, "{how:?} {f} {tb} {c}: {t:?}");
                let mut repaired = t.repaired.clone();
                repaired.sort();
                assert_eq!(repaired, want, "{how:?} {f} {tb} {c}: {t:?}");
                assert!(t.failed.is_empty() && !t.bridge_down && t.taps_down.is_empty(), "{how:?} {f} {tb} {c}: {t:?}");
                assert_eq!(counters(&w).repairs, want.len() as u64);
                assert_eq!(verdict(&w), Some(true), "{how:?} {f} {tb} {c}");
                // In the kernel again, with the marker last applied.
                assert!(matches!(w.rules.marker(f, tb, c), Some(Some(_))));
                assert!(quiet(&tick(&w).await), "{how:?} {f} {tb} {c}: the repair holds");
            }
        }
    }

    #[tokio::test]
    async fn test_lag_is_not_drift() {
        let w = worker().await;
        tick(&w).await;
        // Membership changed; the change path's own rebuild has not run.
        add_vm(&w.mgr, "vm-c", 2, "t1").await;
        w.rules.clear();
        assert!(quiet(&tick(&w).await));
        assert!(w.rules.calls().is_empty(), "the watch rebuilt from the membership: {:?}", w.rules.calls());
        // The change path's rebuild fails: the kernel keeps the older,
        // complete ruleset with its marker.
        w.rules.fail_isolation.store(true, Ordering::SeqCst);
        assert!(!w.mgr.refresh_isolation().await);
        let t = tick(&w).await;
        assert!(t.drift.is_empty(), "{t:?}");
        assert_eq!(t.retried, vec![("ip felucca forward".to_string(), false)]);
        // Both conditions needed: the marker is there, so nothing goes down.
        assert!(!t.bridge_down && w.rules.link_up(net::BRIDGE_NAME) == Some(true), "{t:?}");
        assert_eq!(verdict(&w), Some(false), "a fence whose last rebuild failed is not in force");
        // The next tick retries with the current membership, and then it matches.
        w.rules.fail_isolation.store(false, Ordering::SeqCst);
        let t = tick(&w).await;
        assert_eq!(t.retried, vec![("ip felucca forward".to_string(), true)], "{t:?}");
        assert!(t.drift.is_empty());
        match w.rules.calls().last() {
            Some(RuleCall::Isolation { ips, .. }) => assert_eq!(ips.len(), 3, "the current membership: {ips:?}"),
            other => panic!("{other:?}"),
        }
        assert!(quiet(&tick(&w).await));
        assert_eq!(verdict(&w), Some(true));
        assert_eq!(counters(&w).repairs, 0, "a retry is not a repair");
    }

    #[tokio::test]
    async fn test_could_not_look_rebuilds_nothing_and_goes_stale() {
        let w = worker().await;
        tick(&w).await;
        let before = w.rules.ops().len();
        w.rules.fail_looks.store(true, Ordering::SeqCst);
        // Even with a chain really gone: it cannot be seen.
        break_it(&w.rules, Break::DeleteChain, "ip", "felucca", "forward");
        let t = tick(&w).await;
        assert!(!t.complete && t.drift.is_empty() && t.repaired.is_empty() && t.failed.is_empty(), "{t:?}");
        let ops: Vec<String> = w.rules.ops()[before..].to_vec();
        assert!(ops.iter().all(|o| o.starts_with("list ")), "nothing rebuilt, nothing taken down: {ops:?}");
        assert_eq!(verdict(&w), Some(true), "unchanged for a tick");
        // 20 s without a completed tick: no verdict to trust.
        let stale = lock_std(&w.mgr.watch).verdict(Instant::now() + STALE + Duration::from_secs(1));
        assert_eq!(stale, Some(false));
        // Looks work again: the drift is seen and repaired.
        w.rules.fail_looks.store(false, Ordering::SeqCst);
        let t = tick(&w).await;
        assert_eq!(t.repaired, vec!["ip felucca forward".to_string()]);
    }

    /// Break `chain` so that its rebuild fails: deleted, and every ruleset
    /// naming it rejected.
    fn break_for_good(rules: &FakeRules, f: &str, tb: &str, c: &str) {
        break_it(rules, Break::DeleteChain, f, tb, c);
        lock_std(&rules.reject).push(format!("{f} {tb} {c}"));
    }

    fn mend(rules: &FakeRules) {
        lock_std(&rules.reject).clear();
    }

    #[tokio::test]
    async fn test_a_fence_that_cannot_be_put_back_takes_the_bridge_down_until_a_tick_repairs_it() {
        for (f, tb, c) in [("ip", "felucca", "forward"), ("ip", "felucca", "input"), ("bridge", "felucca", "forward")] {
            let w = worker().await;
            tick(&w).await;
            break_for_good(&w.rules, f, tb, c);
            let t = tick(&w).await;
            assert_eq!(t.failed, vec![format!("{f} {tb} {c}")], "{t:?}");
            assert!(t.bridge_down, "{f} {tb} {c}");
            assert_eq!(w.rules.link_up(net::BRIDGE_NAME), Some(false));
            assert_eq!(verdict(&w), Some(false));
            assert!(!w.mgr.isolation_ok());
            // Taps are left alone: the bridge is what is down.
            assert_eq!(w.rules.link_up("hth-0"), Some(true));
            // Still broken: still down, not taken down again.
            let t = tick(&w).await;
            assert!(!t.bridge_down && !t.bridge_raised, "{t:?}");
            assert_eq!(w.rules.link_up(net::BRIDGE_NAME), Some(false));
            assert_eq!(counters(&w).failed_repairs, 1, "a failing retry is not counted again");
            mend(&w.rules);
            // Repaired but not confirmed — the second look could not be
            // made: the bridge stays down.
            w.rules.fail_looks.store(true, Ordering::SeqCst);
            let t = tick(&w).await;
            assert!(!t.bridge_raised, "{f} {tb} {c}: raised without confirmation: {t:?}");
            assert_eq!(w.rules.link_up(net::BRIDGE_NAME), Some(false));
            w.rules.fail_looks.store(false, Ordering::SeqCst);
            let t = tick(&w).await;
            assert!(t.bridge_raised, "{f} {tb} {c}: {t:?}");
            assert_eq!(w.rules.link_up(net::BRIDGE_NAME), Some(true));
            assert_eq!(verdict(&w), Some(true));
        }
    }

    #[tokio::test]
    async fn test_a_switch_that_cannot_be_set_takes_the_bridge_down() {
        // Wrong value, and the write fails.
        let w = worker().await;
        tick(&w).await;
        w.rules.k().switches.insert(net::BR_NF_SWITCH.into(), "0".into());
        w.rules.fail_switch_write.store(true, Ordering::SeqCst);
        let t = tick(&w).await;
        assert!(t.bridge_down, "{t:?}");
        assert_eq!(verdict(&w), Some(false));
        w.rules.fail_switch_write.store(false, Ordering::SeqCst);
        let t = tick(&w).await;
        assert_eq!(t.repaired, vec![format!("switch {}", net::BR_NF_SWITCH)]);
        assert!(t.bridge_raised);
        assert_eq!(verdict(&w), Some(true));
        // The file is missing (module unloaded): broken, not "could not look".
        let w = worker().await;
        tick(&w).await;
        w.rules.k().switches.remove(net::BR_NF_SWITCH);
        let t = tick(&w).await;
        assert!(t.complete && t.bridge_down, "{t:?}");
        assert!(!w.rules.ops().iter().any(|o| o.starts_with("write ")), "a missing file is not written");
        w.rules.k().switches.insert(net::BR_NF_SWITCH.into(), "1".into());
        assert!(tick(&w).await.bridge_raised);
    }

    #[tokio::test]
    async fn test_a_wrong_switch_is_written_back() {
        let w = worker().await;
        tick(&w).await;
        let v6 = net::bridge_v6_switch();
        w.rules.k().switches.insert(v6.clone(), "0".into());
        let t = tick(&w).await;
        assert_eq!(t.drift, vec![format!("switch {v6}")]);
        assert_eq!(t.repaired, vec![format!("switch {v6}")]);
        assert!(!t.bridge_down);
        assert_eq!(w.rules.k().switches.get(&v6).map(String::as_str), Some("1"));
        assert_eq!(verdict(&w), Some(true));
        // No IPv6 in the kernel at all: there is nothing to check.
        w.rules.k().paths.clear();
        w.rules.k().switches.remove(&v6);
        assert!(quiet(&tick(&w).await));
    }

    #[tokio::test]
    async fn test_a_tap_whose_chain_cannot_be_put_back_goes_down_alone() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_0");
        let t = tick(&w).await;
        assert_eq!(t.taps_down, vec!["hth-0".to_string()], "{t:?}");
        assert!(!t.bridge_down);
        assert_eq!(w.rules.link_up("hth-0"), Some(false));
        assert_eq!(w.rules.link_up("hth-1"), Some(true));
        assert_eq!(w.rules.link_up(net::BRIDGE_NAME), Some(true));
        assert_eq!(verdict(&w), Some(true), "a down tap carries nothing");
        // Down: no carrier any more, but the watch keeps trying it.
        assert!(tick(&w).await.taps_down.is_empty());
        mend(&w.rules);
        let t = tick(&w).await;
        assert_eq!(t.taps_raised, vec!["hth-0".to_string()], "{t:?}");
        assert_eq!(w.rules.link_up("hth-0"), Some(true));
        assert!(matches!(w.rules.marker("netdev", "felucca", "as_hth_0"), Some(Some(_))));
    }

    #[tokio::test]
    async fn test_nat_and_ingress_take_nothing_down_and_are_retried() {
        for c in ["postrouting", "ingress"] {
            let w = worker().await;
            tick(&w).await;
            break_for_good(&w.rules, "ip", "felucca", c);
            let t = tick(&w).await;
            assert_eq!(t.failed, vec![format!("ip felucca {c}")]);
            assert!(!t.bridge_down && t.taps_down.is_empty());
            assert!(w.rules.ops().iter().all(|o| !o.starts_with("link ")), "{c}: something was taken down");
            assert_eq!(verdict(&w), Some(true), "{c} is not isolation");
            let t = tick(&w).await;
            assert_eq!(t.retried, vec![(format!("ip felucca {c}"), false)]);
            mend(&w.rules);
            let t = tick(&w).await;
            assert_eq!(t.retried, vec![(format!("ip felucca {c}"), true)]);
            assert!(quiet(&tick(&w).await));
        }
    }

    #[tokio::test]
    async fn test_report_mode_repairs_and_counts_but_never_takes_anything_down() {
        let w = worker().await;
        let report = |w: &Worker| {
            let m = Arc::clone(&w.mgr);
            async move { m.watch_tick(IsolationWatch::Report).await.unwrap() }
        };
        report(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_0");
        let t = report(&w).await;
        assert_eq!(t.failed.len(), 2, "{t:?}");
        assert!(!t.bridge_down && t.taps_down.is_empty());
        assert!(w.rules.ops().iter().all(|o| !o.starts_with("link ")), "report mode took something down");
        assert_eq!(verdict(&w), Some(false));
        assert_eq!(counters(&w).failed_repairs, 2);
        mend(&w.rules);
        break_it(&w.rules, Break::Flush, "bridge", "felucca", "forward");
        let t = report(&w).await;
        assert_eq!(t.repaired, vec!["bridge felucca forward".to_string(), "netdev felucca as_hth_0".to_string()], "{t:?}");
        assert_eq!(counters(&w).repairs, 2);
        assert_eq!(verdict(&w), Some(true));
    }

    #[tokio::test]
    async fn test_off_starts_no_watch() {
        let w = worker().await;
        let before = w.rules.ops().len();
        assert!(w.mgr.watch_tick(IsolationWatch::Off).await.is_none());
        tokio::time::timeout(Duration::from_secs(1), watch_loop(Arc::clone(&w.mgr), IsolationWatch::Off))
            .await
            .expect("off returns at once");
        assert_eq!(w.rules.ops().len(), before, "off looked at the kernel");
        // `isolation` stays what start-up and the rule changes say.
        assert_eq!(verdict(&w), None);
        w.rules.fences_down.store(true, Ordering::SeqCst);
        assert!(!w.mgr.isolation_ok());
    }

    #[tokio::test]
    async fn test_taps_without_carrier_are_skipped_and_one_without_a_record_is_pinned_by_its_slot() {
        let w = worker().await;
        tick(&w).await;
        // Created a moment ago, not yet handed to its VMM (or asleep).
        w.rules.add_port("hth-5", false);
        let t = tick(&w).await;
        assert_eq!(t.skipped_taps, vec!["hth-5".to_string()]);
        assert!(quiet(&t), "{t:?}");
        // Held by a VMM whose record did not load: pinned from the slot.
        w.rules.add_port("hth-7", true);
        let t = tick(&w).await;
        assert_eq!(t.repaired, vec!["netdev felucca as_hth_7".to_string()], "{t:?}");
        let rules = w.rules.k().chain_mut("netdev", "felucca", "as_hth_7").unwrap().rules.clone();
        assert!(rules.contains(&"ip saddr 10.231.0.9 accept".to_string()), "{rules:?}");
        assert!(rules.contains(&format!("ether saddr != {} drop", net::guest_mac_for_slot(7))), "{rules:?}");
    }

    #[tokio::test]
    async fn test_a_tap_deleted_during_the_tick_is_not_a_failure() {
        let w = worker().await;
        tick(&w).await;
        break_it(&w.rules, Break::DeleteChain, "netdev", "felucca", "as_hth_1");
        *lock_std(&w.rules.vanish_before_apply) = Some("hth-1".into());
        let t = tick(&w).await;
        assert!(t.failed.is_empty() && t.taps_down.is_empty(), "{t:?}");
        assert_eq!(counters(&w).failed_repairs, 0);
        assert_eq!(verdict(&w), Some(true));
    }

    #[tokio::test]
    async fn test_delete_removes_the_tap_under_the_rule_lock() {
        let w = worker().await;
        w.mgr.delete("vm-b").await.unwrap();
        assert!(w.rules.calls().iter().any(|c| matches!(c, RuleCall::RemoveTap { tap, .. } if tap == "hth-1")));
        assert_eq!(w.rules.unlocked_tap_removals.load(Ordering::SeqCst), 0);
        assert!(quiet(&tick(&w).await));
    }

    #[tokio::test]
    async fn test_the_host_fence_is_reported_never_repaired() {
        let w = worker().await;
        tick(&w).await;
        let rules_of = |w: &Worker| w.rules.k().chain_mut("inet", "felucca_host", "input").unwrap().rules.clone();
        let fence = rules_of(&w);
        w.rules.k().chain_mut("inet", "felucca_host", "input").unwrap().rules.clear();
        let before = w.rules.ops().len();
        let t = tick(&w).await;
        assert!(t.fence_warning && !t.bridge_down, "{t:?}");
        assert_eq!(verdict(&w), Some(false));
        let ops: Vec<String> = w.rules.ops()[before..].to_vec();
        assert!(ops.iter().all(|o| o.starts_with("list ")), "something was repaired or taken down: {ops:?}");
        assert!(rules_of(&w).is_empty(), "the agent wrote the host fence");
        // At most once a minute.
        assert!(!tick(&w).await.fence_warning);
        let m = Arc::clone(&w.mgr);
        let later = tokio::task::spawn_blocking(move || m.tick_work(ENFORCE, Instant::now() + Duration::from_secs(61)))
            .await
            .unwrap();
        assert!(later.fence_warning);
        // Back: true again.
        w.rules.k().chain_mut("inet", "felucca_host", "input").unwrap().rules = fence;
        assert!(!tick(&w).await.fence_warning);
        assert_eq!(verdict(&w), Some(true));
        // The table missing entirely is the same.
        w.rules.k().tables.remove(&("inet".to_string(), "felucca_host".to_string()));
        tick(&w).await;
        assert_eq!(verdict(&w), Some(false));
    }

    #[tokio::test]
    async fn test_the_blackhole_route_comes_before_any_fence() {
        let w = worker().await;
        let ops = w.rules.ops();
        let at = |what: &str| ops.iter().position(|o| o == what).unwrap_or_else(|| panic!("{what}: {ops:?}"));
        assert_eq!(at("route 10.231.0.0/24"), 0);
        assert!(at("route 10.231.0.0/24") < at("bridge"));
        assert!(at("bridge") < at("apply ip felucca postrouting"));
        assert!(at("apply ip felucca postrouting") < at("apply ip felucca input"));
        assert!(at("apply ip felucca input") < at("apply bridge felucca forward"));
    }

    #[tokio::test]
    async fn test_a_tick_that_cannot_get_the_rule_lock_is_skipped_and_ticks_never_overlap() {
        let w = worker().await;
        let held = hold_rule_lock(&w.mgr).await;
        let t = w.mgr.watch_tick_within(ENFORCE, Duration::from_millis(50)).await;
        assert!(t.is_none());
        assert_eq!(counters(&w).skipped_ticks, 1);
        drop(held);
        // Two ticks at once run one after the other: the second's first
        // look comes after the first's last.
        let before = w.rules.ops().len();
        let (a, b) = tokio::join!(w.mgr.watch_tick(ENFORCE), w.mgr.watch_tick(ENFORCE));
        assert!(a.is_some() && b.is_some());
        let ops: Vec<String> = w.rules.ops()[before..].to_vec();
        let firsts: Vec<usize> = ops.iter().enumerate().filter(|(_, o)| *o == "list ip felucca").map(|(i, _)| i).collect();
        let lasts: Vec<usize> = ops.iter().enumerate().filter(|(_, o)| *o == "list inet felucca_host").map(|(i, _)| i).collect();
        assert_eq!((firsts.len(), lasts.len()), (2, 2), "{ops:?}");
        assert!(lasts[0] < firsts[1], "the ticks overlapped: {ops:?}");
    }

    #[tokio::test]
    async fn test_no_false_alarm_under_churn() {
        let w = worker().await;
        tick(&w).await;
        for i in 0..300u32 {
            let id = format!("vm-x{i}");
            let slot = 2 + (i % 40);
            plug(&w, &id, slot, if i % 2 == 0 { "t1" } else { "t3" }).await;
            let t = tick(&w).await;
            assert!(quiet(&t), "{i}: create: {t:?}");
            w.mgr.expose(&id, 8080, &HashSet::new(), None).await.unwrap();
            let t = tick(&w).await;
            assert!(quiet(&t), "{i}: expose: {t:?}");
            w.mgr.unexpose(&id, 8080).await.unwrap();
            w.mgr.delete(&id).await.unwrap();
            let t = tick(&w).await;
            assert!(quiet(&t), "{i}: unexpose+delete: {t:?}");
        }
        assert_eq!(counters(&w), Counters::default());
        assert_eq!(verdict(&w), Some(true));
    }

    // ---- A: held slots ----

    fn free(w: &Worker) -> Vec<u32> {
        w.rules.free_slots()
    }

    #[tokio::test]
    async fn test_a_failing_rebuild_holds_the_slot_and_the_next_good_pair_frees_it() {
        for by_watch in [false, true] {
            let w = worker().await;
            w.rules.fail_isolation.store(true, Ordering::SeqCst);
            w.mgr.delete("vm-b").await.unwrap();
            assert!(!free(&w).contains(&1), "held");
            assert!(w.mgr.inner.lock().await.held_slots.contains(&1));
            // A delete in between cannot free it (nor twice).
            w.mgr.delete("vm-b").await.unwrap();
            assert!(!free(&w).contains(&1));
            w.rules.fail_isolation.store(false, Ordering::SeqCst);
            if by_watch {
                let t = tick(&w).await;
                assert_eq!(t.retried, vec![("ip felucca forward".to_string(), true)]);
            } else {
                assert!(w.mgr.refresh_isolation().await);
            }
            assert!(free(&w).contains(&1), "by_watch {by_watch}: the good pair frees it");
            assert!(w.mgr.inner.lock().await.held_slots.is_empty());
            // Taken again, it is not freed by the next good pair.
            add_vm(&w.mgr, "vm-c", 1, "t1").await;
            assert!(w.mgr.refresh_isolation().await);
            assert!(w.mgr.refresh_ingress().await);
            assert!(!free(&w).contains(&1), "by_watch {by_watch}: freed twice");
        }
    }

    #[tokio::test]
    async fn test_a_rolled_back_create_does_not_leak_its_slot_forever() {
        // `commit_or_isolate` deletes a create whose isolation rebuild failed.
        let w = worker().await;
        w.rules.fail_isolation.store(true, Ordering::SeqCst);
        for (i, slot) in (10..20u32).enumerate() {
            add_vm(&w.mgr, &format!("vm-r{i}"), slot, "t1").await;
            w.mgr.delete(&format!("vm-r{i}")).await.unwrap();
        }
        assert_eq!(w.mgr.inner.lock().await.held_slots.len(), 10);
        w.rules.fail_isolation.store(false, Ordering::SeqCst);
        tick(&w).await;
        let f = free(&w);
        assert!((10..20u32).all(|s| f.contains(&s)), "{f:?}");
    }

    // ---- B: start-up ingress ----

    #[tokio::test]
    async fn test_startup_with_an_ingress_chain_that_cannot_be_settled_hands_out_no_address() {
        let w = worker().await;
        w.rules.fail_ingress.store(true, Ordering::SeqCst);
        lock_std(&w.rules.reject).push("ip felucca ingress".into());
        assert!(!w.mgr.start_ingress().await);
        assert_eq!(Manager::claim_slot(&mut *w.mgr.inner.lock().await), Err("IsolationFailed"));
        assert!(w.mgr.isolation_ok(), "ingress is not isolation");
        // The tick that rebuilds it lets addresses through again.
        w.rules.fail_ingress.store(false, Ordering::SeqCst);
        mend(&w.rules);
        let t = tick(&w).await;
        assert_eq!(t.retried, vec![("ip felucca ingress".to_string(), true)]);
        assert!(Manager::claim_slot(&mut *w.mgr.inner.lock().await).is_ok());

        // When only the rebuild fails, the flush empties the chain and
        // addresses are handed out at once.
        let w = worker().await;
        w.rules.fail_ingress.store(true, Ordering::SeqCst);
        assert!(!w.mgr.start_ingress().await);
        assert!(Manager::claim_slot(&mut *w.mgr.inner.lock().await).is_ok());
        let rules = w.rules.k().chain_mut("ip", "felucca", "ingress").unwrap().rules.clone();
        assert_eq!(rules.len(), 1, "only the marker: {rules:?}");
    }
}
