//! Request/response types and the per-connection handler.
//!
//! The handler is generic over `Read + Write` so it can be exercised in tests
//! over an in-process socket pair without needing a real AF_VSOCK device.

use std::io::{Read, Write};
use std::time::Duration;

use serde::{Deserialize, Serialize};

use crate::exec::{run_exec, ExecOutcome};
use crate::netcfg::{apply_set_ip, IpRunner, RealIpRunner};

/// Maximum request line length, in bytes (64 KiB). Longer lines are rejected.
pub const MAX_REQUEST_BYTES: usize = 64 * 1024;
/// Default exec timeout when the request omits `timeout_ms`.
pub const DEFAULT_TIMEOUT_MS: u64 = 30_000;
/// Hard cap on exec timeout regardless of the requested value.
pub const MAX_TIMEOUT_MS: u64 = 300_000;
/// Per-stream (stdout / stderr) output cap, in bytes (1 MiB).
pub const MAX_OUTPUT_BYTES: usize = 1024 * 1024;

/// Incoming request, tagged by the `op` field.
#[derive(Debug, Deserialize)]
#[serde(tag = "op")]
pub enum Request {
    #[serde(rename = "exec")]
    Exec {
        cmd: Vec<String>,
        #[serde(default)]
        timeout_ms: Option<u64>,
    },
    #[serde(rename = "set_ip")]
    SetIp {
        ip: String,
        prefix: u8,
        gw: String,
        dev: String,
    },
}

/// Response written back as a single JSON line.
#[derive(Debug, Serialize, PartialEq)]
#[serde(untagged)]
pub enum Response {
    Exec {
        ok: bool, // always true on this variant
        exit_code: i32,
        stdout: String,
        stderr: String,
        truncated: bool,
    },
    Ok {
        ok: bool, // always true
    },
    Err {
        ok: bool, // always false
        error: String,
    },
}

impl Response {
    pub fn ok() -> Self {
        Response::Ok { ok: true }
    }

    pub fn error(msg: impl Into<String>) -> Self {
        Response::Err {
            ok: false,
            error: msg.into(),
        }
    }

    fn from_exec(o: ExecOutcome) -> Self {
        Response::Exec {
            ok: true,
            exit_code: o.exit_code,
            stdout: o.stdout,
            stderr: o.stderr,
            truncated: o.truncated,
        }
    }
}

/// Clamp a requested timeout to `[1, MAX_TIMEOUT_MS]`, defaulting when absent.
pub fn resolve_timeout(timeout_ms: Option<u64>) -> Duration {
    let ms = match timeout_ms {
        None => DEFAULT_TIMEOUT_MS,
        Some(0) => DEFAULT_TIMEOUT_MS,
        Some(v) => v.min(MAX_TIMEOUT_MS),
    };
    Duration::from_millis(ms)
}

/// Parse + validate a request line and produce the response. The `IpRunner` is
/// injected so `set_ip` can be tested without touching the real network stack.
pub fn dispatch<R: IpRunner>(line: &str, ip_runner: &R) -> Response {
    let req: Request = match serde_json::from_str(line) {
        Ok(r) => r,
        Err(e) => return Response::error(format!("bad request: {e}")),
    };

    match req {
        Request::Exec { cmd, timeout_ms } => {
            if cmd.is_empty() {
                return Response::error("cmd must be a non-empty array");
            }
            let timeout = resolve_timeout(timeout_ms);
            match run_exec(&cmd, timeout) {
                Ok(outcome) => Response::from_exec(outcome),
                Err(e) => Response::error(format!("spawn failed: {e}")),
            }
        }
        Request::SetIp {
            ip,
            prefix,
            gw,
            dev,
        } => match apply_set_ip(ip_runner, &ip, prefix, &gw, &dev) {
            Ok(()) => Response::ok(),
            Err(e) => Response::error(e),
        },
    }
}

/// Per-connection handler: read one line (bounded), dispatch, write one JSON
/// line, return. Generic over the transport so tests use a socket pair.
pub fn handle_connection<S: Read + Write>(stream: S) -> std::io::Result<()> {
    handle_connection_with(stream, &RealIpRunner)
}

/// Variant accepting an injected `IpRunner` (used by tests).
pub fn handle_connection_with<S: Read + Write, R: IpRunner>(
    mut stream: S,
    ip_runner: &R,
) -> std::io::Result<()> {
    let response = match read_line_bounded(&mut stream, MAX_REQUEST_BYTES) {
        Ok(Some(line)) => dispatch(&line, ip_runner),
        Ok(None) => return Ok(()), // empty connection, nothing to do
        Err(LineError::TooLong) => Response::error("request too long"),
        Err(LineError::Io(e)) => return Err(e),
    };
    write_response(&mut stream, &response)
}

