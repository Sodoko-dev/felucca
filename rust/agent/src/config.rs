//! Configuration loading for hearth-agent.
//!
//! Precedence (highest first): command-line flags > env vars > JSON config file > defaults.
//! Keys: bind/HEARTH_AGENT_BIND/--bind, control_plane/HEARTH_CONTROL_PLANE/--control-plane,
//! advertise_addr/HEARTH_ADVERTISE_ADDR/--advertise-addr, data_dir/HEARTH_DATA_DIR/--data-dir,
//! token/HEARTH_TOKEN/--token, pool_size/HEARTH_POOL_SIZE/--pool-size,
//! net/HEARTH_NET/--net (on/off/true/false/1/0), net_cidr/HEARTH_NET_CIDR/--net-cidr.
//! bind→port quirk: trailing :N in bind overrides port.

use serde::Deserialize;
use std::collections::HashMap;

#[derive(Debug, Clone)]
pub struct Config {
    pub bind: String,
    pub control_plane: String,
    pub advertise_addr: String,
    pub data_dir: String,
    pub token: String,
    pub net: bool,
    pub net_cidr: String,
    pub pool_size: u32,
    /// Derived from bind (trailing :port wins).
    pub port: u16,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            bind: "0.0.0.0:9090".into(),
            control_plane: "http://127.0.0.1:8080".into(),
            advertise_addr: String::new(),
            data_dir: "/srv/ignis".into(),
            token: String::new(),
            net: true,
            net_cidr: "10.231.0.0/24".into(),
            pool_size: 0,
            port: 9090,
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
    net: Option<serde_json::Value>,
    net_cidr: Option<String>,
    pool_size: Option<u64>,
    port: Option<u64>,
}

fn parse_bool(v: &str) -> Option<bool> {
    let lo = v.to_lowercase();
    match lo.as_str() {
        "on" | "true" | "1" => Some(true),
        "off" | "false" | "0" => Some(false),
        _ => None,
    }
}

pub fn load(args: &[String], env: &HashMap<String, String>) -> Config {
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

    if let Some(path) = config_path {
        if let Ok(data) = std::fs::read_to_string(&path) {
            if let Ok(fc) = serde_json::from_str::<FileConfig>(&data) {
                if let Some(v) = fc.bind { cfg.bind = v; }
                if let Some(v) = fc.control_plane { cfg.control_plane = v; }
                if let Some(v) = fc.advertise_addr { cfg.advertise_addr = v; }
                if let Some(v) = fc.data_dir { cfg.data_dir = v; }
                if let Some(v) = fc.token { cfg.token = v; }
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
            }
        }
    }

    // --- 2) Env layer ---
    if let Some(v) = env.get("HEARTH_AGENT_BIND") { cfg.bind = v.clone(); }
    if let Some(v) = env.get("HEARTH_CONTROL_PLANE") { cfg.control_plane = v.clone(); }
    if let Some(v) = env.get("HEARTH_ADVERTISE_ADDR") { cfg.advertise_addr = v.clone(); }
    if let Some(v) = env.get("HEARTH_DATA_DIR") { cfg.data_dir = v.clone(); }
    if let Some(v) = env.get("HEARTH_TOKEN") { cfg.token = v.clone(); }
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

    // --- 3) Flag layer (highest precedence) ---
    let mut i = 1usize;
    while i < args.len() {
        match args[i].as_str() {
            "--bind" => { if i + 1 < args.len() { cfg.bind = args[i+1].clone(); i += 2; continue; } }
            "--control-plane" => { if i + 1 < args.len() { cfg.control_plane = args[i+1].clone(); i += 2; continue; } }
            "--advertise-addr" => { if i + 1 < args.len() { cfg.advertise_addr = args[i+1].clone(); i += 2; continue; } }
            "--data-dir" => { if i + 1 < args.len() { cfg.data_dir = args[i+1].clone(); i += 2; continue; } }
            "--token" => { if i + 1 < args.len() { cfg.token = args[i+1].clone(); i += 2; continue; } }
            "--net" => {
                if i + 1 < args.len() {
                    if let Some(b) = parse_bool(&args[i+1]) { cfg.net = b; }
                    i += 2; continue;
                }
            }
            "--net-cidr" => { if i + 1 < args.len() { cfg.net_cidr = args[i+1].clone(); i += 2; continue; } }
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

    cfg
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

/// Returns true if the request is authorized.
/// When token is empty, everything is allowed.
/// Otherwise the Authorization header must be "Bearer <token>".
pub fn authorized(token: &str, authorization: Option<&str>) -> bool {
    if token.is_empty() { return true; }
    let Some(auth) = authorization else { return false; };
    let Some(bearer) = auth.strip_prefix("Bearer ") else { return false; };
    constant_time_eq(token, bearer)
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
        let cfg = load(&["hearth-agent".into()], &HashMap::new());
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
        let cfg = load(&args, &HashMap::new());
        assert_eq!(cfg.port, 8888);
    }

    #[test]
    fn test_env_overrides_default() {
        let mut env = HashMap::new();
        env.insert("HEARTH_DATA_DIR".into(), "/tmp/ignis".into());
        env.insert("HEARTH_NET".into(), "off".into());
        env.insert("HEARTH_POOL_SIZE".into(), "3".into());
        let cfg = load(&["hearth-agent".into()], &env);
        assert_eq!(cfg.data_dir, "/tmp/ignis");
        assert!(!cfg.net);
        assert_eq!(cfg.pool_size, 3);
    }

    #[test]
    fn test_flag_overrides_env() {
        let mut env = HashMap::new();
        env.insert("HEARTH_NET".into(), "off".into());
        let args: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "on".into()];
        let cfg = load(&args, &env);
        assert!(cfg.net);
    }

    #[test]
    fn test_net_on_off_parsing() {
        let args: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "off".into()];
        let cfg = load(&args, &HashMap::new());
        assert!(!cfg.net);
        let args2: Vec<String> = vec!["hearth-agent".into(), "--net".into(), "1".into()];
        let cfg2 = load(&args2, &HashMap::new());
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
    fn test_authorized_no_token() {
        assert!(authorized("", None));
        assert!(authorized("", Some("Bearer anything")));
    }

    #[test]
    fn test_authorized_with_token() {
        assert!(authorized("mytoken", Some("Bearer mytoken")));
        assert!(!authorized("mytoken", Some("Bearer wrong")));
        assert!(!authorized("mytoken", None));
        assert!(!authorized("mytoken", Some("mytoken")));
    }

    #[test]
    fn test_split_host_port() {
        assert_eq!(split_host_port("http://127.0.0.1:8080"), ("127.0.0.1", 8080));
        assert_eq!(split_host_port("http://127.0.0.1:8080/api"), ("127.0.0.1", 8080));
        assert_eq!(split_host_port("https://hearth.example.com"), ("hearth.example.com", 8080));
        assert_eq!(split_host_port("192.168.1.1:9000"), ("192.168.1.1", 9000));
    }
}
