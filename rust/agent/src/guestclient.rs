//! Host→guest client over Firecracker hybrid vsock.
//!
//! Firecracker exposes guest vsock ports as a host Unix domain socket. To reach
//! guest port 52 we connect to `<instance_dir>/v.sock`, send `CONNECT 52\n`, and
//! wait for an `OK <port>\n` line from Firecracker. After that the byte stream is
//! a direct pipe to the guest listener, over which we speak the §1 line protocol:
//! one JSON request line, one JSON response line, then the guest closes.

use std::time::Duration;

use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;
use tokio::sync::Semaphore;
use tokio::time::timeout;

/// Cap on a single response line (defensive; guest caps stdout/stderr at 1 MiB each).
const MAX_RESPONSE: usize = 4 * 1024 * 1024;
/// Firecracker guest-agent vsock port (per §1).
const GUEST_PORT: u32 = 52;
/// Cap on Firecracker's own `OK <port>` / `ERROR <n>` handshake reply. It is
/// read a byte at a time so nothing beyond the newline is buffered (the
/// streamed path hands the raw socket on), which is only affordable because
/// the line is a dozen bytes and comes from Firecracker, not the guest.
const MAX_HANDSHAKE: usize = 128;

/// Guest round-trips in flight across the whole node.
///
/// The tenant is root inside their VM and can replace the guest agent with a
/// listener of their choosing, so the work each round-trip costs the agent is
/// theirs to pick — and the agent's runtime serves every other tenant on the
/// node. The permit is taken inside the caller's deadline, so waiting for a
/// slot surfaces as that call's timeout rather than a spurious failure.
const MAX_CONCURRENT_GUEST_CALLS: usize = 32;
static GUEST_SLOTS: Semaphore = Semaphore::const_new(MAX_CONCURRENT_GUEST_CALLS);

/// A held guest round-trip permit; the slot is released when it is dropped.
/// The streamed path hands one to its caller, which must keep it alive for as
/// long as the guest connection is being read.
pub type GuestSlot = tokio::sync::SemaphorePermit<'static>;

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
    // Held for the whole exchange, including the connect: an unslotted client
    // must not even open the socket.
    let _slot = acquire_slot().await?;
    let stream = UnixStream::connect(uds)
        .await
        .map_err(|e| GuestError(format!("connect {}: {}", uds, e)))?;
    let mut stream = BufReader::new(stream);

    // Firecracker hybrid-vsock handshake: ask to connect to the guest port.
    let connect = format!("CONNECT {}\n", GUEST_PORT);
    stream
        .write_all(connect.as_bytes())
        .await
        .map_err(|e| GuestError(format!("write CONNECT: {}", e)))?;

    // Read a single line; Firecracker replies `OK <assigned_port>\n` on success,
    // or closes / errors when no guest is listening on the port.
    let ok_line = read_handshake_line(stream.get_mut()).await?;
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

    let resp = read_response_line(&mut stream).await?;
    Ok(resp)
}

/// Take a guest round-trip slot, or report the wait as the caller's failure.
async fn acquire_slot() -> Result<GuestSlot> {
    GUEST_SLOTS
        .acquire()
        .await
        .map_err(|_| GuestError("guest round-trip semaphore closed".into()))
}

/// Read Firecracker's handshake reply one byte at a time, so nothing past the
/// newline is consumed — `exec_stream` hands the raw socket to its caller and
/// any over-read byte would be a guest frame lost. Bounded by MAX_HANDSHAKE.
async fn read_handshake_line(stream: &mut UnixStream) -> Result<String> {
    let mut buf: Vec<u8> = Vec::with_capacity(MAX_HANDSHAKE);
    let mut byte = [0u8; 1];
    loop {
        let n = stream
            .read(&mut byte)
            .await
            .map_err(|e| GuestError(format!("read: {}", e)))?;
        if n == 0 {
            if buf.is_empty() {
                return Err(GuestError("connection closed before any data".into()));
            }
            break;
        }
        if byte[0] == b'\n' {
            break;
        }
        buf.push(byte[0]);
        if buf.len() > MAX_HANDSHAKE {
            return Err(GuestError(format!("handshake exceeded {} bytes", MAX_HANDSHAKE)));
        }
    }
    Ok(String::from_utf8_lossy(&buf).into_owned())
}

