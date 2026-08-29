//! Configuration loading for hearth-agent.
//!
//! Precedence (highest first): command-line flags > env vars > JSON config file > defaults.
//! Keys: bind/HEARTH_AGENT_BIND/--bind, control_plane/HEARTH_CONTROL_PLANE/--control-plane,
//! advertise_addr/HEARTH_ADVERTISE_ADDR/--advertise-addr, data_dir/HEARTH_DATA_DIR/--data-dir,
//! token/HEARTH_TOKEN/--token, pool_size/HEARTH_POOL_SIZE/--pool-size,
//! net/HEARTH_NET/--net (on/off/true/false/1/0), net_cidr/HEARTH_NET_CIDR/--net-cidr,
//! join_url/HEARTH_JOIN_URL/--join, join_token/HEARTH_JOIN_TOKEN/--join-token,
//! bind_any/HEARTH_BIND_ANY/--bind-any (on/off).
//! bind→port quirk: trailing :N in bind overrides port.

use crate::ipalloc::Cidr;
use serde::Deserialize;
use std::collections::HashMap;

#[derive(Debug, Clone)]
pub struct Config {
    pub bind: String,
    pub control_plane: String,
    pub advertise_addr: String,
    pub data_dir: String,
    pub token: String,
    /// WireGuard overlay join (v4 P2): hearthd join URL + single-use token.
    pub join_url: String,
    pub join_token: String,
    pub net: bool,
    pub net_cidr: String,
    pub pool_size: u32,
    /// Derived from bind (trailing :port wins).
    pub port: u16,
    /// Opt back in to the wildcard listener. Off by default: 0.0.0.0 includes
    /// the bridge gateway, which every guest has as its default route, so a
    /// wildcard bind hands the root control API to every tenant's sandbox.
    pub bind_any: bool,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            bind: "0.0.0.0:9090".into(),
            control_plane: "http://127.0.0.1:8080".into(),
            advertise_addr: String::new(),
            data_dir: "/srv/ignis".into(),
            token: String::new(),
            join_url: String::new(),
            join_token: String::new(),
            net: true,
            net_cidr: "10.231.0.0/24".into(),
            pool_size: 0,
            port: 9090,
            bind_any: false,
        }
    }
}

/// Partial config from JSON file (all optional).
#[derive(Deserialize, Default)]
struct FileConfig {
    bind: Option<String>,
    control_plane: Option<String>,
    advertise_addr: Option<String>,
    data_dir: Option<String>,
    token: Option<String>,
    join_url: Option<String>,
    join_token: Option<String>,
    net: Option<serde_json::Value>,
    net_cidr: Option<String>,
    pool_size: Option<u64>,
    port: Option<u64>,
    bind_any: Option<serde_json::Value>,
}

fn parse_bool(v: &str) -> Option<bool> {
    let lo = v.to_lowercase();
    match lo.as_str() {
        "on" | "true" | "1" => Some(true),
        "off" | "false" | "0" => Some(false),
        _ => None,
    }
}

