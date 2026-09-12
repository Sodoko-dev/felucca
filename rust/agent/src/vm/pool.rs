//! Warm pool management — port of Manager.refillPool / Manager.claimFromPool in vm.zig.
//!
//! Background task every 5s tops every pool up to its target: the legacy
//! config pool (pool_size generic 1 vcpu/256 MiB ubuntu-base VMs) plus the
//! feluccad-managed template pools (v4 P4, PUT /v1/pools). All pooled VMs are
//! state "pooled" on disk with names pool-<hex-ts>.

use std::sync::Arc;
use tokio::time::{sleep, Duration};

use super::Manager;

/// Spawn the background pool-refill task. Runs every 5 seconds.
pub async fn pool_loop(mgr: Arc<Manager>) {
    let mut tick: u64 = 0;
    loop {
        mgr.refill_pool().await;
        // Image-cache GC (v4 P5.4) piggybacks here: every 120 ticks (~10 min),
        // first sweep one full interval after start so reconcile and feluccad's
        // post-register pool push have long since landed. The hour age gate
        // keeps anything recently pulled or published out of reach.
        tick += 1;
        if tick % 120 == 0 {
            mgr.gc_images(3600).await;
        }
        sleep(Duration::from_secs(5)).await;
    }
}

/// FC liveness sweep every 5 seconds. In-process reapers already catch child
/// exits; this covers FCs adopted after an agent restart (reparented to init,
/// no reaper task exists for them). Runs regardless of pool size.
pub async fn liveness_loop(mgr: Arc<Manager>) {
    loop {
        mgr.sweep_dead().await;
        sleep(Duration::from_secs(5)).await;
    }
}