/// Read one `\n`-terminated line (the `\n` is stripped), capped at MAX_RESPONSE.
///
/// Buffered, and bounded by `take` rather than by checking after each byte:
/// this content is entirely the tenant's to choose, and a byte-at-a-time loop
/// let one 4 MiB newline-free answer cost four million read syscalls and as
/// many wakeups on the runtime every other tenant shares. The same answer is
/// now ~1000 reads.
async fn read_response_line<R: tokio::io::AsyncBufRead + Unpin>(reader: &mut R) -> Result<String> {
    let mut buf: Vec<u8> = Vec::with_capacity(256);
    // +1 so a line of exactly MAX_RESPONSE bytes still reads its terminator.
    let mut limited = reader.take(MAX_RESPONSE as u64 + 1);
    let n = limited
        .read_until(b'\n', &mut buf)
        .await
        .map_err(|e| GuestError(format!("read: {}", e)))?;
    if n == 0 && buf.is_empty() {
        return Err(GuestError("connection closed before any data".into()));
    }
    if buf.last() == Some(&b'\n') {
        buf.pop();
    } else if buf.len() > MAX_RESPONSE {
        // Cap reached with no terminator in sight.
        return Err(GuestError(format!("response exceeded {} bytes", MAX_RESPONSE)));
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
///
/// The returned `GuestSlot` is this call's round-trip permit: it must stay
/// alive for as long as the stream is read, or the concurrency bound only
/// covers the handshake.
pub async fn exec_stream(
    dir: &str,
    cmd: &[String],
    timeout_ms: u64,
) -> Result<(UnixStream, GuestSlot)> {
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
        let slot = acquire_slot().await?;
        let mut stream = UnixStream::connect(&uds)
            .await
            .map_err(|e| GuestError(format!("connect {}: {}", uds, e)))?;
        let connect = format!("CONNECT {}\n", GUEST_PORT);
        stream
            .write_all(connect.as_bytes())
            .await
            .map_err(|e| GuestError(format!("write CONNECT: {}", e)))?;
        let ok_line = read_handshake_line(&mut stream).await?;
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
        Ok((stream, slot))
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
        let (mut stream, _slot) = exec_stream(&dir_path, &cmd, 5_000).await.expect("stream setup");
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

    // ---- L8: bounded, buffered reads and a concurrency ceiling ----

    /// Fake server that answers with `body` and never terminates the line.
    async fn endless_line_server(uds: String, body: Vec<u8>) {
        let listener = UnixListener::bind(&uds).unwrap();
        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let _ = srv_read_line(&mut stream).await; // CONNECT 52
            stream.write_all(b"OK 1\n").await.unwrap();
            let _ = srv_read_line(&mut stream).await; // request line
            let _ = stream.write_all(&body).await;
            let _ = stream.shutdown().await;
        });
    }

    #[tokio::test]
    async fn test_response_over_the_cap_is_rejected_not_buffered_forever() {
        // The tenant is root inside the VM and picks what comes back here, so
        // a newline-free answer must hit the cap instead of being read to EOF.
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        endless_line_server(format!("{}/v.sock", dir_path), vec![b'a'; MAX_RESPONSE + 4096]).await;

        let err = exec(&dir_path, &["true".to_string()], 10_000).await.unwrap_err();
        assert!(
            err.0.contains("exceeded"),
            "expected the cap to fire, got: {}",
            err.0
        );
    }

    #[tokio::test]
    async fn test_large_response_under_the_cap_round_trips_intact() {
        // Proves the buffered reader is not lossy at size: the same payload
        // used to cost one syscall per byte.
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let filler = "x".repeat(1024 * 1024);
        let mut body = format!(
            "{{\"ok\":true,\"exit_code\":0,\"stdout\":\"{}\",\"stderr\":\"\"}}",
            filler
        )
        .into_bytes();
        body.push(b'\n');
        endless_line_server(format!("{}/v.sock", dir_path), body).await;

        let resp = exec(&dir_path, &["true".to_string()], 30_000).await.expect("exec ok");
        assert_eq!(resp["stdout"].as_str().unwrap().len(), filler.len());
    }

    #[tokio::test]
    async fn test_handshake_line_is_capped() {
        // Firecracker's reply is a dozen bytes; a peer that never terminates
        // it must not be read forever either.
        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);
        let listener = UnixListener::bind(&uds).unwrap();
        tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let _ = srv_read_line(&mut stream).await;
            let _ = stream.write_all(&vec![b'O'; MAX_HANDSHAKE + 64]).await;
            let _ = stream.shutdown().await;
        });

        let err = exec(&dir_path, &["true".to_string()], 5_000).await.unwrap_err();
        assert!(
            err.0.contains("handshake exceeded"),
            "expected the handshake cap to fire, got: {}",
            err.0
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn test_concurrent_round_trips_are_capped() {
        // Was: nothing bounded concurrent execs, so a tenant could schedule
        // arbitrarily many guest reads on the runtime every tenant shares.
        use std::sync::atomic::{AtomicUsize, Ordering};
        use std::sync::Arc;

        let dir = tempfile::tempdir().unwrap();
        let dir_path = dir.path().to_str().unwrap().to_string();
        let uds = format!("{}/v.sock", dir_path);
        let listener = UnixListener::bind(&uds).unwrap();

        let live = Arc::new(AtomicUsize::new(0));
        let peak = Arc::new(AtomicUsize::new(0));
        {
            let (live, peak) = (Arc::clone(&live), Arc::clone(&peak));
            tokio::spawn(async move {
                loop {
                    let Ok((mut stream, _)) = listener.accept().await else { break };
                    let (live, peak) = (Arc::clone(&live), Arc::clone(&peak));
                    tokio::spawn(async move {
                        let n = live.fetch_add(1, Ordering::SeqCst) + 1;
                        peak.fetch_max(n, Ordering::SeqCst);
                        let _ = srv_read_line(&mut stream).await;
                        let _ = stream.write_all(b"OK 1\n").await;
                        let _ = srv_read_line(&mut stream).await;
                        // Hold the connection open long enough that every
                        // permitted caller overlaps with the others.
                        tokio::time::sleep(Duration::from_millis(150)).await;
                        let _ = stream.write_all(b"{\"ok\":true}\n").await;
                        let _ = stream.shutdown().await;
                        live.fetch_sub(1, Ordering::SeqCst);
                    });
                }
            });
        }

        let mut tasks = Vec::new();
        for _ in 0..(MAX_CONCURRENT_GUEST_CALLS * 2) {
            let d = dir_path.clone();
            tasks.push(tokio::spawn(async move {
                let _ = exec(&d, &["true".to_string()], 30_000).await;
            }));
        }
        for t in tasks {
            t.await.unwrap();
        }

        let observed = peak.load(Ordering::SeqCst);
        assert!(
            observed <= MAX_CONCURRENT_GUEST_CALLS,
            "peak concurrent guest round-trips {} exceeded the cap {}",
            observed,
            MAX_CONCURRENT_GUEST_CALLS
        );
        // The cap must actually be reached, or the test proves nothing.
        assert!(observed > 1, "expected real overlap, saw {}", observed);
    }

    #[tokio::test]
    async fn test_slot_is_released_after_a_failed_round_trip() {
        // A permit leaked on the error path would wedge the node after
        // MAX_CONCURRENT_GUEST_CALLS unreachable guests: the next caller waits
        // on a slot that is never coming back. More failures than there are
        // permits must still all complete.
        let (_dir, uds) = tmp_uds("nope");
        let parent = std::path::Path::new(&uds).parent().unwrap().to_str().unwrap();
        let all = tokio::time::timeout(Duration::from_secs(30), async {
            for _ in 0..(MAX_CONCURRENT_GUEST_CALLS + 4) {
                assert!(exec(parent, &["true".to_string()], 1000).await.is_err());
            }
        })
        .await;
        assert!(all.is_ok(), "a leaked permit wedged the guest client");
    }
}
