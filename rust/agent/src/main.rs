//! hearth-agent — node agent in Rust.
//!
//! Registers with the control plane, heartbeats every 5s, and manages
//! Firecracker microVMs over its local REST API (0.0.0.0:<port>).
//! Port of backend/src/agent/main.zig, 1:1, no feature creep.

mod config;
mod fc;
mod guestclient;
mod ipalloc;
mod net;
mod registration;
mod server;
mod vm;
mod wg;

use crate::ipalloc::Cidr;
use crate::registration::{detect_advertise_addr, heartbeat_loop, NodeId};
use crate::server::{build_router, AppState};
use crate::vm::{pool::{liveness_loop, pool_loop}, Manager};
use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use tokio::net::TcpListener;

#[tokio::main]
async fn main() {
    let args: Vec<String> = std::env::args().collect();
    let env: HashMap<String, String> = std::env::vars().collect();

    let mut cfg = config::load(&args, &env);

    // WireGuard overlay (v4 P2): a persisted enrollment reconfigures the
    // tunnel on every boot; --join + --join-token performs first enrollment.
    if cfg.join_url.is_empty() != cfg.join_token.is_empty() {
        // Half-configured join is an operator mistake — silently skipping it
        // would register this node off-overlay while they believe it joined.
        eprintln!("fatal: --join and --join-token must be set together");
        std::process::exit(1);
    }
    // Err = wg.json exists but is unreadable/corrupt. Fatal here, not a
    // fallback: the file's presence proves enrollment, and starting in
    // direct mode instead would register the wrong address with the hub.
    let wg_state = match wg::load_state(&cfg.data_dir) {
        Ok(s) => s,
        Err(e) => {
            eprintln!("fatal: {}", e);
            std::process::exit(1);
        }
    };
    let joined: Option<wg::JoinInfo> = if let Some(info) = wg_state {
        if !cfg.join_url.is_empty() {
            eprintln!(
                "warn: persisted wg enrollment ({}) takes precedence over --join; \
                 delete it to re-enroll with a fresh token",
                wg::state_path(&cfg.data_dir)
            );
        }
        Some(info)
    } else if !cfg.join_url.is_empty() && !cfg.join_token.is_empty() {
        // First join: ensure a keypair, enroll, persist the grant.
        let pubkey = match wg::ensure_key(&wg::key_path(&cfg.data_dir)) {
            Ok(pk) => pk,
            Err(e) => {
                eprintln!("fatal: wg key: {}", e);
                std::process::exit(1);
            }
        };
        let hostname = registration::read_hostname();
        // Bounded: a hub that accepts the TCP connection but stalls must not
        // wedge boot forever (no API server, no heartbeat, no diagnostics).
        let join_result = tokio::time::timeout(
            std::time::Duration::from_secs(30),
            wg::join(&cfg.join_url, &cfg.join_token, &pubkey, &hostname),
        )
        .await
        .unwrap_or_else(|_| Err("timed out after 30s".into()));
        match join_result {
            Ok(info) => {
                if let Err(e) = wg::save_state(&cfg.data_dir, &info) {
                    // Fail NOW, while the operator is watching: the token is
                    // consumed, and without wg.json the next boot would
                    // crash-loop on a burned token weeks later instead.
                    eprintln!("fatal: wg joined but state not persisted: {}", e);
                    std::process::exit(1);
                }
                Some(info)
            }
            Err(e) => {
                if e.contains("status 401") {
                    eprintln!(
                        "fatal: wg join: {} (this node's key file may already be \
                         enrolled — mint a fresh token, or restore wg.json)",
                        e
                    );
                } else {
                    eprintln!("fatal: wg join: {}", e);
                }
                std::process::exit(1);
            }
        }
    } else {
        None
    };
    if let Some(info) = &joined {
        wg::validate_join_info(info).unwrap_or_else(|e| {
            eprintln!("fatal: wg state invalid: {}", e);
            std::process::exit(1);
        });
        if let Err(e) = wg::ensure_interface(info, &wg::key_path(&cfg.data_dir)) {
            eprintln!("fatal: wg interface: {}", e);
            std::process::exit(1);
        }
        // Route control-plane traffic over the overlay; advertise our overlay
        // IP so hearthd reaches this agent through the tunnel. The hub's API
        // port was recorded in wg.json at enrollment (from the join URL), so
        // it survives reboots after the one-time join flags are removed.
        cfg.control_plane = format!("http://{}:{}", info.server_overlay_ip, info.api_port);
        cfg.advertise_addr = info.overlay_ip.clone();
        eprintln!(
            "info: wg overlay joined: {} -> {} ({})",
            info.overlay_ip, info.server_overlay_ip, info.server_endpoint
        );
    }

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

    // control_plane/token also drive image pulls (v4 P4); cfg.control_plane
    // is final here (the wg overlay rewrite above already happened).
    let mgr = Manager::new(
        cfg.data_dir.clone(),
        cfg.net,
        cidr,
        cfg.pool_size,
        cfg.control_plane.clone(),
        cfg.token.clone(),
    );

    // Host networking + IP allocator.
    mgr.setup_host().await;

    // Reconcile persisted instances, then rebuild tenant isolation for
    // adopted VMs (the nft sets don't survive an agent-host reboot).
    mgr.reconcile().await;
    mgr.refresh_isolation().await;
    // Ingress DNAT rules don't survive a host reboot either; rebuild them
    // from the exposes persisted in meta.json.
    mgr.refresh_ingress().await;

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

    // Async pool refill. Always spawned (v4 P4): even with pool_size == 0,
    // hearthd can push template pools at runtime via PUT /v1/pools.
    {
        let mgr3 = Arc::clone(&mgr);
        tokio::spawn(async move {
            pool_loop(mgr3).await;
        });
    }

    // FC liveness sweep (adopted FCs have no in-process reaper).
    {
        let mgr4 = Arc::clone(&mgr);
        tokio::spawn(async move {
            liveness_loop(mgr4).await;
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
