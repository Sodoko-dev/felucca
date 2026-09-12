//! WireGuard overlay join flow for felucca-agent (v4 P2): keypair on disk,
//! one-shot enrollment against feluccad, and the `wg-felucca` interface.
//!
//! A worker enrolls once via POST {join_url}/api/v1/nodes/join with a
//! single-use join token, persists the granted overlay assignment to
//! <data_dir>/wg.json, and reconfigures the tunnel from that state on every
//! boot. Two join transports (P2.3): an `http://` join_url uses the built-in
//! minimal client (registration::tcp_request), while an `https://` join_url
//! shells out to `curl` ONE-SHOT for the enrollment exchange — the config
//! (token included) is piped on stdin via `curl --config -`, never argv. The
//! persistent control-plane channel stays plain HTTP inside the wg tunnel;
//! TLS is only for the bootstrap call that crosses the untrusted network.
//! Shell-out to ip/wg uses net.rs's bare-then-`sudo -n` idiom
//! (net::run); only `wg genkey`/`wg pubkey` run unprivileged directly since
//! they are pure computation and the private key must never touch argv.

use crate::config::split_host_port;
use crate::net::run;
use serde::{Deserialize, Serialize};
use std::io::Write;
use std::process::{Command, Stdio};

pub const WG_IFACE: &str = "wg-felucca";

/// Overlay assignment returned by feluccad's join endpoint; persisted verbatim
/// to wg.json so later boots can rebuild the tunnel without a token.
#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct JoinInfo {
    pub overlay_ip: String,
    pub overlay_prefix: u8,
    pub server_overlay_ip: String,
    pub server_pubkey: String,
    pub server_endpoint: String,
    pub keepalive_s: u32,
    /// feluccad API port over the overlay. Not part of the server response —
    /// recorded client-side from the join URL at enrollment so later boots
    /// don't fall back to a default the hub may not listen on.
    #[serde(default = "default_api_port")]
    pub api_port: u16,
    /// This node's own feluccad→agent credential, minted by the same exchange.
    ///
    /// Transient, and `skip_serializing` is the point: it is persisted to its
    /// own 0600 file (config::save_node_token) so the credential lives in
    /// exactly ONE place on disk, and so the register route — which issues the
    /// same token and never writes wg.json — has somewhere to put a rotation.
    /// Absent from an older feluccad's response, which serde defaults to "".
    #[serde(default, skip_serializing)]
    pub agent_token: String,
}

fn default_api_port() -> u16 {
    8080
}

// ---- key management ----

/// Ensure a WireGuard private key exists at `path`; return its PUBLIC key.
///
/// Generates ONLY when the file does not exist (ErrorKind::NotFound). Any
/// other read error — and an existing-but-empty file — is returned as an
/// error, never silently regenerated: rotating the key would orphan the
/// enrollment on the feluccad side.
pub fn ensure_key(path: &str) -> Result<String, String> {
    let private = match std::fs::read_to_string(path) {
        Ok(s) => {
            let t = s.trim().to_string();
            if t.is_empty() {
                return Err(format!("{}: existing key file is empty", path));
            }
            t
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => generate_key(path)?,
        Err(e) => return Err(format!("{}: read: {}", path, e)),
    };
    derive_pubkey(&private)
}

/// Generate a fresh private key with `wg genkey` and write it 0600.
fn generate_key(path: &str) -> Result<String, String> {
    // `wg genkey` is pure computation — unprivileged, no sudo fallback.
    let out = Command::new("wg")
        .arg("genkey")
        .stderr(Stdio::null())
        .output()
        .map_err(|e| format!("wg genkey: {}", e))?;
    if !out.status.success() {
        return Err("wg genkey failed".into());
    }
    let key = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if key.is_empty() {
        return Err("wg genkey produced no output".into());
    }

    if let Some(parent) = std::path::Path::new(path).parent() {
        std::fs::create_dir_all(parent)
            .map_err(|e| format!("create {}: {}", parent.display(), e))?;
    }
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = std::fs::OpenOptions::new()
        .write(true).create(true).truncate(true).mode(0o600)
        .open(path)
        .map_err(|e| format!("open {}: {}", path, e))?;
    f.write_all(key.as_bytes())
        .map_err(|e| format!("write {}: {}", path, e))?;
    Ok(key)
}

/// Derive the public key via `wg pubkey` with the private key piped on
/// stdin — never argv (argv is world-readable through /proc).
fn derive_pubkey(private: &str) -> Result<String, String> {
    let mut child = Command::new("wg")
        .arg("pubkey")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("wg pubkey: {}", e))?;
    child.stdin.take()
        .ok_or("wg pubkey: no stdin")?
        .write_all(private.as_bytes())
        .map_err(|e| format!("wg pubkey stdin: {}", e))?;
    let out = child.wait_with_output()
        .map_err(|e| format!("wg pubkey: {}", e))?;
    if !out.status.success() {
        return Err("wg pubkey failed (corrupt private key?)".into());
    }
    let pk = String::from_utf8_lossy(&out.stdout).trim().to_string();
    if pk.is_empty() {
        return Err("wg pubkey produced no output".into());
    }
    Ok(pk)
}

