//! Host→guest client over Firecracker hybrid vsock.
//!
//! Firecracker exposes guest vsock ports as a host Unix domain socket. To reach
//! guest port 52 we connect to `<instance_dir>/v.sock`, send `CONNECT 52\n`, and
//! wait for an `OK <port>\n` line from Firecracker. After that the byte stream is
//! a direct pipe to the guest listener, over which we speak the §1 line protocol:
//! one JSON request line, one JSON response line, then the guest closes.

use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::UnixStream;
use tokio::time::timeout;

/// Cap on a single response line (defensive; guest caps stdout/stderr at 1 MiB each).
const MAX_RESPONSE: usize = 4 * 1024 * 1024;
/// Firecracker guest-agent vsock port (per §1).
const GUEST_PORT: u32 = 52;

#[derive(Debug)]
pub struct GuestError(pub String);

impl std::fmt::Display for GuestError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "GuestError: {}", self.0)
    }
}
impl std::error::Error for GuestError {}

pub type Result<T> = std::result::Result<T, GuestError>;

/// Connect to the FC hybrid-vsock UDS at `<dir>/v.sock`, complete the
/// `CONNECT <port>` handshake, send one request line, and read one response line.
/// The whole exchange is bounded by `deadline_ms`.
async fn round_trip(dir: &str, request_line: &str, deadline_ms: u64) -> Result<String> {
    let uds = format!("{}/v.sock", dir);
    let fut = round_trip_inner(&uds, request_line);
    match timeout(Duration::from_millis(deadline_ms), fut).await {
        Ok(r) => r,
        Err(_) => Err(GuestError(format!("guest round-trip timed out after {}ms", deadline_ms))),
    }
}

async fn round_trip_inner(uds: &str, request_line: &str) -> Result<String> {
    let mut stream = UnixStream::connect(uds)
        .await
        .map_err(|e| GuestError(format!("connect {}: {}", uds, e)))?;

    // Firecracker hybrid-vsock handshake: ask to connect to the guest port.
    let connect = format!("CONNECT {}\n", GUEST_PORT);
    stream
        .write_all(connect.as_bytes())
        .await
        .map_err(|e| GuestError(format!("write CONNECT: {}", e)))?;

    // Read a single line; Firecracker replies `OK <assigned_port>\n` on success,
    // or closes / errors when no guest is listening on the port.
    let ok_line = read_line(&mut stream).await?;
    if !ok_line.starts_with("OK ") {
        return Err(GuestError(format!("handshake: expected 'OK <port>', got {:?}", ok_line)));
    }

    // Now speak the guest line protocol: one JSON request, one JSON response.
    let mut req = request_line.to_string();
    if !req.ends_with('\n') {
        req.push('\n');
    }
    stream
        .write_all(req.as_bytes())
        .await
        .map_err(|e| GuestError(format!("write request: {}", e)))?;

    let resp = read_line(&mut stream).await?;
    Ok(resp)
}

/// Read one `\n`-terminated line (the `\n` is stripped), capped at MAX_RESPONSE.
async fn read_line(stream: &mut UnixStream) -> Result<String> {
    let mut buf: Vec<u8> = Vec::with_capacity(256);
    let mut byte = [0u8; 1];
    loop {
        let n = stream
            .read(&mut byte)
            .await
            .map_err(|e| GuestError(format!("read: {}", e)))?;
        if n == 0 {
            // EOF before newline.
            if buf.is_empty() {
                return Err(GuestError("connection closed before any data".into()));
            }
            break;
        }
        if byte[0] == b'\n' {
            break;
        }
        buf.push(byte[0]);
        if buf.len() > MAX_RESPONSE {
            return Err(GuestError(format!("response exceeded {} bytes", MAX_RESPONSE)));
        }
    }
    Ok(String::from_utf8_lossy(&buf).into_owned())
}

/// Run `cmd` in the guest, returning the guest's parsed JSON response verbatim.
/// `timeout_ms` is the guest-side command timeout; the host deadline adds a 5s
/// margin so we don't give up before the guest reports a timeout itself.
pub async fn exec(dir: &str, cmd: &[String], timeout_ms: u64) -> Result<serde_json::Value> {
    let request = serde_json::json!({
        "op": "exec",
        "cmd": cmd,
        "timeout_ms": timeout_ms,
    });
    let line = serde_json::to_string(&request)
        .map_err(|e| GuestError(format!("encode request: {}", e)))?;
    let deadline = timeout_ms.saturating_add(5_000);
    let resp = round_trip(dir, &line, deadline).await?;
    serde_json::from_str(&resp)
        .map_err(|e| GuestError(format!("decode response {:?}: {}", resp, e)))
}

