//! felucca-agent — node agent in Rust.
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
use crate::registration::{
    adopt_node_token, detect_advertise_addr, heartbeat_loop, NodeId, NodeToken,
};
use crate::server::{build_router, AppState};
use crate::vm::{pool::{liveness_loop, pool_loop}, Manager};
use std::collections::HashMap;
use std::sync::{Arc, RwLock};
use tokio::net::TcpListener;
use tracing::{error, info, warn};

/// Unconditional fatal diagnostic. Bypasses the RUST_LOG filter deliberately:
/// every caller exits(1) next, and a filtered journal on an exit path would
/// crash-loop the unit with zero evidence (the P6 review's worst finding).
/// Also emitted through tracing for structured consumers when the filter
/// allows. The "fatal:" prefix is the pre-P6 grep phrase, kept on purpose.
fn fatal(msg: &str) -> ! {
    eprintln!("fatal: {msg}");
    tracing::error!("fatal: {msg}");
    std::process::exit(1);
}

#[tokio::main]
async fn main() {
    // Structured logging to stderr; RUST_LOG overrides the default "info" filter.
    // with_ansi(false): journald captures raw bytes — no escape codes in journals.
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".into()),
        )
        .with_writer(std::io::stderr)
        .with_ansi(false)
        .init();

    let args: Vec<String> = std::env::args().collect();
    let env: HashMap<String, String> = std::env::vars().collect();

    let mut cfg = match config::load(&args, &env) {
        Ok(c) => c,
        Err(e) => {
            fatal(&e);
        }
    };

    // This API execs into every guest on the node and streams their disks, so
    // an agent with no real credential is worse than an agent that is down.
    // Refuse the empty token and the placeholders shipped in the repo rather
    // than serving root-equivalent endpoints behind a public constant.
    if let Err(e) = config::validate_token(&cfg.token) {
        fatal(&format!("auth token: {e}"));
    }

    // This node's OWN credential, if feluccad has issued one. feluccad stopped
    // reusing the fleet admin token as its feluccad→agent bearer: it mints a
    // per-node token at enrollment and presents THAT. The agent accepts it
    // inbound alongside the configured shared token, and presents it outbound
    // on the agent routes instead of the fleet key.
    //
    // No file = never enrolled, or a deployment older than per-node
    // credentials: the shared token carries both directions and the fleet
    // keeps working. That fallback is the whole rollout story, so it must not
    // be an error. A file that exists but cannot be used IS one — validate
    // first, then fatal, so the guards above apply to this credential too and
    // an agent never starts on one that feluccad's calls will not match.
    let node_token: NodeToken = Arc::new(RwLock::new(String::new()));
    match config::load_node_token(&cfg.data_dir) {
        Ok(Some(t)) => {
            *node_token.write().expect("fresh lock") = t;
            info!(
                path = %config::node_token_path(&cfg.data_dir),
                "loaded this node's own control-plane credential"
            );
        }
        Ok(None) => {}
        Err(e) => fatal(&format!("node token: {e}")),
    }

    // WireGuard overlay (v4 P2): a persisted enrollment reconfigures the
    // tunnel on every boot; --join + --join-token performs first enrollment.
    if cfg.join_url.is_empty() != cfg.join_token.is_empty() {
        // Half-configured join is an operator mistake — silently skipping it
        // would register this node off-overlay while they believe it joined.
        fatal("--join and --join-token must be set together");
    }
    // Err = wg.json exists but is unreadable/corrupt. Fatal here, not a
    // fallback: the file's presence proves enrollment, and starting in
    // direct mode instead would register the wrong address with the hub.
    let wg_state = match wg::load_state(&cfg.data_dir) {
        Ok(s) => s,
        Err(e) => {
            fatal(&format!("wg state load: {e}"));
        }
    };
    let joined: Option<wg::JoinInfo> = if let Some(info) = wg_state {
        if !cfg.join_url.is_empty() {
            warn!(
                path = %wg::state_path(&cfg.data_dir),
                "persisted wg enrollment takes precedence over --join; delete it to re-enroll with a fresh token"
            );
        }
        Some(info)
    } else if !cfg.join_url.is_empty() && !cfg.join_token.is_empty() {
        // First join: ensure a keypair, enroll, persist the grant.
        let pubkey = match wg::ensure_key(&wg::key_path(&cfg.data_dir)) {
            Ok(pk) => pk,
            Err(e) => {
                fatal(&format!("wg key: {e}"));
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
                    fatal(&format!("wg joined but state not persisted: {e}"));
                }
                // The same exchange mints this node's feluccad→agent credential
                // (a re-join rotates it). Same reasoning as wg.json, and the
                // stakes are higher: feluccad has already switched to this
                // token, so losing it silently means every control-plane call
                // to this node 401s from the next boot onward.
                if !info.agent_token.is_empty() {
                    if let Err(e) = adopt_node_token(&cfg.data_dir, &node_token, &info.agent_token)
                    {
                        fatal(&format!("wg joined but node credential unusable: {e}"));
                    }
                }
                Some(info)
            }
            Err(e) => {
                if e.contains("status 401") {
                    fatal(&format!(
                        "wg join (this node's key file may already be enrolled — mint a fresh token, or restore wg.json): {e}"
                    ));
                }
                fatal(&format!("wg join: {e}"));
            }
        }
    } else {
        None
    };
    if let Some(info) = &joined {
        wg::validate_join_info(info).unwrap_or_else(|e| {
            fatal(&format!("wg state invalid: {e}"));
        });
        if let Err(e) = wg::ensure_interface(info, &wg::key_path(&cfg.data_dir)) {
            fatal(&format!("wg interface: {e}"));
        }
        // Route control-plane traffic over the overlay; advertise our overlay
        // IP so feluccad reaches this agent through the tunnel. The hub's API
        // port was recorded in wg.json at enrollment (from the join URL), so
        // it survives reboots after the one-time join flags are removed.
        cfg.control_plane = format!("http://{}:{}", info.server_overlay_ip, info.api_port);
        cfg.advertise_addr = info.overlay_ip.clone();
        info!(
            overlay_ip = %info.overlay_ip,
            server_ip = %info.server_overlay_ip,
            endpoint = %info.server_endpoint,
            "wg overlay joined"
        );
    }

    // Auto-detect advertise_addr when unset.
    if cfg.advertise_addr.is_empty() {
        cfg.advertise_addr = detect_advertise_addr(&cfg.control_plane)
            .unwrap_or_else(|| "127.0.0.1".to_string());
    }

    let cidr = Cidr::parse(&cfg.net_cidr)
        .unwrap_or_else(|| Cidr::parse("10.231.0.0/24").unwrap());

    info!(
        control_plane = %cfg.control_plane,
        data_dir = %cfg.data_dir,
        advertise = %cfg.advertise_addr,
        port = cfg.port,
        net = if cfg.net { "on" } else { "off" },
        cidr = %cfg.net_cidr,
        pool = cfg.pool_size,
        auth = if !cfg.token.is_empty() { "on" } else { "off" },
        // Which credential this node answers to and dials out with. "shared"
        // is the grandfathered case; re-enrolling with a join token moves it
        // to "own". Never the value itself.
        node_cred = if node_token.read().map(|t| !t.is_empty()).unwrap_or(false) {
            "own"
        } else {
            "shared"
        },
        "felucca-agent startup"
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

    // Host networking + IP allocator. The port goes in first: the guest→host
    // fence installed during bridge setup names it in its drop rule.
    net::set_agent_port(cfg.port);
    mgr.setup_host().await;

    // Reconcile persisted instances, then rebuild tenant isolation for
    // adopted VMs (the nft sets don't survive an agent-host reboot).
    mgr.reconcile().await;
    mgr.refresh_isolation().await;
    // Refuse to serve tenants on a node whose fences are not up. Without them
    // every sandbox here shares one flat network, and nothing on the tenant's
    // side would show it — feluccad would keep placing new tenants on a worker
    // with no isolation at all. Run `--net off` to serve unnetworked guests
    // deliberately instead.
    if cfg.net && !net::netfilter_ready() {
        fatal(
            "cross-tenant isolation is not in force on this node (see the errors above) — \
             refusing to serve tenants",
        );
    }
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
        // The node token, not cfg.token, is what goes on the wire to
        // /api/v1/agents/register and /api/v1/agents/heartbeat once feluccad
        // has issued one — so a compromised worker no longer yields the fleet
        // key. data_dir is where a rotation issued mid-run is persisted.
        let ntok = Arc::clone(&node_token);
        let ddir = cfg.data_dir.clone();
        tokio::spawn(async move {
            heartbeat_loop(mgr2, cp, aa, cfg.port, tok, nid, ntok, ddir).await;
        });
    }

    // Async pool refill. Always spawned (v4 P4): even with pool_size == 0,
    // feluccad can push template pools at runtime via PUT /v1/pools.
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
        // Same handle the registration loop writes rotations into, so the
        // inbound gate accepts a freshly issued credential without a restart.
        node_token: Arc::clone(&node_token),
    };
    let app = build_router(state);

    // Never the wildcard by default: 0.0.0.0 includes the bridge gateway that
    // every guest routes through, which puts this root API one curl away from
    // inside any tenant's sandbox. An explicit host in `bind` wins; otherwise
    // we listen on the management address we advertise to the control plane.
    // `bind_any` is the deliberate opt-out for operators who front the agent
    // with their own firewall.
    let bind_ip = if cfg.bind_any {
        "0.0.0.0".to_string()
    } else {
        match config::bind_host(&cfg.bind) {
            "" => cfg.advertise_addr.clone(),
            host => host.to_string(),
        }
    };
    if cfg.net && config::addr_in_guest_cidr(&bind_ip, cidr) {
        fatal(&format!(
            "refusing to listen on {} — it is inside the guest CIDR {}, which every sandbox can route to",
            bind_ip, cfg.net_cidr
        ));
    }
    let bind_addr = config::join_host_port(&bind_ip, cfg.port);
    let listener = TcpListener::bind(&bind_addr).await
        .unwrap_or_else(|e| panic!("bind {}: {}", bind_addr, e));
    info!(addr = %bind_addr, "listening");

    // Loopback is served alongside the management address, not instead of it:
    // local health checks and the rollout scripts use 127.0.0.1:<port>, and no
    // guest can route there. This is why dropping the wildcard costs nothing.
    if !cfg.bind_any && bind_ip != "127.0.0.1" {
        let loopback = format!("127.0.0.1:{}", cfg.port);
        match TcpListener::bind(&loopback).await {
            Ok(l) => {
                info!(addr = %loopback, "listening");
                let app2 = app.clone();
                tokio::spawn(async move {
                    if let Err(e) = axum::serve(l, app2).await {
                        error!(err = %e, "loopback listener stopped");
                    }
                });
            }
            Err(e) => warn!(addr = %loopback, err = %e, "loopback listener unavailable"),
        }
    }

    axum::serve(listener, app).await
        .unwrap_or_else(|e| panic!("serve: {}", e));
}
