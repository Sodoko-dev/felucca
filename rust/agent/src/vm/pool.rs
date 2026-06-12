//! Warm pool management — port of Manager.refillPool / Manager.claimFromPool in vm.zig.
//!
//! Background task every 5s tops every pool up to its target: the legacy
//! config pool (pool_size generic 1 vcpu/256 MiB ubuntu-base VMs) plus the
//! hearthd-managed template pools (v4 P4, PUT /v1/pools). All pooled VMs are
//! state "pooled" on disk with names pool-<hex-ts>.

use std::sync::Arc;
use tokio::time::{sleep, Duration};

use super::Manager;

/// Spawn the background pool-refill task. Runs every 5 seconds.
pub async fn pool_loop(mgr: Arc<Manager>) {
    loop {
        mgr.refill_pool().await;
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
