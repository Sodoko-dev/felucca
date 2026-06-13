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
        /// When true the response is a sequence of JSON lines — chunk frames
        /// `{"stream":"stdout"|"stderr","data":...}` followed by a terminal
        /// `{"done":true,...}` line — instead of a single buffered response.
        #[serde(default)]
        stream: bool,
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
        // `stream: true` never reaches dispatch (handle_connection_with routes
        // it to the streaming path, which owns the write side); a direct
        // dispatch call falls back to the buffered behavior.
        Request::Exec { cmd, timeout_ms, .. } => {
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
            Ok(()) => {
                // Snapshot-restored guests answer host-side ARP only after
                // their first transmit; announce makes that transmit happen
                // now instead of at the guest's first organic packet.
                ip_runner.announce(&gw);
                Response::ok()
            }
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
    let line = match read_line_bounded(&mut stream, MAX_REQUEST_BYTES) {
        Ok(Some(line)) => line,
        Ok(None) => return Ok(()), // empty connection, nothing to do
        Err(LineError::TooLong) => {
            return write_response(&mut stream, &Response::error("request too long"))
        }
        Err(LineError::Io(e)) => return Err(e),
    };
    // A streaming exec owns the write side for its whole run; everything else
    // goes through the one-line dispatch.
    if let Ok(Request::Exec {
        cmd,
        timeout_ms,
        stream: true,
    }) = serde_json::from_str::<Request>(&line)
    {
        return handle_exec_stream(&mut stream, &cmd, timeout_ms);
    }
    write_response(&mut stream, &dispatch(&line, ip_runner))
}

/// Run a streamed exec, writing chunk frames and a terminal `done` frame as
/// JSON lines on the connection. Failures before/during the run terminate the
/// stream with `{"done":true,"ok":false,...}` — except write failures (the
/// peer hung up), which just end the connection.
fn handle_exec_stream<S: Write>(
    stream: &mut S,
    cmd: &[String],
    timeout_ms: Option<u64>,
) -> std::io::Result<()> {
    if cmd.is_empty() {
        return write_frame(
            stream,
            &serde_json::json!({"done": true, "ok": false, "error": "cmd must be a non-empty array"}),
        );
    }
    let timeout = resolve_timeout(timeout_ms);
    let mut emit = |kind: crate::exec::StreamKind, data: &str| {
        let name = match kind {
            crate::exec::StreamKind::Stdout => "stdout",
            crate::exec::StreamKind::Stderr => "stderr",
        };
        write_frame(stream, &serde_json::json!({"stream": name, "data": data}))
    };
    match crate::exec::run_exec_streamed(cmd, timeout, &mut emit) {
        Ok(end) => write_frame(
            stream,
            &serde_json::json!({
                "done": true,
                "ok": true,
                "exit_code": end.exit_code,
                "truncated": end.truncated,
            }),
        ),
        // BrokenPipe-class emit failures land here too; the terminal write
        // then fails the same way and the error propagates to the accept loop,
        // which logs and moves on (same as any client disconnect).
        Err(e) => write_frame(
            stream,
            &serde_json::json!({"done": true, "ok": false, "error": format!("exec failed: {e}")}),
        ),
    }
}

