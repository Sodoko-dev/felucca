//! Agent registration and heartbeat loop.
//!
//! Port of heartbeatLoop/registerOnce/heartbeatOnce in backend/src/agent/main.zig.
//! Plain HTTP over TCP with Connection: close; bearer token when configured.

use crate::config::split_host_port;
use crate::vm::Manager;
use std::sync::{Arc, RwLock};
use tokio::time::{sleep, Duration};
use tracing::{info, warn};

/// Shared node id set after successful registration.
pub type NodeId = Arc<RwLock<String>>;

/// Register (retry until success), then heartbeat every 5s.
pub async fn heartbeat_loop(
    mgr: Arc<Manager>,
    control_plane: String,
    advertise_addr: String,
    port: u16,
    token: String,
    node_id: NodeId,
) {
    // Register with retry.
    loop {
        match register_once(&control_plane, &advertise_addr, port, &token, &node_id).await {
            Ok(_) => break,
            Err(e) => warn!(err = %e, "register failed"),
        }
        sleep(Duration::from_secs(5)).await;
    }

    // Heartbeat every 5s.
    loop {
        if let Err(e) = heartbeat_once(&mgr, &control_plane, &token, &node_id).await {
            warn!(err = %e, "heartbeat failed");
        }
        sleep(Duration::from_secs(5)).await;
    }
}

async fn register_once(
    control_plane: &str,
    advertise_addr: &str,
    port: u16,
    token: &str,
    node_id: &NodeId,
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
    let resp = tcp_request(host, cp_port, "POST", "/api/v1/agents/register", &body, token).await
        .map_err(|e| format!("register request failed: {}", e))?;

    if resp.status >= 300 {
        return Err(format!("register rejected: {}", resp.status));
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
) -> Result<(), String> {
    let id = {
        let n = node_id.read().map_err(|_| "lock")?;
        if n.is_empty() { return Err("NotRegistered".into()); }
        n.clone()
    };

    let mem_free = read_mem_available_mib();
    let vm_count = mgr.live_count().await;
    let pool_size = mgr.pool_count().await;

    let body = format!(
        "{{\"id\":{},\"mem_free_mib\":{},\"vm_count\":{},\"pool_size\":{}}}",
        json_str(&id),
        mem_free,
        vm_count,
        pool_size,
    );

    let (host, port) = split_host_port(control_plane);
    let resp = tcp_request(host, port, "POST", "/api/v1/agents/heartbeat", &body, token).await
        .map_err(|e| format!("heartbeat request: {}", e))?;

    if resp.status >= 300 {
        return Err(format!("heartbeat rejected: {}", resp.status));
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
}
