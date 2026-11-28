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
//! TAKING SOMETHING DOWN NEEDS POSITIVE EVIDENCE TWICE (W2). A look has three
//! outcomes (in place / positively not / could not look) and so has a repair
//! (applied / refused by the kernel / unknown). Something goes down only when
//! the first look says positively not, AND the repair was refused or it was
//! applied and a second look says positively not. "Unknown" anywhere takes
//! nothing down: the tick is incomplete and tries again. What goes down: the
//! guest bridge for a fence between guests or towards the host, a switch, or
//! a dormant table that stays dormant; one tap for its anti-spoof chain;
//! nothing for NAT or ingress (their absence closes traffic, it does not open
//! it, and they never enter `isolation` — X2). The same evidence refuses new
//! guests (`Record::refusal`, W3, X5), in `enforce` and in `report`.
//!
//! And nothing is taken down, and no refusal comes, from a table until the
//! watch has once read back, in that table, a marker of a chain the agent
//! applied there (calibration per table, X3): a build that cannot read what
//! this kernel prints must not take the bridge and every tap down.

use super::{lock_std, Manager};
use crate::config::IsolationWatch;
use crate::ipalloc::Cidr;
use crate::net::{self, Applied, Chain, Listing};
use std::collections::{BTreeMap, BTreeSet};
use std::sync::Arc;
use tokio::time::{Duration, Instant};
use tracing::{error, info, warn};

/// How often the watch looks.
pub(crate) const TICK: Duration = Duration::from_secs(5);
/// How long a tick waits for the rule lock before it is skipped (N12, W10:
/// was 5 s — a burst of queued rule changes made a healthy worker's ticks
/// skip until its verdict went stale).
pub(crate) const LOCK_WAIT: Duration = Duration::from_secs(15);
/// A verdict older than this is no verdict (N9', W10: was 20 s). Also: with
/// the watch on, no completed tick within this of its start is `false`.
pub(crate) const STALE: Duration = Duration::from_secs(45);
/// How long one tick may hold the rule lock (W4): past it the tick stops
/// looking and repairing, takes nothing down for what it did not finish,
/// and marks itself incomplete.
pub(crate) const TICK_BUDGET: Duration = Duration::from_secs(15);
/// The host fence's warning and `report` mode's "not fenced" line, at most
/// this often.
const WARN_EVERY: Duration = Duration::from_secs(60);

/// The outcome of the last rebuild of one chain.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum Built {
    /// Applied, with this marker hash.
    Applied(String),
    /// Refused, or no answer: retried every tick.
    Failed,
}

/// The watch's counters (logged in one line whenever one changes; the agent
/// has no metrics endpoint).
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub(crate) struct Counters {
    pub(crate) repairs: u64,
    /// Once per episode (W12): a chain failing tick after tick counts once.
    pub(crate) failed_repairs: u64,
    pub(crate) skipped_ticks: u64,
}

/// What a table's calibration says (X3).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Calib {
    /// A marker of a chain the agent applied has been read back here.
    Yes,
    /// Not yet: repairs go on, nothing is taken down or refused for it.
    NotYet,
    /// A chain the agent applied is listed without a readable marker: the
    /// table is left alone for the rest of this process.
    Never,
}

/// What the manager knows about its chains, and what the watch last saw.
#[derive(Debug, Default)]
pub(crate) struct Record {
    chains: BTreeMap<Chain, Built>,
    /// Chains whose failing rebuild has been logged, with what was logged.
    retry_logged: BTreeMap<Chain, String>,
    /// What is failing now, by label, with the text logged for it: counted
    /// once per episode, logged again when the text changes (X11).
    failing: BTreeMap<String, String>,
    /// Taps held down (by the watch, or by start-up for want of a pin),
    /// until a tick or a create, wake, start or fork pins them again.
    taps_down: BTreeSet<String>,
    /// Held-down taps found raised by something else, logged once.
    taps_relowered: BTreeSet<String>,
    /// The watch took the guest bridge down.
    bridge_down: bool,
    bridge_relowered: bool,
    /// The guest bridge does not exist (W15), logged once.
    bridge_gone: bool,
    /// Tables seen dormant after a rebuild, logged once (W7, X14).
    dormant: BTreeSet<String>,
    /// Tables whose markers have been read back, and those that cannot be
    /// (X3).
    calibrated: BTreeSet<&'static str>,
    uncalibratable: BTreeSet<&'static str>,
    route_failed: bool,
    report_warned: Option<Instant>,
    watch_started: Option<Instant>,
    last_complete: Option<Instant>,
    /// Positive evidence twice that a fence, a switch or a table is not in
    /// place (the take-down condition; W3's refusal).
    broken: bool,
    /// A fence or a switch positively not in place whose repair's outcome is
    /// unknown (X5: refuses new guests, takes nothing down).
    suspect: bool,
    /// The last completed tick (and any incomplete one since) left a fence
    /// or a switch unrepaired, or a tap without its pin.
    unrepaired: bool,
    host_fence: bool,
    /// Whether the host fence was in place when this agent started (W16);
    /// None until a look could be made.
    host_fence_expected: Option<bool>,
    fence_warned: Option<Instant>,
    /// Start-up could neither rebuild nor empty the ingress chain, and no
    /// guest address is handed out (`Inner.slots_withheld`, mirrored here so
    /// `isolation` can say so without the state lock — Z2).
    pub(crate) withheld: bool,
    pub(crate) counters: Counters,
}

impl Record {
    /// Remember the outcome of one rebuild: its marker hash, or None when it
    /// was not applied. Called under the rule lock.
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

