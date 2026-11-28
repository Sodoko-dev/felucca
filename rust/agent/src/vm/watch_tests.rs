    use super::super::rule_fake::{add_vm, hold_rule_lock, install, FakeRules, RuleCall};
    use super::super::PinFailure;
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
        let at = Instant::now() + Duration::from_secs(61);
        let later = tokio::task::spawn_blocking(move || m.tick_work(ENFORCE, at, at + TICK_BUDGET))
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
        let t = w.mgr.watch_tick_within(ENFORCE, Duration::from_millis(50), TICK_BUDGET).await;
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
        // Changed on purpose (Z2): a worker that hands out no address says so.
        assert!(!w.mgr.isolation_ok(), "withheld slots must report isolation down");
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

    // ---- r7: the review's fixes (W1..W17) ----

    fn ops_since(w: &Worker, n: usize) -> Vec<String> {
        w.rules.ops()[n..].to_vec()
    }

    fn bridge_up(w: &Worker) -> bool {
        w.rules.link_up(net::BRIDGE_NAME) == Some(true)
    }

    #[tokio::test]
    async fn test_w1_the_self_check_reads_back_what_start_up_applied() {
        let w = worker().await;
        let n = w.rules.ops().len();
        assert_eq!(w.mgr.watch_self_check().await, SelfCheck::Readable);
        assert!(ops_since(&w, n).iter().all(|o| o.starts_with("list ")), "the self-check only looks");
        // A kernel that prints the marker in another form: the watch must not
        // run, and the chain and the line it printed are named. (A fresh
        // worker each time: a table, once calibrated, stays so — X3.)
        let w = worker().await;
        *lock_std(&w.rules.rewrite_listing) = Some(Box::new(|l: String| l.replace("counter packets 0 bytes 0 comment", "counter name \"m\" comment")));
        match w.mgr.watch_self_check().await {
            SelfCheck::Unreadable { chain, line } => {
                assert_eq!(chain, "ip felucca postrouting");
                assert!(line.starts_with("counter name \"m\" comment \"felucca:"), "{line}");
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(mode_after_self_check(ENFORCE, &w.mgr.watch_self_check().await), IsolationWatch::Off);
        // A kernel whose chain lines read otherwise (the chain not found).
        let w = worker().await;
        *lock_std(&w.rules.rewrite_listing) = Some(Box::new(|l: String| l.replace("\tchain postrouting {", "\tchain postrouting { # handle 2")));
        assert!(matches!(w.mgr.watch_self_check().await, SelfCheck::Unreadable { .. }));
        // Nothing was taken down by any of it.
        assert!(bridge_up(&w) && w.rules.link_up("hth-0") == Some(true));
        // A look that cannot be made: the watch starts, the ticks deal with it.
        let w = worker().await;
        w.rules.fail_looks.store(true, Ordering::SeqCst);
        assert_eq!(w.mgr.watch_self_check().await, SelfCheck::CouldNotLook);
        assert_eq!(mode_after_self_check(ENFORCE, &SelfCheck::CouldNotLook), ENFORCE);
        assert_eq!(mode_after_self_check(IsolationWatch::Report, &SelfCheck::Readable), IsolationWatch::Report);
        assert_eq!(mode_after_self_check(IsolationWatch::Off, &SelfCheck::Readable), IsolationWatch::Off);
    }

    #[tokio::test]
    async fn test_w2_a_second_look_that_cannot_be_made_takes_nothing_down() {
        let w = worker().await;
        tick(&w).await;
        break_it(&w.rules, Break::DeleteChain, "ip", "felucca", "forward");
        w.rules.looks_fail_after_apply.store(true, Ordering::SeqCst);
        // The flag counts applies already made by the set-up: reset it.
        w.rules.reset_applied();
        let t = tick(&w).await;
        assert_eq!(t.drift, vec!["ip felucca forward".to_string()]);
        assert_eq!(t.unconfirmed, vec!["ip felucca forward".to_string()], "{t:?}");
        assert!(!t.complete && !t.bridge_down && t.failed.is_empty(), "{t:?}");
        assert!(bridge_up(&w));
        assert!(lock_std(&w.mgr.watch).applied(&Chain::Forward), "the record stays Applied");
        // Changed on purpose (X5): positively missing, the repair not
        // confirmed — new guests are refused meanwhile (nothing goes down).
        assert!(lock_std(&w.mgr.watch).refusal().is_some());
        w.rules.looks_fail_after_apply.store(false, Ordering::SeqCst);
        assert!(quiet(&tick(&w).await), "the repair was in after all");
        assert!(lock_std(&w.mgr.watch).refusal().is_none());
    }

    #[tokio::test]
    async fn test_w2_a_repair_refused_takes_down_and_one_unknown_does_not() {
        for refused in [true, false] {
            let w = worker().await;
            tick(&w).await;
            break_it(&w.rules, Break::DeleteChain, "ip", "felucca", "forward");
            let list = if refused { &w.rules.reject } else { &w.rules.no_answer };
            lock_std(list).push("ip felucca forward".into());
            let t = tick(&w).await;
            if refused {
                assert!(t.bridge_down && t.failed == vec!["ip felucca forward".to_string()], "{t:?}");
                assert!(!bridge_up(&w));
            } else {
                assert!(!t.bridge_down && t.failed.is_empty() && !t.complete, "{t:?}");
                assert_eq!(t.unconfirmed, vec!["ip felucca forward".to_string()]);
                assert!(bridge_up(&w));
                // Retried quietly, and still nothing down while unknown.
                let t = tick(&w).await;
                assert!(!t.bridge_down, "{t:?}");
                assert!(bridge_up(&w));
            }
        }
    }

    fn spec(id: &str) -> crate::vm::CreateSpec {
        crate::vm::CreateSpec {
            id: id.into(),
            name: id.into(),
            vcpus: 1,
            mem_mib: 256,
            tenant_id: Some("t1".into()),
            image: crate::vm::DEFAULT_IMAGE.into(),
            image_sha256: None,
            disk_gb: 0,
        }
    }

    #[tokio::test]
    async fn test_w3_no_new_guest_while_a_fence_is_positively_unrepaired() {
        for mode in [IsolationWatch::Enforce, IsolationWatch::Report] {
            let w = worker().await;
            w.mgr.watch_tick(mode).await.unwrap();
            break_for_good(&w.rules, "ip", "felucca", "forward");
            w.mgr.watch_tick(mode).await.unwrap();
            assert!(lock_std(&w.mgr.watch).refusal().is_some(), "{mode:?}");
            w.rules.clear();
            // create, before any work: no instance directory, no image copy.
            assert_eq!(w.mgr.create(&spec("vm-new")).await, Err("IsolationFailed".into()), "{mode:?}");
            assert!(!std::path::Path::new(&format!("{}/instances/vm-new", w.mgr.data_dir)).exists());
            // start and wake (vm-a is stopped): refused before the pin.
            assert_eq!(w.mgr.start("vm-a").await, Err(super::super::OpError::Other("IsolationFailed".into())), "{mode:?}");
            // fork: before anything is reserved.
            assert_eq!(w.mgr.fork("vm-a", "vm-k", "k").await, Err("IsolationFailed".to_string()));
            assert!(w.rules.calls().iter().all(|c| !matches!(c, RuleCall::PinTap { .. })), "{mode:?}");
            // Sleep, stop, delete, expose and unexpose stay allowed.
            assert!(w.mgr.expose("vm-b", 81, &HashSet::new(), None).await.is_ok());
            assert!(w.mgr.unexpose("vm-b", 81).await.is_ok());
            // Repaired: allowed again.
            mend(&w.rules);
            w.mgr.watch_tick(mode).await.unwrap();
            assert!(lock_std(&w.mgr.watch).refusal().is_none(), "{mode:?}");
            assert_ne!(w.mgr.start("vm-a").await, Err(super::super::OpError::Other("IsolationFailed".into())));
        }
        // Not for the host fence, nor for "could not look".
        let w = worker().await;
        tick(&w).await;
        w.rules.k().chain_mut("inet", "felucca_host", "input").unwrap().rules.clear();
        tick(&w).await;
        assert!(lock_std(&w.mgr.watch).refusal().is_none());
        w.rules.fail_looks.store(true, Ordering::SeqCst);
        tick(&w).await;
        assert!(lock_std(&w.mgr.watch).refusal().is_none());
    }

    #[tokio::test]
    async fn test_w4_a_tick_stops_at_its_budget_and_takes_nothing_down() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "input");
        w.rules.slow_list_ms.store(40, Ordering::SeqCst);
        let m = Arc::clone(&w.mgr);
        let started = std::time::Instant::now();
        // 60 ms of budget: the first two listings use it up.
        let t = w.mgr.watch_tick_within(ENFORCE, LOCK_WAIT, Duration::from_millis(60)).await.unwrap();
        assert!(t.out_of_time && !t.complete, "{t:?}");
        assert!(started.elapsed() < Duration::from_secs(2), "took {:?}", started.elapsed());
        assert!(!t.bridge_down && bridge_up(&w), "{t:?}");
        drop(m);
    }

    #[tokio::test]
    async fn test_w7_a_dormant_table_is_a_fence_not_in_place() {
        // Changed on purpose (X14): a rebuild beginning with `add table`
        // clears the dormant flag (lab), so a dormant table is drift that the
        // ordinary rebuild repairs; only one that stays dormant after an
        // applied rebuild takes the bridge down.
        let w = worker().await;
        tick(&w).await;
        lock_std(&w.rules.dormant).insert(("ip".into(), "felucca".into()));
        let t = tick(&w).await;
        assert!(t.drift.contains(&"ip felucca forward".to_string()), "{t:?}");
        assert!(t.repaired.contains(&"ip felucca forward".to_string()), "{t:?}");
        assert!(!t.bridge_down && bridge_up(&w), "{t:?}");
        assert!(lock_std(&w.rules.dormant).is_empty(), "the rebuild cleared the flag");
        assert_eq!(verdict(&w), Some(true));
        // A flag that stays.
        w.rules.dormant_sticky.store(true, Ordering::SeqCst);
        lock_std(&w.rules.dormant).insert(("ip".into(), "felucca".into()));
        let t = tick(&w).await;
        assert!(t.bridge_down, "{t:?}");
        assert_eq!(verdict(&w), Some(false));
        assert!(lock_std(&w.mgr.watch).refusal().is_some());
        w.rules.dormant_sticky.store(false, Ordering::SeqCst);
        lock_std(&w.rules.dormant).clear();
        let t = tick(&w).await;
        assert!(t.bridge_raised, "{t:?}");
        assert_eq!(verdict(&w), Some(true));
        assert!(lock_std(&w.mgr.watch).dormant.is_empty(), "X11: cleared once it is not dormant");
    }

    #[tokio::test]
    async fn test_w8_what_the_watch_holds_down_is_taken_down_again_if_raised() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_0");
        let t = tick(&w).await;
        assert!(t.bridge_down, "{t:?}");
        // Something else raises the bridge and the tap.
        w.rules.k().links.get_mut(net::BRIDGE_NAME).unwrap().up = true;
        tick(&w).await;
        assert!(!bridge_up(&w), "raised by someone else while unfenced: down again");
        // The tap: down (the bridge took everything down, but its own hold is
        // the watch's too once its chain fails).
        mend(&w.rules);
        lock_std(&w.rules.reject).push("as_hth_0".into());
        tick(&w).await;
        w.rules.k().links.get_mut("hth-0").unwrap().up = true;
        tick(&w).await;
        assert_eq!(w.rules.link_up("hth-0"), Some(false), "a held-down tap raised by someone else: down again");
        mend(&w.rules);
        let t = tick(&w).await;
        assert_eq!(t.taps_raised, vec!["hth-0".to_string()], "{t:?}");
    }

    #[tokio::test]
    async fn test_w8_taps_start_up_took_down_are_pinned_and_raised_by_the_watch() {
        let w = worker().await;
        tick(&w).await;
        // As `reconcile` leaves one it could not pin: down, handed over.
        w.rules.k().links.get_mut("hth-1").unwrap().up = false;
        lock_std(&w.mgr.watch).hold_tap_down("hth-1");
        let t = tick(&w).await;
        assert_eq!(t.taps_raised, vec!["hth-1".to_string()], "{t:?}");
        assert_eq!(w.rules.link_up("hth-1"), Some(true));
    }

    #[tokio::test]
    async fn test_w9_the_route_is_reasserted_while_the_bridge_is_down() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        let n = w.rules.ops().len();
        tick(&w).await;
        let ops = ops_since(&w, n);
        let route = ops.iter().position(|o| o == "route 10.231.0.0/24").expect("re-asserted");
        let down = ops.iter().position(|o| o == "link felucca0 down").expect("taken down");
        assert!(route < down, "{ops:?}");
        let n = w.rules.ops().len();
        tick(&w).await;
        assert!(ops_since(&w, n).contains(&"route 10.231.0.0/24".to_string()), "and on every tick while down");
    }

    #[tokio::test]
    async fn test_w10_no_tick_ever_completing_is_false_and_a_panicking_tick_is_survived() {
        let w = worker().await;
        let now = Instant::now();
        lock_std(&w.mgr.watch).mark_started(now);
        assert_eq!(lock_std(&w.mgr.watch).verdict(now + STALE / 2), None);
        assert_eq!(lock_std(&w.mgr.watch).verdict(now + STALE + Duration::from_secs(1)), Some(false));
        // A tick that panics: nothing completes, the next one runs.
        w.rules.panic_on_list.store(true, Ordering::SeqCst);
        assert!(tick_opt(&w).await.is_none());
        assert!(lock_std(&w.mgr.watch).verdict(now + STALE + Duration::from_secs(1)) == Some(false));
        let t = tick(&w).await;
        assert!(t.complete, "{t:?}");
        assert_eq!(verdict(&w), Some(true));
    }

    async fn tick_opt(w: &Worker) -> Option<Tick> {
        w.mgr.watch_tick(ENFORCE).await
    }

    #[tokio::test]
    async fn test_w11_withheld_slots_are_retried_in_every_mode_and_refused_before_any_work() {
        let w = worker().await;
        w.rules.fail_ingress.store(true, Ordering::SeqCst);
        lock_std(&w.rules.reject).push("ip felucca ingress".into());
        assert!(!w.mgr.start_ingress().await);
        assert_eq!(w.mgr.create(&spec("vm-new")).await, Err("IsolationFailed".into()));
        assert!(!std::path::Path::new(&format!("{}/instances/vm-new", w.mgr.data_dir)).exists());
        w.rules.fail_ingress.store(false, Ordering::SeqCst);
        mend(&w.rules);
        // No watch at all (`off`): the retry loop alone.
        let retry = tokio::spawn(withheld_retry_loop(Arc::clone(&w.mgr)));
        let mut cleared = false;
        for _ in 0..100 {
            if !w.mgr.inner.lock().await.slots_withheld {
                cleared = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        retry.abort();
        assert!(cleared, "the retry never rebuilt the ingress chain");
    }

    #[tokio::test]
    async fn test_w12_a_failure_repeated_tick_after_tick_is_counted_and_warned_once() {
        let w = worker().await;
        let report = IsolationWatch::Report;
        w.mgr.watch_tick(report).await.unwrap();
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_0");
        let first = w.mgr.watch_tick(report).await.unwrap();
        assert_eq!(first.drift, vec!["netdev felucca as_hth_0".to_string()]);
        for _ in 0..3 {
            let t = w.mgr.watch_tick(report).await.unwrap();
            assert!(t.drift.is_empty(), "warned again: {t:?}");
        }
        assert_eq!(counters(&w).failed_repairs, 1, "counted once per episode");
        // A chain whose retry keeps failing: once too.
        break_for_good(&w.rules, "ip", "felucca", "postrouting");
        for _ in 0..3 {
            w.mgr.watch_tick(report).await.unwrap();
        }
        assert_eq!(counters(&w).failed_repairs, 2);
    }

    #[tokio::test]
    async fn test_w15_a_deleted_bridge_is_reported_once_and_left_alone() {
        let w = worker().await;
        tick(&w).await;
        w.rules.k().links.remove(net::BRIDGE_NAME);
        let n = w.rules.ops().len();
        for _ in 0..3 {
            tick(&w).await;
        }
        assert_eq!(verdict(&w), Some(false));
        assert!(!ops_since(&w, n).iter().any(|o| o.starts_with("link felucca0") || o.starts_with("route")), "{:?}", ops_since(&w, n));
    }

    #[tokio::test]
    async fn test_w16_a_host_fence_absent_at_start_does_not_enter_the_verdict() {
        let w = worker().await;
        w.rules.k().tables.remove(&("inet".to_string(), "felucca_host".to_string()));
        assert_eq!(w.mgr.watch_self_check().await, SelfCheck::Readable);
        let t = tick(&w).await;
        assert!(!t.fence_warning, "{t:?}");
        assert_eq!(verdict(&w), Some(true), "no firewall unit here: not unfenced for ever");
    }

    // ---- r8: the second review's fixes (X2..X14) ----

    #[tokio::test]
    async fn test_x2_nat_and_ingress_never_make_a_tick_incomplete() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        assert!(tick(&w).await.bridge_down);
        // The fence is mended; NAT gets no answer and ingress is refused.
        mend(&w.rules);
        break_it(&w.rules, Break::DeleteChain, "ip", "felucca", "postrouting");
        lock_std(&w.rules.no_answer).push("ip felucca postrouting".into());
        break_for_good(&w.rules, "ip", "felucca", "ingress");
        let t = tick(&w).await;
        assert!(t.complete, "NAT or ingress made the tick incomplete: {t:?}");
        assert!(t.bridge_raised, "the bridge must come back: {t:?}");
        assert_eq!(verdict(&w), Some(true), "neither enters isolation");
        // And for 45 s and more: still true while they keep failing.
        tick(&w).await;
        assert_eq!(verdict(&w), Some(true));
    }

    #[tokio::test]
    async fn test_x3_nothing_is_taken_down_from_a_table_not_yet_read_back() {
        // No marker of the ip table has ever been read back: the whole table
        // is gone before the first tick, and the forward repair is refused.
        let w = worker().await;
        w.rules.k().tables.remove(&("ip".to_string(), "felucca".to_string()));
        lock_std(&w.rules.reject).push("ip felucca forward".into());
        let t = tick(&w).await;
        assert!(!t.bridge_down && bridge_up(&w), "{t:?}");
        assert!(lock_std(&w.mgr.watch).refusal().is_none(), "no refusal from an uncalibrated table");
        // Once a marker of that table has been read back, it acts.
        mend(&w.rules);
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        assert!(tick(&w).await.bridge_down);
    }

    #[tokio::test]
    async fn test_x3_a_table_whose_markers_do_not_read_is_left_alone() {
        let w = worker().await;
        // This kernel prints ip markers in a form this build does not read.
        *lock_std(&w.rules.rewrite_listing) =
            Some(Box::new(|l: String| if l.starts_with("table ip ") { l.replace("counter packets 0 bytes 0 comment", "counter name \"m\" comment") } else { l }));
        let n = w.rules.ops().len();
        let t = tick(&w).await;
        assert!(t.drift.iter().all(|d| !d.starts_with("ip ")), "the ip table was acted on: {t:?}");
        assert!(!ops_since(&w, n).iter().any(|o| o.starts_with("apply ip felucca")));
        assert!(!t.bridge_down && bridge_up(&w));
        // Left alone for the process, even when its chains are then deleted.
        break_for_good(&w.rules, "ip", "felucca", "forward");
        assert!(!tick(&w).await.bridge_down);
        // The bridge table is still watched.
        break_it(&w.rules, Break::Flush, "bridge", "felucca", "forward");
        assert_eq!(tick(&w).await.repaired, vec!["bridge felucca forward".to_string()]);
        // And the netdev table too, calibrated on its own.
        let w = worker().await;
        w.rules.k().tables.remove(&("netdev".to_string(), "felucca".to_string()));
        lock_std(&w.rules.reject).push("as_hth_0".into());
        let t = tick(&w).await;
        assert!(t.taps_down.is_empty() && w.rules.link_up("hth-0") == Some(true), "{t:?}");
    }

    #[tokio::test]
    async fn test_x4_a_held_tap_goes_down_again_only_on_a_positive_look() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_0");
        assert_eq!(tick(&w).await.taps_down, vec!["hth-0".to_string()]);
        // A start pins and raises it meanwhile (as the fake's pin does)…
        mend(&w.rules);
        let ip = crate::ipalloc::fmt_ip(w.mgr.cidr.host_at(0));
        assert!(w.mgr.rules.pin_tap("hth-0", Some(&ip), &net::guest_mac_for_slot(0)));
        // …and this tick cannot look at the netdev table (the ports it can
        // list): it must not go down again.
        lock_std(&w.rules.unknown_tables).insert(("netdev".into(), "felucca".into()));
        tick(&w).await;
        assert_eq!(w.rules.link_up("hth-0"), Some(true), "taken down on 'held and up' alone");
        // The pin sites hand it back (`release_tap`).
        lock_std(&w.mgr.watch).hold_tap_down("hth-9");
        lock_std(&w.mgr.watch).release_tap("hth-9");
        assert!(!lock_std(&w.mgr.watch).taps_down.contains("hth-9"));
    }

    #[test]
    fn test_x4_every_pin_site_releases_the_held_tap() {
        let src = include_str!("mod.rs");
        let prod = src.split("#[cfg(test)]").next().unwrap();
        let sites: Vec<usize> = prod.match_indices("if !self.rules.pin_tap(").map(|(i, _)| i).collect();
        assert_eq!(sites.len(), 4, "create, wake, fork, start");
        for at in sites {
            let after = &prod[at..at + 700];
            assert!(after.contains("release_tap(&tap)"), "a pin site does not release the held tap: {}", &after[..120]);
        }
    }

    #[tokio::test]
    async fn test_x5_new_guests_are_refused_when_the_bridge_is_gone_or_a_repair_is_unconfirmed() {
        let w = worker().await;
        tick(&w).await;
        w.rules.k().links.remove(net::BRIDGE_NAME);
        tick(&w).await;
        assert_eq!(w.mgr.create(&spec("vm-new")).await, Err("IsolationFailed".into()));
        assert!(!std::path::Path::new(&format!("{}/instances/vm-new", w.mgr.data_dir)).exists());
        // A fence positively missing whose repair gets no answer.
        let w = worker().await;
        tick(&w).await;
        break_it(&w.rules, Break::DeleteChain, "ip", "felucca", "input");
        lock_std(&w.rules.no_answer).push("ip felucca input".into());
        let t = tick(&w).await;
        assert!(!t.bridge_down, "{t:?}");
        assert_eq!(w.mgr.create(&spec("vm-new")).await, Err("IsolationFailed".into()));
    }

    #[tokio::test]
    async fn test_x6_a_tap_unpinned_with_an_unknown_repair_is_not_isolated() {
        let w = worker().await;
        tick(&w).await;
        break_it(&w.rules, Break::DeleteChain, "netdev", "felucca", "as_hth_0");
        lock_std(&w.rules.no_answer).push("as_hth_0".into());
        tick(&w).await;
        assert_eq!(verdict(&w), Some(false));
        assert_eq!(w.rules.link_up("hth-0"), Some(true), "nothing down on an unknown");
    }

    #[tokio::test]
    async fn test_x7_a_tick_with_no_budget_runs_no_command() {
        let w = worker().await;
        tick(&w).await;
        let n = w.rules.ops().len();
        let t = w.mgr.watch_tick_within(ENFORCE, LOCK_WAIT, Duration::ZERO).await.unwrap();
        assert!(t.out_of_time);
        assert!(ops_since(&w, n).is_empty(), "{:?}", ops_since(&w, n));
    }

    #[tokio::test]
    async fn test_x8_the_route_is_never_installed_with_the_watch_mutex_held() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        tick(&w).await;
        tick(&w).await;
        assert!(w.rules.ops().iter().any(|o| o.starts_with("route ")));
        assert_eq!(w.rules.route_under_watch_lock.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn test_x9_a_failed_expose_leaves_no_doomed_mapping_on_disk() {
        let w = worker().await;
        let meta = format!("{}/instances/vm-b/meta.json", w.mgr.data_dir);
        // Another write of the record lands while the rebuild runs, with the
        // doomed mapping in it.
        let m2 = meta.clone();
        *lock_std(&w.rules.on_ingress_rebuild) = Some(Box::new(move || {
            std::fs::write(&m2, "{\"doomed\": 23456}").unwrap();
        }));
        w.rules.fail_ingress.store(true, Ordering::SeqCst);
        assert!(w.mgr.expose("vm-b", 90, &HashSet::new(), Some(23456)).await.is_err());
        let on_disk = std::fs::read_to_string(&meta).unwrap();
        assert!(!on_disk.contains("23456"), "{on_disk}");
    }

    #[tokio::test]
    async fn test_x11_a_removed_taps_failure_is_forgotten() {
        let w = worker().await;
        let report = IsolationWatch::Report;
        w.mgr.watch_tick(report).await.unwrap();
        break_for_good(&w.rules, "netdev", "felucca", "as_hth_1");
        w.mgr.watch_tick(report).await.unwrap();
        assert_eq!(counters(&w).failed_repairs, 1);
        // The tap goes (a delete); a new one under the same name fails again:
        // counted again.
        w.rules.k().links.remove("hth-1");
        w.mgr.watch_tick(report).await.unwrap();
        w.rules.add_port("hth-1", true);
        w.mgr.watch_tick(report).await.unwrap();
        assert_eq!(counters(&w).failed_repairs, 2, "the old episode hid the new one");
    }

    #[tokio::test]
    async fn test_x12_reconcile_hands_taps_it_could_not_pin_to_the_watch() {
        let w = worker().await;
        let taps = vec![("vm-a".to_string(), 0u32, crate::ipalloc::fmt_ip(w.mgr.cidr.host_at(0)))];
        for why in [PinFailure::NotSetUp, PinFailure::Unpinned] {
            *lock_std(&w.rules.adopted_pin_fails) = Some(why);
            w.rules.k().links.get_mut("hth-0").unwrap().up = true;
            assert_eq!(w.mgr.repin_adopted(&taps), 1);
            assert!(lock_std(&w.mgr.watch).taps_down.contains("hth-0"), "{why:?}");
            // Taken down only when it was pinned-but-refused, not when it
            // could not be set up at all.
            assert_eq!(w.rules.link_up("hth-0"), Some(why != PinFailure::Unpinned), "{why:?}");
        }
        *lock_std(&w.rules.adopted_pin_fails) = None;
        assert_eq!(w.mgr.repin_adopted(&taps), 0);
        // The watch then pins and raises it.
        w.rules.k().links.get_mut("hth-0").unwrap().up = false;
        break_it(&w.rules, Break::DeleteChain, "netdev", "felucca", "as_hth_0");
        let t = tick(&w).await;
        assert_eq!(t.taps_raised, vec!["hth-0".to_string()], "{t:?}");
    }

    #[tokio::test]
    async fn test_x13_refusals_reach_wake_pool_refill_and_the_commit() {
        let w = worker().await;
        tick(&w).await;
        break_for_good(&w.rules, "ip", "felucca", "forward");
        tick(&w).await;
        assert!(lock_std(&w.mgr.watch).refusal().is_some());
        // wake (a sleeping sandbox): refused before anything is spawned.
        {
            let mut g = w.mgr.inner.lock().await;
            let v = g.vms.iter_mut().find(|v| v.id == "vm-a").unwrap();
            v.state = super::super::VmState::Sleeping;
            v.snapshot_valid = true;
        }
        assert_eq!(w.mgr.wake_vm("vm-a").await, Err(super::super::OpError::Other("IsolationFailed".into())));
        // pool refill: no pool VM is made.
        w.mgr
            .set_pools(vec![crate::vm::PoolSpec {
                image: crate::vm::DEFAULT_IMAGE.into(),
                image_sha256: String::new(),
                vcpus: 1,
                mem_mib: 256,
                disk_gb: 0,
                count: 1,
            }])
            .await
            .unwrap();
        w.mgr.refill_pool().await;
        assert!(w.mgr.inner.lock().await.vms.iter().all(|v| !v.id.starts_with("pool-")));
        // The re-check where a new VM is committed: it is taken back out.
        add_vm(&w.mgr, "vm-c", 5, "t1").await;
        assert_eq!(w.mgr.commit_or_isolate("vm-c").await, Err("IsolationFailed".into()));
        assert!(w.mgr.inner.lock().await.vms.iter().all(|v| v.id != "vm-c"));
    }

    #[tokio::test]
    async fn test_x13_a_create_on_the_slot_of_a_sandbox_still_being_deleted_is_watched_by_its_own_record() {
        // The delete is parked between its step 1 (slot off the record) and
        // the removal of the record; the slot is handed to a new sandbox
        // meanwhile. The watch must pin the new sandbox's tap from the NEW
        // record — it used to find the dying record (no address) first and
        // skip the tap.
        let w = worker().await;
        tick(&w).await;
        let held = hold_rule_lock(&w.mgr).await;
        let deleting = {
            let m = Arc::clone(&w.mgr);
            tokio::spawn(async move { m.delete("vm-b").await })
        };
        // The delete is now past step 1, waiting for the rule lock.
        let mut parked = false;
        for _ in 0..200 {
            if w.mgr.inner.lock().await.vms.iter().any(|v| v.id == "vm-b" && v.ip.is_none()) {
                parked = true;
                break;
            }
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        assert!(parked);
        // A new sandbox on slot 1 (its address), with a VMM on the tap.
        let new_ip = crate::ipalloc::fmt_ip(w.mgr.cidr.host_at(1));
        w.mgr.inner.lock().await.vms.push(super::super::Vm {
            id: "vm-new".into(),
            name: "vm-new".into(),
            dir_id: "vm-new".into(),
            vcpus: 1,
            mem_mib: 256,
            state: super::super::VmState::Running,
            pid: None,
            slot: Some(1),
            ip: Some(new_ip.clone()),
            vsock: false,
            tenant_id: Some("t9".into()),
            exposes: Vec::new(),
            image: crate::vm::DEFAULT_IMAGE.into(),
            image_sha256: None,
            disk_gb: 0,
            snapshot_valid: false,
        });
        break_it(&w.rules, Break::DeleteChain, "netdev", "felucca", "as_hth_1");
        let m = Arc::clone(&w.mgr);
        let at = Instant::now();
        let t = tokio::task::spawn_blocking(move || m.tick_work(ENFORCE, at, at + TICK_BUDGET)).await.unwrap();
        assert_eq!(t.repaired, vec!["netdev felucca as_hth_1".to_string()], "{t:?}");
        let rules = w.rules.k().chain_mut("netdev", "felucca", "as_hth_1").unwrap().rules.clone();
        assert!(rules.contains(&format!("ip saddr {} accept", new_ip)), "{rules:?}");
        drop(held);
        let _ = deleting.await;
    }

    // ---- r10 ----

    #[tokio::test]
    async fn test_z2_withheld_slots_report_isolation_down_in_every_mode() {
        for mode in [IsolationWatch::Off, IsolationWatch::Enforce] {
            let w = worker().await;
            if mode == ENFORCE {
                tick(&w).await;
            }
            assert!(w.mgr.isolation_ok(), "{mode:?}");
            w.rules.fail_ingress.store(true, Ordering::SeqCst);
            lock_std(&w.rules.reject).push("ip felucca ingress".into());
            assert!(!w.mgr.start_ingress().await);
            assert!(!w.mgr.isolation_ok(), "{mode:?}: no guest address is handed out, and isolation said true");
            // The healthz body and the heartbeat carry the same value.
            assert!(crate::registration::heartbeat_body("n", 0, 0, 0, w.mgr.isolation_ok()).ends_with("\"isolation_ok\":false}"));
            if mode == ENFORCE {
                tick(&w).await;
                assert!(!w.mgr.isolation_ok(), "a tick must not hide it");
            }
            // The cause goes; the retry rebuilds the chain: true again.
            w.rules.fail_ingress.store(false, Ordering::SeqCst);
            mend(&w.rules);
            if mode == ENFORCE {
                tick(&w).await;
            } else {
                let retry = tokio::spawn(withheld_retry_loop(Arc::clone(&w.mgr)));
                for _ in 0..100 {
                    if w.mgr.isolation_ok() {
                        break;
                    }
                    tokio::time::sleep(Duration::from_millis(20)).await;
                }
                retry.abort();
            }
            assert!(w.mgr.isolation_ok(), "{mode:?}: still down after the rebuild");
        }
    }