// ---- validation (defense-in-depth before anything reaches a command line) ----

/// Validate every JoinInfo field before it is interpolated into ip/wg argv.
pub fn validate_join_info(info: &JoinInfo) -> Result<(), String> {
    if info.overlay_ip.parse::<std::net::Ipv4Addr>().is_err() {
        return Err(format!("bad overlay_ip: {}", info.overlay_ip));
    }
    if info.server_overlay_ip.parse::<std::net::Ipv4Addr>().is_err() {
        return Err(format!("bad server_overlay_ip: {}", info.server_overlay_ip));
    }
    if !(1..=32).contains(&info.overlay_prefix) {
        return Err(format!("bad overlay_prefix: {}", info.overlay_prefix));
    }
    if !valid_pubkey(&info.server_pubkey) {
        return Err(format!("bad server_pubkey: {}", info.server_pubkey));
    }
    if !valid_endpoint(&info.server_endpoint) {
        return Err(format!("bad server_endpoint: {}", info.server_endpoint));
    }
    if info.keepalive_s > 65535 {
        return Err(format!("bad keepalive_s: {}", info.keepalive_s));
    }
    Ok(())
}

/// A WireGuard public key is exactly 44 chars of base64 ending in '='.
fn valid_pubkey(k: &str) -> bool {
    k.len() == 44
        && k.ends_with('=')
        && k.bytes().take(43).all(|b| b.is_ascii_alphanumeric() || b == b'+' || b == b'/')
}

/// host:port with a u16 port and a non-empty host of [A-Za-z0-9.:\[\]-]
/// (DNS names and bracketed IPv6 like "[fd00::1]:51820" allowed). wg(8)'s
/// `set` parser is keyword-positional, not getopt, so a host that begins
/// with '-' cannot be smuggled as an option.
fn valid_endpoint(ep: &str) -> bool {
    let Some(colon) = ep.rfind(':') else { return false; };
    let (host, port) = (&ep[..colon], &ep[colon + 1..]);
    if host.is_empty() || port.parse::<u16>().is_err() {
        return false;
    }
    host.bytes().all(|b| {
        b.is_ascii_alphanumeric() || b == b'.' || b == b':' || b == b'-' || b == b'[' || b == b']'
    })
}

// ---- join (one-shot enrollment) ----

