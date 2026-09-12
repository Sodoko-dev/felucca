//! End-to-end test of the per-connection handler over a real Unix socket pair.
//!
//! This exercises the exact same `handle_connection` code path the vsock
//! listener uses, but over an in-process `UnixStream` pair so no AF_VSOCK
//! device is required. Only the public API is available here, so `set_ip`
//! goes through the real runner — we therefore only test `exec` and malformed
//! input end-to-end (set_ip's runner injection is unit-tested in-crate).

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::UnixStream;
use std::thread;

use felucca_guest::handle_connection;

/// Send `request` over a socket pair handled by `handle_connection`, return the
/// single response line (without the trailing newline).
fn roundtrip(request: &str) -> String {
    let (mut client, server) = UnixStream::pair().expect("socketpair");

    let handler = thread::spawn(move || {
        handle_connection(server).expect("handler");
    });

    client.write_all(request.as_bytes()).unwrap();
    client.write_all(b"\n").unwrap();
    client.flush().unwrap();
    // Signal EOF on the write half so the handler's read can complete even if it
    // keeps reading after the newline.
    client.shutdown(std::net::Shutdown::Write).unwrap();

    let mut reader = BufReader::new(client);
    let mut line = String::new();
    reader.read_line(&mut line).unwrap();
    handler.join().unwrap();
    line.trim_end().to_string()
}

#[test]
fn exec_echo_roundtrip() {
    // Mirrors the §5 acceptance command. The uid is not asserted here because
    // the test host is not root; inside the real guest the agent runs as root
    // and `id -u` yields 0. We assert the literal echo output and exit code.
    let resp = roundtrip(r#"{"op":"exec","cmd":["/bin/sh","-c","echo from-guest; id -u"]}"#);
    let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(v["ok"], true);
    assert_eq!(v["exit_code"], 0);
    let stdout = v["stdout"].as_str().unwrap();
    assert!(stdout.starts_with("from-guest\n"), "got {stdout:?}");
    // Second line is the numeric uid.
    let uid_line = stdout.lines().nth(1).unwrap();
    assert!(uid_line.parse::<u32>().is_ok(), "uid line was {uid_line:?}");
}

#[test]
fn exec_nonzero_exit() {
    let resp = roundtrip(r#"{"op":"exec","cmd":["/bin/sh","-c","exit 3"]}"#);
    let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(v["ok"], true);
    assert_eq!(v["exit_code"], 3);
}

#[test]
fn exec_timeout_returns_124() {
    let resp = roundtrip(r#"{"op":"exec","cmd":["/bin/sh","-c","sleep 30"],"timeout_ms":300}"#);
    let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(v["exit_code"], 124);
}

#[test]
fn malformed_json_returns_error() {
    let resp = roundtrip("this is not json");
    let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(v["ok"], false);
    assert!(v["error"].as_str().unwrap().contains("bad request"));
}

#[test]
fn empty_cmd_returns_error() {
    let resp = roundtrip(r#"{"op":"exec","cmd":[]}"#);
    let v: serde_json::Value = serde_json::from_str(&resp).unwrap();
    assert_eq!(v["ok"], false);
}