/// Write one JSON value as a `\n`-terminated line and flush (each frame must
/// hit the wire immediately — streaming is the point).
fn write_frame<S: Write>(w: &mut S, v: &serde_json::Value) -> std::io::Result<()> {
    let mut bytes = serde_json::to_vec(v)
        .unwrap_or_else(|_| br#"{"done":true,"ok":false,"error":"serialize failed"}"#.to_vec());
    bytes.push(b'\n');
    w.write_all(&bytes)?;
    w.flush()
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
    fn set_ip_announces_presence_after_success() {
        let runner = FakeIpRunner::default();
        let resp = dispatch(
            r#"{"op":"set_ip","ip":"10.231.0.7","prefix":24,"gw":"10.231.0.1","dev":"eth0"}"#,
            &runner,
        );
        assert_eq!(resp, Response::ok());
        assert_eq!(runner.announced(), vec!["10.231.0.1".to_string()]);
    }

    #[test]
    fn set_ip_does_not_announce_on_failure() {
        let runner = FakeIpRunner::default();
        let resp = dispatch(
            r#"{"op":"set_ip","ip":"999.1.1.1","prefix":24,"gw":"10.231.0.1","dev":"eth0"}"#,
            &runner,
        );
        assert!(matches!(resp, Response::Err { .. }));
        assert!(runner.announced().is_empty());
    }

    /// In-memory Read+Write stream: reads from a canned request, records writes.
    struct FakeStream {
        input: std::io::Cursor<Vec<u8>>,
        output: Vec<u8>,
    }

    impl FakeStream {
        fn new(request: &str) -> Self {
            FakeStream {
                input: std::io::Cursor::new(request.as_bytes().to_vec()),
                output: Vec::new(),
            }
        }

        fn frames(&self) -> Vec<serde_json::Value> {
            String::from_utf8_lossy(&self.output)
                .lines()
                .map(|l| serde_json::from_str(l).expect("each output line is JSON"))
                .collect()
        }
    }

    impl Read for FakeStream {
        fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
            self.input.read(buf)
        }
    }

    impl Write for FakeStream {
        fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
            self.output.extend_from_slice(buf);
            Ok(buf.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            Ok(())
        }
    }

    #[test]
    fn streamed_exec_emits_chunk_frames_then_done() {
        let mut s = FakeStream::new(
            "{\"op\":\"exec\",\"cmd\":[\"/bin/sh\",\"-c\",\"echo hi; echo oops 1>&2\"],\"stream\":true}\n",
        );
        handle_connection_with(&mut s, &FakeIpRunner::default()).unwrap();
        let frames = s.frames();
        assert!(frames.len() >= 3, "expected >=3 frames, got {frames:?}");
        let done = frames.last().unwrap();
        assert_eq!(done["done"], serde_json::json!(true));
        assert_eq!(done["ok"], serde_json::json!(true));
        assert_eq!(done["exit_code"], serde_json::json!(0));
        assert_eq!(done["truncated"], serde_json::json!(false));
        let stdout: String = frames
            .iter()
            .filter(|f| f["stream"] == serde_json::json!("stdout"))
            .map(|f| f["data"].as_str().unwrap())
            .collect();
        let stderr: String = frames
            .iter()
            .filter(|f| f["stream"] == serde_json::json!("stderr"))
            .map(|f| f["data"].as_str().unwrap())
            .collect();
        assert_eq!(stdout, "hi\n");
        assert_eq!(stderr, "oops\n");
    }

    #[test]
    fn streamed_exec_empty_cmd_is_done_error() {
        let mut s = FakeStream::new("{\"op\":\"exec\",\"cmd\":[],\"stream\":true}\n");
        handle_connection_with(&mut s, &FakeIpRunner::default()).unwrap();
        let frames = s.frames();
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0]["done"], serde_json::json!(true));
        assert_eq!(frames[0]["ok"], serde_json::json!(false));
    }

    #[test]
    fn streamed_exec_nonzero_exit_in_done_frame() {
        let mut s =
            FakeStream::new("{\"op\":\"exec\",\"cmd\":[\"/bin/sh\",\"-c\",\"exit 9\"],\"stream\":true}\n");
        handle_connection_with(&mut s, &FakeIpRunner::default()).unwrap();
        let done = s.frames().pop().unwrap();
        assert_eq!(done["exit_code"], serde_json::json!(9));
    }

    #[test]
    fn buffered_exec_unaffected_by_stream_false() {
        let mut s = FakeStream::new(
            "{\"op\":\"exec\",\"cmd\":[\"/bin/sh\",\"-c\",\"echo hi\"],\"stream\":false}\n",
        );
        handle_connection_with(&mut s, &FakeIpRunner::default()).unwrap();
        let frames = s.frames();
        assert_eq!(frames.len(), 1, "buffered exec is one response line");
        assert_eq!(frames[0]["ok"], serde_json::json!(true));
        assert_eq!(frames[0]["stdout"], serde_json::json!("hi\n"));
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
