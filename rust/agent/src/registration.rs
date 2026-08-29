//! Agent registration and heartbeat loop.
//!
//! Port of heartbeatLoop/registerOnce/heartbeatOnce in backend/src/agent/main.zig.
//! Plain HTTP over TCP with Connection: close; bearer token when configured.

use crate::config::{save_node_token, split_host_port, validate_token};
use crate::vm::Manager;
use std::sync::{Arc, RwLock};
use tokio::time::{sleep, Duration};
use tracing::{error, info, warn};

/// Shared node id set after successful registration.
pub type NodeId = Arc<RwLock<String>>;

/// This node's own hearthd credential (`hearth_nt_...`), empty until hearthd
/// issues one. Shared with the HTTP server: the agent presents it OUTBOUND on
/// the agent routes and accepts it INBOUND from hearthd, so a rotation at
/// re-enrollment takes effect in both directions without a restart.
pub type NodeToken = Arc<RwLock<String>>;

/// The credential to present on hearthd's agent routes: this node's own token
/// once hearthd has issued one, else the configured shared token.
///
/// The fallback is what makes this a rollout rather than a flag day — an agent
/// that never joined, or a deployment older than per-node credentials, keeps
/// working. There is deliberately NO fallback the other way: a rejected node
/// token must never be retried with the shared one, or anyone who can make one
/// request fail could downgrade the agent into handing over the fleet key.
pub(crate) fn outbound_token(node_token: &NodeToken, shared: &str) -> String {
    // A poisoned lock (a writer panicked) still holds a valid credential;
    // dropping to the shared token there would leak exactly what we removed.
    let cur = node_token.read().unwrap_or_else(|e| e.into_inner());
    if cur.is_empty() { shared.to_string() } else { cur.clone() }
}

/// Take up a per-node credential hearthd just issued: validate it, persist it
/// 0600, and start using it for both directions.
///
/// `Err` on an unusable credential (never adopted, never written) and on a
/// persist failure (adopted in memory anyway — see below). Callers decide how
/// loud that is: enrollment is fatal, a running agent logs and carries on.
pub fn adopt_node_token(
    data_dir: &str,
    node_token: &NodeToken,
    issued: &str,
) -> Result<(), String> {
    let issued = issued.trim();
    {
        let cur = node_token.read().unwrap_or_else(|e| e.into_inner());
        if *cur == issued {
            return Ok(());
        }
    }
    // A node token gets no exemption from the startup guards (C1/H5): refuse
    // it outright rather than trade a working credential for an unusable one.
    validate_token(issued)
        .map_err(|e| format!("hearthd issued an unusable node token: {}", e))?;

    let persisted = save_node_token(data_dir, issued);

    // Adopt in memory even when the write failed: hearthd has already switched
    // to this credential, so refusing it would 401 every control-plane call to
    // this node until an operator re-enrolls. The restart is the lossy part,
    // which is what the error tells the operator.
    {
        let mut w = node_token.write().unwrap_or_else(|e| e.into_inner());
        *w = issued.to_string();
    }
    info!("using this node's own control-plane credential");

    persisted.map_err(|e| {
        format!("node token not persisted ({}) — it is lost on restart; re-enroll this node", e)
    })
}

/// Register (retry until success), then heartbeat every 5s.
#[allow(clippy::too_many_arguments)]
pub async fn heartbeat_loop(
    mgr: Arc<Manager>,
    control_plane: String,
    advertise_addr: String,
    port: u16,
    token: String,
    node_id: NodeId,
    node_token: NodeToken,
    data_dir: String,
) {
    register_with_retry(
        &control_plane, &advertise_addr, port, &token, &node_id, &node_token, &data_dir, "register",
    )
    .await;

    // Heartbeat every 5s.
    loop {
        match heartbeat_once(&mgr, &control_plane, &token, &node_id, &node_token).await {
            Ok(()) => {}
            // hearthd has no record of our id. Retrying it forever would leave
            // this worker invisible to the fleet with no way back, so drop the
            // stale id and enroll again — the same path taken at startup.
            Err(HeartbeatError::Unregistered) => {
                warn!("hearthd has no record of this node — re-registering");
                {
                    let mut n = node_id.write().unwrap_or_else(|e| e.into_inner());
                    n.clear();
                }
                register_with_retry(
                    &control_plane, &advertise_addr, port, &token, &node_id, &node_token,
                    &data_dir, "re-register",
                )
                .await;
            }
            Err(e) => warn!(err = %e, "heartbeat failed"),
        }
        sleep(Duration::from_secs(5)).await;
    }
}