fn write_response<S: Write>(w: &mut S, resp: &Response) -> std::io::Result<()> {
    let mut bytes = serde_json::to_vec(resp)
        .unwrap_or_else(|_| br#"{"ok":false,"error":"serialize failed"}"#.to_vec());
    bytes.push(b'\n');
    w.write_all(&bytes)?;
    w.flush()
}

enum LineError {
    TooLong,
    Io(std::io::Error),
}

/// Read a single `\n`-terminated line, rejecting anything longer than `max`
/// bytes (counted before the newline). Returns `Ok(None)` on immediate EOF.
/// Reads directly off the stream in 4 KiB chunks (no BufReader, so the caller
/// keeps full ownership of the stream for the subsequent write).
fn read_line_bounded<S: Read>(stream: &mut S, max: usize) -> Result<Option<String>, LineError> {
    let mut buf: Vec<u8> = Vec::with_capacity(256);
    let mut chunk = [0u8; 4096];
    loop {
        let n = match stream.read(&mut chunk) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) => return Err(LineError::Io(e)),
        };
        for &b in &chunk[..n] {
            if b == b'\n' {
                return Ok(Some(String::from_utf8_lossy(&buf).into_owned()));
            }
            buf.push(b);
            if buf.len() > max {
                return Err(LineError::TooLong);
            }
        }
    }
    if buf.is_empty() {
        Ok(None)
    } else {
        // EOF without trailing newline: accept the accumulated bytes.
        Ok(Some(String::from_utf8_lossy(&buf).into_owned()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::netcfg::tests::FakeIpRunner;

    fn dispatch_fake(line: &str) -> Response {
        dispatch(line, &FakeIpRunner::default())
    }

    #[test]
    fn rejects_malformed_json() {
        match dispatch_fake("not json") {
            Response::Err { ok, error } => {
                assert!(!ok);
                assert!(error.contains("bad request"));
            }
            other => panic!("expected error, got {other:?}"),
        }
    }

    #[test]
    fn rejects_unknown_op() {
        assert!(matches!(
            dispatch_fake(r#"{"op":"frobnicate"}"#),
            Response::Err { ok: false, .. }
        ));
    }

    #[test]
    fn rejects_empty_cmd() {
        match dispatch_fake(r#"{"op":"exec","cmd":[]}"#) {
            Response::Err { ok: false, error } => assert!(error.contains("non-empty")),
            other => panic!("expected error, got {other:?}"),
        }
    }

    #[test]
    fn resolve_timeout_defaults_and_caps() {
        assert_eq!(resolve_timeout(None), Duration::from_millis(DEFAULT_TIMEOUT_MS));
        assert_eq!(resolve_timeout(Some(0)), Duration::from_millis(DEFAULT_TIMEOUT_MS));
        assert_eq!(resolve_timeout(Some(1500)), Duration::from_millis(1500));
        assert_eq!(
            resolve_timeout(Some(999_999)),
            Duration::from_millis(MAX_TIMEOUT_MS)
        );
    }

    #[test]
    fn set_ip_routes_to_runner_and_returns_ok() {
        let runner = FakeIpRunner::default();
        let resp = dispatch(
            r#"{"op":"set_ip","ip":"10.231.0.7","prefix":24,"gw":"10.231.0.1","dev":"eth0"}"#,
            &runner,
        );
        assert_eq!(resp, Response::ok());
        let calls = runner.calls();
        // flush, add, link set up, route replace
        assert_eq!(calls.len(), 4);
        assert!(calls[0].contains(&"flush".to_string()));
        assert!(calls[1].iter().any(|a| a == "10.231.0.7/24"));
    }

    #[test]
    fn read_line_bounded_rejects_overlong() {
        let big = format!("{}\n", "x".repeat(MAX_REQUEST_BYTES + 10));
        let mut cursor = std::io::Cursor::new(big.into_bytes());
        match read_line_bounded(&mut cursor, MAX_REQUEST_BYTES) {
            Err(LineError::TooLong) => {}
            _ => panic!("expected TooLong"),
        }
    }

    #[test]
    fn read_line_bounded_eof_without_newline() {
        let mut cursor = std::io::Cursor::new(b"abc".to_vec());
        assert_eq!(
            read_line_bounded(&mut cursor, MAX_REQUEST_BYTES).ok().flatten(),
            Some("abc".to_string())
        );
    }

    #[test]
    fn read_line_bounded_empty_is_none() {
        let mut cursor = std::io::Cursor::new(Vec::new());
        assert!(read_line_bounded(&mut cursor, MAX_REQUEST_BYTES)
            .ok()
            .flatten()
            .is_none());
    }
}
