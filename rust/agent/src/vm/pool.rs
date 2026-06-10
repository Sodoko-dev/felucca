//! Warm pool management — port of Manager.refillPool / Manager.claimFromPool in vm.zig.
//!
//! Background task every 5s tops the pool up to pool_size paused generic VMs
//! (1 vcpu/256 MiB, base rootfs, state "pooled" on disk, names pool-<hex-ts>).

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
