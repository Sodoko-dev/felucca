//! hearth-agent — node agent in Rust.
//!
//! Registers with the control plane, heartbeats every 5s, and manages
//! Firecracker microVMs over its local REST API (0.0.0.0:<port>).
//! Port of backend/src/agent/main.zig, 1:1, no feature creep.

mod config;
mod fc;
mod ipalloc;
mod net;
mod registration;
mod server;
mod vm;

use crate::ipalloc::Cidr;
use crate::registration::{detect_advertise_addr, heartbeat_loop, NodeId};
use crate::server::{build_router, AppState};
use crate::vm::{pool::pool_loop, Manager};
use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use tokio::net::TcpListener;

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let env: HashMap<String, String> = std::env::vars().collect();

    let mut cfg = config::load(&args, &env);

    // Auto-detect advertise_addr when unset.
    if cfg.advertise_addr.is_empty() {
        cfg.advertise_addr = detect_advertise_addr(&cfg.control_plane)
            .unwrap_or_else(|| "127.0.0.1".to_string());
    }

    let cidr = Cidr::parse(&cfg.net_cidr)
        .unwrap_or_else(|| Cidr::parse("10.231.0.0/24").unwrap());

    eprintln!(
        "info: hearth-agent: control_plane={} data_dir={} advertise={} port={} net={} cidr={} pool={} auth={}",
        cfg.control_plane, cfg.data_dir, cfg.advertise_addr, cfg.port,
        if cfg.net { "on" } else { "off" },
        cfg.net_cidr, cfg.pool_size,
        if !cfg.token.is_empty() { "on" } else { "off" },
    );

    let mgr = Manager::new(cfg.data_dir.clone(), cfg.net, cidr, cfg.pool_size);

    // Host networking + IP allocator.
    mgr.setup_host().await;

    // Reconcile persisted instances.
    mgr.reconcile().await;

    // Background registration + heartbeat loop.
    let node_id: NodeId = Arc::new(RwLock::new(String::new()));
    {
        let mgr2 = Arc::clone(&mgr);
        let cp = cfg.control_plane.clone();
        let aa = cfg.advertise_addr.clone();
        let tok = cfg.token.clone();
        let nid = Arc::clone(&node_id);
        tokio::spawn(async move {
            heartbeat_loop(mgr2, cp, aa, cfg.port, tok, nid).await;
        });
    }

    // Async pool refill.
    if cfg.pool_size > 0 {
        let mgr3 = Arc::clone(&mgr);
        tokio::spawn(async move {
            pool_loop(mgr3).await;
        });
    }

    let state = AppState {
        mgr: Arc::clone(&mgr),
        token: cfg.token.clone(),
    };
    let app = build_router(state);

    let bind_addr = format!("0.0.0.0:{}", cfg.port);
    let listener = TcpListener::bind(&bind_addr).await
        .unwrap_or_else(|e| panic!("bind {}: {}", bind_addr, e));
    eprintln!("info: listening on {}", bind_addr);

    axum::serve(listener, app).await
        .unwrap_or_else(|e| panic!("serve: {}", e));
}