pub fn load(args: &[String], env: &HashMap<String, String>) -> Result<Config, String> {
    let mut cfg = Config::default();

    // --- 1) File layer (lowest precedence above defaults) ---
    // Find --config flag or HEARTH_CONFIG env.
    let mut config_path: Option<String> = env.get("HEARTH_CONFIG").cloned();
    let mut i = 1usize;
    while i < args.len() {
        if args[i] == "--config" {
            if i + 1 < args.len() {
                config_path = Some(args[i + 1].clone());
                i += 2;
                continue;
            }
        }
        i += 1;
    }

    // A path was named explicitly, so an unreadable or malformed file is an
    // error, not a fallback to defaults: those defaults carry an empty token
    // and the wildcard bind, so a deleted or corrupted config would silently
    // demote a locked-down agent to an open one.
    if let Some(path) = config_path {
        let data = std::fs::read_to_string(&path)
            .map_err(|e| format!("config {}: {}", path, e))?;
        let fc: FileConfig = serde_json::from_str(&data)
            .map_err(|e| format!("config {}: {}", path, e))?;
        if let Some(v) = fc.bind { cfg.bind = v; }
        if let Some(v) = fc.control_plane { cfg.control_plane = v; }
        if let Some(v) = fc.advertise_addr { cfg.advertise_addr = v; }
        if let Some(v) = fc.data_dir { cfg.data_dir = v; }
        if let Some(v) = fc.token { cfg.token = v; }
        if let Some(v) = fc.join_url { cfg.join_url = v; }
        if let Some(v) = fc.join_token { cfg.join_token = v; }
        if let Some(v) = fc.net {
            match &v {
                serde_json::Value::Bool(b) => cfg.net = *b,
                serde_json::Value::String(s) => {
                    if let Some(b) = parse_bool(s) { cfg.net = b; }
                }
                _ => {}
            }
        }
        if let Some(v) = fc.net_cidr { cfg.net_cidr = v; }
        if let Some(v) = fc.pool_size { cfg.pool_size = v as u32; }
        if let Some(v) = fc.port { cfg.port = v as u16; }
        if let Some(v) = fc.bind_any {
            match &v {
                serde_json::Value::Bool(b) => cfg.bind_any = *b,
                serde_json::Value::String(s) => {
                    if let Some(b) = parse_bool(s) { cfg.bind_any = b; }
                }
                _ => {}
            }
        }
    }

    // --- 2) Env layer ---
    if let Some(v) = env.get("HEARTH_AGENT_BIND") { cfg.bind = v.clone(); }
    if let Some(v) = env.get("HEARTH_CONTROL_PLANE") { cfg.control_plane = v.clone(); }
    if let Some(v) = env.get("HEARTH_ADVERTISE_ADDR") { cfg.advertise_addr = v.clone(); }
    if let Some(v) = env.get("HEARTH_DATA_DIR") { cfg.data_dir = v.clone(); }
    if let Some(v) = env.get("HEARTH_TOKEN") { cfg.token = v.clone(); }
    if let Some(v) = env.get("HEARTH_JOIN_URL") { cfg.join_url = v.clone(); }
    if let Some(v) = env.get("HEARTH_JOIN_TOKEN") { cfg.join_token = v.clone(); }
    if let Some(v) = env.get("HEARTH_NET") {
        if let Some(b) = parse_bool(v) { cfg.net = b; }
    }
    if let Some(v) = env.get("HEARTH_NET_CIDR") { cfg.net_cidr = v.clone(); }
    if let Some(v) = env.get("HEARTH_POOL_SIZE") {
        if let Ok(n) = v.parse::<u32>() { cfg.pool_size = n; }
    }
    if let Some(v) = env.get("HEARTH_AGENT_PORT") {
        if let Ok(n) = v.parse::<u16>() { cfg.port = n; }
    }
    if let Some(v) = env.get("HEARTH_BIND_ANY") {
        if let Some(b) = parse_bool(v) { cfg.bind_any = b; }
    }

    // --- 3) Flag layer (highest precedence) ---
    let mut i = 1usize;
    while i < args.len() {
        match args[i].as_str() {
            "--bind" => { if i + 1 < args.len() { cfg.bind = args[i+1].clone(); i += 2; continue; } }
            "--control-plane" => { if i + 1 < args.len() { cfg.control_plane = args[i+1].clone(); i += 2; continue; } }
            "--advertise-addr" => { if i + 1 < args.len() { cfg.advertise_addr = args[i+1].clone(); i += 2; continue; } }
            "--data-dir" => { if i + 1 < args.len() { cfg.data_dir = args[i+1].clone(); i += 2; continue; } }
            "--token" => { if i + 1 < args.len() { cfg.token = args[i+1].clone(); i += 2; continue; } }
            "--join" => { if i + 1 < args.len() { cfg.join_url = args[i+1].clone(); i += 2; continue; } }
            "--join-token" => { if i + 1 < args.len() { cfg.join_token = args[i+1].clone(); i += 2; continue; } }
            "--net" => {
                if i + 1 < args.len() {
                    if let Some(b) = parse_bool(&args[i+1]) { cfg.net = b; }
                    i += 2; continue;
                }
            }
            "--net-cidr" => { if i + 1 < args.len() { cfg.net_cidr = args[i+1].clone(); i += 2; continue; } }
            "--bind-any" => {
                if i + 1 < args.len() {
                    if let Some(b) = parse_bool(&args[i+1]) { cfg.bind_any = b; }
                    i += 2; continue;
                }
            }
            "--pool-size" => {
                if i + 1 < args.len() {
                    if let Ok(n) = args[i+1].parse::<u32>() { cfg.pool_size = n; }
                    i += 2; continue;
                }
            }
            "--port" => {
                if i + 1 < args.len() {
                    if let Ok(n) = args[i+1].parse::<u16>() { cfg.port = n; }
                    i += 2; continue;
                }
            }
            _ => {}
        }
        i += 1;
    }

    // bind→port quirk: trailing :N in bind overrides port.
    if let Some(colon) = cfg.bind.rfind(':') {
        if let Ok(p) = cfg.bind[colon + 1..].parse::<u16>() {
            cfg.port = p;
        }
    }

    Ok(cfg)
}