/// Open a streamed exec (v5 P5.1): connect, complete the CONNECT handshake,
/// send the request with `"stream": true`, and return the stream positioned at
/// the first response byte. From there the guest writes NDJSON frames
/// (`{"stream":...,"data":...}` chunks, then one `{"done":true,...}` line) and
/// closes. Only the setup is bounded here (10s); the caller owns the
/// data-phase deadline, sized to the command's timeout.
pub async fn exec_stream(dir: &str, cmd: &[String], timeout_ms: u64) -> Result<UnixStream> {
    let request = serde_json::json!({
        "op": "exec",
        "cmd": cmd,
        "timeout_ms": timeout_ms,
        "stream": true,
    });
    let line = serde_json::to_string(&request)
        .map_err(|e| GuestError(format!("encode request: {}", e)))?;
    let uds = format!("{}/v.sock", dir);

    let setup = async {
        let mut stream = UnixStream::connect(&uds)
            .await
            .map_err(|e| GuestError(format!("connect {}: {}", uds, e)))?;
        let connect = format!("CONNECT {}\n", GUEST_PORT);
        stream
            .write_all(connect.as_bytes())
            .await
            .map_err(|e| GuestError(format!("write CONNECT: {}", e)))?;
        let ok_line = read_line(&mut stream).await?;
        if !ok_line.starts_with("OK ") {
            return Err(GuestError(format!(
                "handshake: expected 'OK <port>', got {:?}",
                ok_line
            )));
        }
        let mut req = line.clone();
        req.push('\n');
        stream
            .write_all(req.as_bytes())
            .await
            .map_err(|e| GuestError(format!("write request: {}", e)))?;
        Ok(stream)
    };
    match timeout(Duration::from_millis(10_000), setup).await {
        Ok(r) => r,
        Err(_) => Err(GuestError("guest stream setup timed out after 10000ms".into())),
    }
}