    /// The hash last applied to `chain`, if its latest rebuild succeeded.
    pub(crate) fn hash_of(&self, chain: &Chain) -> Option<&str> {
        match self.chains.get(chain) {
            Some(Built::Applied(h)) => Some(h),
            _ => None,
        }
    }

    /// A tap start-up took down for want of a pin (W8): the watch pins and
    /// raises it on a later tick, as it does for taps it took down itself.
    pub(crate) fn hold_tap_down(&mut self, tap: &str) {
        self.taps_down.insert(tap.to_string());
    }

    /// A create, wake, start or fork pinned and raised `tap` (X4): it is not
    /// the watch's to hold down any more.
    pub(crate) fn release_tap(&mut self, tap: &str) {
        self.taps_down.remove(tap);
        self.taps_relowered.remove(tap);
    }

    /// The watch has started (W10).
    pub(crate) fn mark_started(&mut self, now: Instant) {
        self.watch_started.get_or_insert(now);
    }

    /// Why no new guest may be put on this node (W3, X5), or None.
    pub(crate) fn refusal(&self) -> Option<&'static str> {
        if self.bridge_gone {
            Some("the guest bridge is gone")
        } else if self.broken {
            Some("a fence, a switch or a table of this agent is positively not in place and could not be put back")
        } else if self.suspect {
            Some("a fence or a switch of this agent was positively not in place and its repair has no answer yet")
        } else {
            None
        }
    }

    /// Whether the host fence was found in place at start (W16), once known.
    pub(crate) fn expect_host_fence(&mut self, present: bool) {
        if self.host_fence_expected.is_none() {
            self.host_fence_expected = Some(present);
            self.host_fence = present;
            if !present {
                warn!("the host fence (iifname \"felucca0\" drop, chain input of table inet felucca_host) is not \
                       loaded as this agent starts; it is not watched in this run (no firewall unit?) and does not \
                       enter `isolation`");
            }
        }
    }

    /// `isolation` once the watch has run, None before (and with the watch
    /// off): then start-up's and the last rule change's verdict stands. NAT
    /// and ingress do not enter it.
    pub(crate) fn verdict(&self, now: Instant) -> Option<bool> {
        let Some(t) = self.last_complete else {
            // W10: on, but no tick completed for `STALE`: no verdict to trust.
            return match self.watch_started {
                Some(s) if now.saturating_duration_since(s) > STALE => Some(false),
                _ => None,
            };
        };
        let fence_failed = self.chains.iter().any(|(c, b)| c.fences() && *b == Built::Failed);
        let host_fence = self.host_fence || self.host_fence_expected == Some(false);
        Some(
            now.saturating_duration_since(t) <= STALE
                && !self.unrepaired
                && !self.bridge_gone
                && host_fence
                && !fence_failed,
        )
    }

    /// Note that `label` is failing with `text`: (a new episode — count it,
    /// the moment to log).
    fn fail_note(&mut self, label: &str, text: &str) -> (bool, bool) {
        match self.failing.insert(label.to_string(), text.to_string()) {
            None => (true, true),
            Some(old) => (false, old != text),
        }
    }

    fn recovered(&mut self, label: &str) {
        self.failing.remove(label);
    }

    fn is_failing(&self, label: &str) -> bool {
        self.failing.contains_key(label)
    }

    /// Calibrate `fam` from a listing (X3): has a marker of one of `applied`
    /// (chain names the agent applied there) been read back?
    fn calibrate(&mut self, fam: &'static str, text: &str, applied: &[String]) -> Calib {
        if self.uncalibratable.contains(fam) {
            return Calib::Never;
        }
        if self.calibrated.contains(fam) {
            return Calib::Yes;
        }
        for name in applied {
            match net::first_rule(text, name) {
                Some(Some(line)) => {
                    if net::chain_marker(text, name).flatten().is_some() {
                        self.calibrated.insert(fam);
                        return Calib::Yes;
                    }
                    self.uncalibratable.insert(fam);
                    eprintln!(
                        "error: isolation watch: the chain {name} of table {fam} felucca, which this agent applied, \
                         is listed without a marker it can read; its first rule reads: {line} — nothing in this \
                         table is watched or taken down in this process"
                    );
                    error!(table = %format!("{fam} felucca"), chain = %name, first_rule = %line,
                        "isolation watch: a chain this agent applied is listed without a marker it can read; nothing \
                         in this table is watched or taken down in this process");
                    return Calib::Never;
                }
                // No such chain, or no rule in it: no evidence either way.
                _ => continue,
            }
        }
        Calib::NotYet
    }
}

/// What one tick did — for the tests, and nothing else reads it.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub(crate) struct Tick {
    /// Every look that enters the verdict succeeded, within the budget.
    pub(crate) complete: bool,
    /// The budget ran out (W4).
    pub(crate) out_of_time: bool,
    /// Drift warnings, by "<family> <table> <chain>" or "switch <path>".
    pub(crate) drift: Vec<String>,
    /// Drift repaired and confirmed by a second look.
    pub(crate) repaired: Vec<String>,
    /// Drift that positively could not be repaired (refused, or applied and
    /// a second look says it is not there).
    pub(crate) failed: Vec<String>,
    /// Repairs whose outcome is not known (no answer, or the second look
    /// could not be made): nothing is taken down for them.
    pub(crate) unconfirmed: Vec<String>,
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
    /// No table, no chain, no marker at its head, or the table dormant:
    /// positively not in place.
    Absent,
    Unknown,
}