/// Tokens that ship in the repo's example configs and lab scripts. They are
/// public constants, so a deploy still carrying one has no credential at all —
/// anyone can read it from GitHub and drive the root agent API with it.
const PLACEHOLDER_TOKENS: &[&str] = &[
    "REPLACE_WITH_SAME_TOKEN_AS_HEARTHD",
    "REPLACE_WITH_OUTPUT_OF__openssl_rand_-hex_32",
    "hearth-lab-token",
];

/// Shortest secret we will let guard the agent API. There is no rate limiting
/// on the bearer gate, so anything guessable online is no gate at all.
const MIN_TOKEN_LEN: usize = 16;

/// Reject tokens that authenticate nobody in practice. Startup calls this and
/// refuses to run on Err: this API execs into every guest on the node and
/// streams their disks, so an agent with no real credential is worse than an
/// agent that is down.
pub fn validate_token(token: &str) -> Result<(), String> {
    if token.is_empty() {
        return Err("no token configured — set `token` in the config file, HEARTH_TOKEN, or --token".into());
    }
    let lower = token.to_ascii_lowercase();
    // Prefix match, not just the exact literals: every shipped placeholder
    // carries REPLACE_WITH, and operators invent variations of it.
    if lower.contains("replace_with")
        || PLACEHOLDER_TOKENS.iter().any(|p| p.to_ascii_lowercase() == lower)
    {
        return Err("token is a known placeholder — generate a real one with `openssl rand -hex 32`".into());
    }
    if token.len() < MIN_TOKEN_LEN {
        return Err(format!(
            "token is shorter than {} bytes — generate one with `openssl rand -hex 32`",
            MIN_TOKEN_LEN
        ));
    }
    Ok(())
}

// ---- per-node credential (hearthd -> agent), persisted ----

/// File holding the per-node bearer hearthd mints for this worker at
/// enrollment (`hearth_nt_...`). hearthd presents it on every control-plane
/// call to this node and resolves it to a NODE principal, never to admin, so a
/// token harvested off one worker is worth that worker and nothing else.
///
/// Its own file rather than a field in wg.json: the same credential is issued
/// by BOTH enrollment routes (POST /api/v1/nodes/join and a register carrying
/// a join token) and only the first of those writes wg.json. One store means a
/// rotation has exactly one place to land.
pub fn node_token_path(data_dir: &str) -> String {
    format!("{}/node-token", data_dir)
}

/// Persist this node's own credential: 0600, temp+rename.
///
/// Same shape as wg::save_state — a crash mid-write leaves either the old
/// credential or the new one, never a truncated file. 0600 because this is a
/// secret and `data_dir` is not private (audit finding M2 was exactly a
/// world-readable persisted credential).
pub fn save_node_token(data_dir: &str, token: &str) -> Result<(), String> {
    // The startup guards apply to a node token too (C1/H5). Refusing here as
    // well as at load keeps an unusable credential from ever reaching the
    // file, where it would brick the NEXT boot rather than this one.
    validate_token(token).map_err(|e| format!("refusing to persist node token: {}", e))?;

    std::fs::create_dir_all(data_dir)
        .map_err(|e| format!("create {}: {}", data_dir, e))?;

    let path = node_token_path(data_dir);
    let tmp = format!("{}.tmp", path);
    {
        use std::io::Write;
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        let mut f = std::fs::OpenOptions::new()
            .write(true).create(true).truncate(true).mode(0o600)
            .open(&tmp)
            .map_err(|e| format!("open {}: {}", tmp, e))?;
        // .mode() is honoured only when open(2) CREATES the file: a .tmp left
        // by a crashed write keeps whatever mode it already had. Set it
        // explicitly so every path through here ends at 0600.
        std::fs::set_permissions(&tmp, std::fs::Permissions::from_mode(0o600))
            .map_err(|e| format!("chmod {}: {}", tmp, e))?;
        f.write_all(token.as_bytes())
            .map_err(|e| format!("write {}: {}", tmp, e))?;
        // Durable before the rename: the rename is what publishes the new
        // credential, and it must never name unwritten bytes.
        f.sync_all()
            .map_err(|e| format!("sync {}: {}", tmp, e))?;
    }
    std::fs::rename(&tmp, &path)
        .map_err(|e| format!("rename {}: {}", path, e))
}