/// Register, retrying every 5s until hearthd accepts. `what` names the attempt
/// in the log so a re-enrollment is distinguishable from the startup one.
#[allow(clippy::too_many_arguments)]
async fn register_with_retry(
    control_plane: &str,
    advertise_addr: &str,
    port: u16,
    token: &str,
    node_id: &NodeId,
    node_token: &NodeToken,
    data_dir: &str,
    what: &str,
) {
    loop {
        match register_once(
            control_plane, advertise_addr, port, token, node_id, node_token, data_dir,
        )
        .await
        {
            Ok(_) => return,
            Err(e) => warn!(err = %e, attempt = what, "register failed"),
        }
        sleep(Duration::from_secs(5)).await;
    }
}

/// Why a heartbeat failed. `Unregistered` is separated from every other failure
/// because it is the one the agent can repair itself: hearthd 404s a node id it
/// holds no record of — reclaimed after a partition longer than the stale-node
/// TTL, or lost when the control plane came back on an older snapshot.
enum HeartbeatError {
    Unregistered,
    Failed(String),
}

impl std::fmt::Display for HeartbeatError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            HeartbeatError::Unregistered => f.write_str("hearthd has no record of this node"),
            HeartbeatError::Failed(e) => f.write_str(e),
        }
    }
}

#[allow(clippy::too_many_arguments)]
async fn register_once(
    control_plane: &str,
    advertise_addr: &str,
    port: u16,
    token: &str,
    node_id: &NodeId,
    node_token: &NodeToken,
    data_dir: &str,
) -> Result<(), String> {
    let hostname = read_hostname();
    let cpus = read_cpus();
    let mem_total = read_mem_total_mib();
    let addr = format!("{}:{}", advertise_addr, port);

    let body = format!(
        "{{\"hostname\":{},\"addr\":{},\"cpus\":{},\"mem_total_mib\":{}}}",
        json_str(&hostname),
        json_str(&addr),
        cpus,
        mem_total,
    );

    let (host, cp_port) = split_host_port(control_plane);
    let bearer = outbound_token(node_token, token);
    let resp = tcp_request(host, cp_port, "POST", "/api/v1/agents/register", &body, &bearer).await
        .map_err(|e| format!("register request failed: {}", e))?;

    if resp.status >= 300 {
        return Err(format!("register rejected: {}", resp.status));
    }

    // hearthd returns `agent_token` when this registration enrolled the node
    // (it carried a join token), and mints a fresh one every time — so this is
    // also the rotation path. Additive: an older hearthd omits the field and
    // the agent stays on whatever credential it already had.
    if let Some(issued) = extract_json_str(&resp.body, "agent_token") {
        if !issued.trim().is_empty() {
            if let Err(e) = adopt_node_token(data_dir, node_token, &issued) {
                // Not fatal to the registration: the node IS registered, and
                // the credential it keeps using still works until hearthd
                // rotates. Loud enough to finish the rollout by hand.
                error!(err = %e, "node credential from register");
            }
        }
    }

    // Parse {"id":"..."} from the response. The register response is the
    // frozen pre-P4 shape on purpose: warm-pool specs have exactly ONE
    // delivery channel (PUT /v1/pools — hearthd pushes to a node right
    // after it registers), so empty-list teardown semantics stay
    // unambiguous and this tolerant substring parse keeps working against
    // any hearthd vintage.
    let id = extract_json_str(&resp.body, "id")
        .ok_or("no id in register response")?;
    {
        let mut n = node_id.write().map_err(|_| "lock")?;
        *n = id.clone();
    }
    info!(node_id = %id, "registered as node");
    Ok(())
}