/// Enroll against feluccad: POST {join_url}/api/v1/nodes/join with the join
/// token as bearer auth and our pubkey + hostname. 200 returns the overlay
/// assignment (validated before returning); non-200 is failure (401 invalid
/// or used token, 503 overlay off).
///
/// Transport depends on the join_url scheme: http:// uses the built-in
/// minimal client (tcp_request); https:// shells out to curl one-shot
/// (curl_join). Both produce (status, body); parse/validate is shared.
pub async fn join(
    join_url: &str,
    token: &str,
    pubkey: &str,
    hostname: &str,
) -> Result<JoinInfo, String> {
    let body = serde_json::json!({ "pubkey": pubkey, "hostname": hostname }).to_string();

    let (status, resp_body, api_port) = if join_url.starts_with("https://") {
        let (status, resp_body) = curl_join(join_url, token, &body)?;
        // split_host_port defaults a portless URL to 8080 — wrong for TLS;
        // derive the https port explicitly (explicit port in URL, else 443).
        (status, resp_body, https_api_port(join_url))
    } else if join_url.starts_with("http://") {
        let (host, port) = split_host_port(join_url);
        let resp = crate::registration::tcp_request(
            host, port, "POST", "/api/v1/nodes/join", &body, token,
        )
        .await
        .map_err(|e| format!("join request failed: {}", e))?;
        (resp.status, resp.body, port)
    } else {
        return Err(format!(
            "join_url must start with http:// or https://: {}",
            join_url
        ));
    };

    if status != 200 {
        return Err(format!(
            "join rejected: status {} body {}",
            status,
            resp_body.trim()
        ));
    }

    let mut info: JoinInfo = serde_json::from_str(&resp_body)
        .map_err(|e| format!("join response parse: {}", e))?;
    // Record which port the hub's API answers on — the same port we just
    // joined through — so post-reboot boots target it over the overlay.
    info.api_port = api_port;
    validate_join_info(&info)?;
    Ok(info)
}

/// feluccad API port for an https join_url: the explicit port in the URL when
/// present, else 443 (the https default). split_host_port is NOT used here
/// because its portless fallback is 8080.
fn https_api_port(join_url: &str) -> u16 {
    let mut a = join_url.strip_prefix("https://").unwrap_or(join_url);
    if let Some(slash) = a.find('/') {
        a = &a[..slash];
    }
    match a.rfind(':') {
        // unwrap_or(443) also covers a bracketed portless IPv6 host like
        // "[fd00::1]" whose inner colon is not a port separator.
        Some(colon) => a[colon + 1..].parse::<u16>().unwrap_or(443),
        None => 443,
    }
}

/// Escape a value for a double-quoted curl config string: backslash and
/// double quote (backslash first, or the quote escape would be re-escaped).
/// Shared with the image-pull config (vm/image.rs).
pub(crate) fn curl_config_escape(s: &str) -> String {
    s.replace('\\', "\\\\").replace('"', "\\\"")
}

/// Build the curl config for the one-shot https join. Passed on STDIN via
/// `curl --config -` so the bearer token never appears on argv (argv is
/// world-readable through /proc). write-out appends "\n<http_code>" to the
/// output so curl_join can split status from body.
fn build_curl_config(join_url: &str, token: &str, body: &str) -> String {
    let url = format!("{}/api/v1/nodes/join", join_url.trim_end_matches('/'));
    format!(
        concat!(
            "url = \"{}\"\n",
            "request = \"POST\"\n",
            "header = \"Authorization: Bearer {}\"\n",
            "header = \"Content-Type: application/json\"\n",
            "data = \"{}\"\n",
            "write-out = \"\\n%{{http_code}}\"\n",
            "max-time = 30\n",
        ),
        curl_config_escape(&url),
        curl_config_escape(token),
        curl_config_escape(body),
    )
}