fn seen(l: &Listing, chain: &Chain) -> Seen {
    match l {
        Listing::Unknown => Seen::Unknown,
        Listing::Missing => Seen::Absent,
        // X14: a dormant table's chains are unhooked: not in place, and a
        // rebuild (each begins with `add table`) clears the flag.
        Listing::Listed(text) if net::table_dormant(text) => Seen::Absent,
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

/// Install (again) the route that answers "unreachable" for the guest
/// network (W9): at start-up, before the bridge is taken down, and on every
/// tick while it is down. A failure is an ERROR, logged when `failed_before`
/// says it is new (None: always). Never called with the watch's mutex held
/// (X8): it runs `ip`.
pub(crate) fn ensure_guest_route(k: &dyn net::Kernel, cidr: Cidr, failed_before: Option<bool>) -> bool {
    let ok = k.guest_route(&net::guest_net(cidr));
    if !ok && failed_before != Some(true) {
        error!(net = %net::guest_net(cidr), "the unreachable route for the guest network could not be installed: \
               while the guest bridge is down, traffic for guest addresses may follow the default route");
    }
    ok
}

/// What start-up's read-back found (W1, X3).
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum SelfCheck {
    /// Every chain start-up applied reads back with a marker.
    Readable,
    /// A look could not be made: the watch starts, its ticks deal with it
    /// (and take nothing down before they have read a marker back, X3).
    CouldNotLook,
    /// A chain start-up applied successfully is listed without a readable
    /// marker: the watch must not run in this process.
    Unreadable { chain: String, line: String },
}

/// The mode the watch runs in after start-up's self-check (W1; a function
/// so that main's decision is tested, X13).
pub(crate) fn mode_after_self_check(configured: IsolationWatch, check: &SelfCheck) -> IsolationWatch {
    match (configured, check) {
        (IsolationWatch::Off, _) => IsolationWatch::Off,
        (_, SelfCheck::Unreadable { .. }) => IsolationWatch::Off,
        (mode, _) => mode,
    }
}

/// Run the watch until the process ends: one tick every `TICK`, a tick
/// never starting before the previous one ended. `off` starts nothing.
pub async fn watch_loop(mgr: Arc<Manager>, mode: IsolationWatch) {
    if mode == IsolationWatch::Off {
        return;
    }
    lock_std(&mgr.watch).mark_started(Instant::now());
    let mut every = tokio::time::interval(TICK);
    every.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        every.tick().await;
        mgr.watch_tick(mode).await;
    }
}

/// W11: while start-up could not settle the ingress chain, retry its rebuild
/// every `TICK` — in every watch mode, `off` included (there, only an
/// expose, unexpose or delete would otherwise ever rebuild it).
pub async fn withheld_retry_loop(mgr: Arc<Manager>) {
    let mut every = tokio::time::interval(TICK);
    every.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        every.tick().await;
        if mgr.inner.lock().await.slots_withheld {
            mgr.refresh_ingress().await;
        }
    }
}

impl Manager {
    /// W1: after start-up applied its chains, read them back once — `ip`,
    /// `bridge`, and `netdev` when taps were adopted (X3). A chain that was
    /// applied but is listed without a readable marker means this build
    /// cannot read what this kernel prints: the watch would see every chain
    /// absent and take the bridge and every tap down. Also records whether
    /// the host fence is there at start (W16).
    pub async fn watch_self_check(self: &Arc<Self>) -> SelfCheck {
        if !self.rules_on() {
            return SelfCheck::Readable;
        }
        let _rules = self.ruleset_lock.lock().await;
        let me = Arc::clone(self);
        tokio::task::spawn_blocking(move || me.self_check_work()).await.unwrap_or(SelfCheck::CouldNotLook)
    }