async fn heartbeat_once(
    mgr: &Arc<Manager>,
    control_plane: &str,
    token: &str,
    node_id: &NodeId,
    node_token: &NodeToken,
) -> Result<(), HeartbeatError> {
    let id = {
        let n = node_id
            .read()
            .map_err(|_| HeartbeatError::Failed("lock".into()))?;
        if n.is_empty() { return Err(HeartbeatError::Unregistered); }
        n.clone()
    };

    let mem_free = read_mem_available_mib();
    let vm_count = mgr.live_count().await;
    let pool_size = mgr.pool_count().await;
    // So hearthd can stop scheduling onto a worker whose guest-network fences
    // are down: every sandbox placed there would share one flat network, and
    // nothing on the tenant's side would show it.
    let isolation_ok = mgr.isolation_ok();

    let body = format!(
        "{{\"id\":{},\"mem_free_mib\":{},\"vm_count\":{},\"pool_size\":{},\"isolation_ok\":{}}}",
        json_str(&id),
        mem_free,
        vm_count,
        pool_size,
        isolation_ok,
    );

    let (host, port) = split_host_port(control_plane);
    let bearer = outbound_token(node_token, token);
    let resp = tcp_request(host, port, "POST", "/api/v1/agents/heartbeat", &body, &bearer).await
        .map_err(|e| HeartbeatError::Failed(format!("heartbeat request: {}", e)))?;

    heartbeat_outcome(resp.status)
}

/// Classify a heartbeat response status. Pure, so the one status that changes
/// the agent's behaviour is testable without a control plane.
///
/// 404 is the node-record-is-gone signal and the only status the agent repairs
/// itself. 401 is deliberately NOT included: a rejected credential must never
/// drive a re-register, or anyone who can make one call fail could push the
/// agent back through enrollment.
fn heartbeat_outcome(status: u16) -> Result<(), HeartbeatError> {
    if status == 404 {
        return Err(HeartbeatError::Unregistered);
    }
    if status >= 300 {
        return Err(HeartbeatError::Failed(format!("heartbeat rejected: {}", status)));
    }
    Ok(())
}

// ---- host info readers ----

pub(crate) fn read_hostname() -> String {
    match std::fs::read_to_string("/etc/hostname") {
        Ok(s) => {
            let t = s.trim().to_string();
            if t.is_empty() { "unknown".into() } else { t }
        }
        Err(_) => "unknown".into(),
    }
}

fn read_cpus() -> u32 {
    let data = std::fs::read_to_string("/proc/cpuinfo").unwrap_or_default();
    let count = data.lines().filter(|l| l.starts_with("processor")).count() as u32;
    if count == 0 { 1 } else { count }
}

fn read_mem_total_mib() -> u64 {
    read_meminfo_key("MemTotal:") / 1024
}

fn read_mem_available_mib() -> u64 {
    read_meminfo_key("MemAvailable:") / 1024
}

fn read_meminfo_key(key: &str) -> u64 {
    let data = std::fs::read_to_string("/proc/meminfo").unwrap_or_default();
    for line in data.lines() {
        if line.starts_with(key) {
            let rest = line[key.len()..].trim();
            let num = rest.split_whitespace().next().unwrap_or("");
            return num.parse::<u64>().unwrap_or(0);
        }
    }
    0
}

// ---- minimal HTTP client (TCP, Connection: close) ----

pub(crate) struct Response {
    pub(crate) status: u16,
    pub(crate) body: String,
}

pub(crate) async fn tcp_request(
    host: &str,
    port: u16,
    method: &str,
    path: &str,
    body: &str,
    token: &str,
) -> Result<Response, String> {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpStream;

    let addr = format!("{}:{}", host, port);
    let mut stream = TcpStream::connect(&addr).await
        .map_err(|e| format!("connect {}: {}", addr, e))?;

    let auth_hdr = if !token.is_empty() {
        format!("Authorization: Bearer {}\r\n", token)
    } else {
        String::new()
    };

    let request = format!(
        "{} {} HTTP/1.1\r\nHost: {}:{}\r\n{}Connection: close\r\nContent-Type: application/json\r\nContent-Length: {}\r\n\r\n{}",
        method, path, host, port, auth_hdr, body.len(), body
    );

    stream.write_all(request.as_bytes()).await
        .map_err(|e| format!("write: {}", e))?;
    stream.flush().await.map_err(|e| format!("flush: {}", e))?;

    let mut buf = Vec::new();
    stream.read_to_end(&mut buf).await
        .map_err(|e| format!("read: {}", e))?;

    let raw = String::from_utf8_lossy(&buf);
    parse_http_response(&raw)
}