/// Load this node's own credential.
///
/// `Ok(None)` strictly means "no such file": a node that never enrolled, or a
/// deployment that predates per-node credentials. That is the safe-rollout
/// case, and the caller falls back to the configured shared token — it must
/// NOT be a startup failure, or upgrading the agent would take the fleet down.
///
/// Everything else is `Err` and the caller treats it as fatal. A file that
/// exists but cannot be used is not the same as no file: hearthd is presenting
/// this credential on every call to this node, so guessing "no credential"
/// would come up as a node that 401s the control plane with no explanation.
pub fn load_node_token(data_dir: &str) -> Result<Option<String>, String> {
    let path = node_token_path(data_dir);

    // symlink_metadata, so the checks below describe THIS path and not
    // whatever it points at. save_node_token publishes by rename and would
    // replace a symlink anyway, so one here is never something we wrote.
    let meta = match std::fs::symlink_metadata(&path) {
        Ok(m) => m,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(e) => {
            return Err(format!(
                "{}: unreadable node token ({}); fix the file's owner/permissions, \
                 or delete it to fall back to the configured shared token",
                path, e
            ));
        }
    };
    if meta.file_type().is_symlink() {
        return Err(format!(
            "{}: node token is a symlink — refusing to read this node's credential \
             through one; delete it and re-enroll with a fresh join token",
            path
        ));
    }
    use std::os::unix::fs::PermissionsExt;
    let mode = meta.permissions().mode() & 0o777;
    if mode & 0o077 != 0 {
        // Refuse rather than warn-and-use: a credential this node's other
        // users can read is already disclosed, and quietly running on it is
        // how M2 survived three rounds.
        return Err(format!(
            "{}: node token is readable beyond its owner (mode {:04o}) — run \
             `chmod 600 {}`, or delete it and re-enroll with a fresh join token",
            path, mode, path
        ));
    }

    let data = std::fs::read_to_string(&path)
        .map_err(|e| format!("{}: unreadable node token ({})", path, e))?;
    let token = data.trim().to_string();
    // A node token gets no exemption from the startup guards (C1/H5).
    validate_token(&token).map_err(|e| {
        format!(
            "{}: unusable node token ({}); delete the file to fall back to the \
             configured shared token, or re-enroll with a fresh join token",
            path, e
        )
    })?;
    Ok(Some(token))
}

/// Host component of a `bind` value, or "" when it names no specific address.
///
/// A wildcard is reported as "" on purpose: "0.0.0.0" is what every shipped
/// example and the compiled default say, and it is exactly the value that must
/// NOT be honoured verbatim — the caller substitutes the management address.
/// Only a concrete host is a deliberate operator choice.
pub fn bind_host(bind: &str) -> &str {
    let b = bind.trim();
    // [::1]:9090 — bracketed IPv6 literal.
    if let Some(rest) = b.strip_prefix('[') {
        if let Some(close) = rest.find(']') {
            return wildcard_to_empty(&rest[..close]);
        }
        return "";
    }
    // A lone trailing :port leaves the host empty; a bare host has no colon.
    // More than one colon and no brackets is an unbracketed IPv6 literal,
    // which has no port to strip.
    let host = match (b.find(':'), b.rfind(':')) {
        (Some(f), Some(l)) if f == l => &b[..f],
        (Some(_), Some(_)) => b,
        _ => b,
    };
    wildcard_to_empty(host)
}

fn wildcard_to_empty(host: &str) -> &str {
    match host {
        "0.0.0.0" | "::" | "*" | "" => "",
        h => h,
    }
}

/// Join a host and port into a listen address, bracketing IPv6 literals.
pub fn join_host_port(host: &str, port: u16) -> String {
    if host.contains(':') && !host.starts_with('[') {
        format!("[{}]:{}", host, port)
    } else {
        format!("{}:{}", host, port)
    }
}

/// True when `host` sits inside the guest bridge CIDR. The agent must never
/// listen on an address guests can route to: the gateway is their default
/// route, so a listener there is reachable from inside every sandbox.
pub fn addr_in_guest_cidr(host: &str, cidr: Cidr) -> bool {
    let Ok(ip) = host.parse::<std::net::Ipv4Addr>() else { return false; };
    (u32::from(ip) & cidr.mask()) == cidr.net_base()
}

/// Split "http://host:port/..." into (host, port).
pub fn split_host_port(addr: &str) -> (&str, u16) {
    let mut a = addr;
    if let Some(rest) = a.strip_prefix("http://") { a = rest; }
    else if let Some(rest) = a.strip_prefix("https://") { a = rest; }
    if let Some(slash) = a.find('/') { a = &a[..slash]; }
    if let Some(colon) = a.rfind(':') {
        let host = &a[..colon];
        let port = a[colon + 1..].parse::<u16>().unwrap_or(8080);
        return (host, port);
    }
    (a, 8080)
}