    fn self_check_work(&self) -> SelfCheck {
        let k = self.rules.kernel();
        match k.list_table("inet", "felucca_host") {
            Listing::Listed(l) => lock_std(&self.watch).expect_host_fence(net::host_fence_in(&l)),
            Listing::Missing => lock_std(&self.watch).expect_host_fence(false),
            Listing::Unknown => {}
        }
        // What start-up applied, per table.
        let mut per_table: Vec<(&'static str, Vec<String>)> = Vec::new();
        {
            let w = lock_std(&self.watch);
            for fam in ["ip", "bridge"] {
                let names: Vec<String> = [Chain::Nat, Chain::Input, Chain::Forward, Chain::Ingress, Chain::L2]
                    .into_iter()
                    .filter(|c| c.family() == fam && w.hash_of(c).is_some())
                    .map(|c| c.name())
                    .collect();
                per_table.push((fam, names));
            }
        }
        // The taps start-up pinned for the guests it adopted.
        let taps: Vec<String> = {
            let g = self.inner.blocking_lock();
            super::taps_to_repin(&g.vms).into_iter().map(|(_, slot, _)| Chain::Tap(net::tap_name(slot)).name()).collect()
        };
        if !taps.is_empty() {
            per_table.push(("netdev", taps));
        }
        let mut could_not = false;
        for (fam, names) in per_table {
            if names.is_empty() {
                continue;
            }
            let text = match k.list_table(fam, "felucca") {
                Listing::Listed(t) => t,
                _ => {
                    could_not = true;
                    continue;
                }
            };
            let calib = lock_std(&self.watch).calibrate(fam, &text, &names);
            // W1: EVERY chain start-up applied must read back, not one: a
            // fence chain whose header this build does not recognise would
            // look absent to every tick.
            let unfound = names.iter().find(|n| net::chain_marker(&text, n).flatten().is_none());
            match (calib, unfound) {
                (Calib::Yes, None) => {}
                (Calib::Yes, Some(n)) => {
                    let line = match net::first_rule(&text, n) {
                        Some(Some(l)) => l.to_string(),
                        Some(None) => "<the chain has no rule>".to_string(),
                        None => format!(
                            "<the chain is not in the listing; its first chain line reads: {}>",
                            text.lines().find(|l| l.trim_start().starts_with("chain ")).unwrap_or("").trim()
                        ),
                    };
                    return SelfCheck::Unreadable { chain: format!("{fam} felucca {n}"), line };
                }
                (Calib::Never, _) => {
                    // The calibration logged it; name the chain here too.
                    let (chain, line) = names
                        .iter()
                        .find_map(|n| match net::first_rule(&text, n) {
                            Some(Some(l)) => Some((format!("{fam} felucca {n}"), l.to_string())),
                            _ => None,
                        })
                        .unwrap_or_default();
                    return SelfCheck::Unreadable { chain, line };
                }
                (Calib::NotYet, _) => {
                    // Applied, yet not in the listing in any readable form: a
                    // chain header this build does not recognise.
                    let head = text.lines().find(|l| l.trim_start().starts_with("chain ")).unwrap_or("").trim().to_string();
                    return SelfCheck::Unreadable {
                        chain: format!("{fam} felucca {}", names[0]),
                        line: format!("<the chain is not in the listing; its first chain line reads: {}>", head),
                    };
                }
            }
        }
        if could_not { SelfCheck::CouldNotLook } else { SelfCheck::Readable }
    }

    /// One tick. None when it was not run: the network is off, the watch is
    /// off, the rule lock was busy for `LOCK_WAIT` (skipped, counted), or the
    /// tick's task failed.
    ///
    /// The rule lock is held for the look and the repairs only, across the
    /// await of the blocking task that does them — the commands must not
    /// run on a runtime thread — and for at most `TICK_BUDGET` of work.
    pub(crate) async fn watch_tick(self: &Arc<Self>, mode: IsolationWatch) -> Option<Tick> {
        self.watch_tick_within(mode, LOCK_WAIT, TICK_BUDGET).await
    }

    /// `watch_tick`, waiting `lock_wait` for the rule lock and working for
    /// `budget` (seams for tests).
    pub(crate) async fn watch_tick_within(
        self: &Arc<Self>,
        mode: IsolationWatch,
        lock_wait: Duration,
        budget: Duration,
    ) -> Option<Tick> {
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
        let now = Instant::now();
        let tick = tokio::task::spawn_blocking(move || me.tick_work(mode, now, now + budget)).await;
        drop(rules);
        match tick {
            Ok(t) => Some(t),
            Err(e) => {
                error!(err = %e, "isolation watch: the tick failed; the next one runs as usual");
                None
            }
        }
    }

    /// Rebuild one of the five fixed chains with the builder start-up and
    /// the change paths use, and the CURRENT membership, and remember the
    /// outcome. Under the rule lock, on a blocking thread.
    fn rebuild_chain(&self, chain: &Chain) -> Applied {
        let (outcome, hash) = match chain {
            Chain::Forward => {
                let members = Self::isolation_members(&self.inner.blocking_lock());
                let (outcome, _) = self.rules.rebuild_isolation(&members);
                (outcome, net::marker_of(&net::isolation_ruleset(&members)))
            }
            Chain::Ingress => {
                let members = Self::ingress_members(&self.inner.blocking_lock());
                let outcome = self.rules.rebuild_ingress(&members);
                (outcome, net::marker_of(&net::ingress_ruleset(&members)))
            }
            _ => {
                let Some(rs) = net::host_chain_ruleset(chain, self.cidr) else { return Applied::Unknown };
                let outcome = self.rules.kernel().apply(&rs);
                net::report_built(chain, &outcome);
                (outcome, net::marker_of(&rs))
            }
        };
        lock_std(&self.watch).built(chain.clone(), if outcome.ok() { hash } else { None });
        outcome
    }

    /// The tick's work: look, repair, look again, decide. Synchronous, on a
    /// blocking thread, with the rule lock held by the caller; stops at
    /// `deadline` (W4, X7).
    pub(crate) fn tick_work(&self, mode: IsolationWatch, now: Instant, deadline: Instant) -> Tick {
        let k = self.rules.kernel();
        let enforce = mode == IsolationWatch::Enforce;
        let before = lock_std(&self.watch).counters;
        let mut t = Tick { complete: true, ..Default::default() };
        // Past the budget: stop, incomplete, nothing taken down for the rest.
        let late = |t: &mut Tick| {
            if Instant::now() >= deadline {
                t.complete = false;
                t.out_of_time = true;
                true
            } else {
                false
            }
        };
        // Positive evidence twice that a fence/switch/table is not in place.
        let mut broken = false;
        // Positively not in place, repair outcome unknown (X5).
        let mut suspect = false;
        // A fence or a switch is not in place after this tick, or a tap is
        // without its pin.
        let mut unrepaired = false;
        // Every fence and switch was seen (or confirmed) in place this tick.
        let mut confirmed = true;
        let mut repairs = 0u64;
        let mut failed_repairs = 0u64;

        // ---- the bridge itself (W15) ----
        let bridge_gone = if late(&mut t) {
            confirmed = false;
            lock_std(&self.watch).bridge_gone
        } else {
            match k.link_exists(net::BRIDGE_NAME) {
                Some(false) => {
                    let was = std::mem::replace(&mut lock_std(&self.watch).bridge_gone, true);
                    if !was {
                        error!(bridge = net::BRIDGE_NAME, "isolation watch: the guest bridge is gone; restart the agent to rebuild it");
                    }
                    true
                }
                Some(true) => {
                    lock_std(&self.watch).bridge_gone = false;
                    false
                }
                None => {
                    t.complete = false;
                    confirmed = false;
                    lock_std(&self.watch).bridge_gone
                }
            }
        };

        // ---- the five fixed chains ----
        let ip = if late(&mut t) { Listing::Unknown } else { k.list_table("ip", "felucca") };
        let br = if late(&mut t) { Listing::Unknown } else { k.list_table("bridge", "felucca") };
        // Calibration per table (X3).
        let mut calib: BTreeMap<&'static str, Calib> = BTreeMap::new();
        for (fam, l) in [("ip", &ip), ("bridge", &br)] {
            let c = match l {
                Listing::Listed(text) => {
                    let mut w = lock_std(&self.watch);
                    let names: Vec<String> = [Chain::Nat, Chain::Input, Chain::Forward, Chain::Ingress, Chain::L2]
                        .into_iter()
                        .filter(|c| c.family() == fam && w.hash_of(c).is_some())
                        .map(|c| c.name())
                        .collect();
                    w.calibrate(fam, text, &names)
                }
                _ => {
                    let w = lock_std(&self.watch);
                    if w.uncalibratable.contains(fam) {
                        Calib::Never
                    } else if w.calibrated.contains(fam) {
                        Calib::Yes
                    } else {
                        Calib::NotYet
                    }
                }
            };
            calib.insert(fam, c);
        }
        // Whether what is seen in `fam` may take something down or refuse.
        let may_act = |fam: &str, calib: &BTreeMap<&'static str, Calib>| calib.get(fam) == Some(&Calib::Yes);
        // Only fences, switches and taps make a tick incomplete (X2).
        let unknown = |t: &mut Tick, chain: &Chain| {
            if chain.fences() {
                t.complete = false;
            }
        };
        // (chain, it was drift, what the first look saw)
        let mut rebuilt: Vec<(Chain, bool, Seen)> = Vec::new();
        for chain in [Chain::Nat, Chain::Input, Chain::Forward, Chain::Ingress, Chain::L2] {
            if late(&mut t) {
                break;
            }
            let fam = chain.family();
            if calib.get(fam) == Some(&Calib::Never) {
                continue; // left alone for this process (X3)
            }
            let act = may_act(fam, &calib);
            let listing = if chain == Chain::L2 { &br } else { &ip };
            let look = seen(listing, &chain);
            let state = lock_std(&self.watch).chains.get(&chain).cloned();
            match (state, &look) {
                // The last rebuild failed (or none ran): again, quietly,
                // whatever the look says.
                (None | Some(Built::Failed), _) => {
                    if look == Seen::Unknown {
                        unknown(&mut t, &chain);
                    }
                    match self.rebuild_chain(&chain) {
                        Applied::Committed => rebuilt.push((chain, false, look)),
                        outcome => {
                            let text = format!("{outcome:?}");
                            let changed = lock_std(&self.watch).retry_logged.insert(chain.clone(), text.clone()).as_deref() != Some(text.as_str());
                            if changed {
                                error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                                    outcome = %text,
                                    "isolation watch: the chain's last rebuild failed and rebuilding it failed again; \
                                     it is retried every tick");
                            }
                            t.retried.push((label(&chain), false));
                            if chain.fences() {
                                unrepaired = true;
                                confirmed = false;
                                if outcome == Applied::Unknown {
                                    t.complete = false;
                                }
                                // Taken down only when the kernel positively
                                // has no marker AND it refused: an older,
                                // complete ruleset is still a fence.
                                if act && look == Seen::Absent {
                                    match outcome {
                                        Applied::Refused(_) => broken = true,
                                        _ => suspect = true,
                                    }
                                }
                            }
                        }
                    }
                }
                (Some(Built::Applied(_)), Seen::Unknown) => {
                    unknown(&mut t, &chain);
                    if chain.fences() {
                        confirmed = false;
                    }
                }
                (Some(Built::Applied(want)), Seen::Marker(h)) if *h == want => {}
                (Some(Built::Applied(want)), _) => {
                    let found = match &look {
                        Seen::Marker(h) => format!("marker {h}, expected {want}"),
                        _ => "no table, no chain, no marker at its head, or the table dormant".to_string(),
                    };
                    warn!(table = %format!("{} felucca", chain.family()), chain = %chain.name(), found = %found,
                        "isolation watch: drift — the chain is not what this agent last applied; rebuilding it");
                    t.drift.push(label(&chain));
                    match self.rebuild_chain(&chain) {
                        Applied::Committed => rebuilt.push((chain, true, look)),
                        Applied::Refused(line) => {
                            let (new, log) = lock_std(&self.watch).fail_note(&label(&chain), &line);
                            if log {
                                error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(), nft = %line,
                                    "isolation watch: the repair was refused");
                            }
                            if new {
                                failed_repairs += 1;
                            }
                            t.failed.push(label(&chain));
                            if chain.fences() {
                                unrepaired = true;
                                confirmed = false;
                                if act {
                                    broken = true;
                                }
                            }
                        }
                        Applied::Unknown => {
                            warn!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                                "isolation watch: the repair got no answer (timed out, killed or could not run); \
                                 nothing is taken down, it is tried again");
                            t.unconfirmed.push(label(&chain));
                            if chain.fences() {
                                t.complete = false;
                                unrepaired = true;
                                confirmed = false;
                                if act {
                                    suspect = true;
                                }
                            }
                        }
                    }
                }
            }
        }
        // The second look, for whatever was rebuilt.
        let mut still_dormant: BTreeSet<&'static str> = BTreeSet::new();
        if !rebuilt.is_empty() {
            let out = late(&mut t);
            let (ip2, br2) = if out {
                (Listing::Unknown, Listing::Unknown)
            } else {
                (k.list_table("ip", "felucca"), k.list_table("bridge", "felucca"))
            };
            for (fam, l) in [("ip", &ip2), ("bridge", &br2)] {
                if matches!(l, Listing::Listed(text) if net::table_dormant(text)) {
                    still_dormant.insert(fam);
                }
            }
            for (chain, drift, first) in rebuilt {
                let act = may_act(chain.family(), &calib);
                let look = seen(if chain == Chain::L2 { &br2 } else { &ip2 }, &chain);
                let want = lock_std(&self.watch).hash_of(&chain).map(str::to_string);
                match (&look, &want) {
                    (Seen::Marker(h), Some(w)) if h == w => {
                        lock_std(&self.watch).recovered(&label(&chain));
                        if drift {
                            info!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                                "isolation watch: chain repaired and confirmed");
                            t.repaired.push(label(&chain));
                            repairs += 1;
                        } else {
                            t.retried.push((label(&chain), true));
                        }
                    }
                    // Applied, and the second look could not be made: the
                    // record stays `Applied`, nothing is taken down (W2).
                    (Seen::Unknown, _) => {
                        unknown(&mut t, &chain);
                        t.unconfirmed.push(label(&chain));
                        if chain.fences() {
                            confirmed = false;
                            if act && (drift || first == Seen::Absent) {
                                suspect = true;
                            }
                        }
                    }
                    // Applied, and positively not there.
                    _ => {
                        lock_std(&self.watch).built(chain.clone(), None);
                        if drift {
                            let (new, log) = lock_std(&self.watch).fail_note(&label(&chain), "applied, still not there");
                            if log {
                                error!(table = %format!("{} felucca", chain.family()), chain = %chain.name(),
                                    "isolation watch: the repair was applied but the chain is still not there");
                            }
                            if new {
                                failed_repairs += 1;
                            }
                            t.failed.push(label(&chain));
                        } else {
                            t.retried.push((label(&chain), false));
                        }
                        if chain.fences() {
                            unrepaired = true;
                            confirmed = false;
                            if act && (drift || first == Seen::Absent) {
                                broken = true;
                            }
                        }
                    }
                }
            }
        }
        // X14: dormant even after an applied rebuild: not the agent's to undo.
        for (fam, l) in [("ip", &ip), ("bridge", &br)] {
            let lbl = format!("{fam} felucca");
            let dormant_now = matches!(l, Listing::Listed(text) if net::table_dormant(text));
            if still_dormant.contains(fam) {
                if lock_std(&self.watch).dormant.insert(lbl.clone()) {
                    error!(table = %lbl, "isolation watch: the table stays dormant after a rebuild — its chains are \
                           unhooked although their rules are in place: run `nft delete table {}`, after which the \
                           agent rebuilds it", lbl);
                }
                unrepaired = true;
                confirmed = false;
                if may_act(fam, &calib) {
                    broken = true;
                }
            } else if !dormant_now {
                // X11: cleared once it is not dormant any more.
                lock_std(&self.watch).dormant.remove(&lbl);
            }
        }

        // ---- the switches ----
        let mut switches = vec![net::BR_NF_SWITCH.to_string()];
        if !bridge_gone && !late(&mut t) && k.path_exists(net::IPV6_ROOT) {
            switches.push(net::bridge_v6_switch());
        }
        for path in switches {
            if late(&mut t) {
                break;
            }
            let lbl = format!("switch {path}");
            match k.read_switch(&path) {
                net::Switch::Value(v) if v == "1" => lock_std(&self.watch).recovered(&lbl),
                net::Switch::Unknown => {
                    t.complete = false;
                    confirmed = false;
                }
                net::Switch::Missing => {
                    let (new, log) = lock_std(&self.watch).fail_note(&lbl, "missing");
                    if log {
                        error!(switch = %path,
                            "isolation watch: the switch file is missing (module unloaded or device gone) — it cannot be set");
                    }
                    if new {
                        failed_repairs += 1;
                    }
                    t.failed.push(lbl);
                    unrepaired = true;
                    confirmed = false;
                    broken = true;
                }
                net::Switch::Value(v) => {
                    // X11: warned once per episode, not every tick.
                    if !lock_std(&self.watch).is_failing(&lbl) {
                        warn!(switch = %path, value = %v, "isolation watch: drift — a switch this agent set to 1 is not; setting it again");
                    }
                    t.drift.push(lbl.clone());
                    k.write_switch(&path, "1");
                    match k.read_switch(&path) {
                        net::Switch::Value(v) if v == "1" => {
                            info!(switch = %path, "isolation watch: switch repaired and confirmed");
                            lock_std(&self.watch).recovered(&lbl);
                            t.repaired.push(lbl);
                            repairs += 1;
                        }
                        net::Switch::Unknown => {
                            t.complete = false;
                            confirmed = false;
                            unrepaired = true;
                            suspect = true;
                            t.unconfirmed.push(lbl);
                        }
                        _ => {
                            let (new, log) = lock_std(&self.watch).fail_note(&lbl, "cannot be set");
                            if log {
                                error!(switch = %path, "isolation watch: the switch could not be set again");
                            }
                            if new {
                                failed_repairs += 1;
                            }
                            t.failed.push(lbl);
                            unrepaired = true;
                            confirmed = false;
                            broken = true;
                        }
                    }
                }
            }
        }

        // ---- the taps ----
        // Taps positively seen without their pin this tick (X4: needed before
        // one is taken down again).
        let mut seen_unpinned: BTreeSet<String> = BTreeSet::new();
        if bridge_gone || late(&mut t) {
            // Nothing to look at, or no time left.
        } else {
            match k.bridge_ports() {
                None => t.complete = false,
                Some(ports) => {
                    let nd = if late(&mut t) { Listing::Unknown } else { k.list_table("netdev", "felucca") };
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
                    // Calibrate netdev on the chains the watch is about to check.
                    let nd_calib = match &nd {
                        Listing::Listed(text) => {
                            let names: Vec<String> = pins
                                .iter()
                                .filter(|p| p.2.is_some())
                                .map(|p| Chain::Tap(p.0.clone()).name())
                                .collect();
                            lock_std(&self.watch).calibrate("netdev", text, &names)
                        }
                        _ => {
                            let w = lock_std(&self.watch);
                            if w.uncalibratable.contains("netdev") {
                                Calib::Never
                            } else if w.calibrated.contains("netdev") {
                                Calib::Yes
                            } else {
                                Calib::NotYet
                            }
                        }
                    };
                    let tap_act = nd_calib == Calib::Yes;
                    let mut pending: Vec<(String, String, bool)> = Vec::new(); // (tap, want, was down)
                    for (tap, carrier, ip, mac) in pins {
                        if nd_calib == Calib::Never || late(&mut t) {
                            break;
                        }
                        let was_down = lock_std(&self.watch).taps_down.contains(&tap);
                        // No carrier: no process holds the tap, it carries
                        // nothing. That also skips a tap `ensure_pinned_tap`
                        // is setting up.
                        if !carrier && !was_down {
                            t.skipped_taps.push(tap);
                            continue;
                        }
                        // A record holds the slot but has no address: nothing
                        // to pin to (a sandbox being deleted).
                        let Some(rs) = ip.as_deref().and_then(|ip| net::antispoof_ruleset(&tap, ip, &mac)) else { continue };
                        let Some(want) = net::marker_of(&rs) else { continue };
                        let chain = Chain::Tap(tap.clone());
                        match seen(&nd, &chain) {
                            Seen::Unknown => {
                                t.complete = false;
                                continue;
                            }
                            Seen::Marker(h) if h == want => {
                                if was_down && !late(&mut t) && k.set_link(&tap, true) {
                                    self.tap_raised(&tap, &mut t);
                                }
                                continue;
                            }
                            look => {
                                seen_unpinned.insert(tap.clone());
                                let failing = lock_std(&self.watch).is_failing(&label(&chain));
                                if !was_down && !failing {
                                    warn!(table = "netdev felucca", chain = %chain.name(),
                                        found = %match &look { Seen::Marker(h) => format!("marker {h}, expected {want}"), _ => "no table, no chain, no marker at its head, or the table dormant".into() },
                                        "isolation watch: drift — the tap's anti-spoof chain is not what this agent pins; rebuilding it");
                                    t.drift.push(label(&chain));
                                }
                            }
                        }
                        if late(&mut t) {
                            break;
                        }
                        match k.apply(&rs) {
                            Applied::Committed => pending.push((tap, want, was_down)),
                            // Deleted between the port list and the repair:
                            // that is not a failure.
                            Applied::Refused(_) if k.link_exists(&tap) == Some(false) => {
                                seen_unpinned.remove(&tap);
                                lock_std(&self.watch).taps_down.remove(&tap);
                            }
                            Applied::Refused(line) => {
                                self.tap_unrepaired(k, &tap, was_down, enforce && tap_act, Some(&line), &mut t, &mut unrepaired, &mut failed_repairs);
                            }
                            Applied::Unknown => {
                                // X6: positively unpinned, outcome unknown.
                                t.complete = false;
                                unrepaired = true;
                                t.unconfirmed.push(label(&chain));
                            }
                        }
                    }
                    if !pending.is_empty() {
                        let nd2 = if late(&mut t) { Listing::Unknown } else { k.list_table("netdev", "felucca") };
                        for (tap, want, was_down) in pending {
                            let chain = Chain::Tap(tap.clone());
                            match seen(&nd2, &chain) {
                                Seen::Marker(h) if h == want => {
                                    seen_unpinned.remove(&tap);
                                    lock_std(&self.watch).recovered(&label(&chain));
                                    if !was_down {
                                        info!(table = "netdev felucca", chain = %chain.name(), "isolation watch: chain repaired and confirmed");
                                        t.repaired.push(label(&chain));
                                        repairs += 1;
                                    } else if !late(&mut t) && k.set_link(&tap, true) {
                                        self.tap_raised(&tap, &mut t);
                                    }
                                }
                                Seen::Unknown => {
                                    t.complete = false;
                                    unrepaired = true;
                                    t.unconfirmed.push(label(&chain));
                                }
                                _ => {
                                    if late(&mut t) {
                                        continue;
                                    }
                                    if k.link_exists(&tap) == Some(false) {
                                        seen_unpinned.remove(&tap);
                                        lock_std(&self.watch).taps_down.remove(&tap);
                                        continue;
                                    }
                                    self.tap_unrepaired(k, &tap, was_down, enforce && tap_act, None, &mut t, &mut unrepaired, &mut failed_repairs);
                                }
                            }
                        }
                    }
                    // W8, X4: what it holds down, raised by something else
                    // while its pin is positively still not back THIS tick,
                    // goes down again — budget permitting.
                    for p in &ports {
                        if late(&mut t) {
                            break;
                        }
                        let held = lock_std(&self.watch).taps_down.contains(&p.name);
                        if held
                            && p.up
                            && enforce
                            && tap_act
                            && seen_unpinned.contains(&p.name)
                            && !t.taps_raised.contains(&p.name)
                            && k.set_link(&p.name, false)
                            && lock_std(&self.watch).taps_relowered.insert(p.name.clone())
                        {
                            warn!(tap = %p.name, "isolation watch: a tap held down for want of its pin was raised by \
                                  something else; it is taken down again");
                        }
                    }
                    // Taps that are gone are no longer the watch's to raise,
                    // and their failures are over (X11).
                    let names: BTreeSet<&str> = ports.iter().map(|p| p.name.as_str()).collect();
                    let mut w = lock_std(&self.watch);
                    w.taps_down.retain(|t| names.contains(t.as_str()));
                    let still: BTreeSet<String> = w.taps_down.clone();
                    w.taps_relowered.retain(|t| still.contains(t));
                    w.failing.retain(|lbl, _| match lbl.strip_prefix("netdev felucca as_") {
                        Some(rest) => names.contains(rest.replace('_', "-").as_str()),
                        None => true,
                    });
                }
            }
        }

        // ---- the host fence (felucca-firewall's; reported, never repaired) ----
        let fence = if late(&mut t) {
            None
        } else {
            match k.list_table("inet", "felucca_host") {
                Listing::Listed(l) => Some(net::host_fence_in(&l)),
                Listing::Missing => Some(false),
                Listing::Unknown => {
                    t.complete = false;
                    None
                }
            }
        };

        // ---- the bridge ----
        // The route never runs with the watch's mutex held (X8).
        let route_again = |k: &dyn net::Kernel| {
            let before = lock_std(&self.watch).route_failed;
            let ok = ensure_guest_route(k, self.cidr, Some(before));
            lock_std(&self.watch).route_failed = !ok;
        };
        let bridge_was_down = lock_std(&self.watch).bridge_down;
        if bridge_gone {
            // Nothing to raise or lower (W15).
        } else if broken {
            if bridge_was_down || enforce {
                route_again(k);
            }
            if !enforce {
                let mut w = lock_std(&self.watch);
                let due = match w.report_warned {
                    None => true,
                    Some(at) => now.saturating_duration_since(at) >= WARN_EVERY,
                };
                if due {
                    w.report_warned = Some(now);
                    error!("isolation watch (report mode): a fence or switch could not be put back; the guest bridge is \
                            left up — GUESTS ARE NOT FENCED on this node");
                }
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
            } else if !late(&mut t) && k.link_up(net::BRIDGE_NAME) == Some(true) && k.set_link(net::BRIDGE_NAME, false) {
                // W8: raised by something else while still unfenced.
                let mut w = lock_std(&self.watch);
                if !w.bridge_relowered {
                    w.bridge_relowered = true;
                    warn!(bridge = net::BRIDGE_NAME, "isolation watch: the guest bridge held down for an unrepaired \
                          fence was raised by something else; it is taken down again");
                }
            }
        } else if bridge_was_down {
            route_again(k);
            if raise_after_repair(k, confirmed && t.complete) {
                let mut w = lock_std(&self.watch);
                w.bridge_down = false;
                w.bridge_relowered = false;
                t.bridge_raised = true;
                info!(bridge = net::BRIDGE_NAME, "isolation watch: every fence is confirmed again — the guest bridge is up again");
            }
        }

        // ---- the verdict ----
        let after = {
            let mut w = lock_std(&self.watch);
            if t.complete {
                w.last_complete = Some(now);
                w.unrepaired = unrepaired;
                w.broken = broken;
                w.suspect = suspect;
            } else {
                // Only what was positively seen may change it: a look that
                // failed changes nothing for up to `STALE`.
                w.unrepaired |= unrepaired;
                w.broken |= broken;
                w.suspect |= suspect;
            }
            if let Some(f) = fence {
                w.expect_host_fence(f);
                w.host_fence = f;
                let watched = w.host_fence_expected != Some(false);
                let due = match w.fence_warned {
                    None => true,
                    Some(at) => now.saturating_duration_since(at) >= WARN_EVERY,
                };
                if !f && watched && due {
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

    /// A held-down tap whose pin is back and that is up again.
    fn tap_raised(&self, tap: &str, t: &mut Tick) {
        let mut w = lock_std(&self.watch);
        w.taps_down.remove(tap);
        w.taps_relowered.remove(tap);
        w.recovered(&label(&Chain::Tap(tap.to_string())));
        info!(tap = %tap, "isolation watch: the tap is pinned again and back up");
        t.taps_raised.push(tap.to_string());
    }

    /// A tap whose anti-spoof chain positively could not be put back: taken
    /// down when `take_down` (enforce, netdev calibrated), else left up and
    /// reported.
    #[allow(clippy::too_many_arguments)]
    fn tap_unrepaired(
        &self,
        k: &dyn net::Kernel,
        tap: &str,
        was_down: bool,
        take_down: bool,
        nft: Option<&str>,
        t: &mut Tick,
        unrepaired: &mut bool,
        failed_repairs: &mut u64,
    ) {
        if was_down {
            // Still down; tried again next tick.
            return;
        }
        let chain = Chain::Tap(tap.to_string());
        let (new, log) = lock_std(&self.watch).fail_note(&label(&chain), nft.unwrap_or("applied, still not there"));
        if log {
            error!(table = "netdev felucca", chain = %chain.name(), nft = %nft.unwrap_or("-"), "isolation watch: the repair failed");
        }
        if new {
            *failed_repairs += 1;
        }
        t.failed.push(label(&chain));
        if take_down && k.set_link(tap, false) {
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
#[path = "watch_tests.rs"]
pub(crate) mod tests;