fn parse_http_response(raw: &str) -> Result<Response, String> {
    let mut lines = raw.splitn(2, "\r\n\r\n");
    let headers_part = lines.next().unwrap_or("");
    let body = lines.next().unwrap_or("").to_string();

    let status_line = headers_part.lines().next().unwrap_or("");
    let mut parts = status_line.splitn(3, ' ');
    let _ = parts.next(); // HTTP/1.1
    let code_str = parts.next().unwrap_or("0");
    let status = code_str.parse::<u16>().unwrap_or(0);

    Ok(Response { status, body })
}

// ---- JSON helpers ----

fn json_str(s: &str) -> String {
    let mut out = String::with_capacity(s.len() + 2);
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

/// Extract a string value from a JSON object by key (minimal, no deps —
/// deliberately tolerant of anything around the `"key":"value"` pair).
fn extract_json_str(json: &str, key: &str) -> Option<String> {
    let needle = format!("\"{}\":", key);
    let start = json.find(&needle)?;
    let rest = &json[start + needle.len()..].trim_start();
    if !rest.starts_with('"') { return None; }
    let rest = &rest[1..];
    let mut val = String::new();
    let mut escaped = false;
    for c in rest.chars() {
        if escaped { val.push(c); escaped = false; continue; }
        if c == '\\' { escaped = true; continue; }
        if c == '"' { break; }
        val.push(c);
    }
    Some(val)
}

/// Detect the local IPv4 address used to reach control_plane host via UDP connect.
pub fn detect_advertise_addr(control_plane: &str) -> Option<String> {
    use std::net::UdpSocket;
    let (host, port) = split_host_port(control_plane);
    let addr = format!("{}:{}", host, port);
    let sock = UdpSocket::bind("0.0.0.0:0").ok()?;
    sock.connect(&addr).ok()?;
    let local = sock.local_addr().ok()?;
    Some(local.ip().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_extract_id_from_register_response() {
        // The register response is the frozen {"id":...} shape; the
        // substring parse tolerates extra fields and surrounding noise.
        assert_eq!(extract_json_str(r#"{"id":"node-1"}"#, "id").as_deref(), Some("node-1"));
        assert_eq!(
            extract_json_str(r#"{"id":"node-1","extra":true}"#, "id").as_deref(),
            Some("node-1")
        );
        assert!(extract_json_str(r#"{"other":"x"}"#, "id").is_none());
    }

    // ---- heartbeat outcome classification ----

    #[test]
    fn test_heartbeat_404_drives_a_reregister() {
        // hearthd reclaimed or lost this node's record. Heartbeating the dead
        // id forever would leave the worker invisible with no way back, so this
        // is the one status the agent repairs itself.
        assert!(matches!(
            heartbeat_outcome(404),
            Err(HeartbeatError::Unregistered)
        ));
    }

    #[test]
    fn test_heartbeat_rejection_does_not_drive_a_reregister() {
        // Anything else is a plain failure: retried with the same id, never
        // re-enrolled. 401 especially — a credential someone can make fail must
        // not be a lever that pushes the agent back through enrollment.
        for status in [401, 403, 429, 500, 502, 503] {
            match heartbeat_outcome(status) {
                Err(HeartbeatError::Failed(_)) => {}
                other => panic!(
                    "status {} must be a plain failure, got {}",
                    status,
                    match other {
                        Ok(()) => "success".to_string(),
                        Err(e) => e.to_string(),
                    }
                ),
            }
        }
    }

    #[test]
    fn test_heartbeat_success_statuses_are_ok() {
        for status in [200, 204, 299] {
            assert!(heartbeat_outcome(status).is_ok(), "status {} should be ok", status);
        }
    }

    // ---- per-node credential ----

    const NT: &str = "hearth_nt_9f2c1b8a7d4e6f0312a5b9c8d7e6f504";
    const SHARED: &str = "9f2c1b8a7d4e6f0312a5b9c8d7e6f504132435465768798a";

    fn scratch(name: &str) -> String {
        let d = std::env::temp_dir().join(format!(
            "hearth-reg-{}-{}-{:?}",
            name,
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::remove_dir_all(&d).ok();
        d.to_string_lossy().into_owned()
    }

    #[test]
    fn test_register_response_carries_the_node_credential() {
        // Was: the agent parsed only "id" and threw the credential away, so it
        // kept presenting the fleet admin token on the agent routes.
        let body = r#"{"id":"node-1","agent_token":"hearth_nt_abc123"}"#;
        assert_eq!(extract_json_str(body, "id").as_deref(), Some("node-1"));
        assert_eq!(
            extract_json_str(body, "agent_token").as_deref(),
            Some("hearth_nt_abc123")
        );
        // An older hearthd omits the field: no credential, no error.
        assert!(extract_json_str(r#"{"id":"node-1"}"#, "agent_token").is_none());
    }

    #[test]
    fn test_outbound_token_prefers_the_node_credential() {
        // Was: registration.rs sent cfg.token — the fleet admin token — to
        // /api/v1/agents/register and /api/v1/agents/heartbeat, so every
        // worker had to hold the fleet key.
        let nt: NodeToken = Arc::new(RwLock::new(String::new()));
        // Never joined: the shared token, so a mixed-version fleet still runs.
        assert_eq!(outbound_token(&nt, SHARED), SHARED);
        *nt.write().unwrap() = NT.to_string();
        assert_eq!(outbound_token(&nt, SHARED), NT);
    }

    #[test]
    fn test_adopt_persists_and_serves_both_directions() {
        let dir = scratch("adopt");
        let nt: NodeToken = Arc::new(RwLock::new(String::new()));
        adopt_node_token(&dir, &nt, NT).expect("adopt");
        // Outbound.
        assert_eq!(outbound_token(&nt, SHARED), NT);
        // Inbound: the same value the server checks bearers against.
        assert!(crate::config::authorized_either(
            SHARED,
            &nt.read().unwrap(),
            Some(&format!("Bearer {}", NT))
        ));
        // Persisted 0600, so the next boot starts on the same credential.
        assert_eq!(
            crate::config::load_node_token(&dir).expect("load").as_deref(),
            Some(NT)
        );
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_adopt_rotates_to_a_freshly_issued_credential() {
        let dir = scratch("rotate");
        let nt: NodeToken = Arc::new(RwLock::new(String::new()));
        adopt_node_token(&dir, &nt, "hearth_nt_1111111111111111111111").expect("first");
        adopt_node_token(&dir, &nt, NT).expect("rotation");
        assert_eq!(outbound_token(&nt, SHARED), NT);
        assert_eq!(
            crate::config::load_node_token(&dir).expect("load").as_deref(),
            Some(NT)
        );
        // The superseded credential is gone from both directions.
        assert!(!crate::config::authorized_either(
            SHARED,
            &nt.read().unwrap(),
            Some("Bearer hearth_nt_1111111111111111111111")
        ));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_adopt_refuses_an_unusable_credential_and_keeps_the_working_one() {
        // A node token must not become a way past the C1/H5 startup guards.
        let dir = scratch("guards");
        let nt: NodeToken = Arc::new(RwLock::new(String::new()));
        adopt_node_token(&dir, &nt, NT).expect("adopt");
        for bad in ["", "short", "hearth-lab-token", "REPLACE_WITH_SAME_TOKEN_AS_HEARTHD"] {
            assert!(adopt_node_token(&dir, &nt, bad).is_err(), "adopted {:?}", bad);
        }
        assert_eq!(outbound_token(&nt, SHARED), NT);
        assert_eq!(
            crate::config::load_node_token(&dir).expect("load").as_deref(),
            Some(NT)
        );
        std::fs::remove_dir_all(&dir).ok();
    }
}