/// Constant-time byte-slice equality (XOR loop, no short-circuit).
pub fn constant_time_eq(a: &str, b: &str) -> bool {
    let a = a.as_bytes();
    let b = b.as_bytes();
    if a.len() != b.len() { return false; }
    let mut diff: u8 = 0;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// Returns true if the request is authorized: the Authorization header must be
/// exactly "Bearer <token>".
///
/// An empty configured token authorizes NOBODY. `validate_token` makes an empty
/// token a startup failure, so this branch is only reachable if that guard is
/// ever bypassed — and "no credential configured" must then mean "no access",
/// never "open to the world". This agent runs as root and drives Firecracker.
pub fn authorized(token: &str, authorization: Option<&str>) -> bool {
    if token.is_empty() { return false; }
    let Some(auth) = authorization else { return false; };
    let Some(bearer) = auth.strip_prefix("Bearer ") else { return false; };
    constant_time_eq(token, bearer)
}

/// Authorized when the bearer matches EITHER credential this node answers to:
/// the per-node token hearthd minted for it, or the configured shared token.
///
/// Both are accepted on purpose. hearthd presents the per-node token to a node
/// it enrolled with a join token and the shared token to one it has not
/// re-enrolled yet, and a fleet is upgraded one node at a time — accepting
/// only the node token would 401 every call to a not-yet-re-enrolled worker
/// (exec, expose, sleep, wake, delete, the lifecycle sweep), and accepting
/// only the shared token is the break this replaces.
///
/// `node` is "" until hearthd issues one, and an empty token authorizes nobody
/// (see `authorized`), so the unenrolled case is the shared token alone.
pub fn authorized_either(shared: &str, node: &str, authorization: Option<&str>) -> bool {
    // Bitwise `|`, not `||`: both comparisons run on every request, so how
    // long the check takes never says WHICH credential the caller presented.
    authorized(shared, authorization) | authorized(node, authorization)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_bool() {
        assert_eq!(parse_bool("on"), Some(true));
        assert_eq!(parse_bool("ON"), Some(true));
        assert_eq!(parse_bool("off"), Some(false));
        assert_eq!(parse_bool("true"), Some(true));
        assert_eq!(parse_bool("false"), Some(false));
        assert_eq!(parse_bool("1"), Some(true));
        assert_eq!(parse_bool("0"), Some(false));
        assert_eq!(parse_bool("garbage"), None);
    }

    #[test]
    fn test_defaults() {
        let cfg = load(&["hearth-agent".into()], &HashMap::new()).expect("no config file");
        assert_eq!(cfg.bind, "0.0.0.0:9090");
        assert_eq!(cfg.port, 9090);
        assert!(cfg.net);
        assert_eq!(cfg.net_cidr, "10.231.0.0/24");
        assert_eq!(cfg.pool_size, 0);
        assert_eq!(cfg.data_dir, "/srv/ignis");
    }

    #[test]
    fn test_bind_port_quirk() {
        let args: Vec<String> = vec!["hearth-agent".into(), "--bind".into(), "0.0.0.0:8888".into()];
        let cfg = load(&args, &HashMap::new()).expect("no config file");
        assert_eq!(cfg.port, 8888);
    }

    #[test]
    fn test_env_overrides_default() {
        let mut env = HashMap::new();
        env.insert("HEARTH_DATA_DIR".into(), "/tmp/ignis".into());
        env.insert("HEARTH_NET".into(), "off".into());
        env.insert("HEARTH_POOL_SIZE".into(), "3".into());
        let cfg = load(&["hearth-agent".into()], &env).expect("no config file");
        assert_eq!(cfg.data_dir, "/tmp/ignis");
        assert!(!cfg.net);
        assert_eq!(cfg.pool_size, 3);
    }

    #[test]
    fn test_flag_overrides_env() {
        let mut env = HashMap::new();
        env.insert("HEARTH_NET".into(), "off".into());
        let args: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "on".into()];
        let cfg = load(&args, &env).expect("no config file");
        assert!(cfg.net);
    }

    #[test]
    fn test_join_defaults_empty() {
        let cfg = load(&["hearth-agent".into()], &HashMap::new()).expect("no config file");
        assert!(cfg.join_url.is_empty());
        assert!(cfg.join_token.is_empty());
    }

    #[test]
    fn test_join_env_overrides_default() {
        let mut env = HashMap::new();
        env.insert("HEARTH_JOIN_URL".into(), "http://hub.example.com:8080".into());
        env.insert("HEARTH_JOIN_TOKEN".into(), "hearth_jt_abc".into());
        let cfg = load(&["hearth-agent".into()], &env).expect("no config file");
        assert_eq!(cfg.join_url, "http://hub.example.com:8080");
        assert_eq!(cfg.join_token, "hearth_jt_abc");
    }

    #[test]
    fn test_join_flag_overrides_env() {
        let mut env = HashMap::new();
        env.insert("HEARTH_JOIN_URL".into(), "http://env-host:1111".into());
        env.insert("HEARTH_JOIN_TOKEN".into(), "env-token".into());
        let args: Vec<String> = vec![
            "hearth-agent".into(),
            "--join".into(), "http://flag-host:2222".into(),
            "--join-token".into(), "flag-token".into(),
        ];
        let cfg = load(&args, &env).expect("no config file");
        assert_eq!(cfg.join_url, "http://flag-host:2222");
        assert_eq!(cfg.join_token, "flag-token");
    }

    #[test]
    fn test_net_on_off_parsing() {
        let args: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "off".into()];
        let cfg = load(&args, &HashMap::new()).expect("no config file");
        assert!(!cfg.net);
        let args2: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "1".into()];
        let cfg2 = load(&args2, &HashMap::new()).expect("no config file");
        assert!(cfg2.net);
    }

    #[test]
    fn test_constant_time_eq() {
        assert!(constant_time_eq("secret", "secret"));
        assert!(!constant_time_eq("secret", "secreT"));
        assert!(!constant_time_eq("secret", "secret2"));
        assert!(!constant_time_eq("", "x"));
        assert!(constant_time_eq("", ""));
    }

    #[test]
    fn test_authorized_empty_token_denies_everyone() {
        // Was: an empty token authorized every caller. A deploy that skipped
        // the JSON config then ran the root agent with no authentication.
        assert!(!authorized("", None));
        assert!(!authorized("", Some("Bearer anything")));
        assert!(!authorized("", Some("Bearer ")));
        assert!(!authorized("", Some("")));
    }

    #[test]
    fn test_authorized_with_token() {
        assert!(authorized("mytoken", Some("Bearer mytoken")));
        assert!(!authorized("mytoken", Some("Bearer wrong")));
        assert!(!authorized("mytoken", None));
        assert!(!authorized("mytoken", Some("mytoken")));
    }

    #[test]
    fn test_validate_token_rejects_empty() {
        assert!(validate_token("").is_err());
    }

    #[test]
    fn test_validate_token_rejects_shipped_placeholders() {
        // Every one of these is a literal committed to the public repo.
        assert!(validate_token("REPLACE_WITH_SAME_TOKEN_AS_HEARTHD").is_err());
        assert!(validate_token("REPLACE_WITH_OUTPUT_OF__openssl_rand_-hex_32").is_err());
        assert!(validate_token("hearth-lab-token").is_err());
        // Case-insensitive, and any variation carrying REPLACE_WITH.
        assert!(validate_token("replace_with_same_token_as_hearthd").is_err());
        assert!(validate_token("REPLACE_WITH_ANYTHING_ELSE_ENTIRELY").is_err());
        assert!(validate_token("prefix-replace_with-suffix-padding").is_err());
    }

    #[test]
    fn test_validate_token_rejects_short() {
        assert!(validate_token("short").is_err());
        assert!(validate_token(&"a".repeat(MIN_TOKEN_LEN - 1)).is_err());
        assert!(validate_token(&"a".repeat(MIN_TOKEN_LEN)).is_ok());
    }

    #[test]
    fn test_validate_token_accepts_real_secret() {
        // `openssl rand -hex 32` shape.
        assert!(validate_token(
            "9f2c1b8a7d4e6f0312a5b9c8d7e6f504132435465768798a9bacbdcedfe0f102"
        )
        .is_ok());
    }

    #[test]
    fn test_load_missing_config_file_is_error() {
        // Was: an unreadable config silently fell back to defaults, whose
        // token is empty — a deleted config turned authentication off.
        let mut env = HashMap::new();
        env.insert("HEARTH_CONFIG".into(), "/nonexistent/hearth-agent.json".into());
        assert!(load(&["hearth-agent".into()], &env).is_err());
    }

    #[test]
    fn test_load_malformed_config_file_is_error() {
        let dir = std::env::temp_dir().join("hearth-cfg-malformed");
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("hearth-agent.json");
        std::fs::write(&path, "{ not json").unwrap();
        let mut env = HashMap::new();
        env.insert("HEARTH_CONFIG".into(), path.to_string_lossy().into_owned());
        assert!(load(&["hearth-agent".into()], &env).is_err());
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_bind_host_reports_wildcards_as_unset() {
        // Was: the listener was hardcoded to 0.0.0.0 regardless of `bind`.
        // A wildcard is not an address choice, so it must not be honoured.
        assert_eq!(bind_host("0.0.0.0:9090"), "");
        assert_eq!(bind_host("0.0.0.0"), "");
        assert_eq!(bind_host(":9090"), "");
        assert_eq!(bind_host("[::]:9090"), "");
        assert_eq!(bind_host("*:9090"), "");
        assert_eq!(bind_host(""), "");
    }

    #[test]
    fn test_bind_host_honours_explicit_address() {
        assert_eq!(bind_host("127.0.0.1:9090"), "127.0.0.1");
        assert_eq!(bind_host("10.0.1.42:9090"), "10.0.1.42");
        assert_eq!(bind_host("10.0.1.42"), "10.0.1.42");
        assert_eq!(bind_host("[fd00::1]:9090"), "fd00::1");
        assert_eq!(bind_host("fd00::1"), "fd00::1");
    }

    #[test]
    fn test_bind_any_defaults_off_and_parses() {
        let cfg = load(&["hearth-agent".into()], &HashMap::new()).expect("no config file");
        assert!(!cfg.bind_any);
        let args: Vec<String> = vec!["hearth-agent".into(), "--bind-any".into(), "on".into()];
        assert!(load(&args, &HashMap::new()).expect("no config file").bind_any);
        let mut env = HashMap::new();
        env.insert("HEARTH_BIND_ANY".into(), "true".into());
        assert!(load(&["hearth-agent".into()], &env).expect("no config file").bind_any);
    }

    #[test]
    fn test_join_host_port_brackets_ipv6() {
        assert_eq!(join_host_port("10.0.1.42", 9090), "10.0.1.42:9090");
        assert_eq!(join_host_port("fd00::1", 9090), "[fd00::1]:9090");
        assert_eq!(join_host_port("[fd00::1]", 9090), "[fd00::1]:9090");
    }

    #[test]
    fn test_addr_in_guest_cidr() {
        let c = Cidr::parse("10.231.0.0/24").unwrap();
        // The gateway is every guest's default route — the address that made
        // the wildcard bind reachable from inside sandboxes.
        assert!(addr_in_guest_cidr("10.231.0.1", c));
        assert!(addr_in_guest_cidr("10.231.0.7", c));
        assert!(!addr_in_guest_cidr("10.0.1.42", c));
        assert!(!addr_in_guest_cidr("127.0.0.1", c));
        assert!(!addr_in_guest_cidr("", c));
        assert!(!addr_in_guest_cidr("fd00::1", c));
    }

    // ---- per-node credential ----

    /// Unique scratch dir per test (these run in parallel in one process).
    fn scratch(name: &str) -> String {
        let d = std::env::temp_dir().join(format!(
            "hearth-nodetok-{}-{}-{:?}",
            name,
            std::process::id(),
            std::thread::current().id()
        ));
        std::fs::remove_dir_all(&d).ok();
        std::fs::create_dir_all(&d).unwrap();
        d.to_string_lossy().into_owned()
    }

    fn mode_of(path: &str) -> u32 {
        use std::os::unix::fs::PermissionsExt;
        std::fs::metadata(path).unwrap().permissions().mode() & 0o777
    }

    const NT: &str = "hearth_nt_9f2c1b8a7d4e6f0312a5b9c8d7e6f504";

    #[test]
    fn test_node_token_round_trips_and_is_not_world_readable() {
        // Was: the agent had nowhere to keep the credential hearthd issues, so
        // it kept presenting the fleet admin token instead. M2 is the reason
        // for the mode assertion — a persisted credential must be 0600.
        let dir = scratch("roundtrip");
        assert_eq!(node_token_path(&dir), format!("{}/node-token", dir));
        save_node_token(&dir, NT).expect("save");
        assert_eq!(mode_of(&node_token_path(&dir)), 0o600);
        assert_eq!(load_node_token(&dir).expect("load").as_deref(), Some(NT));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_missing_node_token_file_falls_back_rather_than_failing() {
        // The safe-rollout case: an agent that never enrolled, or a deployment
        // that predates per-node credentials. Ok(None), never Err — otherwise
        // upgrading the agent takes the fleet down.
        assert_eq!(
            load_node_token("/nonexistent/hearth-node-token-test").expect("no file is not an error"),
            None
        );
    }

    #[test]
    fn test_node_token_rotation_replaces_cleanly() {
        // hearthd mints a fresh credential on every re-enrollment.
        let dir = scratch("rotate");
        let first = "hearth_nt_1111111111111111111111111111";
        save_node_token(&dir, first).expect("save first");
        save_node_token(&dir, NT).expect("save second");
        assert_eq!(load_node_token(&dir).expect("load").as_deref(), Some(NT));
        assert_eq!(mode_of(&node_token_path(&dir)), 0o600);
        // temp+rename: the publish is one atomic step, and no half-written
        // file is left where the next boot could read it.
        assert!(!std::path::Path::new(&format!("{}.tmp", node_token_path(&dir))).exists());
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_node_token_does_not_bypass_the_startup_guards() {
        // C1/H5 apply to this credential too, at BOTH ends: a placeholder or a
        // guessable secret is never written, and never loaded if some other
        // writer put one there.
        let dir = scratch("guards");
        for bad in ["", "short", "hearth-lab-token", "REPLACE_WITH_SAME_TOKEN_AS_HEARTHD"] {
            assert!(save_node_token(&dir, bad).is_err(), "persisted {:?}", bad);
            std::fs::write(node_token_path(&dir), bad).unwrap();
            assert!(load_node_token(&dir).is_err(), "loaded {:?}", bad);
        }
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_load_node_token_refuses_a_file_readable_beyond_its_owner() {
        use std::os::unix::fs::PermissionsExt;
        let dir = scratch("perms");
        let path = node_token_path(&dir);
        std::fs::write(&path, NT).unwrap();
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o644)).unwrap();
        let err = load_node_token(&dir).unwrap_err();
        assert!(err.contains("beyond its owner"), "{}", err);
        // Tightening it makes the same file usable again.
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600)).unwrap();
        assert_eq!(load_node_token(&dir).expect("load").as_deref(), Some(NT));
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_load_node_token_refuses_a_symlinked_credential() {
        // A symlink here is never something save_node_token wrote (it
        // publishes by rename), so reading a credential through one would only
        // ever be someone else's choice of file.
        // The scratch name deliberately avoids the word being asserted on —
        // the path is interpolated into the message, so a name like "symlink"
        // would satisfy the assertion no matter which branch produced it.
        let dir = scratch("lnk");
        let real = format!("{}/elsewhere", dir);
        std::fs::write(&real, NT).unwrap();
        std::os::unix::fs::symlink(&real, node_token_path(&dir)).unwrap();
        let err = load_node_token(&dir).unwrap_err();
        assert!(err.contains("is a symlink"), "{}", err);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn test_authorized_either_accepts_both_credentials_and_nothing_else() {
        // Was: only the shared token was accepted, so every hearthd call to a
        // join-enrolled node 401'd — exec, expose, sleep, wake, delete and the
        // lifecycle sweep all failed against it.
        let shared = "9f2c1b8a7d4e6f0312a5b9c8d7e6f504";
        assert!(authorized_either(shared, NT, Some(&format!("Bearer {}", shared))));
        assert!(authorized_either(shared, NT, Some(&format!("Bearer {}", NT))));
        assert!(!authorized_either(shared, NT, Some("Bearer wrong")));
        assert!(!authorized_either(shared, NT, None));
        // The "Bearer " prefix is still required, exactly.
        assert!(!authorized_either(shared, NT, Some(NT)));
        assert!(!authorized_either(shared, NT, Some(&format!("bearer {}", NT))));
        assert!(!authorized_either(shared, NT, Some(&format!("Bearer  {}", NT))));
        // No node token yet (the pre-enrollment case): shared only, and an
        // empty credential still authorizes nobody.
        assert!(authorized_either(shared, "", Some(&format!("Bearer {}", shared))));
        assert!(!authorized_either(shared, "", Some("Bearer ")));
        assert!(!authorized_either("", "", Some("Bearer ")));
        assert!(!authorized_either("", "", Some("Bearer anything")));
    }

    #[test]
    fn test_split_host_port() {
        assert_eq!(split_host_port("http://127.0.0.1:8080"), ("127.0.0.1", 8080));
        assert_eq!(split_host_port("http://127.0.0.1:8080/api"), ("127.0.0.1", 8080));
        assert_eq!(split_host_port("https://hearth.example.com"), ("hearth.example.com", 8080));
        assert_eq!(split_host_port("192.168.1.1:9000"), ("192.168.1.1", 9000));
    }
}