/// Reconfigure the guest's primary interface (fork re-IP). Best-effort: tries up
/// to 3 times, 1s apart, because the just-restored guest may still be settling.
pub async fn set_ip(dir: &str, ip: &str, prefix: u8, gw: &str) -> Result<()> {
    let request = serde_json::json!({
        "op": "set_ip",
        "ip": ip,
        "prefix": prefix,
        "gw": gw,
        "dev": "eth0",
    });
    let line = serde_json::to_string(&request)
        .map_err(|e| GuestError(format!("encode set_ip: {}", e)))?;

    let mut last_err = GuestError("set_ip: no attempts made".into());
    for attempt in 0..3 {
        if attempt > 0 {
            tokio::time::sleep(Duration::from_millis(1_000)).await;
        }
        match round_trip(dir, &line, 5_000).await {
            Ok(resp) => {
                let v: serde_json::Value = serde_json::from_str(&resp)
                    .map_err(|e| GuestError(format!("decode set_ip {:?}: {}", resp, e)))?;
                if v.get("ok").and_then(|b| b.as_bool()) == Some(true) {
                    return Ok(());
                }
                last_err = GuestError(format!("set_ip rejected: {}", resp));
            }
            Err(e) => last_err = e,
        }
    }
    Err(last_err)
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::UnixListener;

    /// Read one `\n`-terminated line from a server-side stream.
    async fn srv_read_line(stream: &mut tokio::net::UnixStream) -> String {
        let mut buf = Vec::new();
        let mut b = [0u8; 1];
        loop {
            let n = stream.read(&mut b).await.unwrap();
            if n == 0 || b[0] == b'\n' {
                break;
            }
            buf.push(b[0]);
        }
        String::from_utf8_lossy(&buf).into_owned()
    }

    /// Fake FC hybrid-vsock server: speaks the CONNECT handshake then echoes a
    /// canned response line. Returns the request line the client sent.
    async fn fake_server(
        uds: String,
        ok_line: &'static str,
        response_line: &'static str,
    ) -> tokio::task::JoinHandle<String> {
        let listener = UnixListener::bind(&uds).unwrap();
        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            // Expect CONNECT 52
            let connect = srv_read_line(&mut stream).await;
            assert_eq!(connect, "CONNECT 52");
            stream.write_all(ok_line.as_bytes()).await.unwrap();
            // Read the JSON request line.
            let req = srv_read_line(&mut stream).await;
            stream.write_all(response_line.as_bytes()).await.unwrap();
            let _ = stream.shutdown().await;
            req
        })
    }

    fn tmp_uds(name: &str) -> (tempfile::TempDir, String) {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().to_str().unwrap().to_string();
        (dir, format!("{}/{}", path, name))
    }

    #[tokio::test]
    async fn test_exec_framing_against_fake_server() {
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);

        let handle = fake_server(
            uds,
            "OK 1234\n",
            "{\"ok\":true,\"exit_code\":0,\"stdout\":\"hi\\n\",\"stderr\":\"\"}\n",
        )
        .await;

        let cmd = vec!["/bin/sh".to_string(), "-c".to_string(), "echo hi".to_string()];
        let resp = exec(&dir_path, &cmd, 30_000).await.expect("exec ok");
        assert_eq!(resp["ok"], serde_json::json!(true));
        assert_eq!(resp["exit_code"], serde_json::json!(0));
        assert_eq!(resp["stdout"], serde_json::json!("hi\n"));

        // Verify the exact request line the client framed.
        let req = handle.await.unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&req).unwrap();
        assert_eq!(parsed["op"], serde_json::json!("exec"));
        assert_eq!(parsed["cmd"], serde_json::json!(["/bin/sh", "-c", "echo hi"]));
        assert_eq!(parsed["timeout_ms"], serde_json::json!(30_000));
    }

    #[tokio::test]
    async fn test_exec_stream_framing_and_passthrough() {
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);

        let handle = fake_server(
            uds,
            "OK 7\n",
            "{\"stream\":\"stdout\",\"data\":\"a\"}\n{\"done\":true,\"ok\":true,\"exit_code\":0,\"truncated\":false}\n",
        )
        .await;

        let cmd = vec!["true".to_string()];
        let mut stream = exec_stream(&dir_path, &cmd, 5_000).await.expect("stream setup");
        let mut buf = String::new();
        stream.read_to_string(&mut buf).await.unwrap();
        let lines: Vec<&str> = buf.lines().collect();
        assert_eq!(lines.len(), 2);
        assert!(lines[0].contains("\"stream\":\"stdout\""));
        assert!(lines[1].contains("\"done\":true"));

        // The request line must carry the stream flag.
        let req = handle.await.unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&req).unwrap();
        assert_eq!(parsed["op"], serde_json::json!("exec"));
        assert_eq!(parsed["stream"], serde_json::json!(true));
        assert_eq!(parsed["timeout_ms"], serde_json::json!(5_000));
    }

    #[tokio::test]
    async fn test_set_ip_framing_against_fake_server() {
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);

        let handle = fake_server(uds, "OK 5\n", "{\"ok\":true}\n").await;

        set_ip(&dir_path, "10.231.0.7", 24, "10.231.0.1")
            .await
            .expect("set_ip ok");

        let req = handle.await.unwrap();
        let parsed: serde_json::Value = serde_json::from_str(&req).unwrap();
        assert_eq!(parsed["op"], serde_json::json!("set_ip"));
        assert_eq!(parsed["ip"], serde_json::json!("10.231.0.7"));
        assert_eq!(parsed["prefix"], serde_json::json!(24));
        assert_eq!(parsed["gw"], serde_json::json!("10.231.0.1"));
        assert_eq!(parsed["dev"], serde_json::json!("eth0"));
    }

    #[tokio::test]
    async fn test_handshake_failure_no_ok() {
        // Server immediately replies with a non-OK line (no guest listener).
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);

        let listener = UnixListener::bind(&uds).unwrap();
        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let _ = srv_read_line(&mut stream).await;
            // Firecracker error reply when nothing listens on the port.
            stream.write_all(b"ERROR 113\n").await.unwrap();
            let _ = stream.shutdown().await;
        });

        let err = exec(&dir_path, &["true".to_string()], 1000).await.unwrap_err();
        assert!(err.0.contains("handshake"), "expected handshake error, got: {}", err.0);
    }

    #[tokio::test]
    async fn test_connect_failure_no_socket() {
        // No server bound → connect fails.
        let (_dir, uds) = tmp_uds("nope");
        let parent = std::path::Path::new(&uds).parent().unwrap().to_str().unwrap();
        let err = exec(parent, &["true".to_string()], 1000).await.unwrap_err();
        assert!(err.0.contains("connect"), "expected connect error, got: {}", err.0);
    }
}