/// One-shot https enrollment via `curl -sS --config -`: config (with token)
/// piped on stdin, stdout split into (http status, response body) using the
/// trailing write-out line.
fn curl_join(join_url: &str, token: &str, body: &str) -> Result<(u16, String), String> {
    let config = build_curl_config(join_url, token, body);
    let mut child = Command::new("curl")
        .args(["-sS", "--config", "-"])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("curl spawn: {}", e))?;
    // The taken stdin handle is dropped at the end of this statement,
    // closing the pipe so curl sees EOF on its config.
    child
        .stdin
        .take()
        .ok_or("curl: no stdin")?
        .write_all(config.as_bytes())
        .map_err(|e| format!("curl stdin: {}", e))?;
    let out = child
        .wait_with_output()
        .map_err(|e| format!("curl: {}", e))?;
    if !out.status.success() {
        return Err(format!(
            "curl join failed ({}): {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }

    let stdout = String::from_utf8_lossy(&out.stdout).to_string();
    // write-out guarantees the last line is the http code.
    let Some(pos) = stdout.rfind('\n') else {
        return Err(format!("curl join: malformed output: {}", stdout.trim()));
    };
    let code = stdout[pos + 1..].trim();
    let status: u16 = code
        .parse()
        .map_err(|_| format!("curl join: bad http code {:?}", code))?;
    Ok((status, stdout[..pos].to_string()))
}

// ---- interface configuration ----

/// The overlay NETWORK in CIDR form: server_overlay_ip masked by
/// overlay_prefix (u32 math like ipalloc), e.g. "10.100.0.0/24" — the worker
/// routes the whole overlay through the hub.
pub fn allowed_ips(info: &JoinInfo) -> String {
    let ip: std::net::Ipv4Addr = info
        .server_overlay_ip
        .parse()
        .unwrap_or(std::net::Ipv4Addr::UNSPECIFIED);
    let prefix = info.overlay_prefix.min(32) as u32;
    let mask: u32 = if prefix == 0 { 0 } else { u32::MAX << (32 - prefix) };
    let net = std::net::Ipv4Addr::from(u32::from(ip) & mask);
    format!("{}/{}", net, prefix)
}

/// Idempotently create, configure, and bring up the WireGuard interface from
/// a (validated) JoinInfo. Mirrors net::ensure_bridge: creation/addr-add
/// ignore "exists"; configuration steps that must take effect return errors.
/// NOTE: the interface name matches feluccad's hub interface. Workers and the
/// hub are separate hosts by design; running an agent with --join on the SAME
/// host as feluccad would clobber the hub's key (documented constraint, see
/// ADR-0006).
pub fn ensure_interface(info: &JoinInfo, key_path: &str) -> Result<(), String> {
    // Pre-flight the key file: `wg set` reads it as the same user, but its
    // failure collapses into the generic message below — an unreadable key
    // (same wrong-owner class load_state catches for wg.json) must name the
    // actual file and fix.
    if let Err(e) = std::fs::File::open(key_path) {
        return Err(format!(
            "wg key {} unreadable: {} — fix the file's owner/permissions",
            key_path, e
        ));
    }

    // Create the interface (ignore "exists").
    run(&["ip", "link", "add", WG_IFACE, "type", "wireguard"]);

    // Key + hub peer (re-applying the same config is a no-op for wg).
    let allowed = allowed_ips(info);
    let keepalive = info.keepalive_s.to_string();
    if !run(&[
        "wg", "set", WG_IFACE,
        "private-key", key_path,
        "peer", &info.server_pubkey,
        "endpoint", &info.server_endpoint,
        "allowed-ips", &allowed,
        "persistent-keepalive", &keepalive,
    ]) {
        return Err(format!(
            "wg set {} failed (missing wireguard kernel module / interface not created / sudo -n denied?)",
            WG_IFACE
        ));
    }

    // `addr replace` is genuinely idempotent, so a failure here is REAL
    // (perms, conflict) and must surface: advertising an address the host
    // doesn't hold would register an unreachable node.
    let addr = format!("{}/{}", info.overlay_ip, info.overlay_prefix);
    if !run(&["ip", "addr", "replace", &addr, "dev", WG_IFACE]) {
        return Err(format!("ip addr replace {} on {} failed", addr, WG_IFACE));
    }

    if !run(&["ip", "link", "set", WG_IFACE, "up"]) {
        return Err(format!("ip link set {} up failed", WG_IFACE));
    }
    Ok(())
}

// ---- persisted state ----

pub fn state_path(data_dir: &str) -> String {
    format!("{}/wg.json", data_dir)
}

pub fn key_path(data_dir: &str) -> String {
    format!("{}/wg.key", data_dir)
}

/// Persist the overlay assignment (pretty JSON, 0600, temp+rename so a crash
/// mid-write never leaves a truncated wg.json — the file is what marks this
/// node as enrolled).
pub fn save_state(data_dir: &str, info: &JoinInfo) -> Result<(), String> {
    std::fs::create_dir_all(data_dir)
        .map_err(|e| format!("create {}: {}", data_dir, e))?;
    let json = serde_json::to_string_pretty(info)
        .map_err(|e| format!("serialize wg state: {}", e))?;

    let path = state_path(data_dir);
    let tmp = format!("{}.tmp", path);
    {
        use std::os::unix::fs::OpenOptionsExt;
        let mut f = std::fs::OpenOptions::new()
            .write(true).create(true).truncate(true).mode(0o600)
            .open(&tmp)
            .map_err(|e| format!("open {}: {}", tmp, e))?;
        f.write_all(json.as_bytes())
            .map_err(|e| format!("write {}: {}", tmp, e))?;
    }
    std::fs::rename(&tmp, &path)
        .map_err(|e| format!("rename {}: {}", path, e))
}

/// Load the persisted overlay assignment. `Ok(None)` strictly means "no
/// wg.json" (never enrolled); `Ok(Some)` is the enrollment grant.
///
/// Everything else is `Err`, and the caller must treat it as fatal: a present
/// wg.json proves this node enrolled, and silently starting in direct mode
/// instead registers the wrong address with the hub (observed in the lab as
/// control_plane=127.0.0.1 plus an endless register-retry loop). That covers
/// both unreadable (EACCES from a wrong file owner under the systemd
/// capability sandbox — no CAP_DAC_OVERRIDE —, EIO, ...) and corrupt
/// (non-UTF-8 or unparseable JSON) state.
pub fn load_state(data_dir: &str) -> Result<Option<JoinInfo>, String> {
    let path = state_path(data_dir);
    let data = match std::fs::read_to_string(&path) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => {
            return Err(format!(
                "{}: unreadable wg state ({}); refusing to guess enrollment status — \
                 fix the file's owner/permissions (or restore it from backup)",
                path, e
            ));
        }
    };
    match serde_json::from_str::<JoinInfo>(&data) {
        Ok(info) => Ok(Some(info)),
        Err(e) => Err(format!(
            "{}: corrupt wg state ({}); refusing to start un-enrolled — restore the \
             file, or delete it and re-join with a fresh token (the hub re-issues the \
             same overlay IP for an unchanged key)",
            path, e
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn good() -> JoinInfo {
        JoinInfo {
            overlay_ip: "10.100.0.2".into(),
            overlay_prefix: 24,
            server_overlay_ip: "10.100.0.1".into(),
            server_pubkey: format!("{}=", "A".repeat(43)),
            server_endpoint: "hub.example.com:51820".into(),
            keepalive_s: 25,
            api_port: 8080,
            agent_token: "felucca_nt_9f2c1b8a7d4e6f0312a5b9c8d7e6f504".into(),
        }
    }

    #[test]
    fn test_join_state_never_persists_the_node_credential() {
        // The node token has exactly one home on disk — its own 0600 file.
        // wg.json is 0600 too, but two copies of a credential is two places to
        // rotate and two places to leak.
        let json = serde_json::to_string(&good()).expect("serialize");
        assert!(!json.contains("agent_token"), "{}", json);
        assert!(!json.contains("felucca_nt_"), "{}", json);
    }

    #[test]
    fn test_join_response_carries_the_node_credential() {
        // Was: the agent parsed the overlay grant and dropped the credential
        // feluccad minted in the same exchange, so feluccad's calls to this node
        // (felucca_nt_...) hit an agent that only knew the shared token.
        let body = r#"{"overlay_ip":"10.100.0.2","overlay_prefix":24,"server_overlay_ip":"10.100.0.1","server_pubkey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","server_endpoint":"hub:51820","keepalive_s":25,"agent_token":"felucca_nt_abc"}"#;
        let info: JoinInfo = serde_json::from_str(body).expect("parse");
        assert_eq!(info.agent_token, "felucca_nt_abc");
        assert!(validate_join_info(&info).is_ok());
        // An older feluccad omits it: still a valid grant, no credential.
        let old = r#"{"overlay_ip":"10.100.0.2","overlay_prefix":24,"server_overlay_ip":"10.100.0.1","server_pubkey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","server_endpoint":"hub:51820","keepalive_s":25}"#;
        let info: JoinInfo = serde_json::from_str(old).expect("parse");
        assert!(info.agent_token.is_empty());
    }

    #[test]
    fn test_validate_accepts_ipv6_endpoint() {
        let mut i = good();
        i.server_endpoint = "[fd00:1::1]:51820".into();
        assert!(validate_join_info(&i).is_ok());
    }

    #[test]
    fn test_api_port_defaults_when_absent() {
        // Server responses never carry api_port; serde defaults it.
        let body = r#"{"overlay_ip":"10.100.0.2","overlay_prefix":24,"server_overlay_ip":"10.100.0.1","server_pubkey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","server_endpoint":"hub:51820","keepalive_s":25}"#;
        let info: JoinInfo = serde_json::from_str(body).expect("parse");
        assert_eq!(info.api_port, 8080);
    }

    #[test]
    fn test_validate_accepts_good() {
        assert!(validate_join_info(&good()).is_ok());
        let mut i = good();
        i.server_endpoint = "192.168.1.10:51820".into();
        assert!(validate_join_info(&i).is_ok());
    }

    #[test]
    fn test_validate_rejects_bad_ips() {
        let mut i = good();
        i.overlay_ip = "999.1.1.1".into();
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.overlay_ip = "10.0.0.1; rm -rf /".into();
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_overlay_ip = "not-an-ip".into();
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_overlay_ip = String::new();
        assert!(validate_join_info(&i).is_err());
    }

    #[test]
    fn test_validate_rejects_bad_prefix() {
        let mut i = good();
        i.overlay_prefix = 0;
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.overlay_prefix = 33;
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.overlay_prefix = 1;
        assert!(validate_join_info(&i).is_ok());
        let mut i = good();
        i.overlay_prefix = 32;
        assert!(validate_join_info(&i).is_ok());
    }

    #[test]
    fn test_validate_rejects_bad_pubkey() {
        // Too short.
        let mut i = good();
        i.server_pubkey = "short=".into();
        assert!(validate_join_info(&i).is_err());
        // 44 chars but no trailing '='.
        let mut i = good();
        i.server_pubkey = "A".repeat(44);
        assert!(validate_join_info(&i).is_err());
        // Right length/suffix but an illegal char inside.
        let mut i = good();
        let mut k = format!("{}=", "A".repeat(43));
        k.replace_range(5..6, ";");
        i.server_pubkey = k;
        assert!(validate_join_info(&i).is_err());
        // Empty.
        let mut i = good();
        i.server_pubkey = String::new();
        assert!(validate_join_info(&i).is_err());
    }

    #[test]
    fn test_validate_rejects_bad_endpoint() {
        let mut i = good();
        i.server_endpoint = "no-port".into();
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_endpoint = "host:notaport".into();
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_endpoint = ":51820".into(); // empty host
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_endpoint = "host:99999".into(); // port > u16
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.server_endpoint = "ho st:51820".into(); // illegal char in host
        assert!(validate_join_info(&i).is_err());
    }

    #[test]
    fn test_validate_rejects_bad_keepalive() {
        let mut i = good();
        i.keepalive_s = 65536;
        assert!(validate_join_info(&i).is_err());
        let mut i = good();
        i.keepalive_s = 65535;
        assert!(validate_join_info(&i).is_ok());
        let mut i = good();
        i.keepalive_s = 0;
        assert!(validate_join_info(&i).is_ok());
    }

    #[test]
    fn test_allowed_ips_slash24() {
        let mut i = good();
        i.server_overlay_ip = "10.100.0.7".into();
        i.overlay_prefix = 24;
        assert_eq!(allowed_ips(&i), "10.100.0.0/24");
    }

    #[test]
    fn test_allowed_ips_slash16() {
        let mut i = good();
        i.server_overlay_ip = "10.100.3.7".into();
        i.overlay_prefix = 16;
        assert_eq!(allowed_ips(&i), "10.100.0.0/16");
    }

    #[test]
    fn test_allowed_ips_slash32() {
        let mut i = good();
        i.server_overlay_ip = "10.100.0.1".into();
        i.overlay_prefix = 32;
        assert_eq!(allowed_ips(&i), "10.100.0.1/32");
    }

    #[test]
    fn test_join_info_serde_round_trip() {
        let info = good();
        let json = serde_json::to_string(&info).expect("serialize");
        let info2: JoinInfo = serde_json::from_str(&json).expect("parse");
        assert_eq!(info2.overlay_ip, info.overlay_ip);
        assert_eq!(info2.overlay_prefix, info.overlay_prefix);
        assert_eq!(info2.server_overlay_ip, info.server_overlay_ip);
        assert_eq!(info2.server_pubkey, info.server_pubkey);
        assert_eq!(info2.server_endpoint, info.server_endpoint);
        assert_eq!(info2.keepalive_s, info.keepalive_s);
    }

    #[test]
    fn test_join_info_parses_server_response_shape() {
        // Exact field names from go/internal/server/join.go.
        let body = r#"{"overlay_ip":"10.100.0.2","overlay_prefix":24,"server_overlay_ip":"10.100.0.1","server_pubkey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=","server_endpoint":"hub:51820","keepalive_s":25}"#;
        let info: JoinInfo = serde_json::from_str(body).expect("parse");
        assert_eq!(info.overlay_ip, "10.100.0.2");
        assert_eq!(info.overlay_prefix, 24);
        assert!(validate_join_info(&info).is_ok());
    }

    #[test]
    fn test_paths() {
        assert_eq!(state_path("/srv/ignis"), "/srv/ignis/wg.json");
        assert_eq!(key_path("/srv/ignis"), "/srv/ignis/wg.key");
    }

    #[test]
    fn test_state_save_load_round_trip() {
        let dir = std::env::temp_dir().join(format!(
            "felucca-wg-test-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        let dir_s = dir.to_string_lossy().to_string();

        let info = good();
        save_state(&dir_s, &info).expect("save");
        let loaded = load_state(&dir_s).expect("load").expect("enrolled");
        assert_eq!(loaded.overlay_ip, info.overlay_ip);
        assert_eq!(loaded.overlay_prefix, info.overlay_prefix);
        assert_eq!(loaded.server_overlay_ip, info.server_overlay_ip);
        assert_eq!(loaded.server_pubkey, info.server_pubkey);
        assert_eq!(loaded.server_endpoint, info.server_endpoint);
        assert_eq!(loaded.keepalive_s, info.keepalive_s);

        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_curl_config_escape_quotes_and_backslashes() {
        assert_eq!(curl_config_escape("plain"), "plain");
        assert_eq!(curl_config_escape(r#"a"b"#), r#"a\"b"#);
        assert_eq!(curl_config_escape(r"a\b"), r"a\\b");
        // Backslash-then-quote: backslash escaped first, then the quote.
        assert_eq!(curl_config_escape(r#"a\"b"#), r#"a\\\"b"#);
        assert_eq!(curl_config_escape(""), "");
    }

    #[test]
    fn test_build_curl_config_shape() {
        let body = r#"{"pubkey":"PK=","hostname":"worker-1"}"#;
        let cfg = build_curl_config("https://hub.example.com:8443", "tok123", body);
        assert!(cfg.contains("url = \"https://hub.example.com:8443/api/v1/nodes/join\""));
        assert!(cfg.contains("request = \"POST\""));
        assert!(cfg.contains("header = \"Authorization: Bearer tok123\""));
        assert!(cfg.contains("header = \"Content-Type: application/json\""));
        // Body quotes must be escaped inside the double-quoted config value.
        assert!(cfg.contains(
            r#"data = "{\"pubkey\":\"PK=\",\"hostname\":\"worker-1\"}""#
        ));
        assert!(cfg.contains("write-out = \"\\n%{http_code}\""));
        assert!(cfg.contains("max-time = 30"));
        // No unescaped raw body line, and the token never appears bare on
        // any non-header line (it only lives in the config, not argv).
        assert!(!cfg.contains(&format!("data = \"{}\"", body)));
    }

    #[test]
    fn test_build_curl_config_trims_trailing_slash() {
        let cfg = build_curl_config("https://hub/", "t", "{}");
        assert!(cfg.contains("url = \"https://hub/api/v1/nodes/join\""));
    }

    #[test]
    fn test_https_api_port_derivation() {
        // Explicit port kept.
        assert_eq!(https_api_port("https://hub.example.com:8443"), 8443);
        assert_eq!(https_api_port("https://hub.example.com:8443/path"), 8443);
        // No port -> 443.
        assert_eq!(https_api_port("https://hub.example.com"), 443);
        assert_eq!(https_api_port("https://hub.example.com/path"), 443);
        // Bracketed IPv6 without a port: inner colons are not a port.
        assert_eq!(https_api_port("https://[fd00::1]"), 443);
        assert_eq!(https_api_port("https://[fd00::1]:8443"), 8443);
    }

    #[test]
    fn test_http_api_port_defaults_8080() {
        // The http join path uses split_host_port, whose portless default
        // is 8080 (NOT 443 — that's https-only).
        let (host, port) = split_host_port("http://hub.example.com");
        assert_eq!(host, "hub.example.com");
        assert_eq!(port, 8080);
        let (_, port) = split_host_port("http://hub.example.com:9090");
        assert_eq!(port, 9090);
    }

    #[test]
    fn test_load_state_missing_is_ok_none() {
        assert!(load_state("/nonexistent/felucca-wg-test")
            .expect("missing file is not an error")
            .is_none());
    }

    #[test]
    fn test_load_state_unparseable_is_err() {
        let dir = std::env::temp_dir().join(format!(
            "felucca-wg-garbage-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("wg.json"), "not json {").unwrap();
        let err = load_state(&dir.to_string_lossy()).unwrap_err();
        assert!(err.contains("corrupt wg state"), "got: {}", err);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    #[cfg(unix)]
    fn test_load_state_unreadable_is_err() {
        use std::os::unix::fs::PermissionsExt;
        let dir = std::env::temp_dir().join(format!(
            "felucca-wg-noperm-{}-{:?}",
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let f = dir.join("wg.json");
        std::fs::write(&f, "{}").unwrap();
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o000)).unwrap();
        // root (CAP_DAC_OVERRIDE) can read 0o000 files; the EACCES branch is
        // only reachable as an unprivileged user, so skip under root.
        if std::fs::read_to_string(&f).is_err() {
            let err = load_state(&dir.to_string_lossy()).unwrap_err();
            assert!(err.contains("unreadable wg state"), "got: {}", err);
        }
        std::fs::set_permissions(&f, std::fs::Permissions::from_mode(0o600)).ok();
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_ensure_interface_unreadable_key_names_the_key() {
        // Must fail BEFORE shelling out, with the key path in the error.
        let err = ensure_interface(&good(), "/nonexistent/felucca-wg-test.key").unwrap_err();
        assert!(err.contains("/nonexistent/felucca-wg-test.key"), "got: {}", err);
    }
}
